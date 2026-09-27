package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectorch/internal/config"
	"projectorch/internal/domain"
	"projectorch/internal/logging"
	"projectorch/internal/ports"
)

// fakeModeEnv switches the test binary into a fake `codex` CLI. The adapter
// is pointed at the test binary as its executable, so these tests exercise the
// real spawn/stdin/kill path without the real CLI, a network or any cost.
const (
	fakeModeEnv = "ORCH_FAKE_CODEX_MODE"
	fakeLogEnv  = "ORCH_FAKE_CODEX_LOG"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeModeEnv); mode != "" {
		runFakeCLI(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeLog(line string) {
	f, err := os.OpenFile(os.Getenv(fakeLogEnv), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}
}

func runFakeCLI(mode string) {
	switch mode {
	case "reply":
		fakeLog("stdin:" + readAll(os.Stdin))
		writeReply()
	case "slow":
		fakeLog("start:" + firstLineOf(readAll(os.Stdin)))
		time.Sleep(400 * time.Millisecond)
		fakeLog("end")
		writeReply()
	case "hang":
		fakeLog("start")
		time.Sleep(2 * time.Minute)
	case "fail":
		os.Exit(3)
	case "session":
		runFakeSessionCLI()
	case "costly":
		runFakeCostlyCLI()
	case "silent":
		_ = readAll(os.Stdin)
		for i, a := range os.Args {
			if a == "--output-last-message" && i+1 < len(os.Args) {
				_ = os.WriteFile(os.Args[i+1], []byte("  \n"), 0o600)
			}
		}
	}
}

// TestSayingNothingIsNotAFailure: told that no reply is needed, an agent may
// finish its turn with nothing to say. That used to be reported as a failed
// agent, set to ERROR, and sent an "unparseable reply" notice.
func TestSayingNothingIsNotAFailure(t *testing.T) {
	a, _, _ := fakeAdapter(t, "silent")
	if err := a.SendMessage(context.Background(), testTask(), msg("No reply needed.")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn to finish", func() bool { return a.Status() == domain.AgentStateIdle })
	select {
	case o := <-a.Outbound():
		if o.Err != nil {
			t.Fatalf("an empty reply must not be a failure: %v", o.Err)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

// runFakeCostlyCLI reports usage the way the real CLI does: turn.completed
// carries running totals for the thread, which grow by 1000 input (400 of it
// cached) and 50 output tokens per call.
func runFakeCostlyCLI() {
	_ = readAll(os.Stdin)
	fakeLog("call")
	n := int64(strings.Count(readFile(os.Getenv(fakeLogEnv)), "call"))
	_, _ = os.Stdout.WriteString(`{"type":"thread.started","thread_id":"thread-costly"}` + "\n")
	_, _ = os.Stdout.WriteString(fmt.Sprintf(`{"type":"turn.completed","usage":{"input_tokens":%d,`+
		`"cached_input_tokens":%d,"output_tokens":%d}}`+"\n", 1000*n, 400*n, 50*n))
	writeReply()
}

func readAll(f *os.File) string {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return string(out)
		}
	}
}

func firstLineOf(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// fakeAdapter starts an adapter whose CLI is the test binary in the given mode.
func fakeAdapter(t *testing.T, mode string) (*CodexAdapter, string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	dir := t.TempDir()
	cliLog := filepath.Join(dir, "cli.log")
	appLog := filepath.Join(dir, "app.log")
	t.Setenv(fakeModeEnv, mode)
	t.Setenv(fakeLogEnv, cliLog)

	lg, err := logging.New(logging.Options{Level: "debug", File: appLog})
	if err != nil {
		t.Fatal(err)
	}
	a := New("opus", config.AgentConfig{Executable: exe, ResponseTimeout: time.Minute}, lg)
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()); _ = lg.Close() })
	return a, cliLog, appLog
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func msg(content string) domain.Message {
	return domain.Message{TaskID: "TASK-001", FromAgent: "human", ToAgent: "opus",
		Type: domain.MsgTaskAssignment, Content: content}
}

// nextOutbound returns the next message or failure, skipping the usage report
// every turn sends first (usage has its own tests).
func nextOutbound(t *testing.T, a *CodexAdapter) ports.Outbound {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case o := <-a.Outbound():
			if o.IsUsageOnly() {
				continue
			}
			return o
		case <-deadline:
			t.Fatal("no outbound within 10s")
			return ports.Outbound{}
		}
	}
}

// TestPromptTravelsOnStdinAndNULIsStripped: a NUL in a task title used to make
// process start fail with "invalid argument" because the prompt was an argv
// entry.
func TestPromptTravelsOnStdinAndNULIsStripped(t *testing.T) {
	a, cliLog, _ := fakeAdapter(t, "reply")
	task := testTask()
	task.Title = "Fix\x00 the bug"

	if err := a.SendTask(context.Background(), task, msg("Start\x00.")); err != nil {
		t.Fatal(err)
	}
	if o := nextOutbound(t, a); o.Err != nil {
		t.Fatalf("invocation failed: %v", o.Err)
	}

	got := readFile(cliLog)
	if !strings.Contains(got, "Fix the bug") || strings.ContainsRune(got, 0) {
		t.Errorf("prompt on stdin should contain the sanitised title, got: %q", got)
	}
}

func TestRenderPromptStripsControlCharacters(t *testing.T) {
	task := testTask()
	task.Title = "a\x00b\x1bc"
	p := renderPrompt(task, msg("x\ty\r\nz"), false)
	if strings.ContainsAny(p, "\x00\x1b") {
		t.Error("control characters must be removed from the prompt")
	}
	if !strings.Contains(p, "x\ty\r\nz") {
		t.Error("newline, carriage return and tab must survive")
	}
}

// TestCancelledInvocationIsNotAFailure: a cancelled call used to be published
// as "exited with code 1" and would mark the agent AGENT_ERROR.
func TestCancelledInvocationIsNotAFailure(t *testing.T) {
	a, cliLog, appLog := fakeAdapter(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())

	if err := a.SendMessage(ctx, nil, msg("go")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "child to start", func() bool { return strings.Contains(readFile(cliLog), "start") })
	cancel()

	waitFor(t, "agent to go idle", func() bool { return a.Status() == domain.AgentStateIdle })
	select {
	case o := <-a.Outbound():
		t.Fatalf("a cancel must not publish an outbound, got %+v", o)
	case <-time.After(300 * time.Millisecond):
	}
	if strings.Contains(readFile(appLog), "level=ERROR") {
		t.Errorf("a cancel must not be logged as an error:\n%s", readFile(appLog))
	}
}

// TestInvocationsAreSerialised: two messages must not run two CLI processes at
// once, and the second waits for the first.
func TestInvocationsAreSerialised(t *testing.T) {
	a, cliLog, _ := fakeAdapter(t, "slow")

	_ = a.SendMessage(context.Background(), nil, msg("first"))
	_ = a.SendMessage(context.Background(), nil, msg("second"))
	if a.Status() != domain.AgentStateWorking {
		t.Errorf("status = %s, want WORKING", a.Status())
	}

	nextOutbound(t, a)
	// After the first reply the agent must still be busy with the second.
	if a.Status() != domain.AgentStateWorking {
		t.Errorf("status = %s between queued calls, want WORKING", a.Status())
	}
	nextOutbound(t, a)
	waitFor(t, "idle after the queue drains", func() bool { return a.Status() == domain.AgentStateIdle })

	lines := strings.Split(strings.TrimSpace(readFile(cliLog)), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "start") || lines[1] != "end" ||
		!strings.HasPrefix(lines[2], "start") || lines[3] != "end" {
		t.Errorf("calls overlapped or were reordered: %v", lines)
	}
}

func TestStopKillsRunningCallAndDropsQueue(t *testing.T) {
	a, cliLog, _ := fakeAdapter(t, "hang")

	for i := 0; i < 3; i++ {
		if err := a.SendMessage(context.Background(), nil, msg("go")); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "first child", func() bool { return strings.Contains(readFile(cliLog), "start") })

	done := make(chan struct{})
	go func() { _ = a.Stop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return promptly")
	}
	if n := strings.Count(readFile(cliLog), "start"); n != 1 {
		t.Errorf("%d children were started, want 1: queued calls must be dropped", n)
	}
	if err := a.SendMessage(context.Background(), nil, msg("late")); err == nil {
		t.Error("a stopped adapter must refuse new messages")
	}
}

// TestLateFailureIsLoggedByTheAdapter: nobody drains Outbound here, as when the
// orchestrator's run loop has already exited.
func TestLateFailureIsLoggedByTheAdapter(t *testing.T) {
	a, _, appLog := fakeAdapter(t, "fail")

	if err := a.SendMessage(context.Background(), nil, msg("go")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "an ERROR log line", func() bool {
		s := readFile(appLog)
		return strings.Contains(s, "level=ERROR") && strings.Contains(s, "invocation failed")
	})
	s := readFile(appLog)
	if !strings.Contains(s, "agentId=opus") || !strings.Contains(s, "no output") {
		t.Errorf("log should name the agent and a non-empty reason:\n%s", s)
	}
	if n := strings.Count(s, "invocation failed"); n != 1 {
		t.Errorf("failure logged %d times, want once", n)
	}
}

// writeReply plays Codex's part: the final message goes to the file named by
// --output-last-message, not to stdout.
func writeReply() {
	for i, a := range os.Args {
		if a == "--output-last-message" && i+1 < len(os.Args) {
			_ = os.WriteFile(os.Args[i+1], []byte(headerMarker+" type=ANSWER to=human\nok"), 0o600)
		}
	}
}

// TestATurnIsChargedOnlyItsOwnTokensAndReportedAfterItsReply: Codex reports
// running totals for the thread, so each turn is charged the growth, and the
// usage arrives after the reply (see the Claude adapter's twin test).
func TestATurnIsChargedOnlyItsOwnTokensAndReportedAfterItsReply(t *testing.T) {
	a, _, _ := fakeAdapter(t, "costly")
	task := testTask()
	for turn := 1; turn <= 3; turn++ {
		if err := a.SendMessage(context.Background(), task, msg(fmt.Sprintf("turn %d", turn))); err != nil {
			t.Fatal(err)
		}
		var got []ports.Outbound
		deadline := time.After(10 * time.Second)
		for len(got) < 2 {
			select {
			case o := <-a.Outbound():
				got = append(got, o)
			case <-deadline:
				t.Fatalf("turn %d: only %d outbound(s)", turn, len(got))
			}
		}
		if got[0].IsUsageOnly() || got[0].Err != nil {
			t.Fatalf("turn %d: the reply must come before the usage, got %+v first", turn, got[0])
		}
		u := got[1].Usage
		if u == nil {
			t.Fatalf("turn %d: no usage after the reply", turn)
		}
		if u.InputTokens != 600 || u.CacheReadTokens != 400 || u.OutputTokens != 50 {
			t.Errorf("turn %d charged %+v, want its own 600 in / 400 cached / 50 out", turn, *u)
		}
	}
}
