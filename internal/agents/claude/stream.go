package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"ritam/internal/agents/core"
	"ritam/internal/ports"
)

// streamEnvelope is one line of `--output-format stream-json` output. Only the
// fields the adapter uses are declared; the format carries a great deal more.
type streamEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// Present on type=stream_event: the raw Anthropic streaming event.
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content_block"`
	} `json:"event"`

	// Present on type=result: the final envelope, same shape Run used to parse.
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// consumeStreamLine parses one streamed line, publishing live activity and
// returning the final result text when the terminating event arrives.
//
// Unparseable lines are ignored rather than surfaced: the stream carries
// internal bookkeeping (hooks, status pings, session init) that this adapter
// has no interest in, and a future CLI version adding more of it must not turn
// into a failed task.
func consumeStreamLine(line string, act *core.ActivityEmitter) (result string, done bool, err error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return "", false, nil
	}

	var env streamEnvelope
	if jsonErr := json.Unmarshal([]byte(trimmed), &env); jsonErr != nil {
		return "", false, nil
	}

	switch env.Type {
	case "stream_event":
		switch env.Event.Type {
		case "content_block_delta":
			switch env.Event.Delta.Type {
			case "text_delta":
				act.Append(ports.ActivityText, env.Event.Delta.Text)
			case "thinking_delta":
				act.Append(ports.ActivityThinking, env.Event.Delta.Thinking)
			}
		case "content_block_start":
			if env.Event.ContentBlock.Type == "tool_use" {
				act.Emit(ports.ActivityTool, env.Event.ContentBlock.Name)
			}
		case "message_stop":
			act.Flush()
		}

	case "result":
		act.Flush()
		if env.IsError || env.Subtype == "error" {
			return "", true, fmt.Errorf("claude-code adapter: CLI reported an error: %s",
				truncate(env.Result, 300))
		}
		// A successful turn with no text is the agent saying nothing, which
		// it may do when no reply is needed; the adapter treats it so.
		return env.Result, true, nil
	}
	return "", false, nil
}
