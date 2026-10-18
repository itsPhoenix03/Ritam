package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/domain"
)

// Navigation as a human experiences it: keys and the mouse. The rule is the
// one every list-based terminal UI uses: in a list the keys move the
// selection (and the inspector follows it); in text they scroll.

func TestTabSwitchesTheCentreView(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44
	want := []centerTab{tabActivity, tabEvents, tabDecisions, tabTeam, tabConversation}
	for _, w := range want {
		m, _ = key(m, "tab")
		if m.centerOrDefault() != w {
			t.Fatalf("tab went to %s, want %s", m.centerOrDefault(), w)
		}
	}
	m, _ = key(m, "shift+tab")
	if m.centerOrDefault() != tabTeam {
		t.Errorf("shift+tab went to %s, want team", m.centerOrDefault())
	}
}

func TestClickingATabSwitchesToIt(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44
	x, y := locate(t, m, " events ")
	m = mouse(m, x+2, y, tea.MouseButtonLeft)
	if m.centerOrDefault() != tabEvents {
		t.Fatalf("clicking the events tab showed %s", m.centerOrDefault())
	}
	if !strings.Contains(stripANSI(m.View()), "[events]") {
		t.Error("the active tab must be marked")
	}
}

func TestJKMoveTheConversationSelectionAndTheInspectorFollows(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44
	for i := range m.agents { // no thinking entries: this walks the messages
		m.agents[i].State = domain.AgentStateIdle
	}
	items := m.convItems()
	last := items[len(items)-1]
	if got, _ := m.selectedConvItem(); got.at != last.at {
		t.Fatal("while following, the newest entry is selected")
	}

	m, _ = key(m, "k")
	if m.follow {
		t.Error("moving off the newest entry stops following")
	}
	m, _ = key(m, "k") // past qwen's failure event, onto gemini's challenge
	sel, _ := m.selectedConvItem()
	if sel.msg == nil {
		t.Fatalf("expected a message selected, got %+v", sel)
	}
	title, lines := m.inspectorContent(60)
	if title != string(sel.msg.Type) || !strings.Contains(plainLines(lines), sel.msg.Content) {
		t.Errorf("the inspector must show the selected message in full; title %q:\n%s", title, plainLines(lines))
	}

	for i := 0; i < len(items); i++ {
		m, _ = key(m, "j")
	}
	if !m.follow {
		t.Error("arriving back at the newest entry resumes following")
	}
}

func TestClickingAMessageSelectsIt(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44
	x, y := locate(t, m, "idempotency key must derive")
	m = mouse(m, x, y, tea.MouseButtonLeft)
	sel, ok := m.selectedConvItem()
	if !ok || sel.msg == nil || sel.msg.ID != "M2" {
		t.Fatalf("clicking codex's answer selected %+v", sel)
	}
	if m.inspect != inspectMessage {
		t.Error("the inspector must show the clicked message")
	}
}

func TestEnterOpensTheInspectorAndVZoomsIt(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44

	m, _ = key(m, "enter")
	if m.activeOrDefault() != paneInspector {
		t.Fatalf("enter on the conversation should focus the inspector, focus = %s", m.activeOrDefault())
	}
	m, _ = key(m, "v")
	if !m.zoom || !strings.Contains(stripANSI(m.View()), "v/esc back") {
		t.Fatal("v should zoom the inspector to the whole body")
	}
	m, _ = key(m, "esc")
	if m.zoom {
		t.Error("esc should leave the zoom")
	}
}

func TestAPendingDecisionIsPutInFrontOfTheHuman(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44
	m.tasks[0].State = domain.TaskAwaitingDecision
	d := &domain.Decision{ID: "D-009", TaskID: "TASK-001", Author: "chair", Status: domain.DecisionProposed,
		Decision: "## Ship it\n\nUse **event.id** as the idempotency key."}

	n, _ := m.Update(taskDetailMsg{taskID: "TASK-001", messages: m.messages, events: m.events,
		decisions: []*domain.Decision{d}})
	m = n.(Model)
	out := stripANSI(m.View())
	for _, want := range []string{"AWAITS YOUR APPROVAL", "AWAITING YOU", "a approve", "Ship it", "event.id"} {
		if !strings.Contains(out, want) {
			t.Errorf("a decision waiting on the human must show %q:\n%s", want, out)
		}
	}
	if m.inspect != inspectDecision {
		t.Error("the inspector should switch to the pending decision")
	}
}

func TestJKMoveTheAgentSelection(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44
	m.activePane = paneAgents

	m, _ = key(m, "j")
	m, _ = key(m, "j")
	if m.agentCursor != 2 {
		t.Fatalf("two presses of j left the cursor on %d, want 2", m.agentCursor)
	}
	m, _ = key(m, "k")
	if m.agentCursor != 1 {
		t.Errorf("k moved the cursor to %d, want 1", m.agentCursor)
	}
	if m.inspect != inspectAgent {
		t.Error("moving in the roster should point the inspector at the agent")
	}
}

func TestJKMoveTheSelectionInTheFoldedAgentsTab(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 80, 30 // narrow: the roster is a centre tab
	m.center = tabAgents

	m, _ = key(m, "j")
	if m.agentCursor != 1 {
		t.Errorf("j in the folded agents tab left the cursor on %d, want 1", m.agentCursor)
	}
}

func TestSelectionStaysOnScreenInALongRoster(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 24 // the roster box is only a few rows tall
	m.activePane = paneAgents
	idents := m.rosterIdents()
	for i := 0; i < len(idents)-1; i++ {
		m, _ = key(m, "j")
	}
	last := idents[len(idents)-1].agent.ID
	if !strings.Contains(stripANSI(m.View()), "] "+string(last)) {
		t.Errorf("selecting the last agent (%s) must scroll it into view", last)
	}
}

func TestJKSwitchTasksInTheTaskList(t *testing.T) {
	m := twoTasks(swarmModel())
	m.width, m.height = 160, 44
	m.activePane = paneTasks

	m, cmd := key(m, "j")
	if m.currentTaskID != "TASK-002" {
		t.Fatalf("j in the task list selected %s, want TASK-002", m.currentTaskID)
	}
	if cmd == nil {
		t.Error("switching task must load its detail")
	}
	m, _ = key(m, "j")
	if m.currentTaskID != "TASK-002" {
		t.Errorf("j past the last task moved to %s; a list should stop at its end", m.currentTaskID)
	}
	m, _ = key(m, "k")
	if m.currentTaskID != "TASK-001" {
		t.Errorf("k selected %s, want TASK-001", m.currentTaskID)
	}
}

func TestScrollingTheActivityBackAndForthFollowsAgain(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 20
	for i := 0; i < 40; i++ {
		m.activityFor("codex").appendAt("tool", "call", time.Now())
	}
	m.center = tabActivity

	m, _ = key(m, "k")
	m, _ = key(m, "k")
	if m.follow {
		t.Fatal("scrolling back must stop following")
	}
	m, _ = key(m, "j")
	m, _ = key(m, "j")
	if !m.follow {
		t.Error("returning to the newest line must resume following")
	}
}

func TestAKeyWithNothingToScrollSaysSo(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 60
	m.activePane = paneInspector
	m.inspect = inspectTask

	m, _ = key(m, "j")
	if !strings.Contains(m.statusLine, "nothing") {
		t.Errorf("a dead key press must explain itself, status = %q", m.statusLine)
	}
}

func TestMouseWheelScrollsThePaneUnderThePointer(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 24
	for i := 0; i < 40; i++ {
		m.activityFor("codex").appendAt("tool", "call", time.Now())
	}
	m.center = tabActivity
	m.activePane = paneAgents // the keyboard focus is elsewhere

	x, y := locate(t, m, gl().horiz+" ACTIVITY")
	m = mouse(m, x+2, y+3, tea.MouseButtonWheelUp)
	if m.scrollOf(paneConversation) == 0 {
		t.Error("the wheel over the activity must scroll it, whatever pane has focus")
	}
	if m.agentCursor != 0 {
		t.Error("the wheel over the activity must not move the agent selection")
	}
}

func TestMouseWheelOverTheRosterMovesTheSelection(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44
	x, y := locate(t, m, gl().horiz+" AGENTS")
	m = mouse(m, x, y+2, tea.MouseButtonWheelDown)
	if m.agentCursor != 1 {
		t.Errorf("the wheel over the roster left the cursor on %d, want 1", m.agentCursor)
	}
}

func TestClickActivatesThePaneAndSelectsTheRow(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44

	x, y := locate(t, m, "] gemini")
	m = mouse(m, x, y, tea.MouseButtonLeft)
	if m.activeOrDefault() != paneAgents {
		t.Errorf("clicking the roster made %s active, want agents", m.activeOrDefault())
	}
	sel, _ := m.selectedIdent()
	if sel.agent.ID != "gemini" || m.inspect != inspectAgent {
		t.Errorf("clicking gemini's row selected %s (inspect %v)", sel.agent.ID, m.inspect)
	}
}

func TestClickingATaskSwitchesToIt(t *testing.T) {
	m := twoTasks(swarmModel())
	m.width, m.height = 160, 44

	x, y := locate(t, m, "002 second")
	n, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	m = n.(Model)
	if m.currentTaskID != "TASK-002" || cmd == nil {
		t.Errorf("clicking TASK-002 selected %s (load cmd %v)", m.currentTaskID, cmd != nil)
	}
}

// TestPaneAtMatchesTheRenderedLayout checks the hit testing against the
// screen itself at several sizes, so the two can never drift apart unnoticed.
func TestPaneAtMatchesTheRenderedLayout(t *testing.T) {
	cases := []struct {
		w, h int
		want map[string]pane
	}{
		{160, 44, map[string]pane{
			"TASKS": paneTasks, "CONVERSATION": paneConversation, "AGENTS": paneAgents,
			"DECISION": paneDecision, "APPROVE": paneInspector,
		}},
		{110, 36, map[string]pane{"CONVERSATION": paneConversation, "AGENTS": paneAgents}},
		{80, 30, map[string]pane{"CONVERSATION": paneConversation}},
	}
	for _, c := range cases {
		m := swarmModel()
		m.width, m.height = c.w, c.h
		for title, want := range c.want {
			x, y := locate(t, m, gl().horiz+" "+title)
			got, _, ok := m.paneAt(x+2, y+1)
			if !ok || got != want {
				t.Errorf("%dx%d: inside %s, paneAt = %v (ok=%v), want %v", c.w, c.h, title, got, ok, want)
			}
		}
	}
}

func TestMouseIsIgnoredWhileTyping(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44
	m.mode = modeCommand
	m.cmd.input = "fil"

	x, y := locate(t, m, gl().horiz+" AGENTS")
	m = mouse(m, x, y+2, tea.MouseButtonWheelDown)
	if m.agentCursor != 0 || m.mode != modeCommand || m.cmd.input != "fil" {
		t.Error("the mouse must not change anything underneath a half-typed command")
	}
}

// TestInstructingACompletedTaskSendsAFollowUp: a completed task's agents have
// stopped, so `i` there must reopen the task (FollowUp).
func TestInstructingACompletedTaskSendsAFollowUp(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.tasks[0].State = domain.TaskCompleted

	n, _ := m.dispatch(ActionSendInstruction, 0)
	m = n.(Model)
	if !strings.Contains(m.prompt.label, "reopens") {
		t.Errorf("the prompt must warn that sending reopens the task, label = %q", m.prompt.label)
	}
	m.prompt.input = "why not a dedupe table?"
	_, cmd := m.submitPrompt()
	if cmd == nil {
		t.Fatal("submitting produced no command")
	}
	cmd()
	if svc.callCount("FollowUp") != 1 || svc.callCount("SendInstruction") != 0 {
		t.Errorf("FollowUp=%d SendInstruction=%d, want 1 and 0",
			svc.callCount("FollowUp"), svc.callCount("SendInstruction"))
	}
}

func TestInstructingARunningTaskStillSendsAnInstruction(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc) // DELIBERATING

	n, _ := m.dispatch(ActionSendInstruction, 0)
	m = n.(Model)
	m.prompt.input = "look at the cookie path"
	_, cmd := m.submitPrompt()
	cmd()
	if svc.callCount("SendInstruction") != 1 || svc.callCount("FollowUp") != 0 {
		t.Errorf("SendInstruction=%d FollowUp=%d, want 1 and 0",
			svc.callCount("SendInstruction"), svc.callCount("FollowUp"))
	}
}

func TestTheInstructCommandFollowsUpACompletedTask(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.tasks[0].State = domain.TaskCompleted

	_, cmd := m.runCommand(`instruct opus "one more thing"`)
	if cmd == nil {
		t.Fatal("no command")
	}
	cmd()
	if svc.callCount("FollowUp") != 1 {
		t.Errorf(":instruct on a completed task must follow up, FollowUp=%d", svc.callCount("FollowUp"))
	}
}
