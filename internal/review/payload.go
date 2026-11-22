package review

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"ritam/internal/domain"
	"ritam/internal/logging"
)

// Call is one tool call as the hook reported it.
type Call struct {
	Task  domain.TaskID
	Agent domain.AgentID
	Kind  domain.AgentKind
	// SessionID is the CLI's session (Claude) or thread (Codex).
	SessionID string
	// Cwd is the folder the CLI says the call runs in.
	Cwd string
	// Tool is the CLI's name for the tool: Bash, Write, apply_patch, ...
	Tool string
	// ToolUseID is the CLI's id for the call.
	ToolUseID string
	// Subagent is the CLI's id for the subagent that made the call; empty
	// when the agent made it itself.
	Subagent string
	// Summary says what the call does in one line: the command, the file or
	// the address, with secrets redacted.
	Summary string
	// Decision is what the server answered.
	Decision Decision
	// Note says why a call was refused.
	Note string
	// Severity is the worst finding's, "none" when no rule fired; empty
	// when the call was not reviewed (no policy, or unreadable).
	Severity string
	// Findings are the rules that fired.
	Findings []Finding
	// Asked: the human was asked; Answer is what came of it: approved,
	// refused, timed out, nobody.
	Asked  bool
	Answer string
	// Stopped: a critical call stopped the turn.
	Stopped bool
}

// payload is what both CLIs send their pre-tool hook on stdin. Claude Code
// and Codex use the same names for these fields; each sends more (Codex a
// turn_id and model, Claude a transcript path and effort), kept out of the
// record.
type payload struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolUseID     string          `json:"tool_use_id"`
	AgentID       string          `json:"agent_id"`
}

var errNoTool = errors.New("the call names no tool")

// decodePayload reads a hook payload. A payload that is not JSON, or names no
// tool, cannot be checked.
func decodePayload(data []byte) (payload, error) {
	var p payload
	if err := json.Unmarshal(data, &p); err != nil {
		return p, err
	}
	if strings.TrimSpace(p.ToolName) == "" {
		return p, errNoTool
	}
	return p, nil
}

// summaryKeys are the tool_input fields that say what a call does, in the
// order they are looked for: the command of the shell tools (Codex's
// exec_command calls it cmd), the file of the file tools, the folder and
// pattern of the search tools, the address of the web tools, and what a
// subagent is asked.
var summaryKeys = []string{"command", "cmd", "file_path", "notebook_path", "pattern", "path", "url", "query",
	"description", "prompt"}

// summaryLimit is the most of a summary kept, in characters.
const summaryLimit = 200

// summarize says in one line what a call does, for the record. Secrets are
// redacted, and a long command is cut short.
func summarize(input json.RawMessage) string {
	var fields map[string]any
	_ = json.Unmarshal(input, &fields)
	text := ""
	for _, k := range summaryKeys {
		if s, ok := fields[k].(string); ok && strings.TrimSpace(s) != "" {
			text = s
			break
		}
		// Codex can pass a command as an argv list.
		if list, ok := fields[k].([]any); ok && len(list) > 0 {
			parts := make([]string, 0, len(list))
			for _, v := range list {
				if s, ok := v.(string); ok {
					parts = append(parts, s)
				}
			}
			text = strings.Join(parts, " ")
			break
		}
	}
	if text == "" && len(input) > 0 {
		text = string(input)
	}
	if files := patchFiles(text); files != "" {
		text = files
	}
	return clip(logging.Redact(strings.Join(strings.Fields(text), " ")), summaryLimit)
}

// patchFiles lists the files an apply_patch patch adds, updates or deletes,
// which say more than its first line does; "" if text is no patch.
func patchFiles(text string) string {
	if !strings.Contains(text, "*** Begin Patch") {
		return ""
	}
	var files []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		for _, op := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: "} {
			if f, ok := strings.CutPrefix(line, op); ok {
				files = append(files, strings.ToLower(strings.Fields(op)[1])+" "+f)
			}
		}
	}
	if len(files) == 0 {
		return ""
	}
	return "patch: " + strings.Join(files, ", ")
}

// clip cuts s to at most n characters, marking the cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-3]) + "..."
}
