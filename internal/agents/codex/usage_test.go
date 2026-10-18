package codex

import (
	"testing"

	"ritam/internal/agents/core"
)

// realTurn is a turn.completed event captured from codex-cli 0.156.1.
const realTurn = `{"type":"turn.completed","usage":{"input_tokens":35012,"cached_input_tokens":14464,` +
	`"cache_write_input_tokens":0,"output_tokens":13,"reasoning_output_tokens":0}}`

func turnEvent(in, cached, out string) string {
	return `{"type":"turn.completed","usage":{"input_tokens":` + in + `,"cached_input_tokens":` + cached +
		`,"output_tokens":` + out + `}}`
}

func TestUsageIsReadFromTurnCompleted(t *testing.T) {
	tot := parseUsage(`{"type":"thread.started","thread_id":"x"}` + "\n" + realTurn + "\n")
	if tot == nil {
		t.Fatal("no usage found")
	}
	var sess core.Session
	u := turnUsage(&sess, *tot)
	if u.InputTokens != 35012-14464 || u.OutputTokens != 13 || u.CacheReadTokens != 14464 {
		t.Errorf("tokens = %d in / %d out / %d cached, want %d / 13 / 14464 (cached input kept apart)",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, 35012-14464)
	}
	if u.CostUSD != 0 {
		t.Error("Codex reports no cost; none must be invented")
	}
}

// TestAResumedThreadIsChargedOnlyForItsOwnTurn: Codex reports running totals
// for the thread. These are the four turns of a real run (TASK-002's codex
// thread); summing the totals charged it 49,802 input tokens for 16,645.
func TestAResumedThreadIsChargedOnlyForItsOwnTurn(t *testing.T) {
	turns := []struct{ in, cached, out string }{
		{"17158", "12032", "513"},
		{"36434", "24064", "1566"},
		{"58797", "43136", "2062"},
		{"81925", "65280", "2300"},
	}
	var sess core.Session
	var in, out int64
	for _, tr := range turns {
		u := turnUsage(&sess, *parseUsage(turnEvent(tr.in, tr.cached, tr.out)))
		in += u.InputTokens
		out += u.OutputTokens
	}
	if in != 81925-65280 || out != 2300 {
		t.Errorf("four turns charged %d in / %d out, want the thread's %d / 2300", in, out, 81925-65280)
	}
}

func TestOnlyTheLastTotalInARunCounts(t *testing.T) {
	tot := parseUsage(turnEvent("100", "0", "10") + "\n" + turnEvent("250", "0", "30") + "\n")
	if tot == nil || tot.Input != 250 || tot.Output != 30 {
		t.Errorf("totals are running, so the last one is the thread's: %+v", tot)
	}
}

func TestATurnKilledBeforeItCompletedReportsNoUsage(t *testing.T) {
	if tot := parseUsage(`{"type":"item.completed"}` + "\n"); tot != nil {
		t.Errorf("no turn.completed means no usage, got %+v", tot)
	}
}
