package tui

import (
	"fmt"
	"strings"
	"time"

	"ritam/internal/domain"
)

// The screen: header, body, command bar, help bar. The body's regions come
// from geometry (layout.go); every pane is drawn to exactly its rectangle,
// because columns are zipped row by row and one row of the wrong width shears
// the whole screen.

// minWidth/minHeight are the floors View() clamps to, so rendering never
// divides by zero before the first WindowSizeMsg arrives.
const (
	minWidth  = 40
	minHeight = 10
)

// Fixed row budgets for the chrome around the body.
const (
	promptHeight  = 3
	helpHeight    = 1
	minBodyHeight = 6
)

// View satisfies tea.Model.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	m.frameIdents = m.computeIdentities()
	g := m.geometry()
	m.frameGeom = &g

	width, height := m.screenSize()
	headH, bodyH, promptH, helpH := m.chromeBudget(height)

	rows := make([]string, 0, height)
	rows = append(rows, m.headerRows(width, headH)...)
	rows = append(rows, m.bodyRows(width, bodyH)...)
	rows = append(rows, m.commandRows(width, promptH)...)
	if helpH > 0 {
		rows = append(rows, m.helpBarRow(width))
	}
	return strings.Join(rows, "\n")
}

// screenSize is the terminal size View() lays out for, clamped to the floors.
func (m Model) screenSize() (width, height int) {
	return maxInt(m.width, minWidth), maxInt(m.height, minHeight)
}

// headerHeight is the header box: one status row, plus one more when
// something needs the human (a decision to approve, a pause to resume).
func (m Model) headerHeight() int {
	if _, ok := m.attention(); ok {
		return 4
	}
	return 3
}

// chromeBudget divides the rows between the chrome and the body, sacrificing
// chrome before content.
func (m Model) chromeBudget(height int) (headH, bodyH, promptH, helpH int) {
	if m.showHelpBar {
		helpH = helpHeight
	}
	headH, promptH = m.headerHeight(), m.inputBoxHeight()+m.commandMenuHeight()
	bodyH = height - headH - promptH - helpH
	if bodyH < minBodyHeight && headH > 3 {
		headH = 3
		bodyH = height - headH - promptH - helpH
	}
	if bodyH < minBodyHeight && helpH > 0 {
		helpH = 0
		bodyH = height - headH - promptH
	}
	if bodyH < minBodyHeight {
		promptH = 1
		bodyH = height - headH - promptH - helpH
	}
	if bodyH < minBodyHeight {
		headH = 1
		bodyH = height - headH - promptH - helpH
	}
	if bodyH < 1 {
		bodyH = 1
	}
	return headH, bodyH, promptH, helpH
}

// ---------------------------------------------------------------------------
// Header
// ---------------------------------------------------------------------------

func (m Model) headerRows(width, height int) []string {
	if height <= 1 {
		return []string{m.headerLine(width).render(width)}
	}
	body := []line{m.headerLine(width - 2)}
	if a, ok := m.attention(); ok && height >= 4 {
		body = append(body, a)
	}
	return renderBox(boxSpec{active: true}, body, width, height)
}

// headerLine is the task's status row.
func (m Model) headerLine(inner int) line {
	sep := st(" "+gl().vert+" ", dimStyle)
	left := line{st(" ORCH ", titleStyle)}
	t := m.currentTask()
	if t == nil {
		return left.join(sep, st("no task", dimStyle), sep, st("press n to create one", dimStyle))
	}
	left = left.join(sep, st(string(t.ID), emphStyle), txt(" "), st(t.Title, bodyStyle))

	right := line{taskStateBadge(t.State)}
	if d, ok := m.elapsed(); ok {
		right = right.join(sep, st(clock(d), bodyStyle))
	}
	// Usage is what the CLIs reported; "--" until they have, because 0
	// would read as "free".
	tok, cost := unavailable, unavailable
	if n := t.InputTokens + t.OutputTokens; n > 0 {
		tok = compactCount(n)
	}
	if t.CostUSD > 0 {
		cost = fmt.Sprintf("%.2f", t.CostUSD)
	}
	right = right.join(sep, st(itoa(t.MessageCount)+" msgs", dimStyle), sep,
		st(tok+" tok", warnStyle), sep, st("$"+cost, warnStyle), txt(" "))

	// The right-hand group wins: a truncated title costs nothing the task
	// list does not also show; a truncated state is the header's one job.
	avail := maxInt(12, inner-right.width())
	return line{raw(left.render(avail), avail)}.join(right...)
}

// attention is the header's second row: the one thing the human should act
// on, if any, with the key that does it.
func (m Model) attention() (line, bool) {
	t := m.currentTask()
	if t == nil {
		if n := m.opts.WorkspaceNotice; n != "" {
			return line{st(" WORKSPACE ", badgeWarnStyle), st(" "+n, warnStyle)}, true
		}
		if l, ok := m.noticeLine(); ok {
			return l, true
		}
		return nil, false
	}
	key := func(k, label string) []cell {
		return []cell{st(" "+k+" ", badgeQuietStyle), st(" "+label+"  ", dimStyle)}
	}
	if d, ok := m.pendingDecision(); ok {
		l := line{st(" DECISION "+string(d.ID)+" AWAITS YOUR APPROVAL ", badgeWarnStyle), txt("  ")}
		l = append(l, key("a", "approve")...)
		l = append(l, key("x", "reject")...)
		l = append(l, key("i", "amend & instruct")...)
		return l, true
	}
	switch t.State {
	case domain.TaskPaused:
		l := line{st(" PAUSED ", badgeWarnStyle), st(" "+collapse(t.PauseReason)+"  ", warnStyle)}
		l = append(l, key("r", "resume")...)
		l = append(l, key("i", "instruct")...)
		return l, true
	case domain.TaskFailed:
		return line{st(" FAILED ", badgeErrStyle), st(" "+collapse(t.FailureReason), errorStyle)}, true
	}
	if qs := m.openQuestionsToAgents(); qs != "" {
		return line{st("waiting on ", dimStyle), st(qs, emphStyle), st(" to answer you", dimStyle)}, true
	}
	if m.allAgentsAreMocks() {
		return line{st(" MOCK AGENTS ", badgeWarnStyle),
			st(" replies are a fixed script; restart with -real to use the CLIs", mockBannerStyle)}, true
	}
	if n := m.opts.WorkspaceNotice; n != "" {
		return line{st(" WORKSPACE ", badgeWarnStyle), st(" "+n, warnStyle)}, true
	}
	return m.noticeLine()
}

// noticeLine shows the start-up notices, the first in full.
func (m Model) noticeLine() (line, bool) {
	ns := m.opts.Notices
	if len(ns) == 0 {
		return nil, false
	}
	text := ns[0]
	if len(ns) > 1 {
		text += fmt.Sprintf("  (+%d more in the log)", len(ns)-1)
	}
	return line{st(" NOTICE ", badgeWarnStyle), st(" "+collapse(text), warnStyle)}, true
}

// openQuestionsToAgents names the agents that owe the human an answer.
func (m Model) openQuestionsToAgents() string {
	var names []string
	seen := map[domain.AgentID]bool{}
	for _, r := range domain.OpenRequests(m.ledger()) {
		if r.Message.FromAgent == domain.HumanAgentID && !seen[r.Message.ToAgent] {
			seen[r.Message.ToAgent] = true
			names = append(names, string(r.Message.ToAgent))
		}
	}
	return strings.Join(names, ", ")
}

// unavailable marks a metric nothing has reported yet.
const unavailable = "--"

// compactCount renders a count the way a header has room for: 950, 12.3k, 4.1M.
func compactCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprint(n)
	}
}

// ---------------------------------------------------------------------------
// Body
// ---------------------------------------------------------------------------

func (m Model) bodyRows(width, height int) []string {
	switch {
	case m.mode == modeCreateForm:
		// The form can be taller than the screen (a long model list); keep
		// the focused field and its selected row in view.
		lines := m.createFormLines(width - 2)
		first, last := -1, -1
		for i, l := range lines {
			if strings.Contains(l.plain(), gl().caret+" ") {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		if last-first+1 > height-2 {
			first = last // too far apart: the selected row wins over the heading
		}
		off := followSelection(0, first, last, height-2, len(lines))
		return renderBox(boxSpec{title: "new task", active: true},
			topWindow(lines, height-2, off), width, height)
	case m.mode == modeHelp:
		all := m.helpLines()
		right := "any key closes"
		if len(all) > height-2 {
			right = "j/k scroll " + dot() + " any other key closes"
		}
		return renderBox(boxSpec{title: "help", right: right, active: true},
			topWindow(all, height-2, m.scrollOf(paneConversation)), width, height)
	case m.zoom:
		return m.inspectorBox(width, height)
	}

	g := m.geometry()
	var cols []column
	if g.tasks.w > 0 {
		cols = append(cols, column{width: g.tasks.w, rows: m.tasksBox(g.tasks.w, g.tasks.h)})
	}
	cols = append(cols, column{width: g.center.w, rows: m.centerColumn(g.center.w, g.center.h)})
	if g.agents.w > 0 {
		right := m.agentsBox(g.agents.w, g.agents.h)
		if g.decision.h > 0 {
			right = append(right, m.decisionBox(g.decision.w, g.decision.h)...)
		}
		right = append(right, m.inspectorBox(g.inspector.w, g.inspector.h)...)
		cols = append(cols, column{width: g.agents.w, rows: right})
	}
	return joinColumns(height, cols...)
}

// ---------------------------------------------------------------------------
// Command bar and help bar
// ---------------------------------------------------------------------------

// commandRows renders the bottom input strip: the ":" command line, the
// instruction prompt, or the status line -- one surface, so the screen does
// not reflow when you start typing.
func (m Model) commandRows(width, height int) []string {
	inner := width - 2
	g := gl()

	var body []line
	var spec boxSpec
	switch m.mode {
	case modeCommand:
		spec = boxSpec{active: true, right: keyLegend("enter run", "tab complete", "esc cancel", inputHelp())}
		body = m.inputBodyLines(inner)
	case modePrompt:
		spec = boxSpec{active: true, right: keyLegend("tab recipient", "enter send", "esc cancel", inputHelp())}
		body = m.inputBodyLines(inner)
	default:
		status := m.statusLine
		if status == "" {
			status = "ready"
		}
		sty := dimStyle
		if m.err != nil {
			sty = errorStyle
		}
		spec = boxSpec{right: ": command, i instruct, / filter"}
		body = []line{{st(g.caret+" ", dimStyle), st(status, sty)}}
	}

	if height <= 1 {
		return []string{body[0].render(width)}
	}

	// The completion menu sits above the box and owns its own rows.
	menu := m.commandMenuRows(width)
	if n := height - m.inputBoxHeight(); len(menu) > n {
		if n < 0 {
			n = 0
		}
		menu = menu[len(menu)-n:]
	}
	return append(menu, renderBox(spec, body, width, height-len(menu))...)
}

type keyHint struct{ key, label string }

// helpBarRow renders the key legend for what is on screen, plus a live run
// summary.
func (m Model) helpBarRow(width int) string {
	keys := []keyHint{{"tab", "view"}, {"]", "pane"}, {"j/k", "move"}, {"enter", "open"}, {"v", "zoom"}}
	if _, pending := m.pendingDecision(); pending {
		keys = append(keys, keyHint{"a", "approve"}, keyHint{"x", "reject"})
	}
	if t := m.currentTask(); t != nil && t.State == domain.TaskPaused {
		keys = append(keys, keyHint{"r", "resume"})
	} else {
		keys = append(keys, keyHint{"p", "pause"})
	}
	keys = append(keys, keyHint{"i", "instruct"}, keyHint{"n", "new"}, keyHint{"/", "filter"},
		keyHint{":", "cmd"}, keyHint{"?", "help"}, keyHint{"q", "quit"})
	l := line{}
	for _, k := range keys {
		l = l.join(st(" "+k.key, bodyStyle), st(" "+k.label, dimStyle))
	}

	working, errored, _ := m.agentSummary()
	summary := fmt.Sprintf("%d agents %s %d working", len(m.agents), dot(), working)
	if errored > 0 {
		summary += fmt.Sprintf(" %s %d error", dot(), errored)
	}
	if open := len(domain.OpenRequests(m.ledger())); open > 0 {
		summary += fmt.Sprintf(" %s %d waiting", dot(), open)
	}
	return spread(l, line{st(summary+" ", dimStyle)}, width).render(width)
}

// maxCommandMenuRows caps how much of the screen the completion list may take.
const maxCommandMenuRows = 8

// commandMenuHeight is how many extra rows the command bar needs for its
// completion list; View() reserves them before laying out the body.
func (m Model) commandMenuHeight() int {
	if m.mode != modeCommand {
		return 0
	}
	return minInt(len(matchCommands(m.cmd.input)), maxCommandMenuRows)
}

// commandMenuRows renders the completion list for the ":" command line.
func (m Model) commandMenuRows(width int) []string {
	matches := matchCommands(m.cmd.input)
	if len(matches) == 0 {
		return nil
	}
	sel := m.cmd.selected
	if sel < 0 || sel >= len(matches) {
		sel = 0
	}
	rows := make([]string, 0, len(matches))
	for i, c := range matches {
		l := line{txt(" "), st(fitWidth(c.usage, 24), labelStyle), st(c.help, dimStyle)}
		if i == sel {
			rows = append(rows, l.renderSelected(width))
			continue
		}
		rows = append(rows, l.render(width))
	}
	if len(rows) > maxCommandMenuRows {
		start := maxInt(0, sel-maxCommandMenuRows+1)
		rows = rows[start : start+maxCommandMenuRows]
	}
	return rows
}

func (m Model) renderCreateForm(width int) string {
	var b strings.Builder
	for i, l := range m.createFormLines(width) {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(l.render(width))
	}
	return b.String()
}

// createFormLines builds the new-task form.
func (m Model) createFormLines(inner int) []line {
	var out []line
	for _, l := range wrapWords(keyLegend("tab/shift+tab fields", "enter next/submit", "esc cancel", inputHelp()), maxInt(8, inner)) {
		out = append(out, line{st(l, dimStyle)})
	}
	out = append(out, blank())

	head := func(idx int, label string) (line, bool) {
		prefix, sty := "  ", dimStyle
		focused := m.form.focus == idx
		if focused {
			prefix, sty = gl().caret+" ", sectionHeaderStyle
		}
		return line{st(prefix+label+": ", sty)}, focused
	}
	// Title and description wrap, so all of what you typed is in view.
	text := func(idx int, label, value string, maxRows int) []line {
		h, focused := head(idx, label)
		return inputLines(h, value, inner, maxRows, focused, focused && m.inputSelected)
	}
	out = append(out, text(0, "Title", m.form.title, 3)...)
	out = append(out, text(1, "Description", m.form.description, 12)...)
	out = append(out, m.formTeamLines(inner)...)
	return out
}

// helpLines builds the full key and command reference.
func (m Model) helpLines() []line {
	out := []line{{st("Keys", sectionHeaderStyle)}}
	for _, kb := range keymap {
		if kb.Help == "" {
			continue
		}
		out = append(out, line{txt("  "), st(fitWidth(kb.Key, 10), emphStyle), st(kb.Help, bodyStyle)})
	}

	out = append(out, blank(), line{st("Commands (:)", sectionHeaderStyle)})
	for _, c := range commands {
		out = append(out, line{txt("  "), st(fitWidth(c.usage, 24), emphStyle), st(c.help, dimStyle)})
	}

	out = append(out, blank(), line{st("The screen", sectionHeaderStyle)})
	for _, note := range []string{
		"tasks (left) " + dot() + " conversation (centre) " + dot() + " agents, decision, inspector (right).",
		"The conversation lists each message in one line; select one and the inspector shows it in full, formatted.",
		"conversation: what the agents said " + dot() + " activity: their raw live output " + dot() +
			" events: the audit log " + dot() + " decisions: every decision, accepted or superseded.",
		"The decision box turns yellow when a decision waits on you: a approves, x rejects.",
		"Click a tab, a task, an agent or a message to select it; the wheel scrolls what is under the pointer.",
		"Set NO_COLOR or RITAM_ASCII=1 for a plain-ASCII, monochrome screen.",
		"Tokens and cost are what the CLIs report; Codex reports tokens but not cost, so its cost reads " + unavailable + ".",
	} {
		for i, l := range wrapWords(note, 100) {
			prefix := "  "
			if i > 0 {
				prefix = "    "
			}
			out = append(out, line{txt(prefix), st(l, dimStyle)})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Small formatters
// ---------------------------------------------------------------------------

// collapse flattens embedded newlines and runs of whitespace so a value can be
// shown on exactly one line.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// fitLine truncates to width with an ellipsis, without padding.
func fitLine(s string, width int) string {
	if width < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width <= 3 {
		return string(r[:width])
	}
	return string(r[:width-3]) + "..."
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// clock renders a duration as HH:MM:SS, the form a run timer is read in.
func clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	mi := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, mi, s)
}

// shortDur renders a duration compactly for inline use: "4.2s", "2m11s".
func shortDur(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
