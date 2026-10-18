package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"ritam/internal/agents/claude"
	"ritam/internal/agents/codex"
	"ritam/internal/agents/core"
	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// The team: the agents from ritam.yaml plus the ones the human recruits.
//
// Recruiting picks a model from a provider's catalog and gives it a name and
// a role; the new agent is an ordinary participant from then on. Several
// agents may run the same model, and each agent gets its own instance on
// every task it works on, so the same model can work on several tasks at
// once. Recruited agents are kept in team.json beside the database -- never
// written into ritam.yaml, which stays the human's file.

// modelCacheFor is how long a fetched model list is reused.
const modelCacheFor = 5 * time.Minute

var agentName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

type recruitRecord struct {
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Role        string    `json:"role"`
	AllowWrites bool      `json:"allowWrites"`
	Added       time.Time `json:"added"`
}

// Team adds and removes recruited agents and lists the models to recruit
// from.
type Team struct {
	mu        sync.Mutex
	path      string
	reg       *core.Registry
	cfg       *config.Config
	ws        config.Workspace
	sessions  *core.SessionRegistry
	log       *logging.Logger
	catalogs  []ports.ModelCatalog
	recruited map[domain.AgentID]recruitRecord
	publish   func(domain.Event)
	inUse     func(domain.AgentID) bool

	models   []ports.ModelInfo
	cachedAt time.Time

	// avail is what is known about whether this account can run each model
	// (keyed provider|model), kept in models.json beside team.json.
	avail map[string]availability
	// probe runs one short CLI call to check a model; replaced in tests.
	probe func(ctx context.Context, provider, model string) (ok bool, kind domain.FailureKind, detail string, err error)
}

// availability is what a failed turn or a check found out about a model.
type availability struct {
	OK     bool      `json:"ok"`
	Kind   string    `json:"kind,omitempty"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

func availKey(provider, model string) string { return provider + "|" + model }

// teamFile is where recruited agents are kept: beside the database, or
// nowhere when nothing is stored.
func teamFile(cfg *config.Config) string {
	if cfg.Runtime.TeamFile != "" {
		return cfg.Runtime.TeamFile
	}
	if cfg.Storage.Backend != "sqlite" || cfg.Storage.Path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(cfg.Storage.Path), "team.json")
}

// catalogsFor builds one catalog per provider, using the configured
// executable of an agent of that type when there is one.
func catalogsFor(cfg *config.Config) []ports.ModelCatalog {
	return []ports.ModelCatalog{
		claude.Catalog{},
		codex.Catalog{Executable: templateFor(cfg, "codex").Executable},
	}
}

// templateFor is the configuration a recruit of this provider starts from:
// the provider's entry in ritam.yaml, else (for an older config) a
// configured agent of the same type, else the CLI's usual command.
func templateFor(cfg *config.Config, provider string) config.AgentConfig {
	if p, ok := cfg.Providers[provider]; ok {
		ac := config.AgentConfig{Type: provider, Executable: p.Executable, StartupTimeout: p.StartupTimeout,
			ResponseTimeout: p.ResponseTimeout, AllowedCommands: append([]string(nil), p.AllowedCommands...)}
		if ac.Executable == "" {
			ac.Executable = defaultExecutable(provider)
		}
		if ac.ResponseTimeout <= 0 {
			ac.ResponseTimeout = 5 * time.Minute
		}
		return ac
	}
	names := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if ac := cfg.Agents[n]; ac.Type == provider {
			return config.AgentConfig{Type: provider, Executable: ac.Executable,
				StartupTimeout: ac.StartupTimeout, ResponseTimeout: ac.ResponseTimeout}
		}
	}
	return config.AgentConfig{Type: provider, Executable: defaultExecutable(provider), ResponseTimeout: 5 * time.Minute}
}

// defaultExecutable is the provider's usual CLI command.
func defaultExecutable(provider string) string {
	if provider == "codex" {
		return "codex"
	}
	return "claude"
}

// load registers the agents recruited in earlier sessions.
func (tm *Team) load() error {
	tm.recruited = map[domain.AgentID]recruitRecord{}
	tm.loadAvailability()
	if tm.path == "" {
		return nil
	}
	data, err := os.ReadFile(tm.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("team: %w", err)
	}
	var list []recruitRecord
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("team: %s is not valid (fix or remove it): %w", tm.path, err)
	}
	for _, r := range list {
		if _, err := tm.register(r); err != nil {
			tm.log.Warn("recruited agent not restored", "agent", r.Name, "error", err)
		}
	}
	return nil
}

// Models lists every provider's models, cached for a few minutes.
func (tm *Team) Models(ctx context.Context) ([]ports.ModelInfo, error) {
	tm.mu.Lock()
	if tm.models != nil && time.Since(tm.cachedAt) < modelCacheFor {
		out := tm.withAvailabilityLocked(tm.models)
		tm.mu.Unlock()
		return out, nil
	}
	tm.mu.Unlock()
	var all []ports.ModelInfo
	var errs []string
	for _, c := range tm.catalogs {
		ms, err := c.Models(ctx)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		all = append(all, ms...)
	}
	if len(all) == 0 && len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	// Models a CLI hides (legacy, internal) are left out of the team view.
	shown := all[:0]
	for _, m := range all {
		if !m.Hidden {
			shown = append(shown, m)
		}
	}
	tm.mu.Lock()
	tm.models, tm.cachedAt = shown, time.Now()
	out := tm.withAvailabilityLocked(shown)
	tm.mu.Unlock()
	return out, nil
}

// withAvailabilityLocked copies models with what is known about whether
// this account can run them.
func (tm *Team) withAvailabilityLocked(models []ports.ModelInfo) []ports.ModelInfo {
	out := append([]ports.ModelInfo(nil), models...)
	for i := range out {
		if a, ok := tm.avail[availKey(out[i].Provider, out[i].ID)]; ok {
			out[i].Unavailable, out[i].Checked = !a.OK, a.OK
			out[i].Reason = a.Reason
		}
	}
	return out
}

// MarkUnavailable records that this account cannot run a model, as a failed
// turn showed.
func (tm *Team) MarkUnavailable(provider, model string, kind domain.FailureKind, reason string) {
	if !kind.MarksModelUnavailable() || provider == string(domain.AgentKindMock) {
		return
	}
	tm.setAvailability(provider, model, availability{Kind: string(kind), Reason: reason, At: time.Now()})
}

func (tm *Team) setAvailability(provider, model string, a availability) {
	tm.mu.Lock()
	if tm.avail == nil {
		tm.avail = map[string]availability{}
	}
	tm.avail[availKey(provider, model)] = a
	tm.mu.Unlock()
	if err := tm.saveAvailability(); err != nil {
		tm.log.Warn("saving model availability", "error", err)
	}
}

// CheckModel makes one short call to see whether this account can run the
// model, and remembers the answer.
func (tm *Team) CheckModel(ctx context.Context, provider, model string) (ports.ModelInfo, error) {
	info := ports.ModelInfo{Provider: provider, ID: model}
	if tm.cfg.TUI.UseMockAgents {
		tm.setAvailability(provider, model, availability{OK: true, At: time.Now()})
		info.Checked = true
		return info, nil
	}
	probe := tm.probe
	if probe == nil {
		probe = tm.probeCLI
	}
	ok, kind, detail, err := probe(ctx, provider, model)
	switch {
	case ok:
		tm.setAvailability(provider, model, availability{OK: true, At: time.Now()})
		info.Checked = true
		return info, nil
	case kind.MarksModelUnavailable():
		reason := kind.Advice()
		tm.setAvailability(provider, model, availability{Kind: string(kind), Reason: reason, At: time.Now()})
		info.Unavailable, info.Reason = true, reason
		return info, nil
	case kind != "":
		// A login or a limit says nothing about the model itself.
		return info, fmt.Errorf("could not check %s: %s", model, kind.Advice())
	case err != nil:
		return info, fmt.Errorf("could not check %s: %v", model, err)
	}
	return info, fmt.Errorf("could not check %s: %s", model, detail)
}

// checkPrompt is all a check asks: the cheapest possible answer.
const checkPrompt = "Reply with the single word OK."

// checkTimeout bounds one check call.
var checkTimeout = 2 * time.Minute

// probeCLI runs the provider's CLI once on the model, read-only, in the
// workspace's scratch folder.
func (tm *Team) probeCLI(ctx context.Context, provider, model string) (bool, domain.FailureKind, string, error) {
	exe := templateFor(tm.cfg, provider).Executable
	dir := scratchDir(tm.cfg, tm.ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, "", "", err
	}
	var args []string
	switch provider {
	case "claude-code":
		args = []string{"-p", "--output-format", "json", "--max-turns", "1",
			"--allowedTools", "Read", "--permission-mode", "dontAsk"}
	case "codex":
		args = []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "--color", "never", "-C", dir}
	default:
		return false, "", "", fmt.Errorf("cannot check models of %s", provider)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if provider == "codex" {
		args = append(args, "-")
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(checkPrompt)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	return judgeCheck(provider, stdout.String(), stderr.String(), runErr)
}

// judgeCheck reads a check call's outcome. Only a failure is classified:
// ordinary output (token counts, ids) could otherwise look like an error.
func judgeCheck(provider, stdout, stderr string, runErr error) (bool, domain.FailureKind, string, error) {
	if provider == "claude-code" {
		var env struct {
			IsError bool   `json:"is_error"`
			Subtype string `json:"subtype"`
			Result  string `json:"result"`
		}
		out := strings.TrimSpace(stdout)
		if i := strings.LastIndex(out, "\n{"); i >= 0 {
			out = out[i+1:]
		}
		if json.Unmarshal([]byte(out), &env) == nil && runErr == nil && !env.IsError && env.Subtype != "error" {
			return true, "", "", nil
		}
		text := env.Result + "\n" + stderr + "\n" + stdout
		return false, domain.ClassifyAgentFailure(text), firstLine(stderr, env.Result), runErr
	}
	if runErr == nil {
		return true, "", "", nil
	}
	return false, domain.ClassifyAgentFailure(stderr + "\n" + stdout), firstLine(stderr, stdout), runErr
}

func firstLine(texts ...string) string {
	for _, t := range texts {
		for _, l := range strings.Split(t, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				return l
			}
		}
	}
	return "no output"
}

// availabilityFile is where model availability is kept: in the app home,
// since it is about the account rather than the project (beside team.json
// when there is no app home, as in tests).
func (tm *Team) availabilityFile() string {
	if tm.cfg != nil && tm.cfg.Runtime.ModelsFile != "" {
		return tm.cfg.Runtime.ModelsFile
	}
	if tm.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(tm.path), "models.json")
}

func (tm *Team) loadAvailability() {
	tm.avail = map[string]availability{}
	path := tm.availabilityFile()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &tm.avail); err != nil {
		tm.log.Warn("model availability file is not valid; starting afresh", "path", path, "error", err)
		tm.avail = map[string]availability{}
	}
}

func (tm *Team) saveAvailability() error {
	path := tm.availabilityFile()
	if path == "" {
		return nil
	}
	tm.mu.Lock()
	data, err := json.MarshalIndent(tm.avail, "", "  ")
	tm.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Recruit adds an agent to the team and keeps it.
func (tm *Team) Recruit(ctx context.Context, in ports.RecruitInput) (domain.Agent, error) {
	r := recruitRecord{Name: string(in.Name), Provider: in.Provider, Model: strings.TrimSpace(in.Model),
		Role: string(in.Role), AllowWrites: in.AllowWrites, Added: time.Now()}
	if r.Role == "" {
		r.Role = string(domain.RoleImplementer)
	}
	a, err := tm.register(r)
	if err != nil {
		return domain.Agent{}, err
	}
	if err := tm.save(); err != nil {
		tm.reg.Unregister(a.ID)
		tm.mu.Lock()
		delete(tm.recruited, a.ID)
		tm.mu.Unlock()
		return domain.Agent{}, err
	}
	tm.publish(domain.Event{Type: domain.EvtAgentRegistered, AgentID: a.ID,
		Summary: fmt.Sprintf("recruited %s (%s %s, %s)", a.ID, r.Provider, r.Model, strings.ToLower(r.Role))})
	return a, nil
}

// Dismiss removes a recruited agent that is not working on a task.
func (tm *Team) Dismiss(_ context.Context, id domain.AgentID) error {
	tm.mu.Lock()
	_, ok := tm.recruited[id]
	tm.mu.Unlock()
	if !ok {
		if _, known := tm.reg.Get(id); known {
			return fmt.Errorf("%s comes from ritam.yaml; only recruited agents can be dismissed", id)
		}
		return fmt.Errorf("no such agent: %s", id)
	}
	if tm.inUse != nil && tm.inUse(id) {
		return fmt.Errorf("%s is working on a running task; dismiss it once that task stops", id)
	}
	tm.reg.Unregister(id)
	tm.mu.Lock()
	delete(tm.recruited, id)
	tm.mu.Unlock()
	if err := tm.save(); err != nil {
		return err
	}
	tm.publish(domain.Event{Type: domain.EvtAgentStopped, AgentID: id, Summary: "dismissed " + string(id)})
	return nil
}

// register validates a recruit and adds it to the registry.
func (tm *Team) register(r recruitRecord) (domain.Agent, error) {
	id := domain.AgentID(r.Name)
	if !agentName.MatchString(r.Name) || id == domain.HumanAgentID {
		return domain.Agent{}, fmt.Errorf("agent name %q: use 1-32 lowercase letters, digits, - or _", r.Name)
	}
	if _, exists := tm.reg.Get(id); exists {
		return domain.Agent{}, fmt.Errorf("an agent called %s already exists", id)
	}
	if r.Provider != "claude-code" && r.Provider != "codex" {
		return domain.Agent{}, fmt.Errorf("unknown provider %q (claude-code or codex)", r.Provider)
	}
	// An empty model means the CLI's own default, like an agent in ritam.yaml
	// that sets none.
	ac := templateFor(tm.cfg, r.Provider)
	ac.Model = r.Model
	ac.Role = r.Role
	ac.Enabled = true
	// ritam.yaml can make a provider's agents read-only, whatever they were
	// recruited with.
	ac.AllowWrites = r.AllowWrites && tm.cfg.Providers[r.Provider].MayWrite()
	ac, err := applyWorkspace(ac, tm.ws, tm.cfg)
	if err != nil {
		return domain.Agent{}, err
	}
	role := parseRole(r.Role)
	_, kind, err := buildAdapter(id, ac, role, tm.cfg.TUI.UseMockAgents, tm.sessions, tm.log)
	if err != nil {
		return domain.Agent{}, err
	}
	a := domain.Agent{ID: id, DisplayName: strings.ToUpper(r.Name), Kind: kind, Role: role, Model: r.Model,
		Enabled: true, State: domain.AgentStateStopped}
	if err := tm.reg.RegisterFactory(a, agentFactory(id, ac, role, tm.cfg.TUI.UseMockAgents, tm.sessions, tm.log)); err != nil {
		return domain.Agent{}, err
	}
	tm.mu.Lock()
	tm.recruited[id] = r
	tm.mu.Unlock()
	return a, nil
}

// Recruited reports whether id was recruited rather than configured.
func (tm *Team) Recruited(id domain.AgentID) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	_, ok := tm.recruited[id]
	return ok
}

// save writes team.json atomically.
func (tm *Team) save() error {
	if tm.path == "" {
		return nil
	}
	tm.mu.Lock()
	list := make([]recruitRecord, 0, len(tm.recruited))
	for _, r := range tm.recruited {
		list = append(list, r)
	}
	tm.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tm.path), 0o700); err != nil {
		return fmt.Errorf("team: %w", err)
	}
	tmp := tm.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("team: %w", err)
	}
	if err := os.Rename(tmp, tm.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("team: %w", err)
	}
	return nil
}
