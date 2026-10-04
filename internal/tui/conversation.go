package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"projectorch/internal/domain"
)

// The centre column.
//
// The conversation tab is a list: one compact entry per message (who, to
// whom, what kind, and its gist in one line of plain prose), plus the few
// events that change what the reader should do (a pause, a checkpoint, a
// request closed unanswered). The selected entry is shown in full, rendered
// as the markdown the agent wrote, in the inspector. Everything else the run
// produces -- raw CLI output, turn bookkeeping, agent start/stop -- lives in
// the activity and events tabs, where it does not bury the conversation.

// convItem is one conversation entry: a message, or a notable event.
type convItem struct {
	at  time.Time
	msg *domain.Message
	ev  *domain.Event
	// working is an agent busy on its turn that has not posted anything
	// from it yet: one "thinking" entry, at the time the turn began, that
	// its message replaces when it arrives.
	working domain.AgentID
}

// convItems builds the current task's conversation, oldest first, honouring
// the agent filter.
func (m Model) convItems() []convItem {
	var items []convItem
	for _, msg := range m.messages {
		if msg == nil {
			continue
		}
		if m.streamFilter != "" && msg.FromAgent != m.streamFilter && msg.ToAgent != m.streamFilter {
			continue
		}
		items = append(items, convItem{at: msg.Timestamp, msg: msg})
	}
	if m.streamFilter == "" {
		for i := range m.events {
			if convShowsEvent(m.events[i]) {
				items = append(items, convItem{at: m.events[i].Timestamp, ev: &m.events[i]})
			}
		}
	}
	for _, w := range m.workingItems() {
		if m.streamFilter == "" || m.streamFilter == w.working {
			items = append(items, w)
		}
	}
	// Stable insertion sort: both sources are already in order, so this is
	// a merge in practice.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].at.Before(items[j-1].at); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	return items
}

// convShowsEvent picks the events worth a line in the conversation: the ones
// that change what the human should do or know. Agent lifecycle, per-message
// bookkeeping and state changes between working phases are left to the
// events tab.
func convShowsEvent(e domain.Event) bool {
	switch e.Type {
	case domain.EvtTaskStateChanged:
		to := domain.TaskState(e.Details["to"])
		from := domain.TaskState(e.Details["from"])
		return to == domain.TaskPaused || to.IsTerminal() ||
			from == domain.TaskPaused || from == domain.TaskCompleted
	case domain.EvtCheckpoint, domain.EvtDecisionCaveat, domain.EvtRequestAbandoned,
		domain.EvtDecisionCreated, domain.EvtDecisionAccepted, domain.EvtDecisionRejected,
		domain.EvtAuthorityViolation, domain.EvtValidationRejected, domain.EvtAgentFailed:
		return true
	}
	return false
}

// convSelected is the index of the selected entry: the newest while
// following, otherwise the stored selection, clamped.
func (m Model) convSelected(items []convItem) int {
	n := len(items)
	if n == 0 {
		return -1
	}
	if m.follow {
		return newestEntry(items)
	}
	return clampInt(m.convSel, 0, n-1)
}

// newestEntry is the entry following selects: the newest message or event.
// A thinking entry is passed over, so the inspector keeps showing what was
// last said rather than jumping to whoever started a turn.
func newestEntry(items []convItem) int {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].working == "" {
			return i
		}
	}
	return len(items) - 1
}

// selectedConvItem returns the selected conversation entry.
func (m Model) selectedConvItem() (convItem, bool) {
	items := m.convItems()
	i := m.convSelected(items)
	if i < 0 {
		return convItem{}, false
	}
	return items[i], true
}

// convLines renders the conversation and reports, per line, the entry it
// belongs to.
func (m Model) convLines(items []convItem, inner int) ([]line, []int) {
	return m.convLinesRange(items, inner, 0, len(items))
}

// convOwners maps each conversation line to its entry without rendering
// anything: a message is a header plus its gist (your own messages show
// more of their text), an event one line. It lets the pane pick its window
// before paying to render only what is in it.
func convOwners(items []convItem, inner int) []int {
	if len(items) == 0 {
		return []int{-1}
	}
	owners := make([]int, 0, 2*len(items))
	for i, it := range items {
		owners = append(owners, i)
		if it.msg == nil {
			continue
		}
		n := 1
		switch {
		case humanAuthored(it.msg):
			n = len(youLines(it.msg, inner))
		case it.msg.Type == domain.MsgFinalDecision:
			n = len(forYouLines(it.msg, inner))
		case toYou(it.msg):
			n = len(forYouLines(it.msg, inner))
		}
		for k := 0; k < n; k++ {
			owners = append(owners, i)
		}
	}
	return owners
}

// maxYouLines is how much of one of your own messages the conversation shows
// before the inspector takes over.
const maxYouLines = 4

// humanAuthored reports whether msg is something the human wrote, as opposed
// to an orchestrator notice sent in the human's name (a checkpoint, a
// reminder, a malformed-reply note).
func humanAuthored(msg *domain.Message) bool {
	return msg.FromAgent == domain.HumanAgentID && msg.Metadata["orchestrator"] == "" && msg.Metadata["reason"] == ""
}

// toYou reports whether msg is an agent (or orch) speaking to the human.
func toYou(msg *domain.Message) bool {
	return msg.ToAgent == domain.HumanAgentID && msg.FromAgent != domain.HumanAgentID
}

// youLines is your message's text as the conversation shows it: wrapped, at
// most maxYouLines, the last one ending in an ellipsis when there is more.
func youLines(msg *domain.Message, inner int) []string {
	return shownLines(collapse(strings.TrimSpace(msg.Content)), inner)
}

// forYouLines is what an agent said to you, as plain prose (its markdown
// markers dropped), shown like your own messages.
func forYouLines(msg *domain.Message, inner int) []string {
	return shownLines(mdPlain(msg.Content), inner)
}

// shownLines wraps text to the conversation's width, at most maxYouLines,
// the last one ending in an ellipsis when there is more.
func shownLines(text string, inner int) []string {
	width := maxInt(4, inner-6)
	if text == "" {
		text = "(empty)"
	}
	lines := wrapWords(text, width)
	if len(lines) > maxYouLines {
		lines = lines[:maxYouLines]
		lines[maxYouLines-1] = fitEllipsis(lines[maxYouLines-1]+" "+gl().ellipsis, width)
	}
	return lines
}

// convLinesRange renders entries [from, to) of the conversation.
func (m Model) convLinesRange(items []convItem, inner, from, to int) ([]line, []int) {
	if len(items) == 0 {
		msg := "  (no messages yet)"
		if m.streamFilter != "" {
			msg = "  (nothing from or to " + string(m.streamFilter) + "; / then 'all' clears the filter)"
		}
		return []line{{st(msg, dimStyle)}}, []int{-1}
	}
	idents := m.identities()
	sel := m.convSelected(items)
	g := gl()

	var out []line
	var owners []int
	for i := from; i < to; i++ {
		it := items[i]
		marker := txt("  ")
		if i == sel {
			marker = st(g.caret+" ", sectionHeaderStyle)
		}
		if it.ev != nil {
			text, sty := convEventText(*it.ev)
			l := line{marker, st(it.at.Format("15:04:05")+" ", dimStyle), st(text, sty)}
			out = append(out, l)
			owners = append(owners, i)
			continue
		}
		if it.working != "" {
			out = append(out, line{marker, st(it.at.Format("15:04:05")+" ", dimStyle), agentCell(idents, it.working),
				txt("  "), st(spinnerGlyph(m.spinnerFrame)+" thinking"+g.ellipsis, thinkingStyle)})
			owners = append(owners, i)
			continue
		}
		msg := it.msg
		if humanAuthored(msg) {
			out, owners = m.youEntry(out, owners, msg, i, i == sel, inner)
			continue
		}
		if msg.Type == domain.MsgFinalDecision {
			out, owners = m.barredEntry(out, owners, msg, i, i == sel, inner, " FINAL DECISION ", decisionBadge, decisionBarStyle, decisionTextStyle)
			continue
		}
		if toYou(msg) {
			out, owners = m.forYouEntry(out, owners, msg, i, i == sel, inner)
			continue
		}
		head := line{marker, st(msg.Timestamp.Format("15:04:05")+" ", dimStyle),
			agentCell(idents, msg.FromAgent)}
		if msg.ToAgent != "" {
			head = head.join(st(" "+g.arrow+" ", dimStyle), agentCell(idents, msg.ToAgent))
		}
		typeSty := lipgloss.NewStyle().Foreground(messageTypeColor(msg.Type))
		if i == sel {
			typeSty = typeSty.Bold(true)
		}
		head = head.join(txt("  "), st(string(msg.Type), typeSty))
		if v, ok := domain.VerdictOf(msg); ok {
			head = head.join(st(" "+string(v), verdictStyle(v)))
		}
		out = append(out, head)
		owners = append(owners, i)

		summary := mdSummary(msg.Content)
		if summary == "" {
			summary = "(empty)"
		}
		sumSty := dimStyle
		if i == sel || msg.ToAgent == domain.HumanAgentID {
			// What was said to you, and what you are looking at, read
			// brighter than agent-to-agent traffic.
			sumSty = bodyStyle
		}
		out = append(out, line{txt("    "), st(fitEllipsis(summary, maxInt(4, inner-4)), sumSty)})
		owners = append(owners, i)
	}
	return out, owners
}

// youEntry renders one of the human's own messages: badged, barred down the
// left edge, and with its text shown rather than summarised, so what you
// asked is easy to find in a long run.
func (m Model) youEntry(out []line, owners []int, msg *domain.Message, i int, selected bool, inner int) ([]line, []int) {
	g := gl()
	bar := st(g.youBar+" ", youBarStyle)
	if selected {
		bar = st(g.caret+" ", sectionHeaderStyle)
	}
	head := line{bar, st(msg.Timestamp.Format("15:04:05")+" ", dimStyle), st(" YOU ", youBadgeStyle)}
	if msg.ToAgent != "" {
		head = head.join(st(" "+g.arrow+" ", dimStyle), agentCell(m.identities(), msg.ToAgent))
	}
	out = append(out, head)
	owners = append(owners, i)
	for _, l := range youLines(msg, inner) {
		out = append(out, line{st(g.youBar+"   ", youBarStyle), st(l, youTextStyle)})
		owners = append(owners, i)
	}
	return out, owners
}

// forYouEntry renders a message an agent sent you: an orange bar down the
// left edge and a YOU badge on the recipient, with more of its text shown
// than agent-to-agent traffic gets, so what you are told stands out.
func (m Model) forYouEntry(out []line, owners []int, msg *domain.Message, i int, selected bool, inner int) ([]line, []int) {
	return m.barredEntry(out, owners, msg, i, selected, inner, "", forYouBadge, forYouBarStyle, forYouTextStyle)
}

// barredEntry renders a message that should stand out: a coloured bar down
// the left edge, a badge (on the recipient when the message is to you, or in
// place of the type), and up to maxYouLines of its text as plain prose.
func (m Model) barredEntry(out []line, owners []int, msg *domain.Message, i int, selected bool, inner int,
	badge string, badgeSty, barSty, textSty lipgloss.Style) ([]line, []int) {
	g := gl()
	bar := st(g.youBar+" ", barSty)
	if selected {
		bar = st(g.caret+" ", sectionHeaderStyle)
	}
	typeSty := lipgloss.NewStyle().Foreground(messageTypeColor(msg.Type))
	if selected {
		typeSty = typeSty.Bold(true)
	}
	head := line{bar, st(msg.Timestamp.Format("15:04:05")+" ", dimStyle), agentCell(m.identities(), msg.FromAgent)}
	switch {
	case msg.ToAgent == domain.HumanAgentID:
		head = head.join(st(" "+g.arrow+" ", dimStyle), st(" YOU ", badgeSty))
	case msg.ToAgent != "":
		head = head.join(st(" "+g.arrow+" ", dimStyle), agentCell(m.identities(), msg.ToAgent))
	}
	if badge != "" {
		head = head.join(txt("  "), st(badge, badgeSty)) // the badge names the type
	} else {
		head = head.join(txt("  "), st(string(msg.Type), typeSty))
	}
	if v, ok := domain.VerdictOf(msg); ok {
		head = head.join(st(" "+string(v), verdictStyle(v)))
	}
	out = append(out, head)
	owners = append(owners, i)
	for _, l := range forYouLines(msg, inner) {
		out = append(out, line{st(g.youBar+"   ", barSty), st(l, textSty)})
		owners = append(owners, i)
	}
	return out, owners
}

func verdictStyle(v domain.Verdict) lipgloss.Style {
	switch v {
	case domain.VerdictApprove:
		return lipgloss.NewStyle().Foreground(colorGreen)
	case domain.VerdictReject:
		return lipgloss.NewStyle().Foreground(colorRed)
	}
	return warnStyle
}

// convEventText phrases a notable event for the conversation.
func convEventText(e domain.Event) (string, lipgloss.Style) {
	g := gl()
	switch e.Type {
	case domain.EvtTaskStateChanged:
		to := domain.TaskState(e.Details["to"])
		text := g.horiz + g.horiz + " " + string(to)
		if r := e.Details["reason"]; r != "" {
			text += " " + dot() + " " + collapse(r)
		}
		switch to {
		case domain.TaskPaused:
			return text, warnStyle
		case domain.TaskFailed, domain.TaskCancelled:
			return text, lipgloss.NewStyle().Foreground(colorRed)
		case domain.TaskCompleted:
			return text, lipgloss.NewStyle().Foreground(colorGreen)
		}
		return text, dimStyle
	}
	sty := warnStyle
	if isFailureEvent(e.Type) {
		sty = lipgloss.NewStyle().Foreground(colorRed)
	}
	e.Summary = mdPlain(e.Summary) // a decision's summary is the agent's markdown
	return eventPhrase(e), sty
}

// conversationBox draws the conversation list.
func (m Model) conversationBox(width, height int) []string {
	inner := width - 2
	items := m.convItems()
	rows := height - 2
	owners := convOwners(items, inner)
	off := m.convOffset(items, owners, rows)
	// Render only the entries the window touches.
	var visible []line
	if end := minInt(len(owners), off+rows); len(items) > 0 && off < end {
		first, last := owners[off], owners[end-1]
		lines, own := m.convLinesRange(items, inner, first, last+1)
		skip := 0
		for skip < len(own) && own[skip] == first && skip < off-indexOf(owners, first) {
			skip++
		}
		visible = lines[skip:]
	} else {
		visible, _ = m.convLines(items, inner)
	}
	right := itoa(len(m.messages)) + " msgs"
	if m.streamFilter != "" {
		right = "filter " + string(m.streamFilter)
	}
	return renderBox(boxSpec{
		title: "conversation", right: right, active: m.paneIsActive(paneConversation),
	}, topLines(visible, rows), width, height)
}

// indexOf is the first index of v in s, or -1.
func indexOf(s []int, v int) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// convOffset is the first conversation line to draw: the stored offset,
// moved just enough to keep the selected entry on screen.
func (m Model) convOffset(items []convItem, owners []int, rows int) int {
	sel := m.convSelected(items)
	first, last := -1, -1
	for i, o := range owners {
		if o == sel && sel >= 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if m.follow {
		// Following shows the newest entries, flush to the bottom.
		return maxInt(0, len(owners)-rows)
	}
	return followSelection(m.scrollOf(paneConversation), first, last, rows, len(owners))
}

// followSelection adjusts a top-anchored offset so rows first..last are
// visible, clamped to the content.
func followSelection(off, first, last, rows, total int) int {
	if first >= 0 {
		if last-first+1 > rows {
			last = first + rows - 1
		}
		if first < off {
			off = first
		}
		if last >= off+rows {
			off = last - rows + 1
		}
	}
	if max := total - rows; off > max {
		off = max
	}
	if off < 0 {
		off = 0
	}
	return off
}

// activityBox renders the agents' raw live output: thinking, text, tool
// calls, status. It is the "what are they doing right now" view.
func (m Model) activityBox(width, height int) []string {
	inner := width - 2
	scroll := m.scrollOf(paneConversation)
	if m.follow {
		scroll = 0
	}
	rows, total := m.streamTail(height + scroll)
	all := m.streamLines(rows, inner)
	right := itoa(total) + " lines"
	if !m.showThinking {
		right = "thinking hidden " + dot() + " " + right
	}
	return renderBox(boxSpec{
		title: "activity", right: right, active: m.paneIsActive(paneConversation),
	}, logBody(all, height-2, scroll, total-len(rows)), width, height)
}

// eventLines renders the current task's audit log.
func (m Model) eventLines(inner int) []line {
	idents := m.identities()
	var out []line
	for _, e := range m.events {
		sty := dimStyle
		switch {
		case isFailureEvent(e.Type):
			sty = lipgloss.NewStyle().Foreground(colorRed)
		case convShowsEvent(e):
			sty = warnStyle
		}
		who := txt("")
		if e.AgentID != "" {
			who = agentCell(idents, e.AgentID)
		}
		head := line{st(e.Timestamp.Format("15:04:05")+" ", dimStyle), who}
		if e.AgentID != "" {
			head = head.join(txt(" "))
		}
		body := eventPhrase(e)
		indent := head.width()
		for i, w := range wrapWords(body, maxInt(8, inner-indent)) {
			if i == 0 {
				out = append(out, append(append(line{}, head...), st(w, sty)))
				continue
			}
			out = append(out, line{txt(strings.Repeat(" ", indent)), st(w, sty)})
		}
	}
	if len(out) == 0 {
		out = []line{{st("  (no events for this task)", dimStyle)}}
	}
	return out
}

// eventsBox renders the audit log, read from the newest line up.
func (m Model) eventsBox(width, height int) []string {
	all := m.eventLines(width - 2)
	return renderBox(boxSpec{
		title: "events", right: itoa(len(m.events)), active: m.paneIsActive(paneConversation),
	}, logBody(all, height-2, m.scrollOf(paneConversation), 0), width, height)
}

// decisionSelected is the selected entry of the decisions tab.
func (m Model) decisionSelected() int {
	if len(m.decisions) == 0 {
		return -1
	}
	return clampInt(m.decisionSel, 0, len(m.decisions)-1)
}

// decisionListLines renders the decision record as a list.
func (m Model) decisionListLines(inner int) ([]line, []int) {
	if len(m.decisions) == 0 {
		return []line{{st("  (no decisions yet)", dimStyle)}}, []int{-1}
	}
	sel := m.decisionSelected()
	var out []line
	var owners []int
	for i, d := range m.decisions {
		if d == nil {
			continue
		}
		marker := txt("  ")
		if i == sel {
			marker = st(gl().caret+" ", sectionHeaderStyle)
		}
		out = append(out, line{marker, st(string(d.ID)+" ", emphStyle), decisionStatusCell(d.Status),
			st(" "+dot()+" "+string(d.Author)+" "+dot()+" "+d.Timestamp.Format("15:04"), dimStyle)})
		out = append(out, line{txt("    "), st(fitEllipsis(mdSummary(d.Decision), maxInt(4, inner-4)), bodyStyle)})
		owners = append(owners, i, i)
	}
	return out, owners
}

func decisionStatusCell(s domain.DecisionStatus) cell {
	switch s {
	case domain.DecisionAccepted:
		return st(string(s), lipgloss.NewStyle().Foreground(colorGreen))
	case domain.DecisionRejected:
		return st(string(s), lipgloss.NewStyle().Foreground(colorRed))
	case domain.DecisionProposed:
		return st(string(s), warnStyle)
	}
	return st(string(s), dimStyle)
}

// decisionsTabBox renders the decisions tab.
func (m Model) decisionsTabBox(width, height int) []string {
	all, owners := m.decisionListLines(width - 2)
	sel := m.decisionSelected()
	first, last := -1, -1
	for i, o := range owners {
		if o == sel && sel >= 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	off := followSelection(m.scrollOf(paneConversation), first, last, height-2, len(all))
	return renderBox(boxSpec{
		title: "decisions", right: itoa(len(m.decisions)), active: m.paneIsActive(paneConversation),
	}, topWindow(all, height-2, off), width, height)
}

// tabBarRow renders the centre column's tabs. Each tab is " name ", starting
// at the column's left edge -- tabAt relies on exactly that to map a click.
func (m Model) tabBarRow(width int) string {
	active := m.centerOrDefault()
	l := line{}
	for _, t := range m.centerTabs() {
		if t == active {
			l = l.join(st("["+t.String()+"]", tabActiveStyle))
		} else {
			l = l.join(st(" "+t.String()+" ", dimStyle))
		}
	}
	var notes []string
	if m.streamFilter != "" {
		notes = append(notes, "agent="+string(m.streamFilter))
	}
	switch active {
	case tabConversation, tabActivity:
		if m.follow {
			notes = append(notes, "following "+gl().working)
		} else {
			notes = append(notes, "f follow")
		}
	}
	notes = append(notes, "tab switch")
	// Hints give way to the tabs: drop them, most optional first, until the
	// row fits, rather than truncating a tab name.
	for len(notes) > 0 {
		right := line{st(strings.Join(notes, " "+dot()+" ")+" ", dimStyle)}
		if l.width()+right.width()+1 <= width {
			return spread(l, right, width).render(width)
		}
		notes = notes[:len(notes)-1]
	}
	return l.render(width)
}

// centerColumn draws the tab bar and the active tab, or the focused agent.
func (m Model) centerColumn(width, height int) []string {
	rows := []string{m.tabBarRow(width)}
	paneH := maxInt(3, height-1)
	if id, ok := m.focusedAgentID(); ok {
		return append(rows, m.focusedAgentBox(id, width, paneH)...)
	}
	switch m.centerOrDefault() {
	case tabActivity:
		return append(rows, m.activityBox(width, paneH)...)
	case tabEvents:
		return append(rows, m.eventsBox(width, paneH)...)
	case tabDecisions:
		return append(rows, m.decisionsTabBox(width, paneH)...)
	case tabTasks:
		return append(rows, m.tasksBox(width, paneH)...)
	case tabAgents:
		return append(rows, m.agentsBox(width, paneH)...)
	case tabInspector:
		return append(rows, m.inspectorBox(width, paneH)...)
	case tabTeam:
		return append(rows, m.teamBox(width, paneH)...)
	default:
		return append(rows, m.conversationBox(width, paneH)...)
	}
}

// focusedAgentBox renders one agent's live output over the centre column.
func (m Model) focusedAgentBox(id domain.AgentID, width, height int) []string {
	inner := width - 2
	a, ok := m.agentByID(id)
	if !ok {
		return renderBox(boxSpec{title: "focused", active: true},
			[]line{{st("(agent no longer registered -- press 0 or esc)", dimStyle)}}, width, height)
	}

	stateSty := lipgloss.NewStyle().Foreground(agentStateColor(a.State))
	head := line{
		st(stateGlyph(a.State), stateSty), txt(" "),
		st(string(a.ID), emphStyle), txt("  "),
		st(string(a.Role)+" "+gl().vert+" "+string(a.Kind), dimStyle),
	}
	body := []line{spread(head, line{st(string(a.State), stateSty)}, inner), blank()}

	live := agentIsLive(a.State)
	if buf, ok := m.activity[a.ID]; ok {
		wls := buf.wrappedLinesIn(inner-2, m.scopeFor(a.ID))
		if len(wls) > 0 && !live {
			body = append(body, line{st("  history: this agent is "+string(a.State), dimStyle)})
		}
		for _, wl := range wls {
			sty := activityStyle(wl.kind)
			if !live {
				sty = dimStyle
			}
			body = append(body, line{txt("  "), st(wl.text, sty)})
		}
	}
	if len(body) == 2 {
		body = append(body, line{st("  (no activity for this task yet)", dimStyle)})
	}

	visible := windowLines(body, height-2, m.focusScroll)
	return renderBox(boxSpec{
		title: "FOCUSED " + string(a.ID), right: "0/esc back", active: true,
	}, visible, width, height)
}

// logBody windows a tailed pane and reserves its last row for the "N more"
// marker when there is content off screen.
func logBody(all []line, height, scroll, hiddenAbove int) []line {
	if len(all) <= height && hiddenAbove == 0 {
		return windowLines(all, height, scroll)
	}
	body := windowLines(all, height-1, scroll)
	if hint, ok := hintRow(all, height-1, scroll, hiddenAbove); ok {
		return append(body, hint)
	}
	return append(body, blank())
}
