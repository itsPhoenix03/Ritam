package review

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
	"ritam/internal/review/facts"
	"ritam/internal/review/rules"
)

func testEnv(ports.TurnScope) facts.Env {
	return facts.Env{OS: "linux", Workspace: "/home/me/proj", Build: "/home/me/proj/build/opus", Home: "/home/me",
		Temp: []string{"/tmp"}, RitamHome: "/home/me/.ritam"}
}

// fakeAsker answers questions as told, recording them.
type fakeAsker struct {
	mu     sync.Mutex
	asked  []Question
	answer Answer
	err    error
	wait   bool
}

func (f *fakeAsker) Ask(ctx context.Context, q Question) (Answer, error) {
	f.mu.Lock()
	f.asked = append(f.asked, q)
	f.mu.Unlock()
	if f.wait {
		<-ctx.Done()
		return Answer{}, ctx.Err()
	}
	return f.answer, f.err
}

type policyRig struct {
	srv   *Server
	turn  ports.HookTurn
	calls []Call
	stops []Stop
	mu    sync.Mutex
}

func newPolicyRig(t *testing.T, set Settings, asker Asker) *policyRig {
	t.Helper()
	rs, err := rules.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Start("/bin/ritam", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	r := &policyRig{srv: srv}
	srv.Policy = &Policy{Rules: rs, Settings: func() Settings { return set }, Env: testEnv, Asker: asker,
		OnStop: func(s Stop) { r.mu.Lock(); r.stops = append(r.stops, s); r.mu.Unlock() }}
	srv.OnCall = func(c Call) { r.mu.Lock(); r.calls = append(r.calls, c); r.mu.Unlock() }
	r.turn = srv.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode,
		BuildDir: "build/opus"})
	return r
}

// run sends one call through the real hook and returns its exit code and
// what it told the agent.
func (r *policyRig) run(t *testing.T, tool string, input any) (int, string) {
	t.Helper()
	in, _ := json.Marshal(input)
	payload, _ := json.Marshal(map[string]any{"session_id": "s", "cwd": "/home/me/proj", "hook_event_name": "PreToolUse",
		"tool_name": tool, "tool_input": json.RawMessage(in), "tool_use_id": "toolu_x"})
	env := r.turn.Env()
	var stderr bytes.Buffer
	code := RunHook([]string{"pretool"}, bytes.NewReader(payload), io.Discard, &stderr, func(k string) string { return env[k] })
	return code, stderr.String()
}

func (r *policyRig) last() Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

func bash(cmd string) map[string]string { return map[string]string{"command": cmd} }

func TestAHarmfulCallIsRefusedWithItsReason(t *testing.T) {
	r := newPolicyRig(t, Settings{}, &fakeAsker{})
	code, msg := r.run(t, "Bash", bash("git push --force origin main"))
	if code != blockExit || !strings.Contains(msg, "git.push-force") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	c := r.last()
	if c.Decision != Deny || c.Severity != "harmful" || len(c.Findings) == 0 || c.Stopped {
		t.Errorf("record: %+v", c)
	}
	if code, _ := r.run(t, "Bash", bash("go test ./...")); code != 0 {
		t.Error("ordinary work passes")
	}
}

func TestACriticalCallStopsTheTurn(t *testing.T) {
	r := newPolicyRig(t, Settings{}, &fakeAsker{})
	code, msg := r.run(t, "Bash", bash("rm -rf ~/projects"))
	if code != blockExit || !strings.Contains(msg, "stopped this turn") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.stops)
		r.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stops) != 1 || r.stops[0].Task != "TASK-001" || r.stops[0].Agent != "opus" {
		t.Errorf("stops: %+v", r.stops)
	}
}

func TestASuspiciousCallIsPutToTheHuman(t *testing.T) {
	asker := &fakeAsker{answer: Answer{Allow: true}}
	r := newPolicyRig(t, Settings{AskTimeout: time.Minute}, asker)
	if code, msg := r.run(t, "Bash", bash("rm -rf src")); code != 0 {
		t.Fatalf("approved, yet exit %d: %s", code, msg)
	}
	if len(asker.asked) != 1 || asker.asked[0].Summary != "rm -rf src" || len(asker.asked[0].Reasons) == 0 ||
		asker.asked[0].Severity != "suspicious" {
		t.Errorf("asked: %+v", asker.asked)
	}
	if c := r.last(); !c.Asked || c.Answer != "approved" || c.Decision != Pass {
		t.Errorf("record: %+v", c)
	}

	asker.answer = Answer{Allow: false, Note: "keep src"}
	if code, msg := r.run(t, "Bash", bash("rm -rf src")); code != blockExit || !strings.Contains(msg, "keep src") {
		t.Errorf("refused: exit %d: %s", code, msg)
	}
}

func TestAQuestionNobodyAnswersIsRefused(t *testing.T) {
	r := newPolicyRig(t, Settings{AskTimeout: 100 * time.Millisecond}, &fakeAsker{wait: true})
	code, msg := r.run(t, "Bash", bash("rm -rf src"))
	if code != blockExit || !strings.Contains(msg, "nobody answered") {
		t.Errorf("exit %d: %s", code, msg)
	}
	r = newPolicyRig(t, Settings{}, nil)
	if code, msg := r.run(t, "Bash", bash("rm -rf src")); code != blockExit || !strings.Contains(msg, "nobody is there") {
		t.Errorf("no asker: exit %d: %s", code, msg)
	}
	r = newPolicyRig(t, Settings{}, &fakeAsker{err: ErrNobody})
	if code, msg := r.run(t, "Bash", bash("rm -rf src")); code != blockExit || !strings.Contains(msg, "nobody is there") {
		t.Errorf("nobody attending: exit %d: %s", code, msg)
	}
}

func TestManualModeAsksBeforeEveryCommand(t *testing.T) {
	asker := &fakeAsker{answer: Answer{Allow: true}}
	r := newPolicyRig(t, Settings{Manual: true}, asker)
	if code, _ := r.run(t, "Bash", bash("go test ./...")); code != 0 || len(asker.asked) != 1 || !asker.asked[0].Manual {
		t.Errorf("a command in manual mode: exit %d, asked %+v", code, asker.asked)
	}
	if code, _ := r.run(t, "Read", map[string]string{"file_path": "/home/me/proj/main.go"}); code != 0 || len(asker.asked) != 1 {
		t.Errorf("reading a file is no command: asked %d", len(asker.asked))
	}
	auto := &fakeAsker{}
	r = newPolicyRig(t, Settings{}, auto)
	if code, _ := r.run(t, "Bash", bash("go test ./...")); code != 0 || len(auto.asked) != 0 {
		t.Errorf("auto mode passes ordinary work without asking: %d", len(auto.asked))
	}
}

func TestAScriptWrittenThenRunIsCheckedAsCode(t *testing.T) {
	asker := &fakeAsker{answer: Answer{Allow: false}}
	r := newPolicyRig(t, Settings{}, asker)
	if code, msg := r.run(t, "Write", map[string]string{"file_path": "/home/me/proj/clean.py",
		"content": "import shutil\nshutil.rmtree('/home/me')\n"}); code != 0 {
		t.Fatalf("writing it: exit %d: %s", code, msg)
	}
	code, _ := r.run(t, "Bash", bash("python3 clean.py"))
	c := r.last()
	if code != blockExit || !c.Asked {
		t.Errorf("running it: exit %d, %+v", code, c)
	}
	var ids []string
	for _, f := range c.Findings {
		ids = append(ids, f.Rule)
	}
	if !strings.Contains(strings.Join(ids, " "), "seq.write-then-run") {
		t.Errorf("findings: %v", ids)
	}
}

func TestTheHookWaitsLongEnoughForTheHuman(t *testing.T) {
	r := newPolicyRig(t, Settings{AskTimeout: 3 * time.Minute}, &fakeAsker{})
	cmd := r.srv.HookCommand(domain.AgentKindClaudeCode)
	wait := waitFrom(r.turn.Env()[WaitEnv()])
	if wait <= 3*time.Minute || time.Duration(cmd.TimeoutSeconds)*time.Second <= wait {
		t.Errorf("the human has 3m; the hook waits %s and the CLI %ds", wait, cmd.TimeoutSeconds)
	}
	if waitFrom("") != hookTimeout || waitFrom("999999") != maxHookWait || waitFrom("x") != hookTimeout {
		t.Error("an odd wait falls back or is bounded")
	}
}
