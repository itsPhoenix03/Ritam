package claude

import (
	"context"
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

// These tests drive the adapter against a fake `claude` that records how it
// was invoked. The real CLI's behaviour they mirror was checked live: a
// --session-id chosen by the caller sticks, --resume keeps the same id and the
// context, and resuming an unknown id exits 1 with "No conversation found".

type call struct {
	args   string
	prompt string
	recap  bool
	cwd    string
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

// sessionAdapter starts an adapter on the fake session CLI with the given
// registry.
func sessionAdapter(t *testing.T, reg *core.SessionRegistry, workingDir string) (*ClaudeCodeAdapter, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	cliLog := filepath.Join(t.TempDir(), "cli.log")
	t.Setenv(fakeModeEnv, "session")
	t.Setenv(fakeLogEnv, cliLog)

	a := New("opus", config.AgentConfig{Executable: exe, ResponseTimeout: time.Minute, WorkingDir: workingDir},
		logging.NewDiscard())
	a.UseSessions(reg)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, cliLog
}

func turn(t *testing.T, a *ClaudeCodeAdapter, task *domain.Task, m domain.Message) {
	t.Helper()
	if err := a.SendMessage(context.Background(), task, m); err != nil {
		t.Fatal(err)
	}
	if o := nextOutbound(t, a); o.Err != nil {
		t.Fatalf("turn failed: %v", o.Err)
	}
}

func sessionTask(created time.Time) *domain.Task {
	tk := testTask()
	tk.CreatedAt = created
	return tk
}

var uuidRe = regexp.MustCompile(`--session-id ([0-9a-f-]{36})`)

func TestFirstTurnCreatesASessionAndLaterTurnsResumeIt(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	a, log := sessionAdapter(t, reg, "")
	tk := sessionTask(time.Now())

	turn(t, a, tk, msg("first"))
	turn(t, a, tk, msg("second"))

	cs := calls(t, log)
	if len(cs) != 2 {
		t.Fatalf("want 2 CLI calls, got %d: %+v", len(cs), cs)
	}
	m := uuidRe.FindStringSubmatch(cs[0].args)
	if m == nil {
		t.Fatalf("the first turn must create a session with --session-id, args: %s", cs[0].args)
	}
	id := m[1]
	if !strings.Contains(cs[0].args, "-n ritam TASK-001 opus") {
		t.Errorf("the session should be named for a human browsing /resume, args: %s", cs[0].args)
	}
	if cs[0].prompt != "full" {
		t.Errorf("a new session needs the full brief, got %q", cs[0].prompt)
	}

	if !strings.Contains(cs[1].args, "--resume "+id) || strings.Contains(cs[1].args, "--session-id") {
		t.Errorf("the second turn must resume %s, args: %s", id, cs[1].args)
	}
	if strings.Contains(cs[1].args, "--continue") {
		t.Error("--continue picks the most recent session, which may not be this agent's")
	}
	if cs[1].prompt != "turn" {
		t.Errorf("a resumed session gets the short standing brief, got %q", cs[1].prompt)
	}

	sess, ok := reg.Get(core.SessionKeyFor(tk, "opus"))
	if !ok || sess.ID != id || sess.Turns != 2 || sess.State != core.SessionActive {
		t.Errorf("registry entry: %+v (ok=%v)", sess, ok)
	}
}

func TestEachTaskGetsItsOwnSession(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	a, log := sessionAdapter(t, reg, "")

	turn(t, a, sessionTask(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)), msg("old task"))
	turn(t, a, sessionTask(time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)), msg("new task, same number"))

	cs := calls(t, log)
	if len(cs) != 2 {
		t.Fatalf("want 2 calls, got %d", len(cs))
	}
	a1, a2 := uuidRe.FindStringSubmatch(cs[0].args), uuidRe.FindStringSubmatch(cs[1].args)
	if a1 == nil || a2 == nil || a1[1] == a2[1] {
		t.Errorf("a new task (even a reused TASK-001) must start its own session:\n%s\n%s", cs[0].args, cs[1].args)
	}
}

func TestAMissingSessionIsRebuiltAndTheAgentReBriefed(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	tk := sessionTask(time.Now())
	key := core.SessionKeyFor(tk, "opus")
	_ = reg.Save(core.NewSession(key, domain.AgentKindClaudeCode, "11111111-1111-4111-8111-111111111111", "", ""))
	t.Setenv(fakeMissingEnv, "11111111-1111-4111-8111-111111111111")

	a, log := sessionAdapter(t, reg, "")
	m := msg("follow-up")
	m.Metadata = map[string]string{core.RecapKey: "opus -> codex DELEGATE: look at the cookie"}
	turn(t, a, tk, m)

	cs := calls(t, log)
	if len(cs) != 2 {
		t.Fatalf("want a failed resume then a fresh session, got %d calls: %+v", len(cs), cs)
	}
	if !strings.Contains(cs[0].args, "--resume 11111111-1111-4111-8111-111111111111") {
		t.Errorf("first the recorded session is tried: %s", cs[0].args)
	}
	if uuidRe.FindStringSubmatch(cs[1].args) == nil || cs[1].prompt != "full" || !cs[1].recap {
		t.Errorf("then a new session with the full brief and the recap: %+v", cs[1])
	}

	sess, _ := reg.Get(key)
	if sess.State != core.SessionRebuilt {
		t.Errorf("the entry must say the session was rebuilt, got %q", sess.State)
	}
	if len(sess.Previous) != 1 || sess.Previous[0] != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("the lost id must stay in history, got %v", sess.Previous)
	}
}

func TestSessionsSurviveAnOrchestratorRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	tk := sessionTask(time.Now())

	reg1, _ := core.OpenSessionRegistry(path)
	a1, log1 := sessionAdapter(t, reg1, "")
	turn(t, a1, tk, msg("before restart"))
	_ = a1.Stop(context.Background())
	id := uuidRe.FindStringSubmatch(calls(t, log1)[0].args)[1]

	reg2, err := core.OpenSessionRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	a2, log2 := sessionAdapter(t, reg2, "")
	turn(t, a2, tk, msg("after restart"))

	if cs := calls(t, log2); len(cs) != 1 || !strings.Contains(cs[0].args, "--resume "+id) {
		t.Errorf("after a restart the same session must be resumed (%s), got %+v", id, cs)
	}
}

func TestTurnsRunInTheSessionsWorkingDirectory(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	dir := t.TempDir()
	a, log := sessionAdapter(t, reg, dir)
	tk := sessionTask(time.Now())

	turn(t, a, tk, msg("one"))
	// Even if the configured directory later changes, the session stays
	// where it was created: Claude cannot find it anywhere else.
	a.runner.WorkingDir = t.TempDir()
	turn(t, a, tk, msg("two"))

	for i, c := range calls(t, log) {
		if !sameDir(c.cwd, dir) {
			t.Errorf("call %d ran in %s, want the session's directory %s", i, c.cwd, dir)
		}
	}
}

func sameDir(a, b string) bool {
	ea, _ := filepath.EvalSymlinks(a)
	eb, _ := filepath.EvalSymlinks(b)
	return strings.EqualFold(filepath.Clean(ea), filepath.Clean(eb))
}

func TestRecapIsOnlyForAnAgentWithNoMemory(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	a, log := sessionAdapter(t, reg, "")
	tk := sessionTask(time.Now())
	m := msg("x")
	m.Metadata = map[string]string{core.RecapKey: "earlier: codex answered"}

	turn(t, a, tk, m)
	turn(t, a, tk, m)

	cs := calls(t, log)
	if !cs[0].recap {
		t.Error("a new session must get the transcript of the task so far")
	}
	if cs[1].recap {
		t.Error("a resumed session already holds the conversation; resending it wastes context")
	}
}

func TestReadOnlyFlagsAreSentOnEveryResume(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	a, log := sessionAdapter(t, reg, "")
	tk := sessionTask(time.Now())
	turn(t, a, tk, msg("one"))
	turn(t, a, tk, msg("two"))

	for i, c := range calls(t, log) {
		if !strings.Contains(c.args, "--disallowedTools") || !strings.Contains(c.args, "--permission-mode dontAsk") {
			t.Errorf("call %d lost the read-only flags (permissions are not saved with a session): %s", i, c.args)
		}
	}
}
