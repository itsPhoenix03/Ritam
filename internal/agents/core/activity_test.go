package core

import (
	"strings"
	"testing"
	"time"

	"projectorch/internal/ports"
)

func drainActivity(ch <-chan ports.AgentActivity) []ports.AgentActivity {
	var out []ports.AgentActivity
	for {
		select {
		case a := <-ch:
			out = append(out, a)
		default:
			return out
		}
	}
}

func TestAppendCoalescesRapidText(t *testing.T) {
	e := NewActivityEmitter("opus", 64)
	e.SetTask("TASK-001")

	// Token deltas arrive far faster than any terminal should redraw; they
	// must reach the reader as one batched chunk, not one event per token.
	for _, tok := range []string{"The ", "cookie ", "path ", "differs."} {
		e.Append(ports.ActivityText, tok)
	}
	if got := drainActivity(e.Channel()); len(got) != 0 {
		t.Fatalf("expected coalescing before the flush interval, got %d events", len(got))
	}

	e.Flush()
	got := drainActivity(e.Channel())
	if len(got) != 1 {
		t.Fatalf("expected one coalesced event, got %d", len(got))
	}
	if got[0].Text != "The cookie path differs." {
		t.Errorf("text = %q, want the fragments joined in order", got[0].Text)
	}
	if got[0].TaskID != "TASK-001" || got[0].AgentID != "opus" {
		t.Errorf("activity not tagged: %+v", got[0])
	}
}

func TestAppendFlushesOnItsOwn(t *testing.T) {
	e := NewActivityEmitter("opus", 64)
	e.Append(ports.ActivityText, "hello")

	deadline := time.After(2 * time.Second)
	for {
		if got := drainActivity(e.Channel()); len(got) > 0 {
			if got[0].Text != "hello" {
				t.Fatalf("text = %q", got[0].Text)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("pending text was never flushed; the UI would stall until the next event")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestKindsAreKeptSeparate(t *testing.T) {
	e := NewActivityEmitter("opus", 64)
	e.Append(ports.ActivityThinking, "considering")
	e.Append(ports.ActivityText, "answering")
	e.Flush()

	byKind := map[ports.ActivityKind]string{}
	for _, a := range drainActivity(e.Channel()) {
		byKind[a.Kind] = a.Text
	}
	if byKind[ports.ActivityThinking] != "considering" || byKind[ports.ActivityText] != "answering" {
		t.Fatalf("kinds were mixed: %+v", byKind)
	}
}

func TestEmitFlushesPendingFirstToPreserveOrder(t *testing.T) {
	e := NewActivityEmitter("opus", 64)
	e.Append(ports.ActivityText, "before the tool call")
	e.Emit(ports.ActivityTool, "Read")

	got := drainActivity(e.Channel())
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(got), got)
	}
	if got[0].Kind != ports.ActivityText || got[1].Kind != ports.ActivityTool {
		t.Fatalf("out of order: %s then %s", got[0].Kind, got[1].Kind)
	}
}

// TestSendNeverBlocks is the important one: a stalled UI must never be able to
// wedge an agent mid-answer. Losing presentation output is always preferable
// to losing the agent's actual work.
func TestSendNeverBlocks(t *testing.T) {
	e := NewActivityEmitter("opus", 2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			e.Emit(ports.ActivityTool, "tool")
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked on a full channel; a slow reader would stall the agent")
	}
}

func TestCloseIsIdempotentAndSilencesFurtherOutput(t *testing.T) {
	e := NewActivityEmitter("opus", 8)
	e.Close()
	e.Close() // must not panic

	e.Emit(ports.ActivityText, "after close")
	e.Append(ports.ActivityText, "also after close")
	e.Flush()
	if got := drainActivity(e.Channel()); len(got) != 0 {
		t.Fatalf("a closed emitter must stay silent, got %+v", got)
	}

	// Restarting an adapter must bring its stream back.
	e.Reopen()
	e.Emit(ports.ActivityStatus, "restarted")
	if got := drainActivity(e.Channel()); len(got) != 1 {
		t.Fatalf("expected output after Reopen, got %d events", len(got))
	}
}

func TestEmptyTextIsIgnored(t *testing.T) {
	e := NewActivityEmitter("opus", 8)
	e.Append(ports.ActivityText, "")
	e.Emit(ports.ActivityTool, "")
	e.Flush()
	if got := drainActivity(e.Channel()); len(got) != 0 {
		t.Fatalf("empty chunks should not be published, got %+v", got)
	}
}

// TestRunStreamingDeliversLinesBeforeExit is the property the whole streaming
// feature rests on: lines must reach the caller while the process is still
// running, not in one batch at exit.
func TestRunStreamingDeliversLinesBeforeExit(t *testing.T) {
	r := helperRunner(t, 30*time.Second)

	var (
		lines []string
		first time.Time
	)
	start := time.Now()
	res, err := r.RunStreaming(t.Context(), []string{"stream"}, "", func(line string) {
		if len(lines) == 0 {
			first = time.Now()
		}
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatalf("RunStreaming: %v (stderr: %s)", err, res.Stderr)
	}

	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %v", len(lines), lines)
	}
	for i, want := range []string{"line-0", "line-1", "line-2"} {
		if strings.TrimSpace(lines[i]) != want {
			t.Errorf("line %d = %q, want %q", i, lines[i], want)
		}
	}
	// The helper sleeps between lines, so an implementation that buffered
	// until exit would deliver the first line only at the very end.
	if first.Sub(start) >= res.Duration {
		t.Error("the first line arrived only at process exit: output is buffered, not streamed")
	}
	if !strings.Contains(res.Stdout, "line-2") {
		t.Error("RunStreaming must still accumulate the full stdout")
	}
}

func TestRunStreamingToleratesVeryLongLines(t *testing.T) {
	r := helperRunner(t, 30*time.Second)

	var got []string
	if _, err := r.RunStreaming(t.Context(), []string{"longline"}, "", func(l string) {
		got = append(got, l)
	}); err != nil {
		t.Fatalf("RunStreaming: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one long line, got %d", len(got))
	}
	// Streamed JSON events routinely exceed bufio's 64 KB default, and a split
	// line would be unparseable rather than merely truncated.
	if len(got[0]) < 200_000 {
		t.Errorf("long line was truncated at %d bytes", len(got[0]))
	}
}
