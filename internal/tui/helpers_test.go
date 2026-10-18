package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Shared fixtures and helpers for the TUI tests.

// visibleWidth is the display width of a rendered line.
func visibleWidth(s string) int { return visibleRunes(s) }

// swarmModel is a realistic six-agent, mid-deliberation screen: the situation
// the layout was designed for, and the one the two-agent fixture never
// exercises.
func swarmModel() Model {
	m := populatedModel(newFakeService())
	m.agents = []domain.Agent{
		{ID: "chair", Kind: domain.AgentKindClaudeCode, Role: domain.RoleAuthority, Enabled: true, State: domain.AgentStateWorking},
		{ID: "codex", Kind: domain.AgentKindCodex, Role: domain.RoleImplementer, Enabled: true, State: domain.AgentStateWorking},
		{ID: "gemini", Kind: domain.AgentKindMock, Role: domain.RoleReviewer, Enabled: true, State: domain.AgentStateIdle},
		{ID: "claude-b", Kind: domain.AgentKindClaudeCode, Role: domain.RoleReviewer, Enabled: true, State: domain.AgentStateStarting},
		{ID: "qwen", Kind: domain.AgentKindMock, Role: domain.RoleObserver, State: domain.AgentStateError,
			LastError: "exit 127 · cli not on PATH"},
		{ID: "cursor", Kind: domain.AgentKindMock, Role: domain.RoleObserver, State: domain.AgentStateStopped},
	}

	created := time.Now().Add(-7*time.Minute - 12*time.Second)
	m.tasks = []*domain.Task{{
		ID: "TASK-001", Title: "migrate billing webhooks to v2 envelope",
		AuthorityAgent: "chair",
		Participants:   []domain.AgentID{"chair", "codex", "gemini", "claude-b", "qwen"},
		State:          domain.TaskDeliberating,
		Round:          2, MessageCount: 18,
		Limits:    domain.TaskLimits{MaxDeliberationRounds: 4},
		CreatedAt: created,
	}}
	m.currentTaskID = "TASK-001"

	at := func(d time.Duration) time.Time { return created.Add(d) }
	m.messages = []*domain.Message{
		{ID: "M1", TaskID: "TASK-001", FromAgent: "chair", ToAgent: "codex", Round: 1,
			Type: domain.MsgDelegate, Content: "verify webhook replay behaviour under v2", Timestamp: at(10 * time.Second)},
		{ID: "M2", TaskID: "TASK-001", FromAgent: "codex", ToAgent: "chair", Round: 1,
			Type: domain.MsgAnswer, Content: "idempotency key must derive from event.id, not body hash", Timestamp: at(90 * time.Second)},
		{ID: "M3", TaskID: "TASK-001", FromAgent: "gemini", ToAgent: "chair", Round: 1,
			Type: domain.MsgChallenge, Content: "v2 envelope is safe only with a dedupe table", Timestamp: at(120 * time.Second)},
		{ID: "M4", TaskID: "TASK-001", FromAgent: "codex", ToAgent: "chair", Round: 2,
			Type: domain.MsgApprove, Content: "dedupe table is redundant if claimed_at is set in-txn", Timestamp: at(200 * time.Second)},
	}

	feed := func(id domain.AgentID, k ports.ActivityKind, text string, d time.Duration) {
		m.activityFor(id).appendAt(k, text, at(d))
	}
	feed("chair", ports.ActivityStatus, "synthesising round 2", 210*time.Second)
	feed("codex", ports.ActivityTool, "shell: go test ./billing/...", 215*time.Second)
	feed("codex", ports.ActivityText, "webhook_test.go:88 want 1 charge, got 2", 220*time.Second)
	feed("gemini", ports.ActivityThinking, "cross-region replay is the case nobody has tested", 205*time.Second)

	m.globalEvents = []domain.Event{
		{Type: domain.EvtTaskStateChanged, TaskID: "TASK-001", Timestamp: at(5 * time.Second),
			Summary: "CREATED -> DELIBERATING"},
		{Type: domain.EvtAgentFailed, TaskID: "TASK-001", AgentID: "qwen", Timestamp: at(150 * time.Second),
			Summary: "exit 127 · excluded from round 2"},
	}
	m.events = append([]domain.Event{}, m.globalEvents...)
	m.met.recordTool(at(215 * time.Second))
	m.met.recordError(at(150 * time.Second))
	return m
}

// checkFits is the invariant every layout must hold: no rendered line may be
// wider than the terminal. One over-wide row wraps, and a layout built by
// zipping columns row by row shears from there down.
func checkFits(t *testing.T, name, out string, width int) {
	t.Helper()
	for i, l := range strings.Split(out, "\n") {
		if w := visibleWidth(l); w > width {
			t.Errorf("%s: line %d is %d cols, terminal is %d:\n%q", name, i, w, width, l)
		}
	}
}

// busyModel is a long-running swarm screen: six agents, a full event log, full
// activity rings and a few hundred messages. It is the worst case View() sees
// in practice, and View() runs on every tea.Msg.
func busyModel() Model {
	m := swarmModel()
	base := time.Now().Add(-30 * time.Minute)
	m.messages = nil
	for i := 0; i < 300; i++ {
		from := m.agents[i%len(m.agents)].ID
		m.messages = append(m.messages, &domain.Message{
			ID: domain.MessageID(fmt.Sprintf("M%d", i)), TaskID: "TASK-001",
			FromAgent: from, ToAgent: m.agents[(i+1)%len(m.agents)].ID,
			Round: 1 + i/100, Type: domain.MsgAnswer,
			Content:   fmt.Sprintf("message %d: a moderately long routed protocol message body that wraps in narrow panes", i),
			Timestamp: base.Add(time.Duration(i) * time.Second),
		})
	}
	m.globalEvents = nil
	for i := 0; i < maxGlobalEvents; i++ {
		m.globalEvents = append(m.globalEvents, domain.Event{
			Type: domain.EvtTaskStateChanged, TaskID: "TASK-001",
			AgentID: m.agents[i%len(m.agents)].ID, Timestamp: base.Add(time.Duration(i) * 2 * time.Second),
			Summary: fmt.Sprintf("state change number %d with some descriptive summary text", i),
		})
	}
	m.activity = nil
	kinds := []ports.ActivityKind{ports.ActivityTool, ports.ActivityStatus, ports.ActivityThinking, ports.ActivityText}
	for _, a := range m.agents {
		for i := 0; i < maxActivityEntries; i++ {
			k := kinds[i%len(kinds)]
			m.activityFor(a.ID).appendTask(k, fmt.Sprintf("%s activity %d: some output the agent produced", a.ID, i),
				base.Add(time.Duration(i)*3*time.Second), "TASK-001")
		}
	}
	m.events = append([]domain.Event{}, m.globalEvents...)
	m.width, m.height = 160, 50
	return m
}

// key sends one key press through Update, exactly as the program would.
func key(m Model, k string) (Model, tea.Cmd) {
	var msg tea.KeyMsg
	switch k {
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		msg = tea.KeyMsg{Type: tea.KeyShiftTab}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "backspace":
		msg = tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+a":
		msg = tea.KeyMsg{Type: tea.KeyCtrlA}
	case "ctrl+u":
		msg = tea.KeyMsg{Type: tea.KeyCtrlU}
	case "ctrl+w":
		msg = tea.KeyMsg{Type: tea.KeyCtrlW}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	n, cmd := m.Update(msg)
	return n.(Model), cmd
}

func mouse(m Model, x, y int, b tea.MouseButton) Model {
	n, _ := m.Update(tea.MouseMsg{X: x, Y: y, Button: b, Action: tea.MouseActionPress})
	return n.(Model)
}

// locate finds the screen position of the first occurrence of text in the
// rendered view, in visible columns. Tests aim the mouse with it, so the hit
// testing is checked against what is actually drawn rather than against a
// second copy of the layout arithmetic.
func locate(t *testing.T, m Model, text string) (x, y int) {
	t.Helper()
	for row, l := range strings.Split(m.View(), "\n") {
		plain := stripANSI(l)
		if i := strings.Index(plain, text); i >= 0 {
			return len([]rune(plain[:i])), row
		}
	}
	t.Fatalf("%q is not on screen", text)
	return 0, 0
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			in = true
		case in:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				in = false
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func twoTasks(m Model) Model {
	second := *m.tasks[0]
	second.ID, second.Title = "TASK-002", "second task"
	m.tasks = append(m.tasks, &second)
	return m
}
