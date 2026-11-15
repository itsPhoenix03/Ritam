package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/domain"
)

// The input-safety rules: no sequence of pasted or mistimed keys may quit,
// cancel, approve or reject.

// fakeClock moves step each time it is read; tests change step to switch
// between keys typed by hand and keys arriving as a paste.
type fakeClock struct {
	at   time.Time
	step time.Duration
}

// clockOn gives m a fake clock, at hand-typing pace to begin with.
func clockOn(m Model) (Model, *fakeClock) {
	c := &fakeClock{at: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC), step: time.Second}
	m.now = func() time.Time {
		c.at = c.at.Add(c.step)
		return c.at
	}
	return m, c
}

// pasteKeys is how the native Windows console delivers a paste: one key
// event per character, each newline an Enter.
func pasteKeys(s string) []tea.KeyMsg {
	var out []tea.KeyMsg
	for _, r := range s {
		switch r {
		case '\n':
			out = append(out, tea.KeyMsg{Type: tea.KeyEnter})
		case ' ':
			out = append(out, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
		default:
			out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	return out
}

// feed sends keys through Update, collecting every command they produce.
func feed(m Model, keys []tea.KeyMsg) (Model, []tea.Cmd) {
	var cmds []tea.Cmd
	for _, k := range keys {
		n, cmd := m.Update(k)
		m = n.(Model)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, cmds
}

func TestAMultiLinePasteIntoThePromptSubmitsNothingAndRunsNoCommand(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.decisions = []*domain.Decision{{ID: "DEC-1", TaskID: "TASK-001", Status: domain.DecisionProposed}}
	m, clk := clockOn(m)
	m, _ = key(m, "i")
	if m.mode != modePrompt {
		t.Fatalf("setup: mode %v", m.mode)
	}

	// Keys arrive a millisecond apart, as a paste does.
	clk.step = time.Millisecond
	text := "check the cache\nq\nc\na\nx\nthen quit"
	m, cmds := feed(m, pasteKeys(text))

	if m.mode != modePrompt {
		t.Fatalf("a pasted newline must not submit the prompt, mode = %v", m.mode)
	}
	if want := strings.ReplaceAll(text, "\n", " "); m.prompt.input != want {
		t.Errorf("the paste is typed as text, newlines as spaces:\n got %q\nwant %q", m.prompt.input, want)
	}
	if len(cmds) != 0 {
		t.Errorf("a paste must run no command, got %d", len(cmds))
	}
	for _, call := range []string{"SendInstruction", "Cancel", "Approve", "Reject"} {
		if n := svc.callCount(call); n != 0 {
			t.Errorf("%s was called %d times", call, n)
		}
	}
	if m.quitting {
		t.Error("the paste quit the program")
	}

	// Typed by hand, enter still sends.
	clk.step = time.Second
	m, cmd := key(m, "enter")
	if cmd == nil || m.mode != modeNormal {
		t.Fatalf("enter typed by hand sends the instruction: mode %v", m.mode)
	}
	cmd()
	if svc.lastInstruction != strings.ReplaceAll(text, "\n", " ") {
		t.Errorf("sent %q", svc.lastInstruction)
	}
}

func TestDestructiveKeysRightAfterLeavingAPromptAreIgnored(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.tasks[0].State = domain.TaskCompleted // nothing running: q would quit at once
	m, clk := clockOn(m)
	m, _ = key(m, "i")
	m, _ = key(m, "hello")
	m, _ = key(m, "enter") // a paste that began with its own line break ends the prompt here

	clk.step = 20 * time.Millisecond // slower than a paste, but too soon after the prompt closed
	for _, k := range []string{"q", "c", "a", "x"} {
		n, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		m = n.(Model)
		if cmd != nil || m.mode != modeNormal || m.quitting {
			t.Fatalf("%s right after the prompt closed must be ignored: mode %v", k, m.mode)
		}
	}
	if !strings.Contains(m.statusLine, "ignored") {
		t.Errorf("the status line says why: %q", m.statusLine)
	}

	clk.step = time.Second
	m, cmd := key(m, "q")
	if cmd == nil || !m.quitting {
		t.Error("once the moment has passed, q quits")
	}
}

func TestQuitWithATaskRunningAsksFirst(t *testing.T) {
	m := populatedModel(newFakeService()) // its task is deliberating
	m, cmd := key(m, "q")
	if cmd != nil || m.quitting {
		t.Fatal("q with a task running must not quit at once")
	}
	if m.mode != modeConfirm || !strings.Contains(m.View(), "Quit with 1 task(s) running?") {
		t.Fatalf("q asks first; mode %v", m.mode)
	}
	m, cmd = key(m, "x") // not an answer
	if cmd != nil || m.mode != modeConfirm {
		t.Fatal("only y, enter, n or esc answer the question")
	}
	m, _ = key(m, "esc")
	if m.mode != modeNormal || m.quitting {
		t.Fatal("esc keeps the program running")
	}
	m, _ = key(m, "ctrl+c")
	m, cmd = key(m, "y")
	if cmd == nil || !m.quitting {
		t.Fatal("y quits")
	}
}

func TestAConfirmationAnsweredByAPasteIsNotAnswered(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m, clk := clockOn(m)
	m, _ = key(m, "c")
	if m.mode != modeConfirm || !strings.Contains(m.confirm.label, "cannot be resumed") {
		t.Fatalf("c asks before cancelling, and says it cannot be undone: %+v", m.confirm)
	}
	clk.step = time.Millisecond
	m, cmds := feed(m, pasteKeys("yy"))
	if len(cmds) != 0 || m.mode != modeConfirm {
		t.Fatal("a pasted y must not confirm a cancel")
	}
	clk.step = time.Second
	m, cmd := key(m, "y")
	if cmd == nil {
		t.Fatal("y typed by hand confirms")
	}
	cmd()
	if svc.callCount("Cancel") != 1 {
		t.Errorf("Cancel calls = %d", svc.callCount("Cancel"))
	}
}

func TestRejectAndApproveDoNothingWithoutAPendingDecision(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	for _, k := range []string{"x", "a"} {
		mm, cmd := key(m, k)
		if cmd != nil || mm.mode != modeNormal {
			t.Fatalf("%s with no pending decision must do nothing: mode %v", k, mm.mode)
		}
		if !strings.Contains(mm.statusLine, "no decision") {
			t.Errorf("%s says why: %q", k, mm.statusLine)
		}
	}
	if svc.callCount("Reject")+svc.callCount("Approve") != 0 {
		t.Error("nothing reached the service")
	}
	if _, cmd := m.runCommand("reject because"); cmd != nil {
		t.Error(":reject follows the same rule")
	}
}

func TestARejectionNeedsANote(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.decisions = []*domain.Decision{{ID: "DEC-1", TaskID: "TASK-001", Status: domain.DecisionProposed}}
	m, _ = key(m, "x")
	if m.mode != modePrompt || m.prompt.action != ActionReject {
		t.Fatalf("x asks for the note: mode %v", m.mode)
	}
	m, cmd := key(m, "enter")
	if cmd != nil || m.mode != modePrompt {
		t.Fatal("an empty rejection is not sent; the prompt stays open")
	}
	m, _ = key(m, "use the v2 envelope")
	_, cmd = key(m, "enter")
	if cmd == nil {
		t.Fatal("a rejection with a note is sent")
	}
	cmd()
	if svc.lastNote != "use the v2 envelope" {
		t.Errorf("note = %q", svc.lastNote)
	}
}

func TestAPasteLandingOnTheFormsTeamFieldsIsIgnored(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.mode = modeCreateForm
	m.form = createForm{title: "t", focus: fieldRecruit, recruit: map[int]int{}}
	m, clk := clockOn(m)
	m, _ = key(m, "j") // not part of a burst: handled as usual
	clk.step = time.Millisecond
	m, cmds := feed(m, pasteKeys("c2 c\n"))
	if len(cmds) != 0 || m.mode != modeCreateForm || svc.callCount("CheckModel") != 0 {
		t.Fatal("pasted keys on the team fields must change nothing")
	}
}
