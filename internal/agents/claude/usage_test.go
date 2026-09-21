package claude

import (
	"math"
	"testing"

	"projectorch/internal/agents/core"
)

// realResult is a result envelope captured from Claude Code 2.1.284 (a resumed
// haiku turn), trimmed of fields the adapter does not read. Its top-level
// usage is this turn's; modelUsage is the session's running total -- and so
// is total_cost_usd, which equals modelUsage's costUSD (98 output tokens over
// the session, against this turn's 7).
const realResult = `{"type":"result","subtype":"success","is_error":false,"result":"PELICAN",` +
	`"session_id":"ae44fe92-a261-4a5a-837f-9a08f426eeff","total_cost_usd":0.00356672,` +
	`"usage":{"input_tokens":2,"cache_creation_input_tokens":52,"cache_read_input_tokens":30461,"output_tokens":7},` +
	`"modelUsage":{"claude-haiku-5-5":{"inputTokens":6,"outputTokens":98,"cacheReadInputTokens":77172,"costUSD":0.00356672}}}`

func TestUsageIsReadFromTheResultEnvelope(t *testing.T) {
	u := parseUsage("{\"type\":\"system\"}\n" + realResult + "\n")
	if u == nil {
		t.Fatal("no usage found")
	}
	if u.CostUSD != 0.00356672 {
		t.Errorf("cost = %v", u.CostUSD)
	}
	// 2 fresh + 52 written to the cache; the 30,461 cache reads are the
	// resumed session's history and are deliberately not counted.
	if u.InputTokens != 54 || u.OutputTokens != 7 || u.CacheReadTokens != 30461 {
		t.Errorf("tokens = %d in / %d out / %d cache reads, want 54 / 7 / 30461",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens)
	}
}

// TestAResumedSessionIsChargedOnlyForItsOwnTurn: the running totals Claude
// reported over the twelve turns of opus's session in a real run (TASK-002).
// Summing them charged $10.39 for a session that cost $1.30, and paused the
// task on a budget it had not spent.
func TestAResumedSessionIsChargedOnlyForItsOwnTurn(t *testing.T) {
	reported := []float64{0.1190972, 0.3142324, 0.522754, 0.6767346, 0.7421926, 0.7914694,
		1.0180404, 1.1669086, 1.2192672, 1.2489126, 1.270744, 1.303606}
	var sess core.Session
	total := 0.0
	for _, r := range reported {
		total += sess.CostSince(r)
	}
	if math.Abs(total-1.303606) > 1e-9 {
		t.Errorf("twelve turns charged $%.4f, want the session's $1.3036", total)
	}
}

func TestATurnKilledBeforeItsResultReportsNoUsage(t *testing.T) {
	if u := parseUsage(`{"type":"stream_event"}` + "\n"); u != nil {
		t.Errorf("no result envelope means no usage, got %+v", u)
	}
}
