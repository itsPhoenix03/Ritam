package claude

import (
	"encoding/json"
	"strings"
	"sync"

	"ritam/internal/agents/core"
	"ritam/internal/ports"
)

// toolCallEnvelope is the part of a streamed assistant message that names
// the tools it calls. Its tool_use ids are the ones the CLI gives its hook as
// tool_use_id (seen live on claude 2.1.330), including a subagent's calls.
type toolCallEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
}

// toolCallLog collects the tool calls a turn's stream announces, each once:
// the CLI repeats a message's id across the lines carrying its blocks.
type toolCallLog struct {
	mu    sync.Mutex
	seen  map[string]bool
	calls []ports.ToolCall
}

// observe notes the tool calls in one streamed line, if it has any.
func (l *toolCallLog) observe(line string) {
	if !strings.Contains(line, `"tool_use"`) {
		return
	}
	var env toolCallEnvelope
	if json.Unmarshal([]byte(strings.TrimSpace(line)), &env) != nil || env.Type != "assistant" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range env.Message.Content {
		if c.Type != "tool_use" || c.ID == "" || l.seen[c.ID] {
			continue
		}
		if l.seen == nil {
			l.seen = map[string]bool{}
		}
		l.seen[c.ID] = true
		l.calls = append(l.calls, ports.ToolCall{ID: c.ID, Tool: c.Name})
	}
}

// list is what the stream reported, matched to the hook by id.
func (l *toolCallLog) list() ports.ToolCalls {
	l.mu.Lock()
	defer l.mu.Unlock()
	return ports.ToolCalls{Calls: append([]ports.ToolCall(nil), l.calls...)}
}

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
		if f := resultFailure(claudeEnvelope{Type: env.Type, Subtype: env.Subtype, IsError: env.IsError,
			Result: env.Result}); f != nil {
			return "", true, f
		}
		// A successful turn with no text is the agent saying nothing, which
		// it may do when no reply is needed; the adapter treats it so.
		return env.Result, true, nil
	}
	return "", false, nil
}
