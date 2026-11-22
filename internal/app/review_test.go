package app

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

	turn := sys.Review.BeginTurn("TASK-001", "opus", domain.AgentKindClaudeCode)
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
	if e.Summary != "opus: refused, the call is larger than 1024 KiB, too large to check" ||
		e.Details["decision"] != "deny" || e.Details["note"] == "" {
		t.Errorf("event: %+v", e)
	}
	if _, ok := e.Details["tool"]; ok {
		t.Errorf("empty details are left out: %+v", e.Details)
	}
	sub := toolCallEvent(review.Call{Agent: "opus", Tool: "Read", Subagent: "a-1", Summary: "main.go", Decision: review.Pass})
	if sub.Summary != "opus Read (subagent): main.go" {
		t.Errorf("a subagent's call says so: %q", sub.Summary)
	}
}
