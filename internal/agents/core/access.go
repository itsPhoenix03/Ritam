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
	// Builders are the task's agents that may build, named to an agent that
	// may not, at the sandboxed builder level.
	Builders []domain.AgentID
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
	listed := func() []string {
		cmds, _ := SafeCommands(CleanCommands(append(append([]string(nil), a.commands...), extra...)))
		return cmds
	}
	switch a.level {
	case domain.AccessSandboxedBuilder:
		if a.sandboxed[id] {
			g.Commands = listed()
			break
		}
		if t != nil {
			for _, p := range append([]domain.AgentID{t.AuthorityAgent}, t.Participants...) {
				if p != id && a.sandboxed[p] && !slices.Contains(g.Builders, p) {
					g.Builders = append(g.Builders, p)
				}
			}
		}
	case domain.AccessBuildAndTest:
		g.Commands = listed()
	case domain.AccessAnyCommand:
		g.AnyCommand = true
	}
	return g
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
