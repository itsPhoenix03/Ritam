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
// brief. It does resend a standing brief, every time, because the CLIs
// compact long sessions: a summary can drop the agent's id, its role, the
// authority's rules or -- worst of all -- the reply header format, after
// which its replies stop parsing. Those lines on every turn are the price of
// never depending on what survived compaction.
//
// Both briefs are laid out in the same order: who the agent is, the task,
// what just arrived, the rules, and last the reply format with an example
// built from this task. The reply format comes last because it is what the
// model must act on when it writes, and the write rules once followed it.

// Dialect is how one CLI adapter writes the reply header, and how its
// read-only mode is described. Everything else in the briefs is shared, so a
// fix to the wording is made once for both CLIs.
type Dialect struct {
	// Marker opens a header line: "@@ORCH", "##CODEX".
	Marker string
	// UpperKeys writes the header's keys in capitals (TYPE= TO=). The parser
	// reads either; the prompt shows each CLI the form it has always seen.
	UpperKeys bool
	// ReadOnly tells a read-only agent what its CLI lets it do.
	ReadOnly string
}

// Header is a header line in this dialect.
func (d Dialect) Header(typ, to string) string {
	return fmt.Sprintf("%s %s=%s %s=%s", d.Marker, d.key("type"), typ, d.key("to"), to)
}

func (d Dialect) key(k string) string {
	if d.UpperKeys {
		return strings.ToUpper(k)
	}
	return k
}

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

// RenderBrief builds the full opening prompt: for an agent starting a
// session, or rebuilding one it lost. rules is the agent's WriteRules,
// "" for a read-only agent.
func RenderBrief(t *domain.Task, m domain.Message, d Dialect, rules string) string {
	var b strings.Builder
	self := m.ToAgent
	authority := isAuthority(t, self)

	b.WriteString("You are a coding agent in a multi-agent deliberation. An orchestrator routes every\n")
	b.WriteString("message; you never talk to another agent directly.\n\n")
	fmt.Fprintf(&b, "YOUR AGENT ID: %s\n", self)
	if authority {
		b.WriteString("YOUR ROLE: AUTHORITY. You hold final decision-making responsibility for this task.\n")
		b.WriteString("Decide what investigation is needed, delegate it, weigh the answers, challenge\n")
		b.WriteString("conclusions you do not believe, and issue the final decision when the evidence\n")
		b.WriteString("supports one. Authority is responsibility, not correctness: change your position\n")
		b.WriteString("when the evidence warrants it.\n")
	} else {
		fmt.Fprintf(&b, "YOUR ROLE: SUPPORTING AGENT. The authority for this task is %s.\n", authorityOf(t))
		b.WriteString("You may investigate, propose, implement, review and CHALLENGE the authority.\n")
		b.WriteString("Disagreement is recorded, not suppressed, but back it with evidence.\n")
		b.WriteString("You may NOT issue FINAL_DECISION; the orchestrator rejects it.\n")
	}

	if t != nil {
		fmt.Fprintf(&b, "\nTASK %s: %s\n", t.ID, t.Title)
		if desc := strings.TrimSpace(t.Description); desc != "" {
			b.WriteString(desc)
			b.WriteString("\n")
		}
		b.WriteString("\nAGENTS YOU MAY ADDRESS (use these exact ids):\n")
		for _, p := range addressable(t, self) {
			note := ""
			if p == t.AuthorityAgent {
				note = "  (authority)"
			}
			fmt.Fprintf(&b, "  - %s%s\n", p, note)
		}
		b.WriteString("  - human  (the operator)\n")
		b.WriteString("  - *  (everyone: " + broadcastRule + ")\n")
	}

	writeKnowledge(&b, m)
	// An agent starting (or restarting) its session has no memory of the task
	// so far; the orchestrator's transcript fills that gap.
	WriteRecap(&b, m)
	WriteIncoming(&b, t, m)
	writeRuleSection(&b, authority, d, rules)
	writeReplyFormat(&b, t, self, d)

	// Task titles and agent-supplied content are free text; see SanitizePrompt.
	return SanitizePrompt(b.String())
}

// RenderTurn builds the prompt for a resumed session: the standing brief
// around the incoming message. rules is the agent's WriteRules, "" for a
// read-only agent.
func RenderTurn(t *domain.Task, m domain.Message, d Dialect, rules string) string {
	var b strings.Builder
	self := m.ToAgent
	authority := isAuthority(t, self)

	b.WriteString("[orchestrator] This continues your session in the multi-agent deliberation.\n")
	if authority {
		fmt.Fprintf(&b, "You are %s, the AUTHORITY: you issue the FINAL_DECISION.\n", self)
	} else {
		fmt.Fprintf(&b, "You are %s, a SUPPORTING AGENT: the authority is %s, and you may not issue FINAL_DECISION.\n",
			self, authorityOf(t))
	}

	if t != nil {
		// The task statement, every turn: after compaction it may be all the
		// agent has left of what the work is for.
		fmt.Fprintf(&b, "Task %s: %s\n", t.ID, t.Title)
		if s := taskSummary(t.Description); s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
		ids := []string{}
		for _, p := range addressable(t, self) {
			id := string(p)
			if p == t.AuthorityAgent {
				id += " (authority)"
			}
			ids = append(ids, id)
		}
		ids = append(ids, "human", "* (everyone: "+broadcastRule+")")
		fmt.Fprintf(&b, "You may address: %s\n", strings.Join(ids, ", "))
	}

	writeKnowledge(&b, m)
	WriteIncoming(&b, t, m)
	writeRuleSection(&b, authority, d, rules)
	writeReplyFormat(&b, t, self, d)

	return SanitizePrompt(b.String())
}

// broadcastRule goes with every offer of `*`: a broadcast wakes every agent,
// each for a paid turn.
const broadcastRule = "use to=* only when every participant must act on it"

// summaryLines and summaryChars bound the task statement a resumed turn
// repeats: enough to recall what the task is, not the whole description,
// which the session already holds.
const (
	summaryLines = 3
	summaryChars = 300
)

// taskSummary is the first lines of a task description, capped.
func taskSummary(desc string) string {
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(desc), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	cut := len(lines) > summaryLines
	if cut {
		lines = lines[:summaryLines]
	}
	s := strings.Join(lines, "\n")
	if r := []rune(s); len(r) > summaryChars {
		s, cut = strings.TrimSpace(string(r[:summaryChars])), true
	}
	if cut {
		s += " [...]"
	}
	return s
}

func isAuthority(t *domain.Task, self domain.AgentID) bool {
	return t != nil && t.AuthorityAgent == self
}

func authorityOf(t *domain.Task) domain.AgentID {
	if t == nil || t.AuthorityAgent == "" {
		return "unknown"
	}
	return t.AuthorityAgent
}

// addressable lists the participants self may address: everyone else still on
// the task. A dropped agent is left out; a message to it is recorded but
// never delivered.
func addressable(t *domain.Task, self domain.AgentID) []domain.AgentID {
	var out []domain.AgentID
	for _, p := range t.Participants {
		if p != self && !t.IsDropped(p) {
			out = append(out, p)
		}
	}
	return out
}

func writeKnowledge(b *strings.Builder, m domain.Message) {
	// Relevant project knowledge, retrieved by the orchestrator: what keeps an
	// agent oriented when its own memory of the task is gone.
	if k := strings.TrimSpace(m.Metadata["knowledge"]); k != "" {
		b.WriteString("\nRELEVANT PROJECT KNOWLEDGE (established earlier in this task):\n")
		b.WriteString(k)
		b.WriteString("\n")
	}
}

// writeRuleSection writes how the agent works with the others, then what it may do
// to the project: read-only, or the write rules it was granted.
func writeRuleSection(b *strings.Builder, authority bool, d Dialect, rules string) {
	b.WriteString("\nRULES:\n")
	WriteRoleGuide(b, authority, d)
	if rules == "" {
		fmt.Fprintf(b, "- %s\n", d.ReadOnly)
		// "Supply a patch" was the old advice; nothing ever applied one.
		b.WriteString("  Asked to change something, describe the change precisely (file, function, what to change).\n")
		b.WriteString("  A write-enabled agent will make it.\n")
		return
	}
	b.WriteString("- You have write access to the project folder, under these WRITE RULES:\n")
	b.WriteString(rules)
}

// WriteRoleGuide writes, as rule lines, how this agent works with the others.
// It is in every brief, opening and per-turn, for the agent's actual role: the
// authority's rules once lived only in the opening brief, and were gone once
// the CLI compacted the session.
//
// The supporting-agent lines exist because of a real failure: asked by a
// human to verify something, a supporting agent sent APPROVE -- the only way
// it saw to say "I agree" -- and was refused, because APPROVE belongs to the
// authority. It needed to know two things: how to get another agent to check
// something, and how to record a judgement that is not a decision.
func WriteRoleGuide(b *strings.Builder, authority bool, d Dialect) {
	if authority {
		b.WriteString("- Send DELEGATE to have something investigated and IMPLEMENT to have files changed;\n")
		b.WriteString("  say exactly what you want back.\n")
		b.WriteString("- Supporting agents report judgements as REVIEW with verdict=approve, changes or reject.\n")
		b.WriteString("  Those are advice. You weigh them and decide with APPROVE, REJECT or FINAL_DECISION.\n")
		b.WriteString("- Before deciding, make sure the requests you sent have been answered.\n")
	} else {
		b.WriteString("- To have another agent check or investigate something, send it a QUESTION.\n")
		b.WriteString("  Its ANSWER comes back to you; you do not need the authority to delegate it.\n")
		b.WriteString("- When you have what you need, report to whoever asked you.\n")
		b.WriteString("- To give a judgement on something you reviewed, send a REVIEW with a verdict:\n")
		fmt.Fprintf(b, "    %s %s=approve   (or changes, or reject)\n", d.Header("REVIEW", "<agent id>"), d.key("verdict"))
		b.WriteString("  A verdict is advice. Only the authority approves, rejects or decides.\n")
	}
	b.WriteString("- If you can answer a question the human asked -- even one passed to you by another\n")
	b.WriteString("  agent -- answer the human directly (to=human) and tell that agent in one line.\n")
	b.WriteString("  If someone has already answered the human, do not repeat it.\n")
	b.WriteString("- " + NoticeRule)
}

// writeReplyFormat writes the reply format, last in every prompt.
//
// Only the CLI's final message is delivered: Claude's result is its last text
// block, Codex's reply file its last message. Claude was told its reply "MUST
// begin with" the header, so a header written before a tool call was lost
// with everything after it; both now say the same thing.
func writeReplyFormat(b *strings.Builder, t *domain.Task, self domain.AgentID, d Dialect) {
	authority := isAuthority(t, self)
	b.WriteString("\nREPLY FORMAT (mandatory):\n")
	b.WriteString("Do all your work first. Then write one final message that starts with the header line\n")
	fmt.Fprintf(b, "  %s\n", d.Header("<TYPE>", "<agent id>"))
	b.WriteString("followed by the body. To send several messages, repeat header and body.\n")
	b.WriteString("Text before your last tool call is not delivered.\n")
	fmt.Fprintf(b, "If nothing is asked of you and you have nothing new, reply with exactly %s.\n", NoReply)
	b.WriteString("Do not send thanks or acknowledgements.\n")
	b.WriteString("Types you may send:\n")
	WriteTypeGuide(b, authority)
	typ, to, body := example(t, self)
	b.WriteString("Example for this task:\n")
	fmt.Fprintf(b, "  %s\n  %s\n", d.Header(string(typ), string(to)), body)
}

// example is a header and body the agent could send word for word and have
// accepted: a type it may send, to an id it may address. The examples were
// once fixed (type=DELEGATE to=codex, shown to agents that may not delegate,
// in tasks with no "codex"), and a copied example was a refused message and
// a paid retry.
func example(t *domain.Task, self domain.AgentID) (domain.MessageType, domain.AgentID, string) {
	if t == nil {
		return domain.MsgAnswer, domain.HumanAgentID, "What I found, with files and lines, and what I recommend."
	}
	if !isAuthority(t, self) {
		return domain.MsgAnswer, authorityOf(t), "What I found, with files and lines, and what I recommend."
	}
	if others := addressable(t, self); len(others) > 0 {
		return domain.MsgDelegate, others[0],
			"Find the code this task concerns and report what you find, with files and lines. Do not change files yet."
	}
	return domain.MsgAnswer, domain.HumanAgentID, "Where the work stands, and what I need from you."
}

// typeGroups is the one table of what each message type is for, grouped by
// purpose. Every prompt's type list is drawn from it, filtered by
// domain.AllowedTypes, so both adapters say the same and an agent is offered
// only what it may send.
var typeGroups = []struct {
	purpose string
	types   []typeMeaning
}{
	{"asking", []typeMeaning{
		{domain.MsgQuestion, "you need information"},
		{domain.MsgDelegate, "investigate and report back"},
		{domain.MsgImplement, "change files"},
		{domain.MsgTaskAssignment, "hand over a whole part of the task"},
	}},
	{"reporting", []typeMeaning{
		{domain.MsgAnswer, "reply to a request"},
		{domain.MsgImplementationResult, "what you changed, where"},
		{domain.MsgTestResult, "command, exit code, output"},
		{domain.MsgStatusUpdate, "progress on longer work"},
		{domain.MsgInformation, "something others should know; asks nothing"},
	}},
	{"judging", []typeMeaning{
		{domain.MsgProposal, "a plan or change for others to weigh"},
		{domain.MsgReview, "your judgement, with verdict=approve|changes|reject"},
		{domain.MsgChallenge, "you disagree; give the evidence"},
	}},
	{"deciding", []typeMeaning{
		{domain.MsgRequestChange, "send work back, saying what must change"},
		{domain.MsgApprove, "accept a proposal or result"},
		{domain.MsgReject, "turn it down, saying why"},
		{domain.MsgFinalDecision, "the outcome; ends the task"},
	}},
}

type typeMeaning struct {
	typ     domain.MessageType
	meaning string
}

// WriteTypeGuide writes the message types an agent may send, one line per
// purpose, each with what it means.
func WriteTypeGuide(b *strings.Builder, authority bool) {
	allowed := map[domain.MessageType]bool{}
	for _, t := range domain.AllowedTypes(authority) {
		allowed[t] = true
	}
	for _, g := range typeGroups {
		var items []string
		for _, tm := range g.types {
			if allowed[tm.typ] {
				items = append(items, fmt.Sprintf("%s (%s)", tm.typ, tm.meaning))
			}
		}
		if len(items) > 0 {
			fmt.Fprintf(b, "  %-10s %s\n", g.purpose+":", strings.Join(items, ", "))
		}
	}
}
