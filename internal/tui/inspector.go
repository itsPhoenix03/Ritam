package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The inspector shows, in full, whatever was last selected: a message (as the
// markdown its author wrote), an event, an agent, a task or a decision. The
// conversation stays one line per message because the full text is here.

// inspectorBox draws the inspector.
func (m Model) inspectorBox(width, height int) []string {
	inner := width - 2
	title, all := m.inspectorContent(inner)
	right := "v zoom"
	if m.zoom {
		right = "v/esc back"
	}
	return renderBox(boxSpec{
		title: title, right: right, active: m.paneIsActive(paneInspector) || m.zoom,
	}, topWindow(all, height-2, m.scrollOf(paneInspector)), width, height)
}

// inspectorContent is the inspector's title and lines for the current
// subject.
func (m Model) inspectorContent(inner int) (string, []line) {
	switch m.inspect {
	case inspectAgent:
		sel, ok := m.selectedIdent()
		if !ok {
			return "inspector", []line{{st("(no agents)", dimStyle)}}
		}
		return "agent " + string(sel.agent.ID), m.agentInspectorLines(sel, inner)
	case inspectTask:
		t := m.currentTask()
		if t == nil {
			return "inspector", []line{{st("(no task)", dimStyle)}}
		}
		return "task " + string(t.ID), m.taskInspectorLines(t, inner)
	case inspectDecision:
		d := m.inspectedDecision()
		if d == nil {
			return "inspector", []line{{st("(no decision)", dimStyle)}}
		}
		return "decision " + string(d.ID), m.decisionInspectorLines(d, inner)
	}
	it, ok := m.selectedConvItem()
	if !ok {
		return "inspector", []line{{st("select a message to read it here", dimStyle)}}
	}
	if it.ev != nil {
		return "event", eventInspectorLines(*it.ev, inner)
	}
	if it.working != "" {
		// What the agent is doing on its turn, live.
		if a, ok := identFor(m.identities(), it.working); ok {
			return "agent " + string(it.working), m.agentInspectorLines(a, inner)
		}
		return "agent " + string(it.working), []line{{st(string(it.working)+" is thinking", dimStyle)}}
	}
	return string(it.msg.Type), m.messageInspectorLines(it.msg, inner)
}

// inspectedDecision is the decision selected in the decisions tab, or else
// the newest one.
func (m Model) inspectedDecision() *domain.Decision {
	if m.centerOrDefault() == tabDecisions {
		if i := m.decisionSelected(); i >= 0 {
			return m.decisions[i]
		}
	}
	return m.latestDecision()
}

// inspectKey identifies what the inspector is showing, so its scroll can be
// reset when that changes.
func (m Model) inspectKey() string {
	switch m.inspect {
	case inspectAgent:
		if sel, ok := m.selectedIdent(); ok {
			return "a:" + string(sel.agent.ID)
		}
	case inspectTask:
		return "t:" + string(m.currentTaskID)
	case inspectDecision:
		if d := m.inspectedDecision(); d != nil {
			return "d:" + string(d.ID)
		}
	default:
		if it, ok := m.selectedConvItem(); ok {
			switch {
			case it.msg != nil:
				return "m:" + string(it.msg.ID)
			case it.working != "":
				return "w:" + string(it.working)
			}
			return "e:" + string(it.ev.ID)
		}
	}
	return ""
}

func (m Model) messageInspectorLines(msg *domain.Message, inner int) []line {
	idents := m.identities()
	g := gl()
	head := line{agentCell(idents, msg.FromAgent)}
	if msg.ToAgent != "" {
		head = head.join(st(" "+g.arrow+" ", dimStyle), agentCell(idents, msg.ToAgent))
	}
	head = head.join(txt("  "), st(string(msg.Type), lipgloss.NewStyle().Foreground(messageTypeColor(msg.Type)).Bold(true)))
	if v, ok := domain.VerdictOf(msg); ok {
		head = head.join(st(" "+string(v), verdictStyle(v)))
	}
	meta := msg.Timestamp.Format("15:04:05")
	if msg.Round > 0 {
		meta += " " + dot() + " round " + itoa(msg.Round)
	}
	out := []line{head, {st(meta, dimStyle)}}
	if msg.InReplyTo != "" {
		for _, other := range m.messages {
			if other != nil && other.ID == msg.InReplyTo {
				out = append(out, line{st("re: ", dimStyle), agentCell(idents, other.FromAgent),
					st(" "+string(other.Type)+" "+dot()+" "+fitEllipsis(mdSummary(other.Content), maxInt(8, inner-24)), dimStyle)})
				break
			}
		}
	}
	out = append(out, line{st(strings.Repeat(g.horiz, inner), dimStyle)})
	return append(out, renderMarkdown(msg.Content, inner)...)
}

func eventInspectorLines(e domain.Event, inner int) []line {
	out := []line{
		{st(decamel(string(e.Type)), emphStyle)},
		{st(e.Timestamp.Format("15:04:05"), dimStyle)},
	}
	if e.AgentID != "" {
		out = append(out, line{st("agent ", dimStyle), st(string(e.AgentID), bodyStyle)})
	}
	out = append(out, blank())
	for _, l := range flowLines(e.Summary, inner) {
		out = append(out, line{st(l, bodyStyle)})
	}
	if len(e.Details) > 0 {
		keys := make([]string, 0, len(e.Details))
		for k := range e.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out = append(out, blank())
		for _, k := range keys {
			for i, l := range flowLines(e.Details[k], maxInt(8, inner-len(k)-2)) {
				label := k + ": "
				if i > 0 {
					label = strings.Repeat(" ", len(label))
				}
				out = append(out, line{st(label, dimStyle), st(l, bodyStyle)})
			}
		}
	}
	return out
}

func (m Model) decisionInspectorLines(d *domain.Decision, inner int) []line {
	idents := m.identities()
	out := []line{
		{decisionStatusCell(d.Status), st(" "+dot()+" by ", dimStyle), agentCell(idents, d.Author),
			st(" "+dot()+" "+d.Timestamp.Format("15:04:05"), dimStyle)},
	}
	if d.Supersedes != "" {
		out = append(out, line{st("supersedes "+string(d.Supersedes), dimStyle)})
	}
	if d.SupersededBy != "" {
		out = append(out, line{st("superseded by "+string(d.SupersededBy), dimStyle)})
	}
	if _, pending := m.pendingDecision(); pending && d.Status == domain.DecisionProposed {
		out = append(out, blank(), line{st(" a approve ", badgeOKStyle), txt(" "), st(" x reject ", badgeErrStyle),
			txt(" "), st(" i amend & instruct ", badgeQuietStyle)},
			line{st("the run holds here until you answer", dimStyle)})
	}
	out = append(out, line{st(strings.Repeat(gl().horiz, inner), dimStyle)})
	out = append(out, renderMarkdown(d.Decision, inner)...)
	if d.Reasoning != "" {
		out = append(out, blank(), line{st("REASONING", sectionHeaderStyle)})
		out = append(out, renderMarkdown(d.Reasoning, inner)...)
	}
	if len(d.Evidence) > 0 {
		out = append(out, blank(), line{st("EVIDENCE", sectionHeaderStyle)})
		for _, e := range d.Evidence {
			out = append(out, renderMarkdown("- "+e, inner)...)
		}
	}
	if len(d.Participants) > 0 {
		l := line{st("participants ", dimStyle)}
		for i, id := range d.Participants {
			if i > 0 {
				l = l.join(st(" "+dot()+" ", dimStyle))
			}
			l = l.join(agentCell(idents, id))
		}
		out = append(out, blank(), l)
	}
	return out
}

func (m Model) taskInspectorLines(t *domain.Task, inner int) []line {
	idents := m.identities()
	out := []line{
		{st(t.Title, emphStyle)},
		{taskStateBadge(t.State), st(" "+dot()+" authority ", dimStyle), agentCell(idents, t.AuthorityAgent)},
	}
	if t.PauseReason != "" && t.State == domain.TaskPaused {
		for _, l := range wrapWords(t.PauseReason, inner) {
			out = append(out, line{st(l, warnStyle)})
		}
	}
	if t.FailureReason != "" && t.State == domain.TaskFailed {
		for _, l := range wrapWords(t.FailureReason, inner) {
			out = append(out, line{st(l, lipgloss.NewStyle().Foreground(colorRed))})
		}
	}
	parts := line{st("participants ", dimStyle)}
	for i, id := range t.Participants {
		if i > 0 {
			parts = parts.join(st(" "+dot()+" ", dimStyle))
		}
		parts = parts.join(agentCell(idents, id))
	}
	out = append(out, parts)
	if t.Description != "" {
		out = append(out, line{st(strings.Repeat(gl().horiz, inner), dimStyle)})
		out = append(out, renderMarkdown(t.Description, inner)...)
	}

	out = append(out, blank(), line{st("PROGRESS", sectionHeaderStyle)},
		line{st(fmt.Sprintf("round %d %s %d messages %s created %s", t.Round, dot(), t.MessageCount, dot(),
			t.CreatedAt.Format("Jan 2 15:04")), bodyStyle)},
		line{st(fmt.Sprintf("since your last input: %d rounds %s %d messages", t.AgentRounds, dot(), t.AgentMessages), dimStyle)})
	if n := t.Limits.MaxDeliberationRounds; n > 0 {
		since := t.AgentRounds - t.LastCheckpoint
		out = append(out, line{st("next checkpoint ", dimStyle), st(meter(since, n, 8)+" "+itoa(since)+"/"+itoa(n), bodyStyle)})
	}
	if t.StallCount > 0 {
		msg := fmt.Sprintf("no progress for %d message(s)", t.StallCount)
		if s := t.Limits.StallMessages; s > 0 {
			msg += fmt.Sprintf("; pauses at %d", s)
		}
		out = append(out, line{st(msg, warnStyle)})
	}

	out = append(out, blank(), line{st("USAGE", sectionHeaderStyle)})
	tok, cost := unavailable, unavailable
	if n := t.InputTokens + t.OutputTokens; n > 0 {
		tok = fmt.Sprintf("%s in %s %s out", compactCount(t.InputTokens), dot(), compactCount(t.OutputTokens))
	}
	if t.CostUSD > 0 {
		cost = fmt.Sprintf("$%.2f", t.CostUSD)
	}
	budget := ""
	if t.Limits.MaxCostUSD > 0 {
		budget = fmt.Sprintf(" of $%.2f", t.Limits.MaxCostUSD)
	}
	out = append(out, line{st("tokens ", dimStyle), st(tok, bodyStyle)},
		line{st("cost ", dimStyle), st(cost+budget, bodyStyle)})
	for _, id := range t.Participants {
		if u, ok := t.AgentUsage[id]; ok {
			out = append(out, line{agentCell(idents, id), st(" "+agentUsageText(u), dimStyle)})
		}
	}

	if req := m.requestLines(inner); len(req) > 0 {
		out = append(out, req...)
	}
	return out
}

// agentUsageText is one agent's spend on a task, e.g.
// "12 turns · $1.30 · 66k in · 34k out · 518k cache".
// Cache reads are the session's history re-read every turn: a figure growing
// much faster than the others is a session getting long.
func agentUsageText(u domain.AgentUsage) string {
	turns := "turns"
	if u.Turns == 1 {
		turns = "turn"
	}
	parts := []string{fmt.Sprintf("%d %s", u.Turns, turns)}
	if u.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", u.CostUSD))
	}
	parts = append(parts, compactCount(u.InputTokens)+" in", compactCount(u.OutputTokens)+" out")
	if u.CacheReadTokens > 0 {
		parts = append(parts, compactCount(u.CacheReadTokens)+" cache")
	}
	return strings.Join(parts, " "+dot()+" ")
}

// agentInspectorLines is everything known about one agent.
func (m Model) agentInspectorLines(sel agentIdent, inner int) []line {
	a := sel.agent
	stateSty := lipgloss.NewStyle().Foreground(agentStateColor(a.State))
	buf := m.activity[a.ID]

	body := []line{
		{st("state ", dimStyle), st(string(a.State), stateSty),
			st(" "+dot()+" role ", dimStyle), st(string(a.Role), bodyStyle)},
		{st("cli ", dimStyle), st(string(a.Kind), bodyStyle)},
	}
	chip := st(" READ-ONLY ", badgeQuietStyle)
	if a.Role.CanImplement() {
		chip = st(" MAY IMPLEMENT ", badgeWarnStyle)
	}
	body = append(body, line{chip, st(" enforced by the cli sandbox", dimStyle)})
	if a.Kind == domain.AgentKindMock {
		for _, l := range wrapWords(mockBannerText, inner) {
			body = append(body, line{st(l, mockBannerStyle)})
		}
	}

	if a.LastError != "" {
		errSty, head := lipgloss.NewStyle().Foreground(colorRed), "last error"
		if a.State != domain.AgentStateError {
			errSty, head = dimStyle, "last error (history)"
		}
		body = append(body, blank(), line{st(head, dimStyle)})
		for _, l := range flowLines(a.LastError, inner) {
			body = append(body, line{st(l, errSty)})
		}
	}

	live := agentIsLive(a.State)
	sc := m.scopeFor(a.ID)
	head := "last activity"
	if live {
		head = "now"
	}
	body = append(body, blank(), line{st(head, dimStyle)})
	if e, ok := buf.latestIn(sc); ok {
		textSty := bodyStyle
		if !live {
			textSty = dimStyle
		}
		for _, l := range wrapWords(collapse(e.text), inner) {
			body = append(body, line{st(l, textSty)})
		}
		if !e.last.IsZero() {
			body = append(body, line{st(shortDur(time.Since(e.last))+" ago", dimStyle)})
		}
	} else {
		body = append(body, line{st("(nothing yet)", dimStyle)})
	}
	body = append(body, line{st(fmt.Sprintf("tools %d %s text %d %s thinking %d  (activity tab: everything)",
		buf.countKindIn(ports.ActivityTool, sc), dot(), buf.countKindIn(ports.ActivityText, sc), dot(),
		buf.countKindIn(ports.ActivityThinking, sc)), dimStyle)})

	sent := 0
	for _, msg := range m.messages {
		if msg != nil && msg.FromAgent == a.ID {
			sent++
		}
	}
	body = append(body, line{st(fmt.Sprintf("%d messages sent this task", sent), dimStyle)})
	if t := m.currentTask(); t != nil {
		if u, ok := t.AgentUsage[a.ID]; ok {
			body = append(body, line{st("spent this task ", dimStyle), st(agentUsageText(u), bodyStyle)})
		}
	}

	waiting, owes := m.agentRequests(a.ID)
	if len(waiting)+len(owes) > 0 {
		body = append(body, blank())
	}
	for _, r := range waiting {
		body = append(body, line{st("waiting on ", dimStyle), st(requestLabel(r.Message.ToAgent)+" ", bodyStyle),
			st(fmt.Sprintf("%s %s", r.Message.Type, shortDur(r.Age(time.Now()))), dimStyle)})
	}
	for _, r := range owes {
		body = append(body, line{st("owes ", dimStyle), st(requestLabel(r.Message.FromAgent)+" ", bodyStyle),
			st(fmt.Sprintf("a reply to %s %s", r.Message.Type, shortDur(r.Age(time.Now()))), dimStyle)})
	}
	return body
}
