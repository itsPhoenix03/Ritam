package codex

import (
	"encoding/json"
	"strings"

	"projectorch/internal/agents/core"
	"projectorch/internal/ports"
)

// streamEvent is one line of `codex exec --json` output.
//
// Codex streams at item granularity rather than token granularity: whole
// reasoning steps, command executions and the final message, instead of
// incremental deltas. The UI therefore updates in visible steps for Codex and
// continuously for Claude, which is a property of the CLIs, not of this code.
type streamEvent struct {
	Type string `json:"type"`
	Item struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Text    string `json:"text"`
		Command string `json:"command"`
	} `json:"item"`
}

// threadStarted reports the thread id when line is the `thread.started` event
// a new Codex conversation opens with. Codex chooses the id itself (there is
// no --session-id), so this event is the only way to learn which thread to
// resume next time. Verified live: it is the first line `exec --json` prints.
func threadStarted(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, `"thread.started"`) {
		return "", false
	}
	var ev struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal([]byte(trimmed), &ev) != nil || ev.Type != "thread.started" || ev.ThreadID == "" {
		return "", false
	}
	return ev.ThreadID, true
}

// consumeStreamLine parses one streamed line and publishes live activity.
//
// Unparseable lines are ignored: the stream carries bookkeeping this adapter
// has no interest in, and a future CLI version adding more of it must not turn
// into a failed task. The authoritative reply is still read from the
// --output-last-message file, never from this stream.
func consumeStreamLine(line string, act *core.ActivityEmitter) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return
	}

	var ev streamEvent
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		return
	}

	switch ev.Type {
	case "turn.started":
		act.Emit(ports.ActivityStatus, "turn started")
	case "turn.completed":
		act.Emit(ports.ActivityStatus, "turn completed")
	case "turn.failed":
		act.Emit(ports.ActivityStatus, "turn failed")
	case "item.started", "item.updated", "item.completed":
		switch ev.Item.Type {
		case "reasoning":
			act.Append(ports.ActivityThinking, ev.Item.Text)
		case "agent_message":
			act.Append(ports.ActivityText, ev.Item.Text)
		case "command_execution":
			if ev.Item.Command != "" {
				act.Emit(ports.ActivityTool, ev.Item.Command)
			}
		case "file_change", "mcp_tool_call", "web_search":
			act.Emit(ports.ActivityTool, ev.Item.Type)
		}
	}
}
