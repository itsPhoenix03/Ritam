package core

import (
	"slices"
	"strings"
	"testing"

	"ritam/internal/domain"
)

func accessTask() *domain.Task {
	return &domain.Task{AuthorityAgent: "opus", Participants: []domain.AgentID{"opus", "codex", "sonnet"}}
}

func TestFilesOnlyGrantsNoCommandsWhateverIsListed(t *testing.T) {
	acc := NewAccess(domain.AccessFilesOnly, []string{"cmake", "ctest"})
	acc.SetSandboxed("codex", true)
	for _, id := range []domain.AgentID{"opus", "codex"} {
		g := acc.GrantFor(accessTask(), id, []string{"go test"})
		if g.RunsCommands() {
			t.Errorf("%s may run %v at level 0", id, g.Commands)
		}
	}
	if r := WriteRules("/p", acc.GrantFor(accessTask(), "opus", nil)); !strings.Contains(r, "ask the human") {
		t.Errorf("at level 0 an agent that needs a build asks the human:\n%s", r)
	}
}

func TestOnlySandboxedAgentsBuildAtTheSandboxedBuilderLevel(t *testing.T) {
	acc := NewAccess(domain.AccessSandboxedBuilder, []string{"cmake", "ctest", "rm -rf build"})
	acc.SetSandboxed("codex", true)
	acc.SetSandboxed("opus", false)

	builder := acc.GrantFor(accessTask(), "codex", nil)
	if !slices.Equal(builder.Commands, []string{"cmake", "ctest"}) {
		t.Errorf("the sandboxed agent runs the listed commands, never a destructive one: %v", builder.Commands)
	}
	other := acc.GrantFor(accessTask(), "opus", nil)
	if other.RunsCommands() {
		t.Errorf("an unsandboxed agent may not build at level 1: %v", other.Commands)
	}
	if !slices.Equal(other.Builders, []domain.AgentID{"codex"}) {
		t.Errorf("it is told who builds: %v", other.Builders)
	}
	if r := WriteRules("/p", other); !strings.Contains(r, "codex") || !strings.Contains(r, "sandbox") {
		t.Errorf("the brief names the builder:\n%s", r)
	}

	// A task without a sandboxed participant has nobody to build.
	alone := acc.GrantFor(&domain.Task{AuthorityAgent: "opus", Participants: []domain.AgentID{"opus"}}, "opus", nil)
	if len(alone.Builders) != 0 || alone.RunsCommands() {
		t.Errorf("no builder on the task: %+v", alone)
	}
	if r := WriteRules("/p", alone); !strings.Contains(r, "ask the human") {
		t.Errorf("with no builder the agent asks the human:\n%s", r)
	}
}

func TestBuildAndTestGrantsTheListToEveryAgent(t *testing.T) {
	acc := NewAccess(domain.AccessBuildAndTest, []string{" cmake  --build ", "ctest", "ctest", ""})
	g := acc.GrantFor(accessTask(), "opus", []string{"go test", "curl x"})
	if !slices.Equal(g.Commands, []string{"cmake --build", "ctest", "go test"}) {
		t.Errorf("the workspace's list plus the agent's own, cleaned, blocked ones dropped: %v", g.Commands)
	}
	if g.AnyCommand {
		t.Error("level 2 is not any command")
	}
}

func TestAnyCommandStillKeepsTheBlockedList(t *testing.T) {
	acc := NewAccess(domain.AccessAnyCommand, nil)
	g := acc.GrantFor(accessTask(), "opus", nil)
	if !g.AnyCommand {
		t.Fatal("level 3 lets the agent run any command")
	}
	r := WriteRules("/p", g)
	if !strings.Contains(r, "any command") || !strings.Contains(r, "blocked") {
		t.Errorf("the brief allows any command but still names the blocked ones:\n%s", r)
	}
}

func TestAChangeAppliesToTheNextGrant(t *testing.T) {
	acc := NewAccess(domain.AccessFilesOnly, nil)
	if acc.GrantFor(nil, "opus", nil).RunsCommands() {
		t.Fatal("level 0 runs nothing")
	}
	acc.Set(domain.AccessBuildAndTest, []string{"go test"})
	if g := acc.GrantFor(nil, "opus", nil); !slices.Equal(g.Commands, []string{"go test"}) {
		t.Errorf("after raising the level the next turn may run the list: %v", g.Commands)
	}
}

func TestANilAccessKeepsTheAgentsOwnList(t *testing.T) {
	var acc *Access
	g := acc.GrantFor(nil, "opus", []string{"go test", "rm x"})
	if !slices.Equal(g.Commands, []string{"go test"}) {
		t.Errorf("an adapter built outside the app runs its own safe list: %v", g.Commands)
	}
}
