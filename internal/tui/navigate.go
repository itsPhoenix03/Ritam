package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// Navigation: what j/k, the arrows, page up/down, enter and the mouse do.
//
// One rule everywhere: in a list (tasks, agents, the conversation, the
// decisions tab) the keys move the selection, and the inspector follows it;
// in text (the inspector, the activity and event logs, the help) they scroll.
// The mouse follows the same rule for whichever pane it is over, and a click
// on a tab switches to it.

// wheelStep is how many lines one notch of the wheel scrolls text.
const wheelStep = 3

// navigate moves by n steps in pane p, positive meaning up / earlier.
func (m Model) navigate(p pane, n int) (Model, tea.Cmd) {
	if m.isListPane(p) {
		switch p {
		case paneAgents:
			m = m.moveAgentCursor(-n)
			return m.inspectNow(inspectAgent), nil
		case paneTasks:
			mm, cmd := m.moveTaskSelection(-n)
			return mm.inspectNow(inspectTask), cmd
		}
		switch m.centerOrDefault() {
		case tabDecisions:
			return m.moveDecisionSelection(-n), nil
		case tabTeam:
			return m.moveModelSelection(-n), nil
		}
		return m.moveConvSelection(-n), nil
	}

	before := m.scrollOf(p)
	m = m.scrollPane(p, n)
	if m.scrollOf(p) == before {
		// A key that silently does nothing reads as a broken key. Say why.
		if p == paneConversation && !m.anchorTop(p) && n < 0 {
			m.setStatus("at the newest line")
		} else {
			m.setStatus("nothing more to scroll in %s", p)
		}
	}
	return m, nil
}

// inspectNow points the inspector at a subject, starting it from the top
// when what it shows changes.
func (m Model) inspectNow(s inspectSubject) Model {
	before := m.inspectKey()
	m.inspect = s
	if m.inspectKey() != before {
		m.setScroll(paneInspector, 0)
	}
	return m
}

// moveConvSelection moves the conversation selection by delta entries.
// Moving off the newest entry stops following; arriving back on it resumes.
func (m Model) moveConvSelection(delta int) Model {
	before := m.inspectKey()
	items := m.convItems()
	if len(items) == 0 {
		m.setStatus("no messages yet")
		return m
	}
	cur := m.convSelected(items)
	next := clampInt(cur+delta, 0, len(items)-1)
	if next == cur && delta != 0 {
		if delta > 0 {
			m.setStatus("at the newest message")
		} else {
			m.setStatus("at the first message")
		}
	}
	m.convSel = next
	m.follow = next == newestEntry(items)
	// Store the offset that keeps the selection on screen, so the window
	// stays put while the selection moves inside it.
	inner, rows := m.paneBox(paneConversation)
	_, owners := m.convLines(items, inner)
	first, last := -1, -1
	for i, o := range owners {
		if o == next {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	m.setScroll(paneConversation, followSelection(m.scrollOf(paneConversation), first, last, rows, len(owners)))
	m.inspect = inspectMessage
	if m.inspectKey() != before {
		m.setScroll(paneInspector, 0)
	}
	return m
}

// moveDecisionSelection moves the decisions-tab selection.
func (m Model) moveDecisionSelection(delta int) Model {
	if len(m.decisions) == 0 {
		m.setStatus("no decisions yet")
		return m
	}
	m.decisionSel = clampInt(m.decisionSelected()+delta, 0, len(m.decisions)-1)
	m.inspect = inspectDecision
	m.setScroll(paneInspector, 0)
	return m
}

// moveTaskSelection moves the task-list selection and loads the task.
func (m Model) moveTaskSelection(delta int) (Model, tea.Cmd) {
	if len(m.tasks) == 0 {
		m.setStatus("no tasks")
		return m, nil
	}
	return m.selectTask(clampInt(m.currentTaskIndex()+delta, 0, len(m.tasks)-1))
}

// selectTask makes task idx current and loads its detail.
func (m Model) selectTask(idx int) (Model, tea.Cmd) {
	if idx < 0 || idx >= len(m.tasks) || m.tasks[idx].ID == m.currentTaskID {
		return m, nil
	}
	m.currentTaskID = m.tasks[idx].ID
	m.conversationScroll = 0
	m.follow = true
	m.convSel = 0
	m.decisionSel = 0
	m.paneScroll = nil
	return m, tea.Batch(loadTaskDetailCmd(m.ctx, m.svc, m.currentTaskID), loadAgentsCmd(m.ctx, m.svc, m.currentTaskID))
}

// moveAgentCursor moves the roster selection, clamped.
func (m Model) moveAgentCursor(delta int) Model {
	if n := len(m.rosterIdents()); n > 0 {
		m.agentCursor = clampInt(m.agentCursor+delta, 0, n-1)
	}
	return m
}

// open is enter: read the selected thing. From a list it moves focus to the
// inspector (or, on a narrow screen, to the inspector tab); on the decision
// box it shows the decision there.
func (m Model) open() Model {
	p := m.scrollTarget()
	switch p {
	case paneDecision:
		m = m.inspectNow(inspectDecision)
	case paneAgents:
		m = m.inspectNow(inspectAgent)
	case paneTasks:
		m = m.inspectNow(inspectTask)
	case paneInspector:
		m.zoom = !m.zoom
		return m
	}
	if m.layout() == layoutNarrow {
		m.center = tabInspector
		m.activePane = paneConversation
		return m
	}
	return m.focusPane(paneInspector)
}

// ---------------------------------------------------------------------------
// Mouse
// ---------------------------------------------------------------------------

// handleMouse routes a mouse event to the pane under the pointer.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeNormal && m.mode != modeHelp {
		return m, nil
	}
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	p, row, ok := m.paneAt(msg.X, msg.Y)
	if !ok {
		return m, nil
	}
	tabBar := p == paneConversation && row == -2 && m.mode == modeNormal && !m.zoom
	if p == paneConversation {
		p = m.centerTarget()
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		step := wheelStep
		if m.isListPane(p) {
			step = 1
		}
		if msg.Button == tea.MouseButtonWheelDown {
			step = -step
		}
		return m.navigate(p, step)

	case tea.MouseButtonLeft:
		if m.mode == modeHelp {
			return m, nil
		}
		if tabBar {
			if t, ok := m.tabAt(msg.X); ok {
				m.center = t
				m.activePane = paneConversation
			}
			return m, nil
		}
		m = m.focusPane(p)
		if row < 0 {
			return m, nil
		}
		switch p {
		case paneAgents:
			if i := m.rosterRowOwner(row); i >= 0 {
				m.agentCursor = i
			}
			return m.inspectNow(inspectAgent), nil
		case paneTasks:
			idx := m.taskOffset(m.boxRows(paneTasks)) + row
			if idx < len(m.tasks) {
				mm, cmd := m.selectTask(idx)
				return mm.inspectNow(inspectTask), cmd
			}
		case paneDecision:
			return m.inspectNow(inspectDecision), nil
		case paneConversation:
			return m.clickCenter(row), nil
		}
	}
	return m, nil
}

// clickCenter selects the conversation entry or decision drawn at a row.
func (m Model) clickCenter(row int) Model {
	inner, rows := m.paneBox(paneConversation)
	switch m.centerOrDefault() {
	case tabConversation:
		items := m.convItems()
		_, owners := m.convLines(items, inner)
		r := m.convOffset(items, owners, rows) + row
		if r < len(owners) && owners[r] >= 0 {
			return m.moveConvSelection(owners[r] - m.convSelected(items))
		}
	case tabTeam:
		all, owners := m.teamLines(inner)
		first, last := -1, -1
		sel := clampInt(m.modelSel, 0, maxInt(0, len(m.modelList())-1))
		for i, o := range owners {
			if o == sel && o >= 0 {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		r := followSelection(m.scrollOf(paneConversation), first, last, rows, len(all)) + row
		if r < len(owners) && owners[r] >= 0 {
			m.modelSel = owners[r]
		}
		return m
	case tabDecisions:
		_, owners := m.decisionListLines(inner)
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
		r := followSelection(m.scrollOf(paneConversation), first, last, rows, len(owners)) + row
		if r < len(owners) && owners[r] >= 0 {
			return m.moveDecisionSelection(owners[r] - sel)
		}
	}
	return m
}

// focusPane makes p the keyboard target; a pane shown as a centre tab is
// focused by focusing the centre.
func (m Model) focusPane(p pane) Model {
	if p != paneConversation && m.hostedInCenter(p) {
		m.activePane = paneConversation
		return m
	}
	for _, v := range m.visiblePanes() {
		if v == p {
			m.activePane = p
			return m
		}
	}
	return m
}

// boxRows is the number of interior rows pane p is drawn with.
func (m Model) boxRows(p pane) int {
	_, rows := m.paneBox(p)
	return rows
}

// rosterRowOwner maps a clicked row inside the roster to its agent, or -1.
func (m Model) rosterRowOwner(row int) int {
	inner, rows := m.paneBox(paneAgents)
	_, owners := m.agentRoster(inner)
	r := m.rosterOffset(owners, rows) + row
	if row >= 0 && r < len(owners) {
		return owners[r]
	}
	return -1
}
