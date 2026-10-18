package core

import (
	"slices"
	"strings"
	"sync"

	"ritam/internal/domain"
)

// Access is a workspace's access level and command list, shared by every
// adapter. Adapters read it on each turn, so a change the human makes in the
// settings tab applies from every agent's next turn, mid-task included.
//
// It also knows which agents run their CLI in a sandbox: at the sandboxed
// builder level only those may build and test.
type Access struct {
	mu        sync.RWMutex
	level     domain.AccessLevel
	commands  []string
	sandboxed map[domain.AgentID]bool
}

// NewAccess builds the policy at a level with a command list.
func NewAccess(level domain.AccessLevel, commands []string) *Access {
	a := &Access{sandboxed: map[domain.AgentID]bool{}}
	a.Set(level, commands)
	return a
}

// Get returns the level and a copy of the command list.
func (a *Access) Get() (domain.AccessLevel, []string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.level, append([]string(nil), a.commands...)
}

// Set changes the level and the command list. Blank and repeated commands
// are dropped; destructive ones are kept here (so the settings tab can show
// them as blocked) and filtered out of every grant.
func (a *Access) Set(level domain.AccessLevel, commands []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.level = level
	a.commands = CleanCommands(commands)
}

// SetSandboxed records whether an agent's CLI runs its commands in a
// sandbox.
func (a *Access) SetSandboxed(id domain.AgentID, sandboxed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sandboxed[id] = sandboxed
}

// Forget drops an agent that left the team.
func (a *Access) Forget(id domain.AgentID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sandboxed, id)
}

// Sandboxed reports whether an agent's CLI runs its commands in a sandbox.
func (a *Access) Sandboxed(id domain.AgentID) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sandboxed[id]
}

// Builders lists the team's sandboxed agents, sorted.
func (a *Access) Builders() []domain.AgentID {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []domain.AgentID
	for id, ok := range a.sandboxed {
		if ok {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// Grant is what one write-enabled agent may run on one turn.
type Grant struct {
	Level domain.AccessLevel
	// Commands are the command prefixes the agent may run, destructive ones
	// already removed.
	Commands []string
	// AnyCommand lets the agent run anything but the blocked commands.
	AnyCommand bool
	// Builders are the task's agents that build, named to an agent that
	// does not.
	Builders []domain.AgentID
	// Manual says the human runs builds and tests on this task.
	Manual bool
	// BuildDir is where this agent builds, when it may: build/<agent>, so
	// builders never mix their outputs.
	BuildDir string
	// Assigns tells the authority that several agents build and it hands
	// out the build and test work.
	Assigns []domain.AgentID
}

// RunsCommands reports whether the grant allows running anything at all.
func (g Grant) RunsCommands() bool { return g.AnyCommand || len(g.Commands) > 0 }

// GrantFor works out what agent id may run on task t. extra are commands the
// agent's provider or configuration adds to the workspace's list.
//
// A nil Access is the policy of an adapter built outside the app (tests, a
// direct construction): its own command list and nothing more.
func (a *Access) GrantFor(t *domain.Task, id domain.AgentID, extra []string) Grant {
	if a == nil {
		cmds, _ := SafeCommands(CleanCommands(extra))
		return Grant{Level: domain.AccessBuildAndTest, Commands: cmds}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	g := Grant{Level: a.level}
	builders, everyone := a.buildersLocked(t)
	g.Manual = !everyone && len(builders) == 0
	if everyone || slices.Contains(builders, id) {
		switch a.level {
		case domain.AccessAnyCommand:
			g.AnyCommand = true
		default:
			g.Commands, _ = SafeCommands(CleanCommands(append(append([]string(nil), a.commands...), extra...)))
		}
		g.BuildDir = "build/" + string(id)
	}
	for _, b := range builders {
		if b != id {
			g.Builders = append(g.Builders, b)
		}
	}
	if t != nil && t.AuthorityAgent == id {
		if everyone {
			builders = t.Participants
		}
		if len(builders) > 1 {
			g.Assigns = builders
		}
	}
	return g
}

// BuildersFor reports who builds on task t: the agents named, everyone the
// level lets run commands, or (neither) the human.
func (a *Access) BuildersFor(t *domain.Task) (builders []domain.AgentID, everyone bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.buildersLocked(t)
}

// buildersLocked works out who builds on task t: the agents named, or
// everyone the level lets run commands.
//
// The task's builder mode narrows the level, never widens it. Manual leaves
// builds to the human. A named participant builds alone, and at the
// sandboxed builder level only if its sandbox was verified. Auto follows
// the level: at level 1 the verified sandboxed participants, at levels 2 and
// 3 every agent; at level 0 nobody.
func (a *Access) buildersLocked(t *domain.Task) (builders []domain.AgentID, everyone bool) {
	if a.level == domain.AccessFilesOnly {
		return nil, false
	}
	mode := domain.BuilderAuto
	if t != nil {
		mode = t.Builder
	}
	switch mode {
	case domain.BuilderManual:
		return nil, false
	case domain.BuilderAuto:
		if a.level != domain.AccessSandboxedBuilder {
			return nil, true
		}
		if t == nil {
			return nil, false
		}
		for _, p := range append([]domain.AgentID{t.AuthorityAgent}, t.Participants...) {
			if a.sandboxed[p] && !slices.Contains(builders, p) {
				builders = append(builders, p)
			}
		}
		return builders, false
	}
	named := domain.AgentID(mode)
	if a.level == domain.AccessSandboxedBuilder && !a.sandboxed[named] {
		return nil, false
	}
	return []domain.AgentID{named}, false
}

// CleanCommands trims the commands and drops blank and repeated ones.
func CleanCommands(cmds []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cmds {
		c = strings.Join(strings.Fields(c), " ")
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
