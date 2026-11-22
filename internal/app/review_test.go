package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
	"ritam/internal/review"
)

func realAgentsSystem(t *testing.T, mock bool) *System {
	t.Helper()
	cfg := config.Default()
	cfg.TUI.UseMockAgents = mock
	cfg.Storage.Backend = "memory"
	cfg.Workspace.Root = t.TempDir()
	sys, err := Build(cfg, logging.NewDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sys.Close() })
	return sys
}

// TestEveryToolCallIsRecordedInTheTasksEvents: with real agents ritam runs
// its review server; a call the hook reports becomes a ToolCall event of the
// task, and a turn whose calls did not all reach the hook a HookMissed one.
func TestEveryToolCallIsRecordedInTheTasksEvents(t *testing.T) {
	sys := realAgentsSystem(t, false)
	if sys.Review == nil {
		t.Fatal("real agents need the review server")
	}

	turn := sys.Review.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	env := turn.Env()
	payload := `{"session_id":"s-1","cwd":"D:\\app","hook_event_name":"PreToolUse","tool_name":"Bash",` +
		`"tool_input":{"command":"go build ./..."},"tool_use_id":"toolu_1"}`
	var stderr bytes.Buffer
	if code := review.RunHook([]string{"pretool"}, strings.NewReader(payload), io.Discard, &stderr,
		func(k string) string { return env[k] }); code != 0 {
		t.Fatalf("the hook must pass a readable call: exit %d, %s", code, stderr.String())
	}
	turn.End(ports.ToolCalls{Calls: []ports.ToolCall{{ID: "toolu_1", Tool: "Bash"}, {ID: "toolu_9", Tool: "Write"}}})

	events, err := sys.Service.TaskEvents(context.Background(), "TASK-001")
	if err != nil {
		t.Fatal(err)
	}
	var call, missed *domain.Event
	for i, e := range events {
		switch e.Type {
		case domain.EvtToolCall:
			call = &events[i]
		case domain.EvtHookMissed:
			missed = &events[i]
		}
	}
	if call == nil || call.AgentID != "opus" || call.Summary != "opus Bash: go build ./..." ||
		call.Details["tool"] != "Bash" || call.Details["toolUseId"] != "toolu_1" ||
		call.Details["decision"] != "pass" || call.Details["cwd"] != `D:\app` {
		t.Errorf("tool call event: %+v", call)
	}
	if missed == nil || !strings.Contains(missed.Summary, "1 of the 2") || missed.Details["missed"] != "Write" {
		t.Errorf("hook missed event: %+v", missed)
	}
}

// TestCodexHooksInTheWorkspaceAreGuarded: Codex runs .codex/hooks.json
// outside its sandbox, so an agent may not write it: Claude's file tools are
// denied it and the guard takes away one an agent's commands write.
func TestCodexHooksInTheWorkspaceAreGuarded(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, ".codex", "hooks.json")
	if !slices.Contains(sensitivePaths(root), want) {
		t.Errorf("%s must be denied to the file tools: %v", want, sensitivePaths(root))
	}
}

// TestMockAgentsNeedNoReviewServer: scripted agents run no tools.
func TestMockAgentsNeedNoReviewServer(t *testing.T) {
	if sys := realAgentsSystem(t, true); sys.Review != nil {
		t.Error("no review server with mock agents")
	}
}

func TestARefusedToolCallSaysWhy(t *testing.T) {
	e := toolCallEvent(review.Call{Task: "TASK-001", Agent: "opus", Decision: review.Deny,
		Note: "the call is larger than 1024 KiB, too large to check"})
	if e.Type != domain.EvtToolBlocked || e.Summary != "opus refused (the call is larger than 1024 KiB, too large to check)" ||
		e.Details["decision"] != "deny" || e.Details["note"] == "" {
		t.Errorf("event: %+v", e)
	}
	stopped := toolCallEvent(review.Call{Agent: "opus", Tool: "Bash", Summary: "rm -rf ~", Decision: review.Deny,
		Note: "refused: ...", Severity: "critical", Stopped: true, Findings: []review.Finding{{Rule: "destroy.recursive-delete.outside"}}})
	if stopped.Details["stopped"] != "true" || stopped.Details["rules"] != "destroy.recursive-delete.outside" ||
		!strings.Contains(stopped.Summary, "rm -rf ~") {
		t.Errorf("a stopping call: %+v", stopped)
	}
	if _, ok := e.Details["tool"]; ok {
		t.Errorf("empty details are left out: %+v", e.Details)
	}
	sub := toolCallEvent(review.Call{Agent: "opus", Tool: "Read", Subagent: "a-1", Summary: "main.go", Decision: review.Pass})
	if sub.Summary != "opus Read (subagent): main.go" {
		t.Errorf("a subagent's call says so: %q", sub.Summary)
	}
}

// hookRun sends one tool call through the real hook for a turn.
func hookRun(t *testing.T, turn ports.HookTurn, tool string, input string) (int, string) {
	t.Helper()
	env := turn.Env()
	payload := `{"session_id":"s","cwd":"","hook_event_name":"PreToolUse","tool_name":"` + tool +
		`","tool_input":` + input + `,"tool_use_id":"toolu_1"}`
	var stderr bytes.Buffer
	code := review.RunHook([]string{"pretool"}, strings.NewReader(payload), io.Discard, &stderr,
		func(k string) string { return env[k] })
	return code, stderr.String()
}

// TestRealAgentsCallsAreDecidedByTheRules: with real agents the review has
// ritam's rules: a harmful call is refused, a suspicious one waits for the
// human -- refused at once with nobody attending, passed once approved.
func TestRealAgentsCallsAreDecidedByTheRules(t *testing.T) {
	sys := realAgentsSystem(t, false)
	if sys.Review.Policy == nil || sys.Review.Policy.Rules == nil {
		t.Fatal("real agents need the review's policy")
	}
	turn := sys.Review.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	defer turn.End(ports.ToolCalls{})

	if code, msg := hookRun(t, turn, "Bash", `{"command":"git push --force origin main"}`); code != 2 || !strings.Contains(msg, "git.push-force") {
		t.Errorf("harmful: exit %d: %s", code, msg)
	}
	if code, msg := hookRun(t, turn, "Bash", `{"command":"rm -rf src"}`); code != 2 || !strings.Contains(msg, "nobody is there") {
		t.Errorf("suspicious, nobody attending: exit %d: %s", code, msg)
	}

	// A new turn: in this one, the refused call tried again would be a retry.
	turn = sys.Review.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	defer turn.End(ports.ToolCalls{})
	sys.Attend()
	done := make(chan int, 1)
	go func() {
		code, _ := hookRun(t, turn, "Bash", `{"command":"rm -rf src"}`)
		done <- code
	}()
	var waiting []ports.Approval
	deadline := time.Now().Add(5 * time.Second)
	for len(waiting) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		waiting = sys.Service.PendingApprovals(context.Background())
	}
	if len(waiting) != 1 || waiting[0].Summary != "rm -rf src" || waiting[0].Task != "TASK-001" {
		t.Fatalf("waiting: %+v", waiting)
	}
	if err := sys.Service.AnswerApproval(context.Background(), waiting[0].ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Errorf("approved, yet exit %d", code)
	}
	if err := sys.Service.AnswerApproval(context.Background(), waiting[0].ID, true, ""); err == nil {
		t.Error("an answered call cannot be answered again")
	}
}

func TestApprovalsWaitForTheHuman(t *testing.T) {
	var events []domain.Event
	a := newApprovals(func(e domain.Event) { events = append(events, e) })
	q := review.Question{ID: "q1", Task: "TASK-001", Agent: "opus", Tool: "Bash", Summary: "rm -rf src",
		Reasons: []string{"recursive delete"}, Deadline: time.Now().Add(time.Minute)}
	if _, err := a.Ask(context.Background(), q); err != review.ErrNobody {
		t.Errorf("nobody attending: %v", err)
	}
	a.attend(true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := a.Ask(ctx, q); err == nil {
		t.Error("an unanswered question ends with its context")
	}
	if len(a.pending()) != 0 {
		t.Error("a question that ended is no longer pending")
	}
	if len(events) != 1 || events[0].Type != domain.EvtToolApproval || events[0].Details["approval"] != "q1" {
		t.Errorf("events: %+v", events)
	}
}

func TestTheReviewModeIsSavedAndRecorded(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.ProjectConfig = filepath.Join(t.TempDir(), "ritam.yaml")
	var events []domain.Event
	c := newReviewControl(cfg, true)
	c.publish = func(e domain.Event) { events = append(events, e) }
	c.openTasks = func(context.Context) []domain.TaskID { return []domain.TaskID{"TASK-001", "TASK-002"} }
	if _, err := c.setMode(context.Background(), "yolo"); err == nil {
		t.Error("an unknown mode is refused")
	}
	info, err := c.setMode(context.Background(), "manual")
	if err != nil || info.Mode != "manual" || !c.settings().Manual {
		t.Fatalf("manual: %+v %v", info, err)
	}
	if len(events) != 2 || events[0].Type != domain.EvtReviewMode {
		t.Errorf("events: %+v", events)
	}
	saved, err := config.Load(cfg.Runtime.ProjectConfig)
	if err != nil || saved.Review.Mode != "manual" {
		t.Errorf("saved: %+v %v", saved, err)
	}
}

func TestTheReviewSeesThisMachine(t *testing.T) {
	cfg := config.Default()
	root := t.TempDir()
	e := reviewEnv(cfg, config.Workspace{Root: root})
	if e.Workspace != root || e.Home == "" || len(e.Temp) == 0 || e.Resolve == nil {
		t.Errorf("env: %+v", e)
	}
	if !slices.Contains(e.Protected, ".codex/hooks.json") || !slices.Contains(e.Protected, ".github/workflows/**") {
		t.Errorf("protected: %v", e.Protected)
	}
}

// TestYourOwnRulesApply: a deny pattern in the settings refuses the
// command, however it is written.
func TestYourOwnRulesApply(t *testing.T) {
	cfg := config.Default()
	cfg.TUI.UseMockAgents = false
	cfg.Storage.Backend = "memory"
	cfg.Workspace.Root = t.TempDir()
	cfg.Review.Rules.Deny = []string{"go generate"}
	sys, err := Build(cfg, logging.NewDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sys.Close() })
	turn := sys.Review.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	defer turn.End(ports.ToolCalls{})
	if code, msg := hookRun(t, turn, "Bash", `{"command":"cd pkg && go -C . generate ./..."}`); code != 2 ||
		!strings.Contains(msg, "Your settings deny `go generate`") {
		t.Errorf("exit %d: %s", code, msg)
	}
	info, _ := sys.Service.Review(context.Background())
	if len(info.Deny) != 1 {
		t.Errorf("the settings tab shows your rules: %+v", info)
	}
}

// TestAppHelperHook is not a test: run with "-- deny", the test binary is a
// hook that refuses every call.
func TestAppHelperHook(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || i+1 >= len(os.Args) {
		return
	}
	_, _ = io.ReadAll(os.Stdin)
	if os.Args[i+1] == "deny" {
		fmt.Fprintln(os.Stderr, "this project allows no calls")
		os.Exit(2)
	}
	os.Exit(0)
}

// TestAProjectHookRunsOnceApprovedAndOnlyAsApproved: a project's hook is
// held until approved; approved, it runs; edited, the call is refused until
// it is approved again; and an agent may not write its script.
func TestAProjectHookRunsOnceApprovedAndOnlyAsApproved(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	script := filepath.Join(root, "hooks", "guard.txt")
	_ = os.MkdirAll(filepath.Dir(script), 0o700)
	if err := os.WriteFile(script, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := config.ReviewHook{Name: "guard", Run: []string{os.Args[0], "-test.run=^TestAppHelperHook$", "--", "deny",
		"hooks/guard.txt"}}
	cfg := config.Default()
	cfg.TUI.UseMockAgents = false
	cfg.Storage.Backend = "memory"
	cfg.Workspace.Root = root
	cfg.Runtime.HomeDir = home
	cfg.Runtime.HookRoot = root
	cfg.Runtime.HeldHooks = []config.ReviewHook{hook}
	sys, err := Build(cfg, logging.NewDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sys.Close() })
	turn := sys.Review.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	defer turn.End(ports.ToolCalls{})

	if code, msg := hookRun(t, turn, "Bash", `{"command":"go build ./..."}`); code != 0 {
		t.Fatalf("a held hook does not run: exit %d: %s", code, msg)
	}
	info, _ := sys.Service.Review(context.Background())
	if len(info.HeldHooks) != 1 || !strings.Contains(info.HeldHooks[0], "guard") {
		t.Fatalf("held: %+v", info.HeldHooks)
	}
	if _, err := sys.Service.ApproveHook(context.Background(), "guard"); err != nil {
		t.Fatal(err)
	}
	if code, msg := hookRun(t, turn, "Bash", `{"command":"go build ./..."}`); code != 2 ||
		!strings.Contains(msg, "refused by your hook guard: this project allows no calls") {
		t.Errorf("approved, it runs: exit %d: %s", code, msg)
	}
	if _, err := os.Stat(filepath.Join(home, config.HookApprovalsFile)); err != nil {
		t.Errorf("the approval is kept on this machine: %v", err)
	}

	if err := os.WriteFile(script, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := sys.Review.BeginTurn(ports.TurnScope{Task: "TASK-002", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	defer fresh.End(ports.ToolCalls{})
	if code, msg := hookRun(t, fresh, "Bash", `{"command":"go build ./..."}`); code != 2 ||
		!strings.Contains(msg, "changed since you approved it") {
		t.Errorf("edited, it does not run and the call is refused: exit %d: %s", code, msg)
	}
	e := reviewEnv(cfg, config.Workspace{Root: root})
	if !slices.Contains(e.Protected, script) {
		t.Errorf("the script is protected from agents: %v", e.Protected)
	}
}
