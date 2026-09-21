package claude

import (
	"encoding/json"
	"strings"

	"projectorch/internal/ports"
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
