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
	if r := WriteRules("/p", other); !strings.Contains(r, "codex") || !strings.Contains(r, "ask them") {
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
	acc.SetAllowUnsandboxed(true)
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
	acc.SetAllowUnsandboxed(true)
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
	acc.SetAllowUnsandboxed(true)
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

func TestManualLeavesBuildsToTheHumanAtEveryLevel(t *testing.T) {
	task := accessTask()
	task.Builder = domain.BuilderManual
	acc := NewAccess(domain.AccessAnyCommand, []string{"cmake"})
	acc.SetSandboxed("codex", true)
	for _, id := range []domain.AgentID{"opus", "codex"} {
		g := acc.GrantFor(task, id, nil)
		if g.RunsCommands() || !g.Manual {
			t.Errorf("%s: manual means no agent builds: %+v", id, g)
		}
	}
	if r := WriteRules("/p", acc.GrantFor(task, "opus", nil)); !strings.Contains(r, "run by the human") || !strings.Contains(r, "stays open") {
		t.Errorf("the brief sends builds to the human:\n%s", r)
	}
}

func TestANamedBuilderBuildsAlone(t *testing.T) {
	task := accessTask()
	task.Builder = "sonnet"
	acc := NewAccess(domain.AccessBuildAndTest, []string{"cmake"})
	acc.SetAllowUnsandboxed(true)
	if g := acc.GrantFor(task, "sonnet", nil); !slices.Equal(g.Commands, []string{"cmake"}) || g.BuildDir != "build/sonnet" {
		t.Errorf("the named builder runs the list in its own folder: %+v", g)
	}
	g := acc.GrantFor(task, "opus", nil)
	if g.RunsCommands() || !slices.Equal(g.Builders, []domain.AgentID{"sonnet"}) {
		t.Errorf("the others ask it: %+v", g)
	}

	// At level 1 a named builder must have passed the sandbox check.
	acc.Set(domain.AccessSandboxedBuilder, []string{"cmake"})
	if g := acc.GrantFor(task, "sonnet", nil); g.RunsCommands() || !g.Manual {
		t.Errorf("an unverified agent does not build at level 1: %+v", g)
	}
}

func TestSeveralBuildersEachGetAFolderAndTheAuthorityAssigns(t *testing.T) {
	task := &domain.Task{AuthorityAgent: "opus", Participants: []domain.AgentID{"opus", "codex-1", "codex-2"}}
	acc := NewAccess(domain.AccessSandboxedBuilder, []string{"cmake"})
	acc.SetSandboxed("codex-1", true)
	acc.SetSandboxed("codex-2", true)

	one := acc.GrantFor(task, "codex-1", nil)
	if one.BuildDir != "build/codex-1" || !slices.Equal(one.Builders, []domain.AgentID{"codex-2"}) {
		t.Errorf("each builder has its own folder and knows the other: %+v", one)
	}
	r := WriteRules("/p", one)
	if !strings.Contains(r, "build/codex-1/") || !strings.Contains(r, "do not divide the work") {
		t.Errorf("the builder brief:\n%s", r)
	}
	auth := acc.GrantFor(task, "opus", nil)
	if !slices.Equal(auth.Assigns, []domain.AgentID{"codex-1", "codex-2"}) {
		t.Errorf("the authority assigns the builders: %+v", auth)
	}
	if r := WriteRules("/p", auth); !strings.Contains(r, "you assign the build and test work") {
		t.Errorf("the authority brief:\n%s", r)
	}
}

// TestAnAgentWithNoSandboxRunsNoCommandsUnlessAllowed: by default levels 2
// and 3 build the way level 1 does -- only agents with a verified sandbox
// run commands, and the others ask them -- since an agent with no sandbox
// (Claude Code on native Windows) would run them directly on this machine.
func TestAnAgentWithNoSandboxRunsNoCommandsUnlessAllowed(t *testing.T) {
	for _, level := range []domain.AccessLevel{domain.AccessBuildAndTest, domain.AccessAnyCommand} {
		acc := NewAccess(level, []string{"cmake"})
		acc.SetSandboxed("codex", true)
		task := accessTask()
		if g := acc.GrantFor(task, "opus", nil); g.RunsCommands() || !slices.Equal(g.Builders, []domain.AgentID{"codex"}) {
			t.Errorf("level %s: the unsandboxed agent runs nothing and asks codex: %+v", level, g)
		}
		if g := acc.GrantFor(task, "codex", nil); !g.RunsCommands() {
			t.Errorf("level %s: the sandboxed agent builds: %+v", level, g)
		}
		task.Builder = "opus"
		if g := acc.GrantFor(task, "opus", nil); g.RunsCommands() {
			t.Errorf("level %s: naming an unsandboxed builder does not let it run commands: %+v", level, g)
		}

		acc.SetAllowUnsandboxed(true)
		task.Builder = domain.BuilderAuto
		if g := acc.GrantFor(task, "opus", nil); !g.RunsCommands() {
			t.Errorf("level %s: allowed, every agent builds: %+v", level, g)
		}
	}
}
