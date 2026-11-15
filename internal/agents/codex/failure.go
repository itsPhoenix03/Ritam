package codex

import (
	"encoding/json"
	"strings"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
)

// missingThreadText is what Codex says when `exec resume` names a thread it
// does not have.
const missingThreadText = "no rollout found"

// turnReport is what a turn's JSONL stream said about how it ended.
type turnReport struct {
	// failed is set by a turn.failed event.
	failed bool
	// completed is set by a turn.completed event.
	completed bool
	// failMessage is turn.failed's message; errorMessages those of error
	// events, in order.
	failMessage   string
	errorMessages []string
}

// observe reads one line of the stream for the events that end a turn.
//
// The stream reader used to turn turn.failed into the bare activity line
// "turn failed" and drop its message, and did not handle the error event at
// all, so a turn Codex said had failed, and why, reached the orchestrator as
// an exit code or an empty reply.
func (r *turnReport) observe(line string) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") ||
		(!strings.Contains(trimmed, `"turn.`) && !strings.Contains(trimmed, `"error"`)) {
		return
	}
	var ev struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(trimmed), &ev) != nil {
		return
	}
	switch ev.Type {
	case "turn.completed":
		r.completed = true
	case "turn.failed":
		r.failed = true
		r.failMessage = eventMessage([]byte(trimmed))
	case "error":
		if m := eventMessage([]byte(trimmed)); m != "" {
			r.errorMessages = append(r.errorMessages, m)
		}
	}
}

// message is the text that explains how the turn ended badly: turn.failed's
// own, else the last error event's.
func (r turnReport) message() string {
	if r.failMessage != "" {
		return r.failMessage
	}
	if n := len(r.errorMessages); n > 0 {
		return r.errorMessages[n-1]
	}
	return ""
}

// eventMessage takes an error or turn.failed event's text from whichever of
// the usual fields carries it: a top-level "message", or an "error" that is
// either the text itself or an object with a "message". Fields that are
// absent, or of another shape, are ignored.
func eventMessage(line []byte) string {
	var ev struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return ""
	}
	if s := strings.TrimSpace(ev.Message); s != "" {
		return s
	}
	if len(ev.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(ev.Error, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(ev.Error, &obj) == nil {
		return strings.TrimSpace(obj.Message)
	}
	return ""
}

// turnFailure reads why a turn failed, or returns nil when it did not.
//
// A turn failed when Codex said so (turn.failed, or an error event in a turn
// that never completed) or when the CLI exited non-zero. An error event in a
// turn that then completed is a hiccup Codex recovered from. A clean exit
// with an empty reply is not a failure: an agent may have nothing to say.
//
// The kind is read from the event's message first; stderr -- all of it -- is
// the last resort, and the agent's own output is never read for it.
func turnFailure(res *core.OneShotResult, runErr error, rep turnReport) *domain.AgentFailure {
	if f := core.RunnerFailure(runErr); f != nil {
		return f
	}
	reported := rep.failed || (len(rep.errorMessages) > 0 && !rep.completed)
	if runErr == nil && !reported {
		return nil
	}
	msg, stderr := rep.message(), core.ErrorText(res)
	if strings.Contains(msg, missingThreadText) || strings.Contains(stderr, missingThreadText) {
		return domain.NewFailure(domain.FailureSessionMissing, "codex adapter: "+core.Excerpt(msg+" "+stderr, 300), runErr)
	}
	detail := "codex adapter: "
	switch {
	case msg != "":
		detail += "turn failed: " + core.Excerpt(msg, 300)
	case runErr != nil:
		detail += runErr.Error()
	default:
		detail += "turn failed"
	}
	if msg != "" {
		if kind := domain.ClassifyAgentFailure(msg); kind != "" {
			return core.FailureFromText(msg, kind, detail, runErr)
		}
	}
	return core.FailureFromText(stderr, domain.FailureCrash, detail, runErr)
}
