package codex

import (
	"slices"
	"strings"
	"testing"

	"ritam/internal/agents/core"
	"ritam/internal/config"
)

// TestAThreadOutsideTheWorkspaceIsNotResumed: `exec resume` runs where the
// thread was made, so a moved project, a changed workspace root or an
// edited sessions file would otherwise run the agent somewhere else.
func TestAThreadOutsideTheWorkspaceIsNotResumed(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	ws := t.TempDir()
	a, log := sessionAdapter(t, reg, config.AgentConfig{WorkingDir: ws, WorkspaceRoot: ws})
	tk := sessionTask()
	turn(t, a, tk, codexMsg("one"))

	key := core.SessionKeyFor(tk, "codex")
	sess, _ := reg.Get(key)
	old := sess.ID
	sess.WorkingDir = t.TempDir() // elsewhere
	if err := reg.Save(sess); err != nil {
		t.Fatal(err)
	}
	turn(t, a, tk, codexMsg("two"))

	last := calls(t, log)[1]
	if strings.Contains(last.args, "resume") || last.prompt != "full" {
		t.Errorf("the thread outside the workspace must not be resumed; a new one gets the full brief: %+v", last)
	}
	now, _ := reg.Get(key)
	if now.ID == old || !slices.Contains(now.Previous, old) || !config.Within(ws, now.WorkingDir) {
		t.Errorf("a new thread in the workspace replaces it: %+v", now)
	}
}

// TestTheUsersMCPServersAreNotLoaded: servers from ~/.codex/config.toml would
// give the agent tools ritam never granted, on new and resumed turns alike.
func TestTheUsersMCPServersAreNotLoaded(t *testing.T) {
	a := New("codex", config.AgentConfig{Args: []string{"-c", "mcp_servers.x.command=evil"}}, nil)
	for name, args := range map[string][]string{
		"exec":   a.buildArgs("out.txt"),
		"resume": a.buildResumeArgs("thread-1", "out.txt"),
	} {
		joined := strings.Join(args, " ")
		i := strings.LastIndex(joined, "-c mcp_servers={}")
		if i < 0 || i < strings.Index(joined, "mcp_servers.x") {
			t.Errorf("%s: the MCP servers must be cleared after any configured override: %v", name, args)
		}
	}
}

// TestOnlyAllowedArgsReachCodex: what a security review found getting past
// the old list of refused flags is dropped, flag and value.
func TestOnlyAllowedArgsReachCodex(t *testing.T) {
	a := New("codex", config.AgentConfig{WorkingDir: `D:\proj`, Args: []string{
		"--yolo", "--add-dir", `C:\Users\someone`, "-sdanger-full-access", `-CC:\Users\someone`,
		"--profile", "other", "--full-auto", "--search",
		"-c", "model_provider=other", "-c", `model_providers.other.base_url="https://example.invalid"`,
		"-c", `notify=["prog"]`, "-c", "shell_environment_policy.inherit=all", "-c", `developer_instructions="x"`,
		"-c", "model_reasoning_effort=high", "--enable", "some-feature",
	}}, nil)
	got := strings.Join(a.buildArgs("out.txt"), " ")
	for _, bad := range []string{"--yolo", "--add-dir", "someone", "danger-full-access", "--profile", "other",
		"--full-auto", "--search", "model_provider", "notify", "shell_environment_policy", "developer_instructions"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q reached Codex: %s", bad, got)
		}
	}
	for _, want := range []string{"-c model_reasoning_effort=high", "--enable some-feature"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q should be kept: %s", want, got)
		}
	}
}
