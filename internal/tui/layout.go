package tui

// Geometry.
//
// Every region of the screen is a rectangle computed in one place, geometry,
// and both the renderer and the mouse read it. Keeping two separate copies of
// the arithmetic in step is how a click lands on the pane next to the one
// drawn under the pointer.
//
// The body, by importance to someone running agents:
//
//	tasks | conversation (tabs)             | agents
//	      |                                 | decision
//	      |                                 | inspector
//
// The conversation is what you read; the inspector shows the selected entry
// in full; agents and the decision are status at a glance; the task list is
// navigation. Below wideMinWidth the task list folds into a centre tab,
// below mediumMinWidth the right-hand column does too.

// rect is a screen region; w == 0 means "not shown".
type rect struct{ x, y, w, h int }

func (r rect) contains(x, y int) bool {
	return r.w > 0 && x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h
}

// geom is the body's layout for one frame.
type geom struct {
	body rect
	// center includes the tab bar row at its top; centerBox is the box below.
	center                  rect
	tasks, agents, decision rect
	inspector               rect
}

func (g geom) centerBox() rect {
	return rect{x: g.center.x, y: g.center.y + 1, w: g.center.w, h: maxInt(1, g.center.h-1)}
}

// Column budgets.
const (
	tasksColMin  = 22
	tasksColMax  = 30
	rightColMin  = 40
	rightColMax  = 84
	centerColMin = 48
	// decisionBoxHeight fits a status line, two lines of summary and the
	// approve/reject hint.
	decisionBoxHeight = 6
	minInspectorRows  = 6
)

// geometry lays out the body for the current terminal size and state.
func (m Model) geometry() geom {
	if m.frameGeom != nil {
		return *m.frameGeom
	}
	width, height := m.screenSize()
	headH, bodyH, _, _ := m.chromeBudget(height)
	g := geom{body: rect{x: 0, y: headH, w: width, h: bodyH}}
	if m.mode == modeHelp || m.mode == modeCreateForm {
		g.center = g.body
		return g
	}
	if m.zoom {
		g.inspector = g.body
		return g
	}

	var tasksW, rightW int
	switch m.layout() {
	case layoutWide:
		tasksW = clampInt(width*14/100, tasksColMin, tasksColMax)
		rightW = clampInt(width*34/100, rightColMin, rightColMax)
		if c := width - tasksW - rightW - 2; c < centerColMin {
			rightW = maxInt(rightColMin, rightW-(centerColMin-c))
		}
	case layoutMedium:
		rightW = clampInt(width*40/100, rightColMin, rightColMax)
	}

	x := 0
	if tasksW > 0 {
		g.tasks = rect{x: 0, y: headH, w: tasksW, h: bodyH}
		x = tasksW + 1
	}
	centerW := width - x
	if rightW > 0 {
		centerW -= rightW + 1
	}
	g.center = rect{x: x, y: headH, w: centerW, h: bodyH}

	if rightW > 0 {
		rx := x + centerW + 1
		agentsH := clampInt(m.rosterHeight(rightW-2)+2, 4, maxInt(4, bodyH/3))
		decH := decisionBoxHeight
		insH := bodyH - agentsH - decH
		if insH < minInspectorRows {
			// The inspector is the reason the column exists; the decision
			// box gives way first (its content is a decisions tab away).
			decH, insH = 0, bodyH-agentsH
		}
		g.agents = rect{x: rx, y: headH, w: rightW, h: agentsH}
		if decH > 0 {
			g.decision = rect{x: rx, y: headH + agentsH, w: rightW, h: decH}
		}
		g.inspector = rect{x: rx, y: headH + agentsH + decH, w: rightW, h: insH}
	}
	return g
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// paneAt reports the pane drawn at screen cell (x, y) and the row within its
// box interior (0 = first row under the top border; negative = on the border
// or, for the centre, -1 on its box border and -2 on the tab bar).
func (m Model) paneAt(x, y int) (pane, int, bool) {
	g := m.geometry()
	if !g.body.contains(x, y) {
		return 0, 0, false
	}
	if m.mode == modeHelp || m.mode == modeCreateForm {
		return paneConversation, y - g.body.y - 1, true
	}
	switch {
	case g.inspector.contains(x, y):
		return paneInspector, y - g.inspector.y - 1, true
	case g.tasks.contains(x, y):
		return paneTasks, y - g.tasks.y - 1, true
	case g.agents.contains(x, y):
		return paneAgents, y - g.agents.y - 1, true
	case g.decision.contains(x, y):
		return paneDecision, y - g.decision.y - 1, true
	case g.center.contains(x, y):
		return paneConversation, y - g.center.y - 2, true
	}
	return 0, 0, false
}

// tabAt returns the centre tab drawn at column x of the tab bar.
func (m Model) tabAt(x int) (centerTab, bool) {
	g := m.geometry()
	pos := g.center.x
	for _, t := range m.centerTabs() {
		w := len([]rune(t.String())) + 2
		if x >= pos && x < pos+w {
			return t, true
		}
		pos += w
	}
	return 0, false
}
