package core

import (
	"fmt"
	"strings"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Prompt pieces shared by the CLI adapters.
//
// An agent with a persistent session already holds the task, its role and the
// conversation so far, so a resumed turn does not resend the full opening
// brief. It does resend a short standing brief, every time, because the CLIs
// compact long sessions: a summary can drop the agent's id, its role, or --
// worst of all -- the reply header format, after which its replies stop
// parsing. A few lines per turn is the price of never depending on what
// survived compaction.

// RecapKey is the message metadata key carrying a transcript of the task so
// far. The orchestrator attaches it to every delivery; adapters use it only
// when an agent is starting (or restarting) a session and has no memory of
// what came before.
const RecapKey = ports.RecapMetadataKey

// WriteRecap appends the task transcript to a full opening prompt, if the
// orchestrator supplied one.
func WriteRecap(b *strings.Builder, m domain.Message) {
	r := strings.TrimSpace(m.Metadata[RecapKey])
	if r == "" {
		return
	}
	b.WriteString("\nCONVERSATION SO FAR in this task (you have no earlier memory of it):\n")
	b.WriteString(r)
	b.WriteString("\n")
}

// WriteThreadNote tells the agent that the request in front of it is part of
// a question the human asked, and that answering the human directly is
// preferred. It is written into both the full brief and the per-turn brief:
// whether an agent can see the human behind a request should not depend on
// whether its session is new.
func WriteThreadNote(b *strings.Builder, m domain.Message) {
	note := strings.TrimSpace(m.Metadata[ports.ThreadMetadataKey])
	if note == "" {
		return
	}
	fmt.Fprintf(b, "\nTHIS REQUEST IS PART OF A QUESTION FROM THE HUMAN (%s).\n", note)
	fmt.Fprintf(b, "If you can answer it, answer the human directly (to=human), then tell %s in one line\n", m.FromAgent)
	b.WriteString("that you have. If someone has already answered the human, do not repeat it.\n")
}

// WriteFollowUpNote tells the authority, on the message reopening a completed
// task, how the follow-up ends. Without it a follow-up that set the other
// agents new work had no stated end, and the authority had no reason to send
// a FINAL_DECISION when the work was done.
func WriteFollowUpNote(b *strings.Builder, t *domain.Task, m domain.Message) {
	if m.Metadata[ports.FollowUpMetadataKey] == "" || t == nil || m.ToAgent != t.AuthorityAgent {
		return
	}
	b.WriteString("\nTHIS IS A FOLLOW-UP: the task was completed and the human has reopened it with the message below.\n")
	b.WriteString("If it only asks something, answer the human (to=human); the task then completes again by itself.\n")
	b.WriteString("If you give the other agents new work, the task stays open until that work is done and you send\n")
	b.WriteString("a FINAL_DECISION with the outcome.\n")
}

// RenderTurn builds the prompt for a resumed session: the standing brief plus
// the incoming message. marker is the adapter's reply header (`@@ORCH`,
// `##CODEX`).
func RenderTurn(t *domain.Task, m domain.Message, allowWrites bool, marker string) string {
	var b strings.Builder
	self := m.ToAgent

	role := "SUPPORTING AGENT (you may not issue FINAL_DECISION)"
	if t != nil && t.AuthorityAgent == self {
		role = "AUTHORITY (you issue the FINAL_DECISION)"
	}
	b.WriteString("[orchestrator] This continues your session in the multi-agent deliberation.\n")
	fmt.Fprintf(&b, "You are %s, %s.\n", self, role)

	if t != nil {
		fmt.Fprintf(&b, "Task %s: %s\n", t.ID, t.Title)
		var ids []string
		for _, p := range t.Participants {
			if p == self {
				continue
			}
			id := string(p)
			if p == t.AuthorityAgent {
				id += " (authority)"
			}
			ids = append(ids, id)
		}
		ids = append(ids, "human", "* (everyone)")
		fmt.Fprintf(&b, "You may address: %s\n", strings.Join(ids, ", "))
	}

	if k := strings.TrimSpace(m.Metadata["knowledge"]); k != "" {
		b.WriteString("\nRELEVANT PROJECT KNOWLEDGE:\n")
		b.WriteString(k)
		b.WriteString("\n")
	}

	WriteIncoming(&b, t, m)

	if allowWrites {
		b.WriteString("\nYou have write access to the project folder: follow the WRITE RULES below.\n")
	} else {
		b.WriteString("\nYou are READ-ONLY: do not try to edit files or run commands.\n")
	}

	authority := t != nil && t.AuthorityAgent == self
	fmt.Fprintf(&b, "\nREPLY FORMAT (mandatory): begin with the header line\n  %s type=<MESSAGE_TYPE> to=<agent id>\n", marker)
	b.WriteString("then the body; repeat header+body for several messages. No code fences around it.\n")
	fmt.Fprintf(&b, "MESSAGE_TYPE is one of: %s\n", TypeList(authority))
	if !authority {
		// One line, every turn: compaction must not be able to cost an agent
		// the knowledge of how to consult others or how to record agreement.
		fmt.Fprintf(&b, "Need another agent to check something? Send it a QUESTION, then report back to whoever asked you. "+
			"Judging something? %s type=REVIEW to=<id> verdict=approve|changes|reject (advice only).\n", marker)
	}

	return SanitizePrompt(b.String())
}

// TypeList is the comma-separated list of message types an agent may send.
// It is built from the same rule the orchestrator enforces, so a prompt never
// offers a type that would be refused.
func TypeList(authority bool) string {
	types := domain.AllowedTypes(authority)
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	return strings.Join(out, ", ")
}

// WriteRoleGuide explains, in a full opening brief, how this agent works with
// the others.
//
// The supporting-agent half exists because of a real failure: asked by a human
// to verify something, a supporting agent sent APPROVE -- the only way it saw
// to say "I agree" -- and was refused, because APPROVE belongs to the
// authority. It needed to know two things: how to get another agent to check
// something, and how to record a judgement that is not a decision.
func WriteRoleGuide(b *strings.Builder, authority bool, marker string) {
	b.WriteString("\nWORKING WITH THE OTHER AGENTS:\n")
	b.WriteString("- If you can answer a question the human asked -- even one passed to you by another\n")
	b.WriteString("  agent -- answer the human directly (to=human) and tell that agent in one line.\n")
	b.WriteString("  If someone has already answered the human, do not repeat it.\n")
	if authority {
		b.WriteString("- Supporting agents report judgements as REVIEW with verdict=approve, changes or reject.\n")
		b.WriteString("  Those are advice. You weigh them and decide with APPROVE, REJECT or FINAL_DECISION.\n")
		b.WriteString("- Before deciding, make sure the requests you sent have been answered.\n")
		return
	}
	b.WriteString("- To have another agent check or investigate something, send it a QUESTION.\n")
	b.WriteString("  Its ANSWER comes back to you; you do not need the authority to delegate it.\n")
	b.WriteString("- When you have what you need, report to whoever asked you. If the human asked,\n")
	b.WriteString("  answer the human (to=human).\n")
	b.WriteString("- To give a judgement on something you reviewed, send a REVIEW with a verdict:\n")
	fmt.Fprintf(b, "    %s type=REVIEW to=<agent id> verdict=approve   (or changes, or reject)\n", marker)
	b.WriteString("  A verdict is advice. Only the authority approves, rejects or decides; APPROVE,\n")
	b.WriteString("  REJECT, REQUEST_CHANGE, IMPLEMENT, DELEGATE and FINAL_DECISION are refused from you.\n")
}
