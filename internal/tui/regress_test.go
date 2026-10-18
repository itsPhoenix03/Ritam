package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Regression tests for the verified TUI bugs. Each one reproduces the report
// against the Model directly -- no terminal, no real agent CLI.

func runesKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// nulKey is what bubbletea's key_windows.go delivers for Ctrl, Alt, Caps Lock
// or the Win key: a rune key whose only rune is NUL.
func nulKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0}} }

// ---------------------------------------------------------------------------
// 1. Control characters in text input
// ---------------------------------------------------------------------------

// TestTitleIgnoresNULFromUnmappedWindowsKeys is the reported failure: typing
// "test", pressing Ctrl, typing " 2" stored "test\x00 2", which the agent CLI
// then refused to start with.
func TestTitleIgnoresNULFromUnmappedWindowsKeys(t *testing.T) {
	m := populatedModel(newFakeService())
	m.mode = modeCreateForm
	m.form = createForm{}

	for _, k := range []tea.KeyMsg{
		runesKey("test"), nulKey(), {Type: tea.KeySpace, Runes: []rune{' '}}, runesKey("2"),
	} {
		mm, _ := m.updateCreateForm(k)
		m = mm.(Model)
	}
	if m.form.title != "test 2" {
		t.Fatalf("title = %q, want %q", m.form.title, "test 2")
	}

	m.form.focus = 1
	mm, _ := m.updateCreateForm(runesKey("a\x00b\x1bc\u200bd"))
	if got := mm.(Model).form.description; got != "abcd" {
		t.Errorf("description = %q, want control and zero-width runes dropped", got)
	}
}

func TestPromptAndCommandLineIgnoreControlCharacters(t *testing.T) {
	m := populatedModel(newFakeService())
	m.mode = modePrompt
	m.prompt = promptState{action: ActionSendInstruction, participants: []domain.AgentID{"codex"}}
	for _, k := range []tea.KeyMsg{runesKey("do"), nulKey(), {Type: tea.KeySpace, Runes: []rune{' '}}, runesKey("it")} {
		mm, _ := m.updatePrompt(k)
		m = mm.(Model)
	}
	if m.prompt.input != "do it" {
		t.Errorf("prompt input = %q, want %q", m.prompt.input, "do it")
	}

	m.mode = modeCommand
	m.cmd = commandState{}
	for _, k := range []tea.KeyMsg{runesKey("fil"), nulKey(), runesKey("ter")} {
		mm, _ := m.updateCommandLine(k)
		m = mm.(Model)
	}
	if m.cmd.input != "filter" {
		t.Errorf("command input = %q, want %q", m.cmd.input, "filter")
	}
}

// TestPastedTextIsSanitisedAndUndecorated covers Paste=true, which also
// arrives as KeyRunes. msg.String() would have typed "[...]" around it.
func TestPastedTextIsSanitisedAndUndecorated(t *testing.T) {
	m := populatedModel(newFakeService())
	m.mode = modeCreateForm
	m.form = createForm{}

	paste := tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune("line one\r\nline\ttwo\x00\x07")}
	mm, _ := m.updateCreateForm(paste)
	if got := mm.(Model).form.title; got != "line one line two" {
		t.Errorf("pasted title = %q, want %q", got, "line one line two")
	}

	// An Alt-modified character must not type its "alt+" label either.
	m.mode = modePrompt
	m.prompt = promptState{action: ActionSendInstruction, participants: []domain.AgentID{"codex"}}
	pm, _ := m.updatePrompt(tea.KeyMsg{Type: tea.KeyRunes, Alt: true, Runes: []rune("x")})
	if got := pm.(Model).prompt.input; strings.Contains(got, "alt") {
		t.Errorf("prompt input %q contains the key's label", got)
	}
}

// ---------------------------------------------------------------------------
// 2. Selection under NO_COLOR
// ---------------------------------------------------------------------------

func withColourProfile(p termenv.Profile) (restore func()) {
	saved := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(p)
	return func() { lipgloss.SetColorProfile(saved) }
}

// TestSelectionIsVisibleWithoutColour: reverse video was the only marker, and
// a colourless profile renders it as plain text, so the selected agent and the
// current task were indistinguishable from their neighbours.
func TestSelectionIsVisibleWithoutColour(t *testing.T) {
	defer withColourProfile(termenv.Ascii)()

	m := swarmModel()
	m.width, m.height = 140, 44
	m.agentCursor = 1 // codex
	m.tasks = append(m.tasks, &domain.Task{ID: "TASK-002", Title: "second", AuthorityAgent: "chair",
		State: domain.TaskDeliberating})

	caret := gl().caret
	const inner = 30

	var selected, others int
	for _, l := range m.agentLines(inner) {
		plain := l.render(inner)
		if w := visibleWidth(plain); w != inner {
			t.Errorf("agent row is %d cols, want exactly %d: %q", w, inner, plain)
		}
		isIdentityRow := strings.Contains(plain, "[") && strings.Contains(plain, "]")
		if !isIdentityRow {
			continue
		}
		if strings.HasPrefix(plain, caret) {
			selected++
			if !strings.Contains(plain, "codex") {
				t.Errorf("caret is on the wrong agent: %q", plain)
			}
		} else {
			others++
		}
	}
	if selected != 1 || others != len(m.rosterIdents())-1 {
		t.Errorf("want exactly one caret-marked agent row, got %d selected / %d others", selected, others)
	}

	var current, otherTasks int
	for _, l := range m.taskLines(inner) {
		plain := l.render(inner)
		if visibleWidth(plain) != inner {
			t.Errorf("task row width %d, want %d: %q", visibleWidth(plain), inner, plain)
		}
		switch {
		case strings.HasPrefix(plain, caret) && strings.Contains(plain, "001"):
			current++
		case strings.HasPrefix(plain, caret):
			t.Errorf("caret on a task that is not current: %q", plain)
		default:
			otherTasks++
		}
	}
	if current != 1 || otherTasks != 1 {
		t.Errorf("current task must carry the caret: %d marked, %d unmarked", current, otherTasks)
	}

	if out := m.View(); strings.Contains(out, "\x1b") {
		t.Error("the Ascii profile still emitted escapes; the test is not exercising NO_COLOR")
	}
}

// ---------------------------------------------------------------------------
// 3. Stream scoping
// ---------------------------------------------------------------------------

func streamTexts(m Model) string {
	var b strings.Builder
	for _, r := range m.mergedStream() {
		b.WriteString(r.text + "\n")
	}
	return b.String()
}

// TestActivityDoesNotLeakOtherTasks: live activity is a global feed, and only
// messages arrive per task.
func TestActivityDoesNotLeakOtherTasks(t *testing.T) {
	m := swarmModel()
	now := time.Now()
	m.activityFor("codex").appendTask(ports.ActivityStatus, "other-task-activity", now, "TASK-999")
	m.activityFor("codex").appendTask(ports.ActivityTool, "other-task-tool", now, "TASK-999")
	m.activityFor("codex").appendTask(ports.ActivityStatus, "this-task-activity", now, "TASK-001")

	got := streamTexts(m)
	for _, leaked := range []string{"other-task-activity", "other-task-tool"} {
		if strings.Contains(got, leaked) {
			t.Errorf("TASK-001's activity contains %q:\n%s", leaked, got)
		}
	}
	if !strings.Contains(got, "this-task-activity") {
		t.Errorf("TASK-001's activity lost its own row:\n%s", got)
	}

	m.tasks = append(m.tasks, &domain.Task{ID: "TASK-999", AuthorityAgent: "codex",
		Participants: []domain.AgentID{"codex", "chair"}, State: domain.TaskDeliberating})
	m.currentTaskID = "TASK-999"
	m.messages = nil
	got = streamTexts(m)
	if !strings.Contains(got, "other-task-activity") {
		t.Errorf("TASK-999's own rows are missing:\n%s", got)
	}
	if strings.Contains(got, "this-task-activity") {
		t.Errorf("TASK-001's activity leaked into TASK-999:\n%s", got)
	}
}

// TestActivityFragmentsDoNotMergeAcrossTasks: gluing one task's sentence onto
// another's would misattribute the text.
func TestActivityFragmentsDoNotMergeAcrossTasks(t *testing.T) {
	var buf agentActivity
	now := time.Now()
	buf.appendTask(ports.ActivityText, "first ", now, "TASK-001")
	buf.appendTask(ports.ActivityText, "second", now, "TASK-002")
	if len(buf.entries) != 2 {
		t.Fatalf("entries = %d, want one per task", len(buf.entries))
	}
}

// ---------------------------------------------------------------------------
// 4. Stable agent identity
// ---------------------------------------------------------------------------

type identKey struct {
	sigil string
	color lipgloss.Color
}

func identityTable(m Model) map[domain.AgentID]identKey {
	out := map[domain.AgentID]identKey{}
	for _, a := range m.identities() {
		out[a.agent.ID] = identKey{a.sigil, a.color}
	}
	return out
}

func TestIdentityDoesNotChangeWhenTheTaskAuthorityChanges(t *testing.T) {
	m := swarmModel()
	before := identityTable(m)

	m.tasks[0].AuthorityAgent = "codex"
	if after := identityTable(m); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("changing the authority moved identities:\n before %v\n after  %v", before, after)
	}

	// The same through the real task switch.
	m.tasks = append(m.tasks, &domain.Task{ID: "TASK-002", AuthorityAgent: "gemini",
		Participants: []domain.AgentID{"gemini", "chair"}})
	mm, _ := m.dispatch(ActionNextTask, 0)
	sw := mm.(Model)
	if sw.currentTaskID != "TASK-002" {
		t.Fatalf("task did not switch: %s", sw.currentTaskID)
	}
	if after := identityTable(sw); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("switching task moved identities:\n before %v\n after  %v", before, after)
	}

	// Which agent chairs the task on screen is still indicated, separately.
	for _, a := range sw.identities() {
		if a.chair != (a.agent.ID == "gemini") {
			t.Errorf("chair flag wrong for %s", a.agent.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// 8. Rendering cost and the ticker
// ---------------------------------------------------------------------------

// TestStreamTailIsTheTailOfTheFullMerge: View only builds the newest rows, which
// must be exactly the newest rows of the complete merge, in every filter state.
func TestStreamTailIsTheTailOfTheFullMerge(t *testing.T) {
	m := busyModel()
	for _, filter := range []domain.AgentID{"", "codex"} {
		for _, thinking := range []bool{true, false} {
			m.streamFilter, m.showThinking = filter, thinking
			full := m.mergedStream()
			for _, limit := range []int{1, 7, 60, len(full), len(full) + 50} {
				tail, total := m.streamTail(limit)
				if total != len(full) {
					t.Fatalf("filter=%q thinking=%v: total = %d, want %d", filter, thinking, total, len(full))
				}
				want := full
				if len(want) > limit {
					want = want[len(want)-limit:]
				}
				if len(tail) != len(want) {
					t.Fatalf("filter=%q thinking=%v limit=%d: %d rows, want %d", filter, thinking, limit, len(tail), len(want))
				}
				for i := range want {
					if tail[i] != want[i] {
						t.Fatalf("filter=%q thinking=%v limit=%d: row %d differs:\n got  %+v\n want %+v",
							filter, thinking, limit, i, tail[i], want[i])
					}
				}
			}
		}
	}
}

// TestScrolledActivityStillReachesOldHistory: the tail cut must not stop the
// reader scrolling back through everything the activity log holds.
func TestScrolledActivityStillReachesOldHistory(t *testing.T) {
	m := busyModel()
	m.center = tabActivity
	full := m.mergedStream()
	oldest := full[0].text
	if len(oldest) > 30 {
		oldest = oldest[:30]
	}

	mm, _ := m.dispatch(ActionScrollTop, 0)
	top := mm.(Model)
	if top.follow {
		t.Error("jumping to the oldest line must leave follow")
	}
	if !strings.Contains(top.View(), oldest) {
		t.Errorf("the oldest activity line %q is unreachable", oldest)
	}
}

func TestTickerRunsSlowWhenNothingIsWorking(t *testing.T) {
	if got := tickInterval(true); got != spinnerInterval {
		t.Errorf("working tick = %v, want %v", got, spinnerInterval)
	}
	if got := tickInterval(false); got != time.Second {
		t.Errorf("idle tick = %v, want 1s: only the clock needs driving", got)
	}
}

// ---------------------------------------------------------------------------
// 9. Stale failure display
// ---------------------------------------------------------------------------

func cellStyleOf(l line, contains string) (lipgloss.Style, bool) {
	for _, c := range l {
		if strings.Contains(c.text, contains) {
			return c.style, true
		}
	}
	return lipgloss.Style{}, false
}

// TestStoppedAgentsLastActivityIsShownAsHistory: a stopped agent kept showing
// its final "failed: ..." exactly like a live status line. The roster now
// shows a second row only for an agent that is working (or in ERROR), and the
// inspector labels the rest as history.
func TestStoppedAgentsLastActivityIsShownAsHistory(t *testing.T) {
	m := swarmModel()
	const msg = "failed: exit status 1"
	at := time.Now().Add(-3 * time.Minute)
	m.activityFor("cursor").appendTask(ports.ActivityStatus, msg, at, "TASK-001")
	m.tasks[0].Participants = append(m.tasks[0].Participants, "cursor")

	for _, l := range m.agentLines(60) {
		if strings.Contains(l.plain(), msg) {
			t.Errorf("a stopped agent's last line must not read as current in the roster: %q", l.plain())
		}
	}

	// The same line on a working agent is live and keeps the live style.
	m.activityFor("chair").appendTask(ports.ActivityStatus, "live-status-line", time.Now(), "TASK-001")
	var live line
	for _, l := range m.agentLines(60) {
		if strings.Contains(l.plain(), "live-status-line") {
			live = l
		}
	}
	if sty, ok := cellStyleOf(live, "live-status-line"); !ok || sty.GetForeground() != statusActivityStyle.GetForeground() {
		t.Errorf("a working agent's status line must show, in the live style")
	}

	// The inspector labels it as history, and does not paint the stale error red.
	m.agentCursor = 5 // cursor
	sel, _ := m.selectedIdent()
	if sel.agent.ID != "cursor" {
		t.Fatalf("selected %s, want cursor", sel.agent.ID)
	}
	m.agents[5].LastError = "exit 1"
	sel, _ = m.selectedIdent()
	var sawHistory, sawLine bool
	for _, l := range m.agentInspectorLines(sel, 40) {
		if strings.Contains(l.plain(), "(history)") {
			sawHistory = true
		}
		if strings.Contains(l.plain(), msg) {
			sawLine = true
		}
		if sty, ok := cellStyleOf(l, "exit 1"); ok && sty.GetForeground() == lipgloss.Color(ansiBrRed) {
			t.Error("a stopped agent's old error is styled as a live fault")
		}
	}
	if !sawHistory || !sawLine {
		t.Error("the inspector must show a stopped agent's last activity, labelled as history")
	}
}

// TestActivityOnlyFailuresCountAsErrors: a CLI that dies reports it as a
// "failed:" status line, and no failure event need follow.
func TestActivityOnlyFailuresCountAsErrors(t *testing.T) {
	m := swarmModel()
	m.met = metrics{}
	before := total(m.met.errorSeries())

	ev := func(kind ports.ActivityKind, text string) domain.Event {
		return domain.Event{Type: domain.EvtAgentActivity, AgentID: "codex", TaskID: "TASK-001",
			Summary: text, Timestamp: time.Now(), Details: map[string]string{"kind": string(kind)}}
	}
	mm, _ := m.Update(domainEventMsg(ev(ports.ActivityStatus, "failed: exit status 1")))
	m = mm.(Model)
	if got := total(m.met.errorSeries()); got != before+1 {
		t.Errorf("errors = %d, want %d after a failed: status line", got, before+1)
	}

	// Ordinary chatter, including the word in other kinds, does not count.
	for _, e := range []domain.Event{
		ev(ports.ActivityStatus, "running tests"),
		ev(ports.ActivityText, "failed: this is just prose"),
	} {
		mm, _ = m.Update(domainEventMsg(e))
		m = mm.(Model)
	}
	if got := total(m.met.errorSeries()); got != before+1 {
		t.Errorf("errors = %d, want %d: non-failure activity was counted", got, before+1)
	}
}
