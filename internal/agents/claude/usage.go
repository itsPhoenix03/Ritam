package claude

import (
	"encoding/json"
	"strings"

	"ritam/internal/ports"
)

// parseUsage reads what a turn cost from the final result envelope, which
// Claude Code ends every stream-json run with: total_cost_usd plus token
// counts. It returns nil when there is no such envelope (a turn killed before
// it finished).
//
// Input tokens include those written to the prompt cache but not those read
// from it: a resumed session re-reads its whole history from the cache every
// turn, and counting that would make a token budget measure session length
// rather than work. Cache reads are reported separately; the cost figure
// already prices them.
//
// The token counts are this run's own, but total_cost_usd is the session's
// running total: the caller converts it (core.Session.CostSince).
func parseUsage(stdout string) *ports.Usage {
	lines := strings.Split(stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, "{") || !strings.Contains(l, `"result"`) {
			continue
		}
		var env struct {
			Type    string  `json:"type"`
			CostUSD float64 `json:"total_cost_usd"`
			Usage   struct {
				Input       int64 `json:"input_tokens"`
				CacheCreate int64 `json:"cache_creation_input_tokens"`
				CacheRead   int64 `json:"cache_read_input_tokens"`
				Output      int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(l), &env) != nil || env.Type != "result" {
			continue
		}
		return &ports.Usage{
			InputTokens:     env.Usage.Input + env.Usage.CacheCreate,
			OutputTokens:    env.Usage.Output,
			CacheReadTokens: env.Usage.CacheRead,
			CostUSD:         env.CostUSD,
		}
	}
	return nil
}

// streamUsage is the usage block of a stream event.
type streamUsage struct {
	Input       int64 `json:"input_tokens"`
	CacheCreate int64 `json:"cache_creation_input_tokens"`
	CacheRead   int64 `json:"cache_read_input_tokens"`
	Output      int64 `json:"output_tokens"`
}

// partialUsage is what a turn killed before it finished (a timeout, a
// cancel) had used, from the stream, when there is no result envelope: the
// tokens of every API call the stream reported. message_start opens a call
// with its input, and message_delta brings the call's usage so far, output
// included. A call cut off midway counts what it had reported. The stream
// carries no cost, so the turn's cost stays unknown; its tokens still count
// against the token budgets. nil when the stream reported no call.
func partialUsage(stdout string) *ports.Usage {
	var calls []streamUsage
	for _, l := range strings.Split(stdout, "\n") {
		l = strings.TrimSpace(l)
		if !strings.Contains(l, `"message_start"`) && !strings.Contains(l, `"message_delta"`) {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Event struct {
				Type    string `json:"type"`
				Message struct {
					Usage streamUsage `json:"usage"`
				} `json:"message"`
				Usage *streamUsage `json:"usage"`
			} `json:"event"`
		}
		if json.Unmarshal([]byte(l), &ev) != nil || ev.Type != "stream_event" {
			continue
		}
		switch {
		case ev.Event.Type == "message_start":
			calls = append(calls, ev.Event.Message.Usage)
		case ev.Event.Type == "message_delta" && ev.Event.Usage != nil && len(calls) > 0:
			u, last := *ev.Event.Usage, &calls[len(calls)-1]
			last.Output = u.Output
			// A delta may carry the output alone; the input is then the start's.
			if u.Input+u.CacheCreate+u.CacheRead > 0 {
				last.Input, last.CacheCreate, last.CacheRead = u.Input, u.CacheCreate, u.CacheRead
			}
		}
	}
	if len(calls) == 0 {
		return nil
	}
	out := &ports.Usage{}
	for _, c := range calls {
		out.InputTokens += c.Input + c.CacheCreate
		out.OutputTokens += c.Output
		out.CacheReadTokens += c.CacheRead
	}
	return out
}
