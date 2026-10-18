package app

import (
	"context"
	"fmt"
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
		HeldBack: c.held, File: c.file, Builders: c.live.Builders()}
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

// recordStart notes the level a task starts at in its events.
func (c *accessControl) recordStart(id domain.TaskID) {
	if c.publish == nil {
		return
	}
	level, cmds := c.live.Get()
	c.publish(domain.Event{Type: domain.EvtAccessLevel, TaskID: id,
		Summary: fmt.Sprintf("access level %s", level),
		Details: map[string]string{"level": fmt.Sprint(int(level)), "commands": strings.Join(cmds, ", ")}})
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
