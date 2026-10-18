package claude

import (
	"strings"
	"testing"

	"ritam/internal/agents/core"
	"ritam/internal/config"
	"ritam/internal/domain"
)

// toolFlags returns the --allowedTools and --disallowedTools values.
func toolFlags(argv []string) (allowed, denied string) {
	for i, arg := range argv {
		switch arg {
		case "--allowedTools":
			allowed = argv[i+1]
		case "--disallowedTools":
			denied = argv[i+1]
		}
	}
	return allowed, denied
}

func TestTheAccessLevelDecidesWhatBashMayRun(t *testing.T) {
	acc := core.NewAccess(domain.AccessFilesOnly, []string{"cmake"})
	a := New("opus", config.AgentConfig{AllowWrites: true}, nil)
	a.UseAccess(acc)

	allowed, denied := toolFlags(a.buildArgs())
	if strings.Contains(allowed, "Bash") || !strings.Contains(allowed, "Edit") {
		t.Errorf("level 0: edits, no commands: %q", allowed)
	}

	// Claude Code is not sandboxed, so at level 1 it still runs nothing.
	acc.Set(domain.AccessSandboxedBuilder, []string{"cmake"})
	if allowed, _ = toolFlags(a.buildArgs()); strings.Contains(allowed, "Bash") {
		t.Errorf("level 1: an unsandboxed agent may not build: %q", allowed)
	}

	acc.Set(domain.AccessBuildAndTest, []string{"cmake", "ctest"})
	allowed, _ = toolFlags(a.buildArgs())
	if !strings.Contains(allowed, "Bash(cmake:*)") || !strings.Contains(allowed, "Bash(ctest:*)") || strings.HasSuffix(allowed, ",Bash") {
		t.Errorf("level 2: the listed commands only: %q", allowed)
	}

	acc.Set(domain.AccessAnyCommand, nil)
	allowed, denied = toolFlags(a.buildArgs())
	if !strings.HasSuffix(allowed, ",Bash") {
		t.Errorf("level 3: any command: %q", allowed)
	}
	for _, want := range []string{"Bash(rm:*)", "Bash(curl:*)", "Bash(git push:*)", "Bash(taskkill:*)"} {
		if !strings.Contains(denied, want) {
			t.Errorf("level 3 still denies %s: %q", want, denied)
		}
	}
}

func TestAReadOnlyAgentStaysReadOnlyAtAnyLevel(t *testing.T) {
	a := New("opus", config.AgentConfig{AllowWrites: false}, nil)
	a.UseAccess(core.NewAccess(domain.AccessAnyCommand, nil))
	allowed, denied := toolFlags(a.buildArgs())
	if strings.Contains(allowed, "Bash") || strings.Contains(allowed, "Edit") || !strings.Contains(denied, "Bash") {
		t.Errorf("a read-only agent gets no shell and no edits: allowed %q denied %q", allowed, denied)
	}
	if a.RunsInSandbox() {
		t.Error("Claude Code declares no sandbox")
	}
}
