package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"ritam/internal/agents/core"
	"ritam/internal/brand"
	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/orchestrator"
	"ritam/internal/ports"
	"ritam/internal/review"
	"ritam/internal/review/facts"
	"ritam/internal/review/rules"
)

// reviewControl owns the live review settings and the project file they are
// saved to. The policy reads them on every tool call, so a change from the
// settings tab applies to the next call.
type reviewControl struct {
	mu   sync.Mutex
	live config.ReviewConfig
	// file is the project's settings file; "" keeps changes in memory.
	file string
	// held is what the project asked for that the global settings decide.
	held   string
	active bool
	// hooks are the human's hooks that run: the global ones and the
	// project's approved here; held are the project's not approved.
	hooks, heldHooks []config.ReviewHook
	// approved are the fingerprints of the project's hooks as approved, by
	// name; root is the folder their files are taken from; approvals is
	// the app home's approvals file, "" when there is no app home.
	approved  map[string]string
	root      string
	approvals string
	guard     *core.FileGuard
	// publish records an event; openTasks lists the unfinished tasks.
	publish   func(domain.Event)
	openTasks func(context.Context) []domain.TaskID
}

func newReviewControl(cfg *config.Config, active bool) *reviewControl {
	c := &reviewControl{live: cfg.Review, file: cfg.Runtime.ProjectConfig, active: active}
	c.hooks, c.heldHooks = slices.Clone(cfg.Review.Hooks), slices.Clone(cfg.Runtime.HeldHooks)
	c.root, c.approved = cfg.Runtime.HookRoot, map[string]string{}
	if cfg.Runtime.HomeDir != "" {
		c.approvals = filepath.Join(cfg.Runtime.HomeDir, config.HookApprovalsFile)
	}
	for _, h := range cfg.Runtime.ProjectHooks {
		c.approved[h.Name] = config.HookFingerprint(h, c.root)
	}
	if c.live.Mode == "" {
		c.live.Mode = config.ReviewAuto
	}
	return c
}

func (c *reviewControl) settings() review.Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return review.Settings{Manual: c.live.Manual(), AskTimeout: c.live.Wait()}
}

func (c *reviewControl) info() ports.ReviewInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	info := ports.ReviewInfo{Mode: c.live.Mode, AskTimeout: c.live.Wait(), Active: c.active,
		Warning: config.AutoModeWarning, HeldBack: c.held, File: c.file,
		Deny: c.live.Rules.Deny, Ask: c.live.Rules.Ask, Protect: c.live.Rules.Protect}
	for _, h := range c.hooks {
		info.Hooks = append(info.Hooks, h.Name)
	}
	for _, h := range c.heldHooks {
		info.HeldHooks = append(info.HeldHooks, h.Name+" ("+strings.Join(h.Run, " ")+")")
	}
	return info
}

// setMode switches the mode, saves it and records it in the open tasks.
func (c *reviewControl) setMode(ctx context.Context, mode string) (ports.ReviewInfo, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != config.ReviewAuto && mode != config.ReviewManual {
		return c.info(), fmt.Errorf("review mode %q: use %s or %s", mode, config.ReviewAuto, config.ReviewManual)
	}
	c.mu.Lock()
	from := c.live.Mode
	if mode == from {
		c.mu.Unlock()
		return c.info(), nil
	}
	next := c.live
	next.Mode = mode
	if c.file != "" {
		save := func() error { return config.SaveReviewMode(c.file, mode) }
		var err error
		if c.guard != nil {
			err = c.guard.Write(c.file, save)
		} else {
			err = save()
		}
		if err != nil {
			c.mu.Unlock()
			return c.info(), err
		}
	}
	c.live = next
	c.mu.Unlock()

	summary := fmt.Sprintf("review mode %s -> %s", from, mode)
	if mode == config.ReviewAuto {
		summary += ": " + config.AutoModeWarning
	} else {
		summary += ": every command now waits for your approval"
	}
	if c.publish != nil && c.openTasks != nil {
		for _, id := range c.openTasks(ctx) {
			c.publish(domain.Event{Type: domain.EvtReviewMode, TaskID: id, AgentID: domain.HumanAgentID,
				Summary: summary, Details: map[string]string{"mode": mode, "from": from}})
		}
	}
	return c.info(), nil
}

// newPolicy builds the review's policy: ritam's rules, the live settings,
// the machine as the workspace sees it, the human to ask, and pausing the
// task when a critical call stops a turn.
func newPolicy(cfg *config.Config, ws config.Workspace, ctl *reviewControl, asker review.Asker,
	orch *orchestrator.Orchestrator, log *logging.Logger) (*review.Policy, error) {
	set, err := rules.Builtin()
	if err != nil {
		return nil, fmt.Errorf("%s's review rules: %w", brand.Name, err)
	}
	r := cfg.Review.Rules
	if err := set.Extend(rules.Custom{Deny: r.Deny, Ask: r.Ask, Protect: r.Protect}); err != nil {
		return nil, err
	}
	base := reviewEnv(cfg, ws)
	return &review.Policy{
		Rules:    set,
		Settings: ctl.settings,
		Env: func(s ports.TurnScope) facts.Env {
			e := base
			if s.BuildDir != "" && ws.Root != "" {
				e.Build = filepath.Join(ws.Root, filepath.FromSlash(s.BuildDir))
			}
			return e
		},
		Asker: asker,
		Hooks: func() []review.Hook { return ctl.reviewHooks(ws.Root) },
		OnStop: func(s review.Stop) {
			if s.Task == "" {
				return
			}
			reason := fmt.Sprintf("%s's review stopped %s: %s", brand.Name, s.Agent, s.Reason)
			log.ForAgent(s.Agent).Warn("a critical tool call stopped the turn", "task", string(s.Task), "reason", s.Reason)
			if err := orch.Pause(context.Background(), s.Task, reason); err != nil {
				log.Error("pausing the task after a critical tool call", "task", string(s.Task), "error", err)
			}
		},
	}, nil
}

// reviewEnv describes this machine to the parser: the workspace, the home
// and temporary folders, ritam's own home, the protected files, and the
// well-known variables commands may name.
func reviewEnv(cfg *config.Config, ws config.Workspace) facts.Env {
	e := facts.Env{OS: runtime.GOOS, Workspace: ws.Root, RitamHome: cfg.Runtime.HomeDir, Vars: map[string]string{}}
	if e.OS == "darwin" {
		e.OS = "macos"
	}
	if h, err := os.UserHomeDir(); err == nil {
		e.Home = h
	}
	for _, t := range []string{os.TempDir(), os.Getenv("TEMP"), os.Getenv("TMP"), os.Getenv("TMPDIR")} {
		if t != "" && !containsFold(e.Temp, t) {
			e.Temp = append(e.Temp, t)
		}
	}
	for _, v := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "WINDIR",
		"PROGRAMFILES", "TEMP", "TMP", "TMPDIR", "USER", "USERNAME", "PUBLIC"} {
		if val := os.Getenv(v); val != "" {
			e.Vars[v] = val
		}
	}
	if u := os.Getenv("USERNAME"); u != "" && e.Vars["USER"] == "" {
		e.Vars["USER"] = u
	}
	e.Protected = append([]string(nil), facts.DefaultProtected...)
	for _, f := range sensitiveFiles {
		e.Protected = append(e.Protected, f)
	}
	for _, d := range sensitiveDirs {
		e.Protected = append(e.Protected, d+"/**")
	}
	if ws.Root != "" {
		e.Protected = append(e.Protected, protectedPaths(cfg, ws)...)
	}
	// A project's hook scripts run outside any sandbox once approved: no
	// agent may change them (and a changed one is held again anyway).
	for _, h := range append(append([]config.ReviewHook(nil), cfg.Runtime.HeldHooks...), cfg.Runtime.ProjectHooks...) {
		e.Protected = append(e.Protected, config.HookFiles(h, cfg.Runtime.HookRoot)...)
	}
	e.Resolve = resolveExisting
	return e
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(filepath.Clean(v), filepath.Clean(s)) {
			return true
		}
	}
	return false
}

// resolveExisting resolves symbolic links and junctions on the longest
// part of p that exists, then puts the rest back: a link inside the
// workspace pointing outside it is outside.
func resolveExisting(p string) string {
	p = filepath.Clean(filepath.FromSlash(p))
	rest := ""
	cur := p
	for i := 0; i < 64; i++ {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return real
			}
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
	return p
}

// Review reports how the agents' tool calls are approved.
func (s *Service) Review(_ context.Context) (ports.ReviewInfo, error) {
	if s.review == nil {
		return ports.ReviewInfo{}, ports.ErrNotImplemented
	}
	return s.review.info(), nil
}

// SetReviewMode switches between auto and manual approval.
func (s *Service) SetReviewMode(ctx context.Context, mode string) (ports.ReviewInfo, error) {
	if s.review == nil {
		return ports.ReviewInfo{}, ports.ErrNotImplemented
	}
	return s.review.setMode(ctx, mode)
}

// PendingApprovals lists the tool calls waiting for the human.
func (s *Service) PendingApprovals(_ context.Context) []ports.Approval {
	if s.approvals == nil {
		return nil
	}
	return s.approvals.pending()
}

// AnswerApproval approves or refuses a waiting tool call.
func (s *Service) AnswerApproval(_ context.Context, id string, allow bool, note string) error {
	if s.approvals == nil {
		return fmt.Errorf("no tool call is waiting as %s", id)
	}
	return s.approvals.answer(id, review.Answer{Allow: allow, Note: strings.TrimSpace(note)})
}

// reviewHooks are the human's hooks as the review runs them.
func (c *reviewControl) reviewHooks(dir string) []review.Hook {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]review.Hook, 0, len(c.hooks))
	for _, h := range c.hooks {
		rh := toReviewHook(h, dir)
		if fp, ok := c.approved[h.Name]; ok {
			// A project's hook runs only as it was approved.
			h, root, name := h, c.root, h.Name
			rh.Dir = root
			rh.Check = func() error {
				if config.HookFingerprint(h, root) != fp {
					return fmt.Errorf("this project's hook %s changed since you approved it; approve it again "+
						"in the settings tab (:hooks approve %s)", name, name)
				}
				return nil
			}
		}
		out = append(out, rh)
	}
	return out
}

// approveHook approves one of the project's held hooks on this machine,
// as it stands now, and lets it run from the next tool call.
func (c *reviewControl) approveHook(ctx context.Context, name string) (ports.ReviewInfo, error) {
	c.mu.Lock()
	i := slices.IndexFunc(c.heldHooks, func(h config.ReviewHook) bool { return h.Name == name })
	if i < 0 {
		c.mu.Unlock()
		return c.info(), fmt.Errorf("no hook of this project waits for approval as %q", name)
	}
	h := c.heldHooks[i]
	if c.approvals != "" {
		save := func() error { return config.SaveHookApproval(c.approvals, h, c.root) }
		var err error
		if c.guard != nil {
			err = c.guard.Write(c.approvals, save)
		} else {
			err = save()
		}
		if err != nil {
			c.mu.Unlock()
			return c.info(), err
		}
	}
	c.heldHooks = slices.Delete(c.heldHooks, i, i+1)
	c.hooks = append(c.hooks, h)
	c.approved[h.Name] = config.HookFingerprint(h, c.root)
	c.mu.Unlock()

	summary := fmt.Sprintf("approved this project's review hook %s (%s); it runs on every tool call from now on",
		h.Name, strings.Join(h.Run, " "))
	if c.publish != nil && c.openTasks != nil {
		for _, id := range c.openTasks(ctx) {
			c.publish(domain.Event{Type: domain.EvtReviewMode, TaskID: id, AgentID: domain.HumanAgentID,
				Summary: summary, Details: map[string]string{"hook": h.Name}})
		}
	}
	return c.info(), nil
}

// toReviewHook turns a hook from the settings into one the review runs, in
// the workspace, for the tools and CLIs it names.
func toReviewHook(h config.ReviewHook, dir string) review.Hook {
	tools, agents := h.Tools, h.Agents
	return review.Hook{Name: h.Name, Run: h.Run, Native: h.Native(), Timeout: h.Wait(), Block: h.Blocks(), Dir: dir,
		Wants: func(tool string, kind facts.ToolKind, cli domain.AgentKind) bool {
			if len(agents) > 0 && !slices.Contains(agents, string(cli)) {
				return false
			}
			if len(tools) == 0 {
				return true
			}
			for _, t := range tools {
				if strings.EqualFold(t, tool) || strings.EqualFold(t, string(kind)) {
					return true
				}
			}
			return false
		}}
}

// ApproveHook approves one of the project's held review hooks.
func (s *Service) ApproveHook(ctx context.Context, name string) (ports.ReviewInfo, error) {
	if s.review == nil {
		return ports.ReviewInfo{}, ports.ErrNotImplemented
	}
	return s.review.approveHook(ctx, strings.TrimSpace(name))
}
