package core

import (
	"fmt"
	"strings"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// WriteIncoming writes what the agent is being asked to read this turn: one
// message, or several combined into one delivery, followed by the team
// status. Every prompt renderer uses it, so a combined delivery and the
// team board read the same whether the agent's session is new or resumed.
func WriteIncoming(b *strings.Builder, t *domain.Task, m domain.Message) {
	if n := m.Metadata[ports.BatchMetadataKey]; n != "" {
		WriteFollowUpNote(b, t, m)
		fmt.Fprintf(b, "\n--- %s NEW MESSAGES SINCE YOUR LAST TURN (oldest first) ---\n", n)
		b.WriteString("Read all of them before you reply: a later message may change or supersede an earlier one.\n")
		b.WriteString("Answer everything in one reply, one block per recipient where you can.\n")
		if m.Metadata[ports.BatchHumanMetadataKey] != "" {
			b.WriteString("Messages marked [HUMAN] are from the human operator: answer those first (to=human).\n")
		}
		b.WriteString("\n")
		b.WriteString(m.Content)
		b.WriteString("\n--- END ---\n")
	} else {
		WriteThreadNote(b, m)
		WriteFollowUpNote(b, t, m)
		fmt.Fprintf(b, "\n--- INCOMING %s FROM %s ---\n", m.Type, m.FromAgent)
		b.WriteString(m.Content)
		b.WriteString("\n--- END ---\n")
	}
	WriteTeamStatus(b, t, m)
}

// WriteTeamStatus writes the team board the orchestrator attached, with the
// rule that goes with it: every agent may read it, only the authority may
// act on it.
func WriteTeamStatus(b *strings.Builder, t *domain.Task, m domain.Message) {
	board := strings.TrimSpace(m.Metadata[ports.TeamMetadataKey])
	if board == "" {
		return
	}
	b.WriteString("\nTEAM STATUS (as of this delivery; read-only):\n")
	b.WriteString(board)
	b.WriteString("\n")
	if t != nil && t.AuthorityAgent == m.ToAgent {
		b.WriteString("You are the authority: act on this when it helps. Wait for an agent that is still working\n")
		b.WriteString("on something you need rather than asking again, and re-delegate if one is falling behind.\n")
		return
	}
	authority := "the authority"
	if t != nil && t.AuthorityAgent != "" {
		authority = string(t.AuthorityAgent)
	}
	fmt.Fprintf(b, "This is for your awareness only. Do not reassign or take over others' work; if something\n"+
		"needs to change, tell %s. If someone is still working on what you need, wait for it.\n", authority)
}

// WorkingOn is the status line an adapter shows when a turn starts.
func WorkingOn(m domain.Message) string {
	if n := m.Metadata[ports.BatchMetadataKey]; n != "" {
		return "working on " + n + " new messages"
	}
	return fmt.Sprintf("working on %s from %s", m.Type, m.FromAgent)
}
