package claude

import (
	"strings"
	"testing"

	"ritam/internal/agents/core"
	"ritam/internal/config"
	"ritam/internal/ports"
)

// The fixtures below are real lines captured from
// `claude -p ... --output-format stream-json --include-partial-messages --verbose`,
// trimmed to the fields this adapter reads.

func collect(t *testing.T, lines []string) (result string, err error, acts []ports.AgentActivity) {
	t.Helper()
	e := core.NewActivityEmitter("opus", 256)

	for _, l := range lines {
		r, done, lineErr := consumeStreamLine(l, e)
		if done {
			result, err = r, lineErr
		}
	}
	e.Flush()

	for {
		select {
		case a := <-e.Channel():
			acts = append(acts, a)
		default:
			return result, err, acts
		}
	}
}

func TestConsumeStreamTextDeltasAndResult(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"abc"}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello there"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":", friend!"}}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"@@ORCH type=ANSWER to=codex\nhello"}`,
	}

	result, err, acts := collect(t, lines)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "@@ORCH") {
		t.Errorf("result = %q, want the final envelope text", result)
	}

	var text string
	for _, a := range acts {
		if a.Kind == ports.ActivityText {
			text += a.Text
		}
	}
	if text != "Hello there, friend!" {
		t.Errorf("streamed text = %q, want the deltas joined in order", text)
	}
}

func TestConsumeStreamThinkingAndTools(t *testing.T) {
	lines := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weighing the evidence"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Read"}}}`,
	}
	_, _, acts := collect(t, lines)

	kinds := map[ports.ActivityKind]string{}
	for _, a := range acts {
		kinds[a.Kind] = a.Text
	}
	if kinds[ports.ActivityThinking] != "weighing the evidence" {
		t.Errorf("thinking = %q", kinds[ports.ActivityThinking])
	}
	if kinds[ports.ActivityTool] != "Read" {
		t.Errorf("tool = %q, want the tool name", kinds[ports.ActivityTool])
	}
}

func TestConsumeStreamReportsCLIError(t *testing.T) {
	_, err, _ := collect(t, []string{
		`{"type":"result","subtype":"error","is_error":true,"result":"rate limited"}`,
	})
	if err == nil {
		t.Fatal("a CLI error result must be reported, not treated as an agent reply")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should carry the CLI's message, got %v", err)
	}
}

// TestConsumeStreamTreatsAnEmptyResultAsSilence: see
// TestAnEmptySuccessfulResultIsSilence.
func TestConsumeStreamTreatsAnEmptyResultAsSilence(t *testing.T) {
	got, err, _ := collect(t, []string{
		`{"type":"result","subtype":"success","is_error":false,"result":"   "}`,
	})
	if err != nil || strings.TrimSpace(got) != "" {
		t.Fatalf("an empty successful result is silence, got %q, %v", got, err)
	}
}

// TestConsumeStreamIgnoresNoise covers forward compatibility: the stream
// carries bookkeeping this adapter does not read, and a CLI upgrade adding
// more of it must not turn into a failed task.
func TestConsumeStreamIgnoresNoise(t *testing.T) {
	lines := []string{
		"",
		"not json at all",
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup"}`,
		`{"type":"system","subtype":"status","status":"requesting"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}`,
		`{"type":"brand_new_event_type_from_a_future_cli","payload":{"whatever":1}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"kept"}}}`,
	}
	result, err, acts := collect(t, lines)
	if err != nil {
		t.Fatalf("noise must not produce an error: %v", err)
	}
	if result != "" {
		t.Errorf("no terminating result was present, got %q", result)
	}

	var text string
	for _, a := range acts {
		if a.Kind == ports.ActivityText {
			text += a.Text
		}
	}
	if text != "kept" {
		t.Errorf("real deltas must survive the noise, got %q", text)
	}
}

func TestBuildArgsRequestsStreaming(t *testing.T) {
	a := New("opus", config.AgentConfig{}, nil)
	args := strings.Join(a.buildArgs(), " ")

	for _, want := range []string{"--output-format stream-json", "--include-partial-messages", "--verbose"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q — the UI would show a frozen pane\ngot: %s", want, args)
		}
	}
}
