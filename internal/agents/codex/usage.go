package codex

import (
	"encoding/json"
	"strings"

	"ritam/internal/agents/core"
	"ritam/internal/ports"
)

// threadTotals is what Codex reports in a `turn.completed` event: running
// token totals for the whole thread, not for the turn. A resumed thread's
// first turn already reports everything the thread has used.
type threadTotals struct {
	Input, Cached, Output int64
}

// parseUsage returns the thread totals from the last `turn.completed` event,
// or nil when there is none (a turn killed before it finished). Codex reports
// no cost, so only a token budget bounds Codex spending.
func parseUsage(stdout string) *threadTotals {
	var last *threadTotals
	for _, l := range strings.Split(stdout, "\n") {
		l = strings.TrimSpace(l)
		if !strings.Contains(l, `"turn.completed"`) {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Usage struct {
				Input  int64 `json:"input_tokens"`
				Cached int64 `json:"cached_input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(l), &ev) != nil || ev.Type != "turn.completed" {
			continue
		}
		last = &threadTotals{Input: ev.Usage.Input, Cached: ev.Usage.Cached, Output: ev.Usage.Output}
	}
	return last
}

// turnUsage converts thread totals into this turn's share, using and
// updating the totals sess last recorded.
//
// Cached input is kept out of InputTokens, as for Claude: a resumed thread
// re-reads its history from the cache every turn, and counting it would make
// a token budget measure thread length rather than work.
// reasoning_output_tokens is not added on top of output_tokens; as with the
// OpenAI API it reports, it is read as a breakdown of the output, not an
// addition to it.
func turnUsage(sess *core.Session, tot threadTotals) *ports.Usage {
	in, cached, out := sess.TokensSince(tot.Input, tot.Cached, tot.Output)
	fresh := in - cached
	if fresh < 0 {
		fresh = 0
	}
	return &ports.Usage{InputTokens: fresh, CacheReadTokens: cached, OutputTokens: out}
}
