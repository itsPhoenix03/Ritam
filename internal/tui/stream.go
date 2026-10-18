package tui

import (
	"sort"
	"strings"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The activity stream: each agent's live CLI output (thinking, text, tool
// calls, status), merged into one chronological view. It is ephemeral --
// never persisted -- and is the "what are they doing right now" tab; the
// routed messages themselves are the conversation.

// streamSourceCap bounds how much of each source the merge considers. The
// pane only ever shows the tail, so older entries cost sort time and buy
// nothing.
const streamSourceCap = 400

// streamKind classifies a stream row for styling and filtering.
type streamKind int

const (
	streamMessage streamKind = iota
	streamEvent
	streamThinking
	streamText
	streamTool
	streamStatus
	streamError
	// streamWarn marks something that went through but deserves a second
	// look, such as a decision taken with a request still unanswered.
	streamWarn
)

// streamRow is one line of the merged stream.
type streamRow struct {
	at    time.Time
	agent domain.AgentID
	kind  streamKind
	text  string
}

// streamRef is a stream row before its text has been built: enough to sort and
// count it, without paying for the string work until it is known to be shown.
type streamRef struct {
	at    time.Time
	src   streamSource
	idx   int
	agent domain.AgentID
}

type streamSource uint8

const srcActivity streamSource = 0

// mergedStream builds the whole chronological stream for the current task,
// honouring the active agent filter and the thinking toggle.
func (m Model) mergedStream() []streamRow {
	rows, _ := m.streamTail(0)
	return rows
}

// streamTail builds the newest `limit` rows of the stream (limit <= 0 means
// all of them) and reports how many rows the stream holds in total.
//
// The pane draws a window at the bottom of the stream, so everything older is
// dead weight -- and this runs on every message, at ~12 per second per streaming
// agent. Rows are therefore selected as cheap refs first (filter, count, keep the
// newest `limit` of each source) and only those survivors get their text built
// and wrapped. Correctness of the cut: the newest `limit` rows of the union are
// always among the newest `limit` of their own source.
//
// Scoping: messages come per task from the service; events and live activity
// are global feeds and are filtered to the current task here (see taskScope).
func (m Model) streamTail(limit int) (rows []streamRow, total int) {
	var refs []streamRef
	mark := len(refs)
	// closeSource applies the per-source tail cut once a source's refs are in.
	closeSource := func() {
		n := len(refs) - mark
		total += n
		if limit > 0 && n > limit {
			copy(refs[mark:], refs[len(refs)-limit:])
			refs = refs[:mark+limit]
		}
		mark = len(refs)
	}

	for _, a := range m.agents {
		buf := m.activity[a.ID]
		if buf == nil || (m.streamFilter != "" && a.ID != m.streamFilter) {
			continue
		}
		sc := m.scopeFor(a.ID)
		entries := buf.entries
		if len(entries) > streamSourceCap {
			entries = entries[len(entries)-streamSourceCap:]
		}
		for i, e := range entries {
			if (e.kind == ports.ActivityThinking && !m.showThinking) || !sc.has(e.task) ||
				strings.TrimSpace(e.text) == "" {
				continue
			}
			refs = append(refs, streamRef{at: e.at, src: srcActivity, idx: i, agent: a.ID})
		}
		closeSource()
	}
	// Activity for an id that is no longer in m.agents cannot be reached above;
	// applyAgentActivity already refuses to record it, so nothing is lost.

	sort.SliceStable(refs, func(i, j int) bool { return refs[i].at.Before(refs[j].at) })
	if limit > 0 && len(refs) > limit {
		refs = refs[len(refs)-limit:]
	}

	rows = make([]streamRow, 0, len(refs))
	for _, r := range refs {
		switch r.src {
		default:
			buf := m.activity[r.agent]
			entries := buf.entries
			if len(entries) > streamSourceCap {
				entries = entries[len(entries)-streamSourceCap:]
			}
			e := entries[r.idx]
			rows = append(rows, streamRow{
				at: e.at, agent: r.agent, kind: activityStreamKind(e.kind), text: collapse(e.text),
			})
		}
	}
	return rows, total
}

// streamShowsEvent reports whether an orchestrator event earns a stream row.
//
// Message-sent/received events are dropped because the messages themselves are
// already merged in above; showing both would double-report every exchange and
// make a two-turn conversation look like four.
func streamShowsEvent(e domain.Event) bool {
	switch e.Type {
	case domain.EvtAgentActivity, domain.EvtAgentMessageSent, domain.EvtAgentMessageReceived:
		return false
	}
	return true
}

// isFailureEvent reports whether an event type represents something going
// wrong, so the stream can colour it as such.
func isFailureEvent(t domain.EventType) bool {
	switch t {
	case domain.EvtTaskFailed, domain.EvtAgentFailed, domain.EvtAuthorityViolation,
		domain.EvtValidationRejected, domain.EvtDecisionRejected:
		return true
	}
	return false
}

// eventPhrase renders an event as a short stream line. The event type is kept
// (it is the vocabulary the logs and the docs use) but lower-cased and
// de-camelled so it reads as prose next to agent output rather than shouting.
func eventPhrase(e domain.Event) string {
	phrase := decamel(string(e.Type))
	if s := collapse(e.Summary); s != "" {
		return phrase + " " + dot() + " " + s
	}
	return phrase
}

// decamel turns "TaskStateChanged" into "task state changed".
func decamel(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

// activityStreamKind maps a live-activity kind onto a stream kind.
func activityStreamKind(k ports.ActivityKind) streamKind {
	switch k {
	case ports.ActivityThinking:
		return streamThinking
	case ports.ActivityTool:
		return streamTool
	case ports.ActivityStatus:
		return streamStatus
	default:
		return streamText
	}
}

// firstLine returns the first non-empty line of a multi-line payload. Agent
// content routinely runs to paragraphs; the stream shows the opening line and
// the messages tab shows the rest.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// streamLines formats stream rows as "time · agent · text", wrapping the text
// under a hanging indent so a long line stays attributable to its agent.
func (m Model) streamLines(rows []streamRow, inner int) []line {
	idents := m.identities()
	const tsWidth = 9

	// Size the agent gutter to the widest label actually present rather than
	// to a guess: truncating "[C] opus" to "[C] opu" costs a column of
	// nothing and makes two agents look like one.
	agentWidth := 0
	for _, a := range idents {
		if w := len(a.sigil) + 1 + len(string(a.agent.ID)) + 1; w > agentWidth {
			agentWidth = w
		}
	}
	agentWidth = clampInt(agentWidth, 8, 18)
	if inner-tsWidth-agentWidth < 24 {
		agentWidth = 8
	}
	indent := tsWidth + agentWidth
	body := inner - indent
	if body < 12 {
		// Too narrow for a gutter: drop the agent column and prefix inline.
		agentWidth, indent, body = 0, tsWidth, inner-tsWidth
		if body < 8 {
			body = 8
		}
	}

	var out []line
	for _, r := range rows {
		head := line{st(fitWidth(r.at.Format("15:04:05")+" ", tsWidth), dimStyle)}
		if agentWidth > 0 {
			name := string(r.agent)
			sty := dimStyle
			if id, ok := identFor(idents, r.agent); ok {
				name = id.sigil + " " + string(r.agent)
				sty = id.style()
			}
			head = head.join(st(fitWidth(name, agentWidth-1)+" ", sty))
		}

		wrapped := wrapWords(r.text, body)
		for i, w := range wrapped {
			if i == 0 {
				out = append(out, append(append(line{}, head...), st(w, streamStyle(r.kind))))
				continue
			}
			out = append(out, line{txt(strings.Repeat(" ", indent)), st(w, streamStyle(r.kind))})
		}
	}
	if len(out) == 0 {
		return []line{{st("  (no activity yet)", dimStyle)}}
	}
	return out
}

func containsAgent(list []domain.AgentID, id domain.AgentID) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}
