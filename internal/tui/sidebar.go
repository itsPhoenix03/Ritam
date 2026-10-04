package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"projectorch/internal/domain"
)

// The side columns: the task list on the left; agents, the current decision
// and the inspector on the right. Agents and the decision are status at a
// glance, so each takes as few rows as it can and leaves the rest of the
// column to the inspector, where the reading happens.

// ---------------------------------------------------------------------------
// Tasks
// ---------------------------------------------------------------------------

func (m Model) tasksBox(width, height int) []string {
	inner := width - 2
	all := m.taskLines(inner)
	body := topWindow(all, height-2, m.taskOffset(height-2))
	return renderBox(boxSpec{
		title: "tasks", right: itoa(len(m.tasks)), active: m.paneIsActive(paneTasks),
	}, body, width, height)
}

// taskOffset keeps the current task on screen.
func (m Model) taskOffset(rows int) int {
	idx := m.currentTaskIndex()
	return followSelection(m.scrollOf(paneTasks), idx, idx, rows, len(m.tasks))
}

func (m Model) taskLines(inner int) []line {
	if len(m.tasks) == 0 {
		return []line{{st("  (none -- press n)", dimStyle)}}
	}
	var out []line
	for _, t := range m.tasks {
		sty := lipgloss.NewStyle().Foreground(taskStateColor(t.State))
		mark := " "
		if t.ID == m.currentTaskID {
			mark = gl().caret
		}
		row := line{txt(mark), st(taskMarker(t.State), sty), txt(" "), st(shortTaskID(t.ID), dimStyle),
			txt(" "), st(t.Title, bodyStyle)}
		if t.ID == m.currentTaskID {
			out = append(out, line{st(fitEllipsis(row.plain(), inner), reverseStyle)})
			continue
		}
		out = append(out, row)
	}
	return out
}

// shortTaskID drops the "TASK-" prefix in the narrow task column; the
// header and the inspector spell it out.
func shortTaskID(id domain.TaskID) string {
	return strings.TrimPrefix(string(id), "TASK-")
}

// ---------------------------------------------------------------------------
// Agents
// ---------------------------------------------------------------------------

func (m Model) agentsBox(width, height int) []string {
	inner := width - 2
	all, owners := m.agentRoster(inner)
	body := topWindow(all, height-2, m.rosterOffset(owners, height-2))

	working, errored, _ := m.agentSummary()
	right := itoa(len(m.rosterIdents()))
	switch {
	case errored > 0:
		right = fmt.Sprintf("%d error", errored)
	case working > 0:
		right = fmt.Sprintf("%d working", working)
	}
	return renderBox(boxSpec{title: "agents", right: right, active: m.paneIsActive(paneAgents)}, body, width, height)
}

// rosterHeight is how many rows the roster needs at this width.
func (m Model) rosterHeight(inner int) int {
	lines, _ := m.agentRoster(inner)
	return len(lines)
}

// rosterOffset keeps every row of the selected agent on screen.
func (m Model) rosterOffset(owners []int, rows int) int {
	first, last := -1, -1
	for i, o := range owners {
		if o == m.agentCursor {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	return followSelection(m.scrollOf(paneAgents), first, last, rows, len(owners))
}

// agentLines is the roster without its owners.
func (m Model) agentLines(inner int) []line {
	lines, _ := m.agentRoster(inner)
	return lines
}

// agentRoster builds the roster: one row per agent (state, identity, role,
// state word), plus a second row only while the agent is doing something or
// has failed. Everything else about an agent is in the inspector.
func (m Model) agentRoster(inner int) ([]line, []int) {
	g := gl()
	idents := m.rosterIdents()
	if len(idents) == 0 {
		return []line{{st("  none yet: press n and pick models under Recruit", dimStyle)}}, []int{-1}
	}
	// Rows are owned by their position in idents, which is what
	// agentCursor indexes.
	selected := m.agentCursor
	if selected < 0 || selected >= len(idents) {
		selected = 0
	}

	var out []line
	var owners []int
	for pos, a := range idents {
		stateSty := lipgloss.NewStyle().Foreground(agentStateColor(a.agent.State))
		mark := " "
		if pos == selected {
			mark = g.caret
		}
		glyph := stateGlyph(a.agent.State)
		if a.agent.State == domain.AgentStateWorking {
			glyph = spinnerGlyph(m.spinnerFrame)
		}
		row := line{txt(mark), st(glyph, stateSty), txt(" ")}
		row = append(row, a.label()...)
		row = row.join(st(" "+shortRole(m.roleOn(a.agent)), dimStyle))
		if a.agent.Kind == domain.AgentKindMock {
			row = row.join(st(" mock", mockKindStyle))
		}
		right := line{st(shortState(a.agent.State), stateSty)}
		if n := m.backlog[a.agent.ID]; n > 0 {
			right = line{st(fmt.Sprintf("%d unread ", n), warnStyle), st(shortState(a.agent.State), stateSty)}
		}
		row = spread(row, right, inner)
		if pos == selected {
			row = line{st(fitEllipsis(row.plain(), inner), reverseStyle)}
		}
		out = append(out, row)
		owners = append(owners, pos)

		switch {
		case a.agent.State == domain.AgentStateError && a.agent.LastError != "":
			out = append(out, line{txt("   " + g.arrow + " "),
				st(collapse(a.agent.LastError), lipgloss.NewStyle().Foreground(colorRed))})
			owners = append(owners, pos)
		case agentIsLive(a.agent.State):
			if e, ok := m.activity[a.agent.ID].latestIn(m.scopeFor(a.agent.ID)); ok {
				age := line{}
				if !e.last.IsZero() {
					age = line{txt(" "), st(shortDur(time.Since(e.last)), dimStyle)}
				}
				out = append(out, spread(line{txt("   " + g.arrow + " "), st(collapse(e.text), statusActivityStyle)}, age, inner))
				owners = append(owners, pos)
			}
		}
	}
	return out, owners
}

// roleOn is the agent's role on the selected task: whoever holds the task's
// authority is its authority, whatever role it was configured with, since
// the human can hand authority to another participant.
func (m Model) roleOn(a domain.Agent) domain.AgentRole {
	t := m.currentTask()
	if t == nil || t.AuthorityAgent == "" {
		return a.Role
	}
	if t.AuthorityAgent == a.ID {
		return domain.RoleAuthority
	}
	if a.Role == domain.RoleAuthority {
		return domain.RoleImplementer
	}
	return a.Role
}

// shortRole abbreviates a role for the one-line roster.
func shortRole(r domain.AgentRole) string {
	switch r {
	case domain.RoleAuthority:
		return "authority"
	case domain.RoleImplementer:
		return "implementer"
	case domain.RoleReviewer:
		return "reviewer"
	case domain.RoleObserver:
		return "observer"
	}
	return strings.ToLower(string(r))
}

// agentIsLive reports whether an agent's latest activity is happening now.
func agentIsLive(s domain.AgentState) bool {
	return s == domain.AgentStateWorking || s == domain.AgentStateStarting
}

// mockBannerText is said in full in the inspector: a scripted reply looks
// exactly like a real one on screen.
const mockBannerText = "MOCK AGENTS: replies are a fixed script and ignore your task. " +
	"Restart with -real to use the Claude Code / Codex CLIs."

// shortState abbreviates a state so it fits the roster's right-hand column.
func shortState(s domain.AgentState) string {
	if s == domain.AgentStateUnavailable {
		return "UNAVAIL"
	}
	return string(s)
}

// ---------------------------------------------------------------------------
// Decision
// ---------------------------------------------------------------------------

// latestDecision is the decision the decision box shows: the newest one.
func (m Model) latestDecision() *domain.Decision {
	for i := len(m.decisions) - 1; i >= 0; i-- {
		if m.decisions[i] != nil {
			return m.decisions[i]
		}
	}
	return nil
}

// decisionBox shows the current decision at a glance. A decision waiting on
// the human is drawn in yellow with the keys that answer it: the run holds
// until then, and a held run must not look like a running one.
func (m Model) decisionBox(width, height int) []string {
	inner := width - 2
	d := m.latestDecision()
	spec := boxSpec{title: "decision", active: m.paneIsActive(paneDecision)}
	if d == nil {
		return renderBox(spec, []line{{st("no decision yet", dimStyle)}}, width, height)
	}
	spec.right = string(d.ID)
	_, pending := m.pendingDecision()
	if pending {
		accent := colorYellow
		spec.accent = &accent
		spec.title = "decision " + dot() + " awaiting you"
	}
	body := []line{{decisionStatusCell(d.Status),
		st(" "+dot()+" "+string(d.Author)+" "+dot()+" "+d.Timestamp.Format("15:04"), dimStyle)}}
	summary := wrapWords(mdSummary(d.Decision), inner)
	lines := height - 2 - 1
	if pending {
		lines--
	}
	for i := 0; i < len(summary) && i < maxInt(1, lines); i++ {
		text := summary[i]
		if i == maxInt(1, lines)-1 && i < len(summary)-1 {
			text = fitEllipsis(text+" "+summary[i+1], inner)
		}
		body = append(body, line{st(text, bodyStyle)})
	}
	if pending {
		body = append(body, line{st(" a approve ", badgeOKStyle), txt(" "), st(" x reject ", badgeErrStyle),
			st("  enter: read it", dimStyle)})
	}
	return renderBox(spec, topLines(body, height-2), width, height)
}
