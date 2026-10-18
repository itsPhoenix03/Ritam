package app

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"ritam/internal/agents/core"
	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The workspace's access level: what agents may run beyond editing files.
//
// One policy is shared by every adapter and read on every turn, so the
// human's change applies from each agent's next turn. A change is saved to
// the project's settings file (which overrides the global one) and recorded
// in the events of every open task. A global launch stays at level 0.

// accessControl owns the live policy and its settings file.
type accessControl struct {
	mu   sync.Mutex
	live *core.Access
	// file is the project's settings file; "" (as in tests) keeps changes
	// in memory.
	file string
	// locked says why the level cannot change, "" when it can.
	locked string
	// held says what the project's settings asked for but this machine
	// never approved for this folder, "" when nothing was held back.
	held string
	// saveGrant records an approval on this machine; nil in tests.
	saveGrant func(config.AccessConfig) error
	// publish records an event; openTasks lists the tasks not yet finished.
	publish   func(domain.Event)
	openTasks func(context.Context) []domain.TaskID
}

// newAccessControl builds the policy from the settings, granting only what
// is trusted on this machine (see config.Trusted). A global launch is pinned
// to level 0 whatever the settings say.
func newAccessControl(cfg *config.Config, ws config.Workspace) *accessControl {
	granted, held := cfg.Trusted()
	level := domain.AccessLevel(granted.Level)
	c := &accessControl{file: cfg.Runtime.ProjectConfig, held: held, saveGrant: cfg.Runtime.SaveGrant}
	if ws.Global {
		level = domain.AccessFilesOnly
		c.held = ""
		c.locked = "a global launch (home folder, drive root) stays at level 0: launch " +
			"inside a project folder to choose more"
	}
	c.live = core.NewAccess(level, granted.Commands)
	return c
}

// info reports the policy as the settings tab shows it.
func (c *accessControl) info() ports.AccessInfo {
	level, cmds := c.live.Get()
	_, blocked := core.SafeCommands(cmds)
	return ports.AccessInfo{Level: level, Commands: cmds, Blocked: blocked, Locked: c.locked,
		HeldBack: c.held, File: c.file, Builders: c.live.Builders(), Missing: missingPrograms(cmds)}
}

// set changes the level and the command list.
func (c *accessControl) set(ctx context.Context, in ports.AccessChange) (ports.AccessInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !in.Level.Valid() {
		return c.info(), fmt.Errorf("access level %d: use 0 to 3", int(in.Level))
	}
	cmds := core.CleanCommands(in.Commands)
	if _, blocked := core.SafeCommands(cmds); len(blocked) > 0 {
		return c.info(), fmt.Errorf("%s: always blocked, at every level", strings.Join(blocked, ", "))
	}
	from, before := c.live.Get()
	if in.Level != from && c.locked != "" {
		return c.info(), fmt.Errorf("access cannot change: %s", c.locked)
	}
	if in.Level == from && slices.Equal(cmds, before) {
		return c.info(), nil
	}
	// The approval is recorded on this machine first: a project file that
	// says more than was approved here is held back at the next start.
	approved := config.AccessConfig{Level: int(in.Level), Commands: cmds}
	if c.saveGrant != nil {
		if err := c.saveGrant(approved); err != nil {
			return c.info(), err
		}
	}
	if c.file != "" {
		if err := config.SaveAccess(c.file, approved); err != nil {
			return c.info(), err
		}
	}
	c.live.Set(in.Level, cmds)
	c.held = ""

	summary := "access commands: " + listOrNone(cmds)
	if in.Level != from {
		summary = fmt.Sprintf("access level %s -> %s", from, in.Level)
	}
	details := map[string]string{"level": fmt.Sprint(int(in.Level)), "from": fmt.Sprint(int(from)),
		"commands": strings.Join(cmds, ", ")}
	if c.publish != nil {
		tasks := c.openTasks(ctx)
		if len(tasks) == 0 {
			c.publish(domain.Event{Type: domain.EvtAccessLevel, Summary: summary, Details: details})
		}
		for _, id := range tasks {
			c.publish(domain.Event{Type: domain.EvtAccessLevel, TaskID: id, Summary: summary, Details: details})
		}
	}
	return c.info(), nil
}

// recordStart notes in a new task's events the level it starts at and who
// builds on it.
func (c *accessControl) recordStart(t *domain.Task) {
	if c.publish == nil {
		return
	}
	level, cmds := c.live.Get()
	builds := c.whoBuilds(t)
	c.publish(domain.Event{Type: domain.EvtAccessLevel, TaskID: t.ID,
		Summary: fmt.Sprintf("access level %s; builder %s: %s", level, domain.BuilderLabel(t.Builder), builds),
		Details: map[string]string{"level": fmt.Sprint(int(level)), "commands": strings.Join(cmds, ", "),
			"builder": t.Builder, "builds": builds}})
}

// whoBuilds says in words who builds on task t.
func (c *accessControl) whoBuilds(t *domain.Task) string {
	builders, everyone := c.live.BuildersFor(t)
	switch {
	case everyone:
		return "every agent builds, each in build/<agent>/"
	case len(builders) == 0:
		return "the human builds and tests; the task stays open until they answer"
	}
	names := make([]string, len(builders))
	for i, b := range builders {
		names[i] = string(b)
	}
	return strings.Join(names, ", ") + " build" + map[bool]string{true: "s", false: ""}[len(builders) == 1]
}

// prepareBuilder checks a new task's builder choice and, where a
// participant could build, has its sandbox checked first.
func (s *Service) prepareBuilder(ctx context.Context, in ports.CreateTaskInput) error {
	if s.access == nil {
		return nil
	}
	level, _ := s.access.live.Get()
	participants := append([]domain.AgentID{in.Authority}, in.Participants...)
	switch in.Builder {
	case domain.BuilderAuto, domain.BuilderManual:
	default:
		named := domain.AgentID(in.Builder)
		if !slices.Contains(participants, named) {
			return fmt.Errorf("builder %s is not a participant of the task", named)
		}
		if level == domain.AccessFilesOnly {
			return fmt.Errorf("at access level 0 no agent may build: choose Manual, or raise the level in :settings")
		}
		participants = []domain.AgentID{named}
	}
	if level == domain.AccessSandboxedBuilder && in.Builder != domain.BuilderManual && s.team != nil {
		s.team.verifyCandidates(ctx, participants)
	}
	if level == domain.AccessSandboxedBuilder && in.Builder != domain.BuilderAuto && in.Builder != domain.BuilderManual &&
		!s.access.live.Sandboxed(domain.AgentID(in.Builder)) {
		return fmt.Errorf("%s has not passed the sandbox check, and at access level 1 only sandboxed agents build: "+
			"choose Auto or Manual, or raise the level in :settings", in.Builder)
	}
	return nil
}

// missingPrograms lists the commands whose program is not on PATH, so the
// settings tab can say "cmake is not installed" before an agent finds out.
func missingPrograms(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		prog, _, _ := strings.Cut(c, " ")
		if _, err := exec.LookPath(prog); err != nil {
			out = append(out, prog)
		}
	}
	return out
}

func listOrNone(cmds []string) string {
	if len(cmds) == 0 {
		return "none"
	}
	return strings.Join(cmds, ", ")
}

// sandboxDeclarer is an adapter that says whether its CLI runs the agent's
// commands in a sandbox. An adapter that says nothing is not sandboxed.
type sandboxDeclarer interface{ RunsInSandbox() bool }

// accessUser is an adapter that follows the workspace's access policy.
type accessUser interface{ UseAccess(*core.Access) }

// runsInSandbox reports what an adapter declares about its sandbox.
func runsInSandbox(ad ports.AgentAdapter) bool {
	sd, ok := ad.(sandboxDeclarer)
	return ok && sd.RunsInSandbox()
}

// Access reports the workspace's access level and command list.
func (s *Service) Access(_ context.Context) (ports.AccessInfo, error) {
	if s.access == nil {
		return ports.AccessInfo{}, ports.ErrNotImplemented
	}
	return s.access.info(), nil
}

// SetAccess changes the workspace's access level and command list.
func (s *Service) SetAccess(ctx context.Context, in ports.AccessChange) (ports.AccessInfo, error) {
	if s.access == nil {
		return ports.AccessInfo{}, ports.ErrNotImplemented
	}
	return s.access.set(ctx, in)
}
