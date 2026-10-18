package codex

import (
	"strings"
	"testing"

	"ritam/internal/agents/core"
	"ritam/internal/config"
	"ritam/internal/ports"
)

// The fixtures below are real lines captured from `codex exec --json ...`.
// Codex streams at item granularity rather than token granularity.

func collect(t *testing.T, lines []string) []ports.AgentActivity {
	t.Helper()
	e := core.NewActivityEmitter("codex", 256)
	for _, l := range lines {
		consumeStreamLine(l, e)
	}
	e.Flush()

	var acts []ports.AgentActivity
	for {
		select {
		case a := <-e.Channel():
			acts = append(acts, a)
		default:
			return acts
		}
	}
}

func TestConsumeStreamReportsTurnLifecycleAndMessage(t *testing.T) {
	acts := collect(t, []string{
		`{"type":"thread.started","thread_id":"01a0"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Hello, wonderful human!"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":14114,"output_tokens":9}}`,
	})

	var text, statuses string
	for _, a := range acts {
		switch a.Kind {
		case ports.ActivityText:
			text += a.Text
		case ports.ActivityStatus:
			statuses += a.Text + ";"
		}
	}
	if text != "Hello, wonderful human!" {
		t.Errorf("agent_message text = %q", text)
	}
	if !strings.Contains(statuses, "turn started") || !strings.Contains(statuses, "turn completed") {
		t.Errorf("turn lifecycle missing from statuses: %q", statuses)
	}
}

func TestConsumeStreamReportsReasoningAndCommands(t *testing.T) {
	acts := collect(t, []string{
		`{"type":"item.completed","item":{"type":"reasoning","text":"checking buildArgs"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"rg --files internal"}}`,
		`{"type":"item.completed","item":{"type":"file_change"}}`,
	})

	kinds := map[ports.ActivityKind][]string{}
	for _, a := range acts {
		kinds[a.Kind] = append(kinds[a.Kind], a.Text)
	}
	if got := strings.Join(kinds[ports.ActivityThinking], ""); got != "checking buildArgs" {
		t.Errorf("reasoning = %q", got)
	}
	tools := strings.Join(kinds[ports.ActivityTool], ";")
	if !strings.Contains(tools, "rg --files internal") {
		t.Errorf("command missing from tools: %q", tools)
	}
	if !strings.Contains(tools, "file_change") {
		t.Errorf("file_change missing from tools: %q", tools)
	}
}

// TestConsumeStreamIgnoresNoise covers forward compatibility: a future CLI
// emitting new event types must not break the adapter.
func TestConsumeStreamIgnoresNoise(t *testing.T) {
	acts := collect(t, []string{
		"",
		"Reading prompt from stdin...",
		`{"type":"some.future.event","fields":{"a":1}}`,
		`{"type":"item.completed","item":{"type":"unknown_item_kind","text":"ignored"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"kept"}}`,
	})

	var text string
	for _, a := range acts {
		if a.Kind == ports.ActivityText {
			text += a.Text
		}
	}
	if text != "kept" {
		t.Errorf("real events must survive the noise, got %q", text)
	}
}

func TestBuildArgsRequestsJSONStreaming(t *testing.T) {
	a := New("codex", config.AgentConfig{}, nil)
	args := strings.Join(a.buildArgs("out.txt"), " ")
	if !strings.Contains(args, "--json") {
		t.Errorf("args missing --json — the UI would show a frozen pane\ngot: %s", args)
	}
	// The authoritative reply must still come from the file, not the stream.
	if !strings.Contains(args, "--output-last-message out.txt") {
		t.Errorf("args missing --output-last-message\ngot: %s", args)
	}
}
