package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"projectorch/internal/agents/claude"
	"projectorch/internal/agents/codex"
	"projectorch/internal/agents/core"
	mockagent "projectorch/internal/agents/mock"
	"projectorch/internal/backup"
	"projectorch/internal/brand"
	"projectorch/internal/config"
	"projectorch/internal/decisions"
	"projectorch/internal/domain"
	"projectorch/internal/knowledge"
	"projectorch/internal/logging"
	"projectorch/internal/orchestrator"
	"projectorch/internal/ports"
	"projectorch/internal/proxy"
	"projectorch/internal/storage"
)

// System is everything Build assembled, so a caller (the TUI, a test) can hold
// one value and shut it down cleanly.
type System struct {
	Service      *Service
	Orchestrator *orchestrator.Orchestrator
	Agents       *core.Manager
	Storage      ports.Storage
	Knowledge    *knowledge.Store
	Proxy        ports.LLMProxy
	Config       *config.Config
	// Recovered lists the tasks that were mid-run when orch last exited and
	// are now paused as INTERRUPTED.
	Recovered []domain.TaskID
	// Workspace is where the agents work and the only place they may write.
	Workspace config.Workspace
	// Backups snapshots the project's database into the app home; nil when
	// backups are off or there is no app home (as in tests).
	Backups *backup.Manager

	closeStore  func() error
	stopBackups context.CancelFunc
	backupsBusy sync.WaitGroup
	backupsMu   sync.Mutex
	closing     bool
	log         *logging.Logger
}

// Close takes a last backup and releases the storage. Call it once, after
// the TUI or demo is done.
func (s *System) Close() error {
	if s.closeStore == nil {
		return nil
	}
	if s.Backups != nil {
		s.backupsMu.Lock()
		s.closing = true // no new backup starts from here
		s.backupsMu.Unlock()
		s.stopBackups()
		s.backupsBusy.Wait()
		if _, err := s.Backups.Snapshot(context.Background(), "exit"); err != nil {
			s.log.Warn("backup on exit", "error", err)
		}
	}
	err := s.closeStore()
	s.closeStore = nil
	return err
}

// openStorage builds the configured backend. For SQLite it also returns the
// database, for backups, and takes a backup before upgrading an existing
// database's schema when backups are on.
func openStorage(cfg *config.Config, log *logging.Logger) (*ports.Storage, *storage.SQLite, func() error, error) {
	switch cfg.Storage.Backend {
	case "", "memory":
		return storage.NewInMemory(), nil, func() error { return nil }, nil
	case "sqlite":
		if cfg.Storage.Path == "" {
			return nil, nil, nil, fmt.Errorf("storage: no database path")
		}
		var opts storage.Options
		if cfg.Backups.Enabled && cfg.Runtime.BackupDir != "" {
			opts.BeforeUpgrade = func(db *storage.SQLite, from, to int) error {
				path, err := backup.Take(context.Background(), db, cfg.Runtime.BackupDir, fmt.Sprintf("before-upgrade-v%d", from))
				if err == nil {
					log.Info("backup taken before upgrading the database", "from", from, "to", to, "file", path)
				}
				return err
			}
		}
		db, err := storage.OpenSQLiteWith(cfg.Storage.Path, opts)
		if err != nil {
			return nil, nil, nil, err
		}
		return db.Storage(), db, db.Close, nil
	default:
		return nil, nil, nil, fmt.Errorf("unknown storage backend %q", cfg.Storage.Backend)
	}
}

// Build wires the whole application from configuration.
//
// This is the only place that knows which concrete adapter backs which agent.
// Everything above it works through interfaces, so swapping the in-memory
// storage for SQLite, or a mock agent for real Claude Code, is a change here
// and nowhere else.
func Build(cfg *config.Config, log *logging.Logger) (*System, error) {
	if log == nil {
		log = logging.NewDiscard()
	}

	store, db, closeStore, err := openStorage(cfg, log)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = closeStore()
		}
	}()
	know := knowledge.New(store.Knowledge)
	dec := decisions.New(store.Decisions)

	registry := core.NewRegistry()
	agentMgr := core.NewManager(registry, log)

	// One registry for every agent: it maps (task, agent) to the CLI session
	// that agent resumes each turn.
	var sessions *core.SessionRegistry
	sessions, err = core.OpenSessionRegistry(cfg.Sessions.File)
	if err != nil {
		return nil, err
	}

	ws, err := config.ResolveWorkspace(cfg.Workspace)
	if err != nil {
		return nil, err
	}
	if ws.Notice != "" {
		log.Warn(ws.Notice)
	}

	// Register in a stable order so the TUI and the logs are reproducible.
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		ac := cfg.Agents[name]
		if !ac.Enabled {
			continue
		}
		if ac, err = applyWorkspace(ac, ws, cfg); err != nil {
			return nil, fmt.Errorf("agent %s: %w", name, err)
		}
		id := domain.AgentID(name)
		role := parseRole(ac.Role)

		// Built once here to validate the config and learn its kind; each
		// task the agent works on then gets a fresh adapter from the factory.
		_, kind, err := buildAdapter(id, ac, role, cfg.TUI.UseMockAgents, sessions, log)
		if err != nil {
			return nil, err
		}
		factory := agentFactory(id, ac, role, cfg.TUI.UseMockAgents, sessions, log)

		display := ac.DisplayName
		if display == "" {
			display = strings.ToUpper(name)
		}
		if err := registry.RegisterFactory(domain.Agent{
			ID:          id,
			DisplayName: display,
			Kind:        kind,
			Role:        role,
			Model:       ac.Model,
			Enabled:     true,
			State:       domain.AgentStateStopped,
		}, factory); err != nil {
			return nil, fmt.Errorf("register agent %s: %w", name, err)
		}
	}

	orch := orchestrator.New(orchestrator.Deps{
		Storage:   *store,
		Agents:    agentMgr,
		Knowledge: know,
		Decisions: dec,
		Logger:    log,
		Limits: domain.TaskLimits{
			MaxDeliberationRounds: cfg.Orchestrator.MaxDeliberationRounds,
			MaxMessages:           cfg.Orchestrator.MaxMessages,
			MaxDuration:           cfg.Orchestrator.MaxTaskDuration,
			StallMessages:         cfg.Orchestrator.StallMessages,
			MaxTotalMessages:      cfg.Orchestrator.MaxTotalMessages,
			MaxCostUSD:            cfg.Orchestrator.MaxTaskCostUSD,
			MaxTokens:             cfg.Orchestrator.MaxTaskTokens,
		},
		MaxConcurrentTasks:   cfg.Orchestrator.MaxConcurrentTasks,
		IdleTimeout:          cfg.Orchestrator.IdleTimeout,
		RequireHumanApproval: cfg.Orchestrator.RequireHumanApproval,
	})

	// The agent manager reports runtime state changes as events so the TUI can
	// show an agent crashing without polling.
	agentMgr.OnStateChange = func(a domain.Agent) {
		orch.Events.Publish(context.Background(), domain.Event{
			Type:    domain.EvtAgentStateChanged,
			AgentID: a.ID,
			Summary: fmt.Sprintf("%s is %s", a.DisplayName, a.State),
			Details: map[string]string{"state": string(a.State)},
		})
	}

	// Each task's instance of an agent reports going WORKING and IDLE. That
	// is display state, published without being stored; a failure is kept.
	agentMgr.OnInstanceChange = func(task domain.TaskID, a domain.Agent) {
		ev := domain.Event{
			Type:    domain.EvtAgentStateChanged,
			TaskID:  task,
			AgentID: a.ID,
			Summary: fmt.Sprintf("%s is %s", a.DisplayName, a.State),
			Details: map[string]string{"state": string(a.State)},
		}
		if a.State == domain.AgentStateError || a.State == domain.AgentStateUnavailable {
			orch.Events.Publish(context.Background(), ev)
			return
		}
		orch.Events.PublishEphemeral(ev)
	}

	// Agents recruited in earlier sessions rejoin the team.
	team := &Team{
		path: teamFile(cfg), reg: registry, cfg: cfg, ws: ws, sessions: sessions, log: log,
		catalogs: catalogsFor(cfg),
		publish:  func(e domain.Event) { orch.Events.Publish(context.Background(), e) },
		inUse:    orch.AgentInUse,
	}
	if err := team.load(); err != nil {
		return nil, err
	}
	// A turn that showed the account cannot run a model marks it so the
	// team view says why and the form will not offer it.
	orch.Events.Subscribe(func(e domain.Event) {
		if e.Type == domain.EvtModelUnavailable {
			team.MarkUnavailable(e.Details["provider"], e.Details["model"],
				domain.FailureKind(e.Details["kind"]), domain.FailureKind(e.Details["kind"]).Advice())
		}
	})

	// A task that was running when orch last exited has no run loop behind
	// it now; pause it so it says so and can be resumed.
	recovered, err := orch.RecoverInterrupted(context.Background())
	if err != nil {
		return nil, err
	}
	if len(recovered) > 0 {
		log.Info("paused tasks interrupted by the last exit", "tasks", fmt.Sprint(recovered))
	}

	sys := &System{
		Recovered:    recovered,
		Workspace:    ws,
		closeStore:   closeStore,
		Service:      &Service{orch: orch, storage: *store, knowledge: know, team: team},
		Orchestrator: orch,
		Agents:       agentMgr,
		Storage:      *store,
		Knowledge:    know,
		Proxy:        proxy.New(cfg.Proxy, log),
		Config:       cfg,
		log:          log,
	}
	if db != nil && cfg.Backups.Enabled && cfg.Runtime.BackupDir != "" {
		sys.startBackups(db, orch)
	}
	ok = true
	return sys, nil
}

// startBackups snapshots the project's database when a task finishes and
// every so often while work goes on; Close takes the last one. Each skips
// when nothing changed, so an idle project writes nothing.
func (s *System) startBackups(db *storage.SQLite, orch *orchestrator.Orchestrator) {
	cfg := s.Config
	s.Backups = backup.New(db, cfg.Runtime.BackupDir, cfg.Backups.Keep, s.log)
	ctx, cancel := context.WithCancel(context.Background())
	s.stopBackups = cancel
	s.backupsBusy.Add(1)
	go func() {
		defer s.backupsBusy.Done()
		s.Backups.Run(ctx, cfg.Backups.Interval)
	}()
	orch.Events.Subscribe(func(e domain.Event) {
		switch e.Type {
		case domain.EvtTaskCompleted, domain.EvtTaskFailed, domain.EvtTaskCancelled:
		default:
			return
		}
		s.backupsMu.Lock()
		defer s.backupsMu.Unlock()
		if s.closing {
			return
		}
		s.backupsBusy.Add(1)
		go func() {
			defer s.backupsBusy.Done()
			// Not tied to ctx: a snapshot is short, and Close waits for it.
			if _, err := s.Backups.Snapshot(context.Background(), "task-finished"); err != nil {
				s.log.Warn("backup after a task finished", "task", string(e.TaskID), "error", err)
			}
		}()
	})
}

// scratchDir is the project's tmp folder for the agents, inside the
// project's hidden folder.
func scratchDir(cfg *config.Config, ws config.Workspace) string {
	if cfg != nil && cfg.Runtime.ScratchDir != "" {
		return cfg.Runtime.ScratchDir
	}
	return filepath.Join(ws.Root, brand.HiddenDir(), "tmp")
}

// applyWorkspace confines an agent to the workspace: it runs there (or in a
// folder inside it), may write only when the workspace allows writing at
// all, and may run the workspace's allowed commands plus its own.
func applyWorkspace(ac config.AgentConfig, ws config.Workspace, cfg *config.Config) (config.AgentConfig, error) {
	wc := cfg.Workspace
	dir, err := ws.AgentDir(ac.WorkingDir)
	if err != nil {
		return ac, err
	}
	ac.WorkingDir = dir
	ac.ScratchDir = scratchDir(cfg, ws)
	if !ws.Writable {
		ac.AllowWrites = false
	}
	seen := map[string]bool{}
	var cmds []string
	for _, c := range append(append([]string(nil), wc.AllowedCommands...), ac.AllowedCommands...) {
		c = strings.TrimSpace(c)
		if c != "" && !seen[c] {
			seen[c] = true
			cmds = append(cmds, c)
		}
	}
	ac.AllowedCommands = cmds
	return ac, nil
}

// agentFactory makes a fresh adapter for agent id, one per task it works on.
func agentFactory(id domain.AgentID, ac config.AgentConfig, role domain.AgentRole, forceMock bool, sessions *core.SessionRegistry, log *logging.Logger) core.AdapterFactory {
	return func() (ports.AgentAdapter, error) {
		ad, _, err := buildAdapter(id, ac, role, forceMock, sessions, log)
		return ad, err
	}
}

// buildAdapter selects the adapter implementation for one agent. Mock agents
// are the default in the scaffold so that the orchestration flow can be
// exercised end to end without any provider access.
func buildAdapter(id domain.AgentID, ac config.AgentConfig, role domain.AgentRole, forceMock bool, sessions *core.SessionRegistry, log *logging.Logger) (ports.AgentAdapter, domain.AgentKind, error) {
	if forceMock || ac.Type == "mock" {
		// A mock plays whichever part it has on the task: the task's
		// authority can be any participant, and can change mid-task.
		authority, implementer := mockagent.AuthorityBehavior(), mockagent.ImplementerBehavior()
		behavior := func(self domain.AgentID, t *domain.Task, m domain.Message) []domain.Message {
			if t != nil && t.AuthorityAgent == self {
				return authority(self, t, m)
			}
			return implementer(self, t, m)
		}
		return mockagent.New(id, domain.AgentKindMock, behavior), domain.AgentKindMock, nil
	}

	switch ac.Type {
	case "claude-code":
		a := claude.New(id, ac, log)
		a.UseSessions(sessions)
		return a, domain.AgentKindClaudeCode, nil
	case "codex":
		a := codex.New(id, ac, log)
		a.UseSessions(sessions)
		return a, domain.AgentKindCodex, nil
	default:
		return nil, "", fmt.Errorf("agent %s: unknown type %q", id, ac.Type)
	}
}

func parseRole(s string) domain.AgentRole {
	switch strings.ToUpper(s) {
	case "AUTHORITY":
		return domain.RoleAuthority
	case "REVIEWER":
		return domain.RoleReviewer
	case "OBSERVER":
		return domain.RoleObserver
	default:
		return domain.RoleImplementer
	}
}
