package claude

import (
	"fmt"
	"strings"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
)

// The reply header the orchestrator asks Claude Code to emit. It is our
// convention, not a Claude Code feature: the CLI returns free text, and this
// is how that text is turned back into a routed protocol message.
const headerMarker = "@@ORCH"

// renderPrompt turns an internal protocol message into the prompt text passed
// to `claude -p`.
//
// The prompt carries everything the agent needs to answer in protocol:
// its role and whether it holds authority, the task, exactly which agent ids
// it may address, any relevant shared knowledge the orchestrator retrieved,
// and the reply format. Nothing is assumed to survive from a previous call —
// each invocation is a fresh process with no memory.
func renderPrompt(t *domain.Task, m domain.Message, allowWrites bool) string {
	var b strings.Builder

	self := m.ToAgent
	isAuthority := t != nil && t.AuthorityAgent == self

	b.WriteString("You are a coding agent participating in a multi-agent deliberation.\n")
	b.WriteString("An orchestrator routes every message; you never talk to another agent directly.\n\n")

	fmt.Fprintf(&b, "YOUR AGENT ID: %s\n", self)
	if isAuthority {
		b.WriteString("YOUR ROLE: AUTHORITY. You hold final decision-making responsibility for this task.\n")
		b.WriteString("Decide what investigation is needed, delegate it, weigh the answers, challenge\n")
		b.WriteString("conclusions you do not believe, and issue the final decision when the evidence\n")
		b.WriteString("supports one. Authority is responsibility, not correctness: change your position\n")
		b.WriteString("when the evidence warrants it.\n")
	} else {
		authority := domain.AgentID("unknown")
		if t != nil {
			authority = t.AuthorityAgent
		}
		fmt.Fprintf(&b, "YOUR ROLE: SUPPORTING AGENT. The authority for this task is %q.\n", authority)
		b.WriteString("You may investigate, propose, implement, review and CHALLENGE the authority.\n")
		b.WriteString("Disagreement is expected and is recorded, not suppressed — but back it with evidence.\n")
		b.WriteString("You may NOT issue FINAL_DECISION; the orchestrator will reject it.\n")
	}

	if t != nil {
		fmt.Fprintf(&b, "\nTASK %s: %s\n", t.ID, t.Title)
		if t.Description != "" {
			fmt.Fprintf(&b, "%s\n", t.Description)
		}
		b.WriteString("\nAGENTS YOU MAY ADDRESS (use these exact ids):\n")
		for _, p := range t.Participants {
			if p == self {
				continue
			}
			marker := ""
			if p == t.AuthorityAgent {
				marker = "  (authority)"
			}
			fmt.Fprintf(&b, "  - %s%s\n", p, marker)
		}
		b.WriteString("  - human            (to ask the operator)\n")
		b.WriteString("  - *                (broadcast to all participants)\n")
	}

	// Relevant project knowledge, retrieved by the orchestrator. This is what
	// replaces conversation memory across one-shot invocations.
	if k := strings.TrimSpace(m.Metadata["knowledge"]); k != "" {
		b.WriteString("\nRELEVANT PROJECT KNOWLEDGE (established earlier in this task):\n")
		b.WriteString(k)
		b.WriteString("\n")
	}

	// An agent starting (or restarting) its session has no memory of the task
	// so far; the orchestrator's transcript fills that gap.
	core.WriteRecap(&b, m)
	core.WriteIncoming(&b, t, m)

	if allowWrites {
		b.WriteString("\nYou have write access to the project folder: follow the WRITE RULES below.\n")
	} else {
		b.WriteString("\nYou are running READ-ONLY: you can read, grep and glob files, but you cannot\n")
		b.WriteString("edit anything or run shell commands. If asked to IMPLEMENT, do not attempt to\n")
		b.WriteString("write files — describe the change precisely, or supply a patch in your reply.\n")
	}

	b.WriteString("\nREPLY FORMAT — this is mandatory. Your reply MUST begin with a header line:\n")
	fmt.Fprintf(&b, "  %s type=<MESSAGE_TYPE> to=<agent id>\n", headerMarker)
	b.WriteString("followed by your message body. Emit several blocks if you need to say several\n")
	b.WriteString("things. Do not wrap the header in code fences or add anything before it.\n\n")
	b.WriteString("Valid MESSAGE_TYPE values:\n  ")
	b.WriteString(core.TypeList(isAuthority))
	b.WriteString("\n")
	core.WriteRoleGuide(&b, isAuthority, headerMarker)
	b.WriteString("\nExample:\n")
	fmt.Fprintf(&b, "  %s type=DELEGATE to=codex\n", headerMarker)
	b.WriteString("  Investigate how the session cookie is created and cleared. Do not change code yet.\n")

	// Task titles and agent-supplied content are free text; see SanitizePrompt.
	return core.SanitizePrompt(b.String())
}

// parseOutput parses the reply header format described in renderPrompt back
// into protocol messages (see core.ParseReply).
func parseOutput(raw string) ([]domain.Message, error) {
	return core.ParseReply(raw, headerMarker, "claude-code adapter")
}
