package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/ports"
	"ritam/internal/review"
)

// hookModeEnv switches the test binary into ritam's hook. The review server
// in these tests names the test binary as the hook program, so the fake CLI
// runs the real hook code against a real server.
const hookModeEnv = "RITAM_TEST_HOOK_MODE"

// runHookMode is the test binary as `ritam hook ...`: os.Args[1] is "hook".
func runHookMode() int {
	return review.RunHook(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
}

// cliSettings is what the fake CLI reads of its --settings: the hook.
type cliSettings struct {
	Hooks struct {
		PreToolUse []struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type      string   `json:"type"`
				Command   string   `json:"command"`
				Args      []string `json:"args"`
				Timeout   int      `json:"timeout"`
				OnFailure string   `json:"onFailure"`
			} `json:"hooks"`
		} `json:"PreToolUse"`
	} `json:"hooks"`
}

// readSettings reads the settings a CLI was started with: --settings names
// a file, or holds the JSON itself.
func readSettings(args []string) (cliSettings, error) {
	var s cliSettings
	for i, arg := range args {
		if arg != "--settings" || i+1 >= len(args) {
			continue
		}
		data := []byte(args[i+1])
		if !strings.HasPrefix(args[i+1], "{") {
			var err error
			if data, err = os.ReadFile(args[i+1]); err != nil {
				return s, err
			}
		}
		return s, json.Unmarshal(data, &s)
	}
	return s, errors.New("no --settings")
}

// runFakeHookedCLI behaves like Claude Code making two tool calls, as the
// real CLI was seen doing: it streams each call's tool_use block, then runs
// the hook from its settings with the call on stdin before running it. With
// skip, the second call does not go through the hook.
func runFakeHookedCLI(skip bool) {
	_ = readAll(os.Stdin)
	s, err := readSettings(os.Args[1:])
	if err != nil || len(s.Hooks.PreToolUse) == 0 || len(s.Hooks.PreToolUse[0].Hooks) == 0 {
		fakeLog(fmt.Sprintf("no hook in the settings: %v", err))
		os.Exit(1)
	}
	hook := s.Hooks.PreToolUse[0].Hooks[0]
	cwd, _ := os.Getwd()
	for i, call := range []struct{ id, tool, input string }{
		{"toolu_1", "Bash", `{"command":"go test ./...","description":"Run the tests"}`},
		{"toolu_2", "Read", `{"file_path":"main.go"}`},
	} {
		_, _ = os.Stdout.WriteString(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` +
			call.id + `","name":"` + call.tool + `","input":` + call.input + `}]},"parent_tool_use_id":null}` + "\n")
		if skip && i == 1 {
			continue
		}
		where, _ := json.Marshal(cwd)
		payload := `{"session_id":"s-1","cwd":` + string(where) + `,"hook_event_name":"PreToolUse","tool_name":"` +
			call.tool + `","tool_input":` + call.input + `,"tool_use_id":"` + call.id + `"}`
		cmd := exec.Command(hook.Command, hook.Args...)
		cmd.Env = append(os.Environ(), hookModeEnv+"=1")
		cmd.Stdin = strings.NewReader(payload)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				fakeLog("hook did not start: " + err.Error())
				os.Exit(1)
			}
			code = ee.ExitCode()
		}
		fakeLog(fmt.Sprintf("hook %s exit=%d %s", call.id, code, strings.TrimSpace(stderr.String())))
	}
	_, _ = os.Stdout.WriteString(fakeReply)
}

// hookRecord keeps what a review server reports.
type hookRecord struct {
	mu      sync.Mutex
	calls   []review.Call
	reports []review.Report
}

func (r *hookRecord) snapshot() ([]review.Call, []review.Report) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]review.Call(nil), r.calls...), append([]review.Report(nil), r.reports...)
}

// hookedAdapter is a fake-CLI adapter whose tool calls go through a real
// review server, with the test binary as the hook.
func hookedAdapter(t *testing.T, mode string) (*ClaudeCodeAdapter, *hookRecord, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	srv, err := review.Start(exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	rec := &hookRecord{}
	srv.OnCall = func(c review.Call) { rec.mu.Lock(); rec.calls = append(rec.calls, c); rec.mu.Unlock() }
	srv.OnTurnEnd = func(r review.Report) { rec.mu.Lock(); rec.reports = append(rec.reports, r); rec.mu.Unlock() }

	a, cliLog, _ := fakeAdapter(t, mode)
	a.UseHooks(srv)
	return a, rec, cliLog
}

// runOneTurn sends the agent its task and waits for the turn to be over.
func runOneTurn(t *testing.T, a *ClaudeCodeAdapter, rec *hookRecord) {
	t.Helper()
	if err := a.SendTask(context.Background(), testTask(), msg("Run the tests.")); err != nil {
		t.Fatal(err)
	}
	if o := nextOutbound(t, a); o.Err != nil {
		t.Fatalf("turn failed: %v", o.Err)
	}
	waitFor(t, "the turn's hook report", func() bool { _, reps := rec.snapshot(); return len(reps) == 1 })
}

// TestEveryToolCallGoesThroughRitamsHookFirst: the CLI runs ritam's hook
// from the settings ritam gave it, the hook reaches the review server with
// the turn's token, and each call is recorded against the task and agent.
func TestEveryToolCallGoesThroughRitamsHookFirst(t *testing.T) {
	a, rec, cliLog := hookedAdapter(t, "hooked")
	runOneTurn(t, a, rec)

	log := readFile(cliLog)
	for _, id := range []string{"toolu_1", "toolu_2"} {
		if !strings.Contains(log, "hook "+id+" exit=0") {
			t.Errorf("the hook must pass %s: %s", id, log)
		}
	}
	calls, reps := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want both calls recorded, got %+v", calls)
	}
	first := calls[0]
	if first.Task != "TASK-001" || first.Agent != "opus" || first.Kind != domain.AgentKindClaudeCode ||
		first.Tool != "Bash" || first.ToolUseID != "toolu_1" || first.Summary != "go test ./..." || first.SessionID != "s-1" {
		t.Errorf("recorded call: %+v", first)
	}
	if calls[1].Tool != "Read" || calls[1].Summary != "main.go" {
		t.Errorf("recorded call: %+v", calls[1])
	}
	if rep := reps[0]; rep.Seen != 2 || rep.Reported != 2 || rep.Problem() != "" {
		t.Errorf("every reported call reached the hook: %+v (%s)", rep, rep.Problem())
	}
	if len(a.runner.Env) != 0 {
		t.Errorf("the turn's token must stay in that turn's process, not the adapter: %v", a.runner.Env)
	}
}

// TestACallThatSkippedTheHookIsReported: a call the CLI reported in its
// stream but the hook never saw is named in the turn's report.
func TestACallThatSkippedTheHookIsReported(t *testing.T) {
	a, rec, _ := hookedAdapter(t, "hookskips")
	runOneTurn(t, a, rec)

	_, reps := rec.snapshot()
	rep := reps[0]
	if rep.Seen != 1 || rep.Reported != 2 || len(rep.Missed) != 1 || rep.Missed[0].ID != "toolu_2" {
		t.Fatalf("report: %+v", rep)
	}
	if p := rep.Problem(); !strings.Contains(p, "1 of the 2") || !strings.Contains(p, "Read") {
		t.Errorf("problem: %q", p)
	}
}

// stubHooks is a hook monitor that only names its hook.
type stubHooks struct{}

func (stubHooks) HookCommand(domain.AgentKind) ports.HookCommand {
	return ports.HookCommand{Program: `C:\Program Files\ritam\ritam.exe`, Args: []string{"hook", "pretool"}, TimeoutSeconds: 30}
}

func (stubHooks) BeginTurn(ports.TurnScope) ports.HookTurn { return nil }

// TestTheHookIsInstalledToBlockWhenItFails: before every tool, run directly
// (no shell, no PATH lookup), and blocking the call if it cannot run.
func TestTheHookIsInstalledToBlockWhenItFails(t *testing.T) {
	a := New("opus", config.AgentConfig{AllowWrites: true, ScratchDir: t.TempDir()}, nil)
	a.UseHooks(stubHooks{})
	s, err := readSettings(a.buildArgs())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Hooks.PreToolUse) != 1 || len(s.Hooks.PreToolUse[0].Hooks) != 1 {
		t.Fatalf("want one pre-tool hook: %+v", s)
	}
	group, h := s.Hooks.PreToolUse[0], s.Hooks.PreToolUse[0].Hooks[0]
	if group.Matcher != "*" {
		t.Errorf("the hook must run before every tool, got matcher %q", group.Matcher)
	}
	if h.Type != "command" || h.Command != `C:\Program Files\ritam\ritam.exe` || strings.Join(h.Args, " ") != "hook pretool" {
		t.Errorf("the hook runs ritam by its path, with its arguments apart: %+v", h)
	}
	if h.OnFailure != "block" || h.Timeout != 30 {
		t.Errorf("a hook that fails must block the call: %+v", h)
	}

	a = New("opus", config.AgentConfig{AllowWrites: true, ScratchDir: t.TempDir(), ConfineReads: true}, nil)
	if s, err := readSettings(a.buildArgs()); err != nil || len(s.Hooks.PreToolUse) != 0 {
		t.Errorf("without hooks, no hook in the settings: %+v %v", s, err)
	}
}
