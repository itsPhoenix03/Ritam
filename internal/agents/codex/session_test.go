package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/core"
	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
)

// The real CLI's behaviour these tests mirror was checked live: `exec --json`
// opens with {"type":"thread.started","thread_id":...}; `exec resume <id> -`
// keeps the thread and its context; an unknown id exits 1 with "no rollout
// found"; and `exec resume` has no --sandbox flag, so read-only must be forced
// with `-c sandbox_mode="read-only"` (a resumed turn with it could not write).

const fakeMissingEnv = "RITAM_FAKE_CODEX_MISSING"

// runFakeSessionCLI records its invocation and answers like the real CLI: a
// new run announces a fresh thread id, a resume continues the given one.
func runFakeSessionCLI() {
	args := os.Args[1:]
	stdin := readAll(os.Stdin)
	kind := "other"
	switch {
	case strings.Contains(stdin, "YOUR AGENT ID"):
		kind = "full"
	case strings.Contains(stdin, "continues your session"):
		kind = "turn"
	}
	cwd, _ := os.Getwd()
	fakeLog(fmt.Sprintf("call args=[%s] prompt=%s recap=%v cwd=%s",
		strings.Join(args, " "), kind, strings.Contains(stdin, "CONVERSATION SO FAR"), cwd))

	resume := len(args) > 1 && args[0] == "exec" && args[1] == "resume"
	if resume {
		thread := args[len(args)-2] // ... <thread> -
		if thread == os.Getenv(fakeMissingEnv) {
			_, _ = os.Stderr.WriteString("Error: thread/resume: thread/resume failed: no rollout found for thread id " +
				thread + " (code -32600)\n")
			os.Exit(1)
		}
	} else {
		fmt.Printf("{\"type\":\"thread.started\",\"thread_id\":\"thread-%d\"}\n", time.Now().UnixNano())
	}
	writeReply()
}

type call struct {
	args, prompt, cwd string
	recap             bool
}

var callRe = regexp.MustCompile(`^call args=\[(.*)\] prompt=(\w+) recap=(\w+) cwd=(.*)$`)

func calls(t *testing.T, log string) []call {
	t.Helper()
	var out []call
	for _, l := range strings.Split(readFile(log), "\n") {
		if m := callRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			out = append(out, call{args: m[1], prompt: m[2], recap: m[3] == "true", cwd: m[4]})
		}
	}
	return out
}

func sessionAdapter(t *testing.T, reg *core.SessionRegistry, cfg config.AgentConfig) (*CodexAdapter, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	cliLog := filepath.Join(t.TempDir(), "cli.log")
	t.Setenv(fakeModeEnv, "session")
	t.Setenv(fakeLogEnv, cliLog)
	cfg.Executable, cfg.ResponseTimeout = exe, time.Minute

	a := New("codex", cfg, logging.NewDiscard())
	a.UseSessions(reg)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, cliLog
}

func turn(t *testing.T, a *CodexAdapter, task *domain.Task, m domain.Message) {
	t.Helper()
	if err := a.SendMessage(context.Background(), task, m); err != nil {
		t.Fatal(err)
	}
	if o := nextOutbound(t, a); o.Err != nil {
		t.Fatalf("turn failed: %v", o.Err)
	}
}

func sessionTask() *domain.Task {
	tk := testTask()
	tk.CreatedAt = time.Now()
	return tk
}

func codexMsg(content string) domain.Message {
	m := msg(content)
	m.ToAgent = "codex"
	return m
}

func TestThreadIsCapturedThenResumed(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	a, log := sessionAdapter(t, reg, config.AgentConfig{})
	tk := sessionTask()

	turn(t, a, tk, codexMsg("first"))
	sess, ok := reg.Get(core.SessionKeyFor(tk, "codex"))
	if !ok || !strings.HasPrefix(sess.ID, "thread-") {
		t.Fatalf("the thread id Codex announced must be recorded, got %+v (ok=%v)", sess, ok)
	}

	turn(t, a, tk, codexMsg("second"))
	cs := calls(t, log)
	if len(cs) != 2 {
		t.Fatalf("want 2 calls, got %d", len(cs))
	}
	if strings.Contains(cs[0].args, "resume") || cs[0].prompt != "full" {
		t.Errorf("the first turn is a new thread with the full brief: %+v", cs[0])
	}
	if !strings.HasPrefix(cs[1].args, "exec resume") || !strings.HasSuffix(cs[1].args, sess.ID+" -") {
		t.Errorf("the second turn must resume %s, args: %s", sess.ID, cs[1].args)
	}
	if strings.Contains(cs[1].args, "--last") {
		t.Error("--last picks the most recent thread, which may not be this agent's")
	}
	if cs[1].prompt != "turn" {
		t.Errorf("a resumed thread gets the short standing brief, got %q", cs[1].prompt)
	}
	if got, _ := reg.Get(core.SessionKeyFor(tk, "codex")); got.Turns != 2 {
		t.Errorf("turns = %d, want 2", got.Turns)
	}
}

func TestResumeForcesTheSandbox(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	a, log := sessionAdapter(t, reg, config.AgentConfig{
		// A configured sandbox override must not survive onto a resume, and
		// flags `exec resume` does not accept must not break it.
		Args: []string{"-c", `sandbox_mode="danger-full-access"`, "-c", "model_reasoning_effort=high", "--search"},
	})
	tk := sessionTask()
	turn(t, a, tk, codexMsg("one"))
	turn(t, a, tk, codexMsg("two"))

	resume := calls(t, log)[1].args
	if !strings.Contains(resume, `-c sandbox_mode="read-only"`) {
		t.Errorf("a resumed turn must force the read-only sandbox, args: %s", resume)
	}
	if strings.Contains(resume, "danger-full-access") {
		t.Errorf("a configured sandbox override leaked onto the resume: %s", resume)
	}
	if !strings.Contains(resume, "model_reasoning_effort=high") {
		t.Errorf("harmless config overrides should carry over: %s", resume)
	}
	for _, bad := range []string{"--sandbox ", "--cd ", "--color ", "--search"} {
		if strings.Contains(resume, bad) {
			t.Errorf("`exec resume` does not accept %q, args: %s", bad, resume)
		}
	}
}

func TestAMissingThreadIsRebuilt(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	tk := sessionTask()
	key := core.SessionKeyFor(tk, "codex")
	_ = reg.Save(core.NewSession(key, domain.AgentKindCodex, "gone-thread", "", ""))
	t.Setenv(fakeMissingEnv, "gone-thread")

	a, log := sessionAdapter(t, reg, config.AgentConfig{})
	m := codexMsg("follow-up")
	m.Metadata = map[string]string{core.RecapKey: "opus -> codex DELEGATE: check the cookie"}
	turn(t, a, tk, m)

	cs := calls(t, log)
	if len(cs) != 2 || !strings.Contains(cs[0].args, "gone-thread") {
		t.Fatalf("want a failed resume then a new thread, got %+v", cs)
	}
	if strings.Contains(cs[1].args, "resume") || cs[1].prompt != "full" || !cs[1].recap {
		t.Errorf("the rebuild is a new thread with the full brief and recap: %+v", cs[1])
	}
	sess, _ := reg.Get(key)
	if sess.State != core.SessionRebuilt || !strings.HasPrefix(sess.ID, "thread-") {
		t.Errorf("entry after rebuild: %+v", sess)
	}
	if len(sess.Previous) != 1 || sess.Previous[0] != "gone-thread" {
		t.Errorf("the lost thread must stay in history: %v", sess.Previous)
	}
}

func TestResumeRunsInTheThreadsWorkingDirectory(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	dir := t.TempDir()
	a, log := sessionAdapter(t, reg, config.AgentConfig{WorkingDir: dir})
	tk := sessionTask()
	turn(t, a, tk, codexMsg("one"))
	turn(t, a, tk, codexMsg("two"))

	c := calls(t, log)[1]
	ea, _ := filepath.EvalSymlinks(c.cwd)
	eb, _ := filepath.EvalSymlinks(dir)
	if !strings.EqualFold(filepath.Clean(ea), filepath.Clean(eb)) {
		t.Errorf("`exec resume` has no --cd, so it must run in %s; ran in %s", dir, c.cwd)
	}
}

func TestThreadStartedParsing(t *testing.T) {
	if id, ok := threadStarted(`{"type":"thread.started","thread_id":"01a11f53-f23e"}`); !ok || id != "01a11f53-f23e" {
		t.Errorf("got %q %v", id, ok)
	}
	for _, l := range []string{`{"type":"turn.started"}`, `not json`, `{"type":"thread.started"}`} {
		if _, ok := threadStarted(l); ok {
			t.Errorf("%q must not count as a thread start", l)
		}
	}
}

// TestConfigOverridesReachCodexButSandboxOverridesDoNot is a regression test.
// The flag filter compared names case-insensitively, so `-c` (config override)
// matched the blocked `-C` (change directory) and every configured
// `-c key=value` was silently dropped. Letting `-c` through must not open a
// way round the sandbox, so overrides of it are refused by name.
func TestConfigOverridesReachCodexButSandboxOverridesDoNot(t *testing.T) {
	a := New("codex", config.AgentConfig{Args: []string{
		"-c", "model_reasoning_effort=high",
		"--config=hide_agent_reasoning=true",
		"-c", `sandbox_mode="danger-full-access"`,
		"-c", `approval_policy="never"`,
		"-C", `C:\elsewhere`,
	}}, logging.NewDiscard())

	got := strings.Join(a.buildArgs("out.txt"), " ")
	for _, want := range []string{"-c model_reasoning_effort=high", "-c hide_agent_reasoning=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("configured override %q was dropped: %s", want, got)
		}
	}
	for _, bad := range []string{"danger-full-access", "approval_policy", `C:\elsewhere`} {
		if strings.Contains(got, bad) {
			t.Errorf("%q must not reach Codex: %s", bad, got)
		}
	}
	if !strings.Contains(got, "--sandbox read-only") {
		t.Errorf("the orchestrator's own sandbox flag is missing: %s", got)
	}
}
