package claude

import (
	"context"
	"testing"
	"time"
)

// A real stream's events, trimmed to the fields that matter: one finished
// API call, then a second cut off after its start (the turn was killed).
const killedStream = `{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-haiku-5-5","usage":{"input_tokens":2,"cache_creation_input_tokens":2957,"cache_read_input_tokens":649,"output_tokens":3}}}}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"five"}}}
{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":2,"cache_creation_input_tokens":2957,"cache_read_input_tokens":649,"output_tokens":120}}}
{"type":"assistant","message":{"usage":{"input_tokens":2,"cache_creation_input_tokens":2957,"cache_read_input_tokens":649,"output_tokens":120}}}
{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":40,"cache_creation_input_tokens":0,"cache_read_input_tokens":3606,"output_tokens":1}}}}
`

// TestAKilledTurnStillCountsItsTokens: with no result envelope, the tokens
// of each API call the stream reported are counted, each once.
func TestAKilledTurnStillCountsItsTokens(t *testing.T) {
	if u := parseUsage(killedStream); u != nil {
		t.Fatalf("there is no result envelope: %+v", u)
	}
	u := partialUsage(killedStream)
	if u == nil {
		t.Fatal("the calls the stream reported must be counted")
	}
	if u.InputTokens != 2+2957+40 || u.OutputTokens != 120+1 || u.CacheReadTokens != 649+3606 {
		t.Errorf("each call counted once, the cut-off one with what it reported: %+v", u)
	}
	if u.CostUSD != 0 {
		t.Errorf("the stream carries no cost: %+v", u)
	}
}

// TestATurnKilledMidwayReportsWhatItUsed: the adapter reports a killed
// turn's tokens, so the token budgets see them.
func TestATurnKilledMidwayReportsWhatItUsed(t *testing.T) {
	a, _, _ := fakeAdapter(t, "killedmidway")
	if err := a.SendMessage(context.Background(), testTask(), msg("Go on.")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case o := <-a.Outbound():
			if o.Usage != nil {
				if o.Usage.InputTokens != 2+2957+40 || o.Usage.OutputTokens != 121 {
					t.Errorf("the killed turn's tokens: %+v", o.Usage)
				}
				return
			}
		case <-deadline:
			t.Fatal("a killed turn reported no usage")
		}
	}
}
