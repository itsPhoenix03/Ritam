package tui

import (
	"strings"
	"testing"
	"time"

	"ritam/internal/domain"
)

// longText is a realistic agent-authored decision: multi-paragraph, embedded
// newlines, lines far wider than any terminal.
const longText = "**Decision: No.** A task can never go CREATED -> COMPLETED.\n\n" +
	"Primary evidence, the transition table itself (internal/domain/task.go:38-80): TaskCreated lists only TaskPlanning, TaskPaused, TaskCancelled and TaskFailed as successors, so TaskCompleted is absent.\n" +
	"TaskAwaitingDecision is the only state whose successor list contains TaskCompleted.\n\n" +
	"Consequence: the minimum path to COMPLETED is three hops."

// widestLine returns the widest visible line in a rendered block.
func widestLine(s string) (int, string) {
	worst, worstLine := 0, ""
	for _, l := range strings.Split(s, "\n") {
		if w := visibleWidth(l); w > worst {
			worst, worstLine = w, l
		}
	}
	return worst, worstLine
}

// TestHumanInputDoesNotOverflow: the human's own typing must not wrap and
// push the layout apart.
func TestHumanInputDoesNotOverflow(t *testing.T) {
	const width = 80
	typed := strings.Repeat("the quick brown fox jumps over the lazy dog ", 12)

	m := populatedModel(newFakeService())
	m.width, m.height = width, 40
	m.mode = modeCreateForm
	m.form = createForm{title: typed, description: typed, focus: 0}
	if w, line := widestLine(m.renderCreateForm(width)); w > width {
		t.Errorf("create form renders %d cols wide, terminal is %d:\n%q", w, width, line)
	}

	m.mode = modePrompt
	m.prompt = promptState{
		action: ActionSendInstruction, label: "Instruction", input: typed,
		participants: []domain.AgentID{"opus", "codex"},
	}
	if w, line := widestLine(m.View()); w > width {
		t.Errorf("prompt renders %d cols wide, terminal is %d:\n%q", w, width, line)
	}
	if got := len(strings.Split(m.View(), "\n")); got != 40 {
		t.Errorf("a long prompt must grow its strip within the screen, not past it: %d rows", got)
	}
}

// TestALongInstructionIsShownInFull: the prompt used to show one line and
// clip the rest, so a paragraph could not be read back before sending it.
func TestALongInstructionIsShownInFull(t *testing.T) {
	m := populatedModel(newFakeService())
	m.width, m.height = 100, 40
	m.mode = modePrompt
	words := strings.Fields(strings.Repeat("make the bugs subtle and test the shutdown path ", 8))
	m.prompt = promptState{action: ActionSendInstruction, label: "Instruction",
		input: strings.Join(words, " "), participants: []domain.AgentID{"opus"}}

	view := stripANSI(m.View())
	if w, l := widestLine(view); w > 100 {
		t.Fatalf("the prompt overflows: %d cols\n%q", w, l)
	}
	rows := m.inputBodyLines(98)
	if len(rows) < 4 {
		t.Fatalf("a long instruction should wrap over several rows, got %d", len(rows))
	}
	var shown []string
	for _, r := range rows {
		shown = append(shown, strings.Fields(strings.TrimPrefix(r.plain(), gl().caret+" to opus "))...)
	}
	got := strings.TrimSuffix(strings.Join(shown, " "), gl().cursor)
	if got != strings.Join(words, " ") {
		t.Errorf("every word typed must be shown, in order:\n got  %q\n want %q", got, strings.Join(words, " "))
	}

	// Past a third of the screen it scrolls, keeping the end in view.
	m.prompt.input = strings.Repeat("abcdefgh ", 200) + "THE-END"
	rows = m.inputBodyLines(98)
	if len(rows) > m.maxInputRows() {
		t.Errorf("the strip may not grow past %d rows, got %d", m.maxInputRows(), len(rows))
	}
	text := plainLines(rows)
	if !strings.Contains(text, "earlier line(s)") || !strings.Contains(text, "THE-END") {
		t.Errorf("a very long text shows its end and says how much is above:\n%s", text)
	}
	if got := len(strings.Split(m.View(), "\n")); got != 40 {
		t.Errorf("the screen must stay 40 rows, got %d", got)
	}
}

func TestSelectAllClearAndDeleteWord(t *testing.T) {
	m := populatedModel(newFakeService())
	m.width, m.height = 100, 40
	m.mode = modePrompt
	m.prompt = promptState{action: ActionSendInstruction, label: "Instruction",
		input: "a long instruction with a mistake", participants: []domain.AgentID{"opus"}}

	m, _ = key(m, "ctrl+w")
	if m.prompt.input != "a long instruction with a " {
		t.Errorf("ctrl+w deletes the last word: %q", m.prompt.input)
	}
	m, _ = key(m, "ctrl+a")
	if !m.inputSelected {
		t.Fatal("ctrl+a should select the whole text")
	}
	m, _ = key(m, "f")
	if m.prompt.input != "f" || m.inputSelected {
		t.Errorf("typing over a selection replaces it: %q selected=%v", m.prompt.input, m.inputSelected)
	}
	m.prompt.input = "something long"
	m, _ = key(m, "ctrl+a")
	m, _ = key(m, "backspace")
	if m.prompt.input != "" {
		t.Errorf("backspace on a selection clears it: %q", m.prompt.input)
	}
	m.prompt.input = "start again"
	m, _ = key(m, "ctrl+u")
	if m.prompt.input != "" {
		t.Errorf("ctrl+u clears the field: %q", m.prompt.input)
	}

	// The same keys work in the new-task form.
	m.mode = modeCreateForm
	m.form = createForm{title: "t", description: "a description to throw away", focus: 1}
	m, _ = key(m, "ctrl+a")
	m, _ = key(m, "x")
	if m.form.description != "x" || m.form.title != "t" {
		t.Errorf("select-all applies to the focused field only: title %q description %q", m.form.title, m.form.description)
	}
}

func TestTheTaskFormWrapsLongFields(t *testing.T) {
	m := populatedModel(newFakeService())
	m.width, m.height = 90, 40
	m.mode = modeCreateForm
	desc := strings.Repeat("the scheduler must survive a shutdown during a lease ", 6) + "LAST-WORDS"
	m.form = createForm{title: "test task 2", description: desc, focus: 1}
	lines := m.createFormLines(88)
	text := plainLines(lines)
	if !strings.Contains(text, "LAST-WORDS") || !strings.Contains(text, "the scheduler must survive") {
		t.Errorf("the whole description should be in view:\n%s", text)
	}
	for _, l := range lines {
		if l.width() > 88 {
			t.Errorf("form row overflows: %q", l.plain())
		}
	}
}

func TestTypingKeepsTheCaretVisible(t *testing.T) {
	long := strings.Repeat("abcdefghij", 10) + "ENDMARK"
	got := inputWindow(long, 20, true)
	if visibleWidth(got) > 20 {
		t.Fatalf("input window is %d cols, want <= 20: %q", visibleWidth(got), got)
	}
	if !strings.Contains(got, "ENDMARK") {
		t.Errorf("the caret end must stay visible, got %q", got)
	}
}

// TestAgentAuthoredContentDoesNotOverflowAnyPane: message content and
// decisions are multi-line and very wide, in every view that shows them.
func TestAgentAuthoredContentDoesNotOverflowAnyPane(t *testing.T) {
	m := populatedModel(newFakeService())
	m.messages = append(m.messages, &domain.Message{
		ID: "MSG-9", TaskID: "TASK-001", FromAgent: "opus", ToAgent: "codex",
		Type: domain.MsgFinalDecision, Content: longText,
	})
	m.decisions = []*domain.Decision{{
		ID: "D-001", Status: domain.DecisionAccepted, Author: "opus",
		Decision: longText, Evidence: []string{longText}, Reasoning: longText,
	}}
	m.events = []domain.Event{{Type: domain.EvtDecisionCreated, TaskID: "TASK-001", AgentID: "opus", Summary: longText}}

	for _, width := range []int{200, 110, 80} {
		for _, tab := range []centerTab{tabConversation, tabEvents, tabDecisions, tabActivity} {
			for _, sub := range []inspectSubject{inspectMessage, inspectDecision, inspectTask, inspectAgent} {
				m.width, m.height = width, 40
				m.center, m.inspect = tab, sub
				if w, line := widestLine(m.View()); w > width {
					t.Errorf("%d cols, tab %s, inspector %d: renders %d wide:\n%q", width, tab, sub, w, line)
				}
			}
		}
	}
}

func TestTheInspectorScrollsAndStopsAtBothEnds(t *testing.T) {
	m := populatedModel(newFakeService())
	m.width, m.height = 160, 30
	m.messages = append(m.messages, &domain.Message{ID: "MSG-9", TaskID: "TASK-001", FromAgent: "opus",
		ToAgent: "codex", Type: domain.MsgAnswer, Timestamp: time.Now(), Content: strings.Repeat(longText+"\n\n", 6)})
	m.activePane = paneInspector

	if got := m.scrollActive(+5).scrollOf(paneInspector); got != 0 {
		t.Errorf("scrolling up from the top moved the inspector to %d", got)
	}
	down := m.scrollActive(-3)
	if down.scrollOf(paneInspector) != 3 {
		t.Fatalf("three lines down left the inspector at %d", down.scrollOf(paneInspector))
	}
	far := m
	for i := 0; i < 500; i++ {
		far = far.scrollActive(-10)
	}
	if got, max := far.scrollOf(paneInspector), far.maxScroll(paneInspector); got != max || max == 0 {
		t.Errorf("scrolling down should stop at the last line: %d (max %d)", got, max)
	}
}

func TestSwitchingTabsKeepsEachTabsOwnOffset(t *testing.T) {
	m := busyModel()
	m.center = tabActivity
	m = m.scrollActive(+5)
	if m.scrollOf(paneConversation) != 5 {
		t.Fatalf("activity offset = %d", m.scrollOf(paneConversation))
	}
	m.center = tabEvents
	if m.scrollOf(paneConversation) != 0 {
		t.Error("the events view must not inherit the activity view's offset")
	}
	m.center = tabActivity
	if m.scrollOf(paneConversation) != 5 {
		t.Error("returning to the activity view should restore its offset")
	}
}

func TestPaneCyclingVisitsEveryVisiblePane(t *testing.T) {
	m := populatedModel(newFakeService())
	m.width, m.height = 200, 50
	seen := map[pane]bool{}
	cur := m
	for i := 0; i < 10; i++ {
		cur = cur.cycleActivePane(1)
		seen[cur.activeOrDefault()] = true
	}
	for _, p := range []pane{paneTasks, paneConversation, paneAgents, paneDecision, paneInspector} {
		if !seen[p] {
			t.Errorf("] never reached %s; reached %v", p, seen)
		}
	}

	m.width = 80
	for _, p := range m.visiblePanes() {
		if p != paneConversation {
			t.Errorf("on a narrow screen only the centre is a pane, got %s", p)
		}
	}
}

func TestWindowClampsAndReportsHiddenLines(t *testing.T) {
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = "line"
	}
	visible, below, above := window(lines, 10, 0)
	if len(visible) != 10 || below != 0 || above != 90 {
		t.Errorf("at the bottom: %d lines below=%d above=%d", len(visible), below, above)
	}
	visible, below, above = window(lines, 10, 1000)
	if len(visible) != 10 || above != 0 || below != 90 {
		t.Errorf("over-scrolled: %d lines below=%d above=%d", len(visible), below, above)
	}
	short, below, above := window(lines[:3], 10, 5)
	if len(short) != 3 || below != 0 || above != 0 {
		t.Errorf("short content mishandled: %d lines, below=%d above=%d", len(short), below, above)
	}
}

func TestFlowLinesHonoursNewlinesAndWidth(t *testing.T) {
	got := flowLines(longText, 40)
	for i, l := range got {
		if visibleWidth(l) > 40 || strings.Contains(l, "\n") {
			t.Fatalf("line %d is malformed: %q", i, l)
		}
	}
	if len(got) < 5 {
		t.Errorf("expected the paragraphs to flow into several lines, got %d", len(got))
	}
}
