package claude

import (
	"ritam/internal/agents/core"
	"ritam/internal/domain"
)

// The reply header the orchestrator asks Claude Code to emit. It is our
// convention, not a Claude Code feature: the CLI returns free text, and this
// is how that text is turned back into a routed protocol message.
const headerMarker = "@@ORCH"

// dialect is how Claude's briefs write the header and describe read-only
// mode; the rest of the brief is shared with Codex (core.RenderBrief).
var dialect = core.Dialect{
	Marker:   headerMarker,
	ReadOnly: "You are READ-ONLY: you can read, grep and glob files, but you cannot edit anything or run shell commands.",
}

// renderPrompt is the full brief passed to `claude -p` for a new session: the
// agent's role and authority, the task, exactly which ids it may address, any
// shared knowledge the orchestrator retrieved, the rules and the reply
// format. writeRules is the agent's core.WriteRules, "" when it is read-only.
func renderPrompt(t *domain.Task, m domain.Message, writeRules string) string {
	return core.RenderBrief(t, m, dialect, writeRules)
}

// renderTurn is the standing brief for a resumed session.
func renderTurn(t *domain.Task, m domain.Message, writeRules string) string {
	return core.RenderTurn(t, m, dialect, writeRules)
}

// parseOutput parses the reply header format described in renderPrompt back
// into protocol messages (see core.ParseReply).
func parseOutput(raw string) ([]domain.Message, error) {
	return core.ParseReply(raw, headerMarker, "claude-code adapter")
}
