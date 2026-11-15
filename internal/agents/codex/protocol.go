package codex

import (
	"ritam/internal/agents/core"
	"ritam/internal/domain"
)

// headerMarker is the reply header the orchestrator asks Codex to emit.
//
// It differs from the Claude adapter's marker deliberately. The two CLIs are
// independent applications, and giving each its own wire convention keeps a
// change to one from silently altering how the other is parsed.
const headerMarker = "##CODEX"

// dialect is how Codex's briefs write the header (keys in capitals, as they
// always have been) and describe its read-only sandbox; the rest of the brief
// is shared with Claude (core.RenderBrief).
var dialect = core.Dialect{
	Marker:    headerMarker,
	UpperKeys: true,
	ReadOnly: "You are READ-ONLY, in the read-only sandbox: you can inspect files, but you cannot edit " +
		"anything or make network calls.",
}

// renderPrompt is the full brief passed to `codex exec` for a new thread: the
// agent's role and authority, the task, the exact ids it may address, any
// shared knowledge the orchestrator retrieved, the rules and the reply
// format. writeRules is the agent's core.WriteRules, "" when it is read-only.
func renderPrompt(t *domain.Task, m domain.Message, writeRules string) string {
	return core.RenderBrief(t, m, dialect, writeRules)
}

// renderTurn is the standing brief for a resumed thread.
func renderTurn(t *domain.Task, m domain.Message, writeRules string) string {
	return core.RenderTurn(t, m, dialect, writeRules)
}

// parseOutput parses the `##CODEX` reply header format back into protocol
// messages (see core.ParseReply).
func parseOutput(raw string) ([]domain.Message, error) {
	return core.ParseReply(raw, headerMarker, "codex adapter")
}
