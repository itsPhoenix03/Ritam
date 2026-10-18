package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// builderSystem is a system with real (never started) Claude Code and Codex
// agents at the sandboxed builder level, whose sandbox check is stubbed.
func builderSystem(t *testing.T, sandboxed bool) (*System, *int) {
	t.Helper()
	cfg := config.Default()
	cfg.TUI.UseMockAgents = false
	cfg.Agents = map[string]config.AgentConfig{
		"opus":  {Type: "claude-code", Role: "AUTHORITY", Enabled: true, Executable: "claude", AllowWrites: true},
		"codex": {Type: "codex", Role: "IMPLEMENTER", Enabled: true, Executable: "codex", AllowWrites: true, Model: "gpt-6"},
	}
	cfg.Storage.Path = filepath.Join(t.TempDir(), "ritam.db")
	cfg.Runtime.ModelsFile = filepath.Join(t.TempDir(), "models.json")
	sys, err := Build(cfg, logging.NewDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sys.Close() })
	calls := 0
	tm := sys.Service.team
	tm.versions = map[string]string{"codex": "codex-cli 9.9", "claude-code": "2.1"}
	tm.canary = func(_ context.Context, provider, model string) (bool, string, error) {
		calls++
		if provider != "codex" {
			t.Errorf("only a CLI that declares a sandbox is checked, not %s", provider)
		}
		if sandboxed {
			return true, "", nil
		}
		return false, "it wrote outside the workspace: its CLI is not sandboxed", nil
	}
	if _, err := sys.Service.SetAccess(context.Background(), ports.AccessChange{Level: domain.AccessSandboxedBuilder,
		Commands: []string{"cmake"}}); err != nil {
		t.Fatal(err)
	}
	return sys, &calls
}

func startEvent(t *testing.T, sys *System, id domain.TaskID) string {
	t.Helper()
	evs, _ := sys.Service.TaskEvents(context.Background(), id)
	for _, e := range evs {
		if e.Type == domain.EvtAccessLevel {
			return e.Summary
		}
	}
	t.Fatal("no access event")
	return ""
}

// TestABuilderMustPassTheSandboxCheck: a Codex agent declares a sandbox, but
// builds only once the canary confirmed it; the check runs once per model
// and CLI version, and Claude Code, which declares none, is never checked.
func TestABuilderMustPassTheSandboxCheck(t *testing.T) {
	sys, calls := builderSystem(t, true)
	ctx := context.Background()
	if info, _ := sys.Service.Access(ctx); len(info.Builders) != 0 {
		t.Fatalf("a declared sandbox is not trusted before the check: %v", info.Builders)
	}
	task, err := sys.Service.CreateTask(ctx, ports.CreateTaskInput{Title: "build it", Authority: "opus",
		Participants: []domain.AgentID{"opus", "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Errorf("creating the task checks the candidate once, got %d checks", *calls)
	}
	if info, _ := sys.Service.Access(ctx); !slices.Equal(info.Builders, []domain.AgentID{"codex"}) {
		t.Errorf("after the check codex builds: %v", info.Builders)
	}
	if g := sys.Access.GrantFor(task, "codex", nil); !slices.Equal(g.Commands, []string{"cmake"}) || g.BuildDir != "build/codex" {
		t.Errorf("codex builds in its own folder: %+v", g)
	}
	if s := startEvent(t, sys, task.ID); !strings.Contains(s, "codex builds") {
		t.Errorf("the task records who builds: %q", s)
	}
	if rec, ok := sys.Service.team.sandboxRecordFor("codex", "gpt-6"); !ok || !rec.Sandboxed || rec.CLIVersion != "codex-cli 9.9" {
		t.Errorf("the result is kept with the CLI version: %+v", rec)
	}

	if _, err := sys.Service.CreateTask(ctx, ports.CreateTaskInput{Title: "again", Authority: "opus",
		Participants: []domain.AgentID{"opus", "codex"}}); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Errorf("a known result for the same CLI version is reused, got %d checks", *calls)
	}
	sys.Service.team.versions["codex"] = "codex-cli 10.0"
	if _, err := sys.Service.CreateTask(ctx, ports.CreateTaskInput{Title: "upgraded", Authority: "opus",
		Participants: []domain.AgentID{"opus", "codex"}}); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 {
		t.Errorf("a new CLI version is checked again, got %d checks", *calls)
	}
}

// TestNoBuilderFallsBackToTheHuman: when the check fails, nobody builds at
// level 1, the task records it, and the agents are sent to the human.
func TestNoBuilderFallsBackToTheHuman(t *testing.T) {
	sys, _ := builderSystem(t, false)
	ctx := context.Background()
	task, err := sys.Service.CreateTask(ctx, ports.CreateTaskInput{Title: "build it", Authority: "opus",
		Participants: []domain.AgentID{"opus", "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if s := startEvent(t, sys, task.ID); !strings.Contains(s, "the human builds") {
		t.Errorf("no builder means the human builds: %q", s)
	}
	if g := sys.Access.GrantFor(task, "codex", nil); g.RunsCommands() || !g.Manual {
		t.Errorf("an agent that failed the check does not build: %+v", g)
	}
	_, err = sys.Service.CreateTask(ctx, ports.CreateTaskInput{Title: "named", Authority: "opus",
		Participants: []domain.AgentID{"opus", "codex"}, Builder: "codex"})
	if err == nil || !strings.Contains(err.Error(), "sandbox check") {
		t.Errorf("an unverified agent cannot be named builder at level 1: %v", err)
	}
}

func TestTheBuilderChoiceIsChecked(t *testing.T) {
	sys, _ := builderSystem(t, true)
	ctx := context.Background()
	in := ports.CreateTaskInput{Title: "x", Authority: "opus", Participants: []domain.AgentID{"opus", "codex"}}

	in.Builder = "sonnet"
	if _, err := sys.Service.CreateTask(ctx, in); err == nil || !strings.Contains(err.Error(), "not a participant") {
		t.Errorf("a builder must take part in the task: %v", err)
	}

	in.Builder = domain.BuilderManual
	task, err := sys.Service.CreateTask(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if g := sys.Access.GrantFor(task, "codex", nil); g.RunsCommands() {
		t.Errorf("manual: not even a verified builder runs builds: %+v", g)
	}

	if _, err := sys.Service.SetAccess(ctx, ports.AccessChange{Level: domain.AccessFilesOnly}); err != nil {
		t.Fatal(err)
	}
	in.Builder = "codex"
	if _, err := sys.Service.CreateTask(ctx, in); err == nil || !strings.Contains(err.Error(), "level 0") {
		t.Errorf("nobody may be named builder at level 0: %v", err)
	}
}

// TestTheCanaryIsJudgedByTheFilesItLeaves: ritam, not the agent, decides.
func TestTheCanaryIsJudgedByTheFilesItLeaves(t *testing.T) {
	dir := t.TempDir()
	inside, outside := filepath.Join(dir, "in.txt"), filepath.Join(dir, "out.txt")
	touch := func(p string) {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	touch(inside)
	if ok, note, err := judgeCanary(inside, outside, nil, ""); !ok || err != nil {
		t.Errorf("inside written, outside blocked: sandboxed (%v %q %v)", ok, note, err)
	}
	touch(outside)
	if ok, note, _ := judgeCanary(inside, outside, nil, ""); ok || !strings.Contains(note, "not sandboxed") {
		t.Errorf("a file outside means no sandbox: %v %q", ok, note)
	}
	_ = os.Remove(inside)
	_ = os.Remove(outside)
	if ok, note, err := judgeCanary(inside, outside, nil, ""); ok || err != nil || !strings.Contains(note, "inconclusive") {
		t.Errorf("an agent that ran nothing proves nothing: %v %q %v", ok, note, err)
	}
	if _, _, err := judgeCanary(inside, outside, errors.New("exit 1"), "not logged in"); err == nil {
		t.Error("a CLI that failed to run is an error, not a result")
	}
}

func TestTheCanaryPromptNamesBothFiles(t *testing.T) {
	p := canaryPrompt(`C:\w\.ritam\tmp\canary-1.txt`, `C:\home\canary\canary-1.txt`)
	for _, want := range []string{`"C:\w\.ritam\tmp\canary-1.txt"`, `"C:\home\canary\canary-1.txt"`, "DONE"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}
