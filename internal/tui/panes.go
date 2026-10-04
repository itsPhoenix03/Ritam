package tui

import "strings"

// pane identifies a region of the screen the keys and the mouse can act on.
type pane int

const (
	// paneConversation is the centre column, whatever tab it is showing.
	paneConversation pane = iota
	paneTasks
	paneAgents
	paneDecision
	paneInspector
	// paneAgent is one agent's live output expanded over the centre column.
	paneAgent
)

// String names the pane for status messages.
func (p pane) String() string {
	switch p {
	case paneTasks:
		return "tasks"
	case paneAgents:
		return "agents"
	case paneDecision:
		return "decision"
	case paneInspector:
		return "inspector"
	case paneAgent:
		return "agent"
	default:
		return "conversation"
	}
}

// visiblePanes lists the panes on screen, in the order ] and [ cycle them:
// left to right, top to bottom.
func (m Model) visiblePanes() []pane {
	if m.zoom {
		return []pane{paneInspector}
	}
	if _, focused := m.focusedAgentID(); focused {
		return []pane{paneAgent}
	}
	g := m.geometry()
	var out []pane
	if g.tasks.w > 0 {
		out = append(out, paneTasks)
	}
	out = append(out, paneConversation)
	if g.agents.w > 0 {
		out = append(out, paneAgents)
	}
	if g.decision.w > 0 {
		out = append(out, paneDecision)
	}
	if g.inspector.w > 0 {
		out = append(out, paneInspector)
	}
	return out
}

// centerTarget is the pane the centre column stands for: a folded tab (the
// tasks, the agents, the inspector on a narrow screen) or the conversation
// pane itself.
func (m Model) centerTarget() pane {
	if m.mode == modeHelp {
		return paneConversation
	}
	switch m.centerOrDefault() {
	case tabTasks:
		return paneTasks
	case tabAgents:
		return paneAgents
	case tabInspector:
		return paneInspector
	}
	return paneConversation
}

// scrollTarget is the pane the keys act on.
func (m Model) scrollTarget() pane {
	if m.mode == modeHelp {
		return paneConversation
	}
	p := m.activeOrDefault()
	if p == paneConversation {
		return m.centerTarget()
	}
	return p
}

// paneIsActive reports whether p is the pane the keys act on, so its border
// can say so.
func (m Model) paneIsActive(p pane) bool { return m.mode == modeNormal && m.scrollTarget() == p }

// hostedInCenter reports whether pane p is drawn as a centre tab rather than
// in a column of its own.
func (m Model) hostedInCenter(p pane) bool {
	if p == paneConversation {
		return true
	}
	return m.centerTarget() == p
}

// isListPane reports whether the keys move a selection in p rather than
// scrolling it: the task list, the roster, the conversation and the decisions
// tab are lists; the activity, the events, the help and the inspector are
// text.
func (m Model) isListPane(p pane) bool {
	switch p {
	case paneTasks, paneAgents:
		return true
	case paneConversation:
		if m.mode == modeHelp {
			return false
		}
		switch m.centerOrDefault() {
		case tabConversation, tabDecisions, tabTeam:
			return true
		}
	}
	return false
}

// anchorTop reports whether a text pane's offset counts from its first line
// (the inspector, the help) or back from its last (the activity and event
// logs, read from the newest line up).
func (m Model) anchorTop(p pane) bool {
	if m.mode == modeHelp {
		return true
	}
	switch p {
	case paneInspector, paneDecision, paneTasks, paneAgents:
		return true
	case paneConversation:
		switch m.centerOrDefault() {
		case tabActivity, tabEvents:
			return false
		}
		return true
	}
	return false
}

// scrollKey is where a pane's offset is stored. The centre column keeps one
// offset per tab, so switching tabs never carries a meaningless offset over
// (the activity log counts from the bottom, the conversation from the top).
func (m Model) scrollKey(p pane) int {
	if p == paneConversation {
		if m.mode == modeHelp {
			return 1000
		}
		return 100 + int(m.centerOrDefault())
	}
	return int(p)
}

// scrollOf returns the stored offset of a pane.
func (m Model) scrollOf(p pane) int {
	if p == paneAgent {
		return m.focusScroll
	}
	return m.paneScroll[m.scrollKey(p)]
}

// setScroll stores a pane's offset, clamped to non-negative. The map is
// copied on write: Model is a value type, and a shared map would move the
// offset of every earlier copy too.
func (m *Model) setScroll(p pane, v int) {
	if v < 0 {
		v = 0
	}
	if p == paneAgent {
		m.focusScroll = v
		return
	}
	next := make(map[int]int, len(m.paneScroll)+1)
	for k, o := range m.paneScroll {
		next[k] = o
	}
	next[m.scrollKey(p)] = v
	m.paneScroll = next
}

// activeOrDefault returns the pane the keys act on, falling back to the
// conversation when the stored one is not on screen.
func (m Model) activeOrDefault() pane {
	for _, p := range m.visiblePanes() {
		if p == m.activePane {
			return p
		}
	}
	return m.visiblePanes()[0]
}

// cycleActivePane moves keyboard focus to the next/previous visible pane.
func (m Model) cycleActivePane(delta int) Model {
	visible := m.visiblePanes()
	cur := 0
	active := m.activeOrDefault()
	for i, p := range visible {
		if p == active {
			cur = i
		}
	}
	next := ((cur+delta)%len(visible) + len(visible)) % len(visible)
	m = m.focusPane(visible[next])
	return m
}

// paneBox reports the interior width and visible rows of pane p as drawn.
func (m Model) paneBox(p pane) (inner, rows int) {
	g := m.geometry()
	var r rect
	switch {
	case m.mode == modeHelp:
		r = rect{w: g.body.w, h: g.body.h}
		return maxInt(1, r.w-2), maxInt(1, r.h-2)
	case m.zoom && p == paneInspector:
		r = g.body
	case p == paneConversation || p == paneAgent || m.hostedInCenter(p):
		r = g.centerBox()
	case p == paneTasks:
		r = g.tasks
	case p == paneAgents:
		r = g.agents
	case p == paneDecision:
		r = g.decision
	case p == paneInspector:
		r = g.inspector
	}
	return maxInt(1, r.w-2), maxInt(1, r.h-2)
}

// flowLines turns arbitrary text into display lines that fit width.
//
// Agent-authored content is routinely multi-line and far wider than the
// terminal — a real decision ran to several paragraphs — so embedded newlines
// have to be honoured and long lines wrapped. Without this a single message
// silently destroys the layout of every pane below it.
func flowLines(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimRight(raw, "\r\t ")
		if raw == "" {
			out = append(out, "")
			continue
		}
		out = append(out, wrapWords(raw, width)...)
	}
	return out
}

// window returns the slice of lines visible for a pane of the given height at
// the given scroll offset, where offset 0 means "pinned to the newest line".
//
// It also reports whether there is more content above or below, so the pane
// can say so rather than silently hiding it.
func window(lines []string, height, scroll int) (visible []string, more, above int) {
	if height < 1 {
		height = 1
	}
	if len(lines) <= height {
		return lines, 0, 0
	}

	maxScroll := len(lines) - height
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	end := len(lines) - scroll
	start := end - height
	return lines[start:end], scroll, start
}

// itoa is a tiny strconv.Itoa to keep this file dependency-light.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// inputWindow renders a single-line text input so the caret stays visible.
//
// The human's own typing was the one thing on screen that could still overflow
// its pane: agent output is fitted to width, but the task-creation fields and
// the instruction prompt rendered whatever had been typed, so a long title
// wrapped and pushed the layout apart. This slides a window over the value
// instead, keeping the end (where the caret is) in view.
func inputWindow(value string, width int, focused bool) string {
	caret := ""
	if focused {
		caret = "_"
	}
	if width < 1 {
		width = 1
	}

	r := []rune(value + caret)
	if len(r) <= width {
		return string(r)
	}
	// Show the tail: that is where the caret is and where typing happens.
	tail := string(r[len(r)-width+1:])
	return "<" + tail
}
