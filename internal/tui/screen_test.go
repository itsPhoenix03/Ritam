package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/domain"
)

// The screen as a whole: every size and state fits exactly, the responsive
// columns, what goes where, and the markdown renderer.

// TestLayoutFitsEverySizeAndState is the broad mechanical guard: no line wider
// than the terminal, and exactly as many rows as it has.
func TestLayoutFitsEverySizeAndState(t *testing.T) {
	sizes := []struct{ w, h int }{
		{240, 60}, {200, 52}, {140, 48}, {130, 40}, {129, 40}, {110, 30}, {96, 30},
		{95, 24}, {80, 24}, {72, 20}, {60, 16}, {40, 12}, {20, 8}, {0, 0},
	}
	pending := func(m Model) Model {
		m.tasks[0].State = domain.TaskAwaitingDecision
		m.decisions = []*domain.Decision{{
			ID: "DEC-001", TaskID: "TASK-001", Author: "chair", Status: domain.DecisionProposed,
			Decision:  "## Migrate\n\nUse the **v2 envelope**, deriving the idempotency key from `event.id`.",
			Reasoning: "The replay test shows zero duplicate charges.", Evidence: []string{"webhook_test.go"},
		}}
		return m
	}
	states := map[string]func(Model) Model{
		"default":   func(m Model) Model { return m },
		"activity":  func(m Model) Model { m.center = tabActivity; return m },
		"events":    func(m Model) Model { m.center = tabEvents; return m },
		"decisions": func(m Model) Model { m = pending(m); m.center = tabDecisions; return m },
		"focused":   func(m Model) Model { m.focusedAgent = 1; return m },
		"zoom":      func(m Model) Model { m.zoom = true; return m },
		"agent":     func(m Model) Model { m.inspect = inspectAgent; return m },
		"task":      func(m Model) Model { m.inspect = inspectTask; return m },
		"pending":   pending,
		"paused": func(m Model) Model {
			m.tasks[0].State, m.tasks[0].PauseReason = domain.TaskPaused, "TIMEOUT: no agent activity"
			return m
		},
		"form": func(m Model) Model {
			m.mode = modeCreateForm
			m.form = createForm{}
			return m
		},
		"prompt": func(m Model) Model {
			m.mode = modePrompt
			m.prompt = promptState{action: ActionSendInstruction, label: "instruction", participants: []domain.AgentID{"codex"}}
			return m
		},
		"command":    func(m Model) Model { m.mode = modeCommand; m.cmd.input = ""; return m },
		"help":       func(m Model) Model { m.mode = modeHelp; return m },
		"no-helpbar": func(m Model) Model { m.showHelpBar = false; return m },
	}
	for name, mutate := range states {
		for _, s := range sizes {
			t.Run(fmt.Sprintf("%s/%dx%d", name, s.w, s.h), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("View() panicked: %v", r)
					}
				}()
				m := mutate(swarmModel())
				m.width, m.height = s.w, s.h
				width := maxInt(s.w, minWidth)
				out := m.View()
				checkFits(t, name, out, width)
				if s.h >= minHeight {
					if got := strings.Count(out, "\n") + 1; got != s.h {
						t.Errorf("%s: rendered %d rows, terminal has %d", name, got, s.h)
					}
				}
			})
		}
	}
}

// TestBodyRowsAreExactlyTheTerminalWidth checks the zip itself: a short row
// means a column drew fewer columns than it claimed.
func TestBodyRowsAreExactlyTheTerminalWidth(t *testing.T) {
	for _, w := range []int{240, 160, 110, 80} {
		m := swarmModel()
		m.width, m.height = w, 40
		for i, r := range m.bodyRows(w, 30) {
			if got := visibleWidth(r); got != w {
				t.Errorf("width %d: body row %d is %d cols", w, i, got)
			}
		}
	}
}

// TestLayoutPicksAColumnCountForTheWidth pins the responsive thresholds and
// checks that a pane losing its column stays reachable as a tab.
func TestLayoutPicksAColumnCountForTheWidth(t *testing.T) {
	cases := []struct {
		width   int
		want    layout
		hasTabs []centerTab
	}{
		{240, layoutWide, []centerTab{tabConversation, tabActivity, tabEvents, tabDecisions}},
		{130, layoutWide, nil},
		{129, layoutMedium, []centerTab{tabTasks}},
		{96, layoutMedium, []centerTab{tabTasks}},
		{95, layoutNarrow, []centerTab{tabTasks, tabAgents, tabInspector}},
	}
	for _, c := range cases {
		m := swarmModel()
		m.width = c.width
		if got := m.layout(); got != c.want {
			t.Errorf("width %d: layout = %v, want %v", c.width, got, c.want)
		}
		for _, want := range c.hasTabs {
			found := false
			for _, tb := range m.centerTabs() {
				found = found || tb == want
			}
			if !found {
				t.Errorf("width %d: tab %q unreachable", c.width, want)
			}
		}
	}
}

// TestTheRightColumnHoldsAgentsDecisionAndInspector: what goes where.
func TestTheRightColumnHoldsAgentsDecisionAndInspector(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 200, 50
	g := m.geometry()
	if g.tasks.x != 0 || g.center.x <= g.tasks.x || g.agents.x <= g.center.x {
		t.Fatalf("columns out of order: tasks %+v centre %+v right %+v", g.tasks, g.center, g.agents)
	}
	if g.decision.x != g.agents.x || g.inspector.x != g.agents.x {
		t.Error("the decision and the inspector belong in the right-hand column")
	}
	if !(g.agents.y < g.decision.y && g.decision.y < g.inspector.y) {
		t.Errorf("right column order should be agents, decision, inspector: %+v %+v %+v", g.agents, g.decision, g.inspector)
	}
	if g.inspector.h <= g.agents.h || g.inspector.h <= g.decision.h {
		t.Error("the inspector, where reading happens, should get most of the column")
	}
}

// TestNarrowScreensReachTheInspectorThroughEnter: on a narrow screen enter
// opens the inspector as a centre tab.
func TestNarrowScreensReachTheInspectorThroughEnter(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 80, 30
	mm, _ := m.dispatch(ActionInspect, 0)
	got := mm.(Model)
	if got.centerOrDefault() != tabInspector {
		t.Fatalf("enter did not surface the inspector, tab = %v", got.centerOrDefault())
	}
	if !strings.Contains(got.View(), "idempotency") && !strings.Contains(got.View(), "dedupe") {
		t.Errorf("the selected message should be readable in the inspector tab:\n%s", got.View())
	}
}

// TestTheConversationIsOneEntryPerMessage: the stream used to interleave raw
// CLI output, turn bookkeeping and agent start/stop with the messages, and
// collapse each message onto one huge line.
func TestTheConversationIsOneEntryPerMessage(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 200, 50
	m.messages = append(m.messages, &domain.Message{ID: "M9", TaskID: "TASK-001", FromAgent: "chair",
		ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer, Timestamp: time.Now(),
		Content: "**Both reviews are accurate.**\n\n- item one\n- item two\n\n" + strings.Repeat("long text ", 80)})
	m.events = append(m.events,
		domain.Event{Type: domain.EvtAgentStateChanged, TaskID: "TASK-001", AgentID: "codex", Summary: "CODEX is STOPPED", Timestamp: time.Now()},
		domain.Event{Type: domain.EvtTaskStateChanged, TaskID: "TASK-001", Summary: "INVESTIGATING -> DELIBERATING",
			Details: map[string]string{"from": "INVESTIGATING", "to": "DELIBERATING"}, Timestamp: time.Now()},
		domain.Event{Type: domain.EvtTaskStateChanged, TaskID: "TASK-001", Summary: "DELIBERATING -> PAUSED",
			Details: map[string]string{"from": "DELIBERATING", "to": "PAUSED", "reason": "TIMEOUT: no agent activity"}, Timestamp: time.Now()},
	)
	m.activityFor("codex").appendTask("text", "@@ORCH type=ANSWER to=chair raw protocol output", time.Now(), "TASK-001")

	inner, _ := m.paneBox(paneConversation)
	lines, _ := m.convLines(m.convItems(), inner)
	text := plainLines(lines)
	for _, absent := range []string{"@@ORCH", "is STOPPED", "INVESTIGATING", "**", "- item"} {
		if strings.Contains(text, absent) {
			t.Errorf("the conversation should not show %q:\n%s", absent, text)
		}
	}
	for _, want := range []string{"Both reviews are accurate.", "PAUSED", "TIMEOUT: no agent activity"} {
		if !strings.Contains(text, want) {
			t.Errorf("the conversation should show %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "chair "+gl().arrow+"  YOU "); got != 1 {
		t.Errorf("the answer to the human should be one entry, found %d", got)
	}
	// What an agent tells you is barred down the left like your own
	// messages, with more of it shown, plain, and capped.
	barred := 0
	for _, l := range lines {
		if strings.HasPrefix(l.plain(), gl().youBar+"   ") {
			barred++
		}
	}
	if barred != maxYouLines || !strings.Contains(text, "item one item two") {
		t.Errorf("the answer should show %d barred lines of plain text, got %d:\n%s", maxYouLines, barred, text)
	}
}

func TestMarkdownBecomesPlainProse(t *testing.T) {
	got := mdPlain("## Findings\n\n- **one** fixed\n> quoted `code`\n```\nraw\n```\n---\n1. last")
	if want := "Findings one fixed quoted code raw last"; got != want {
		t.Errorf("mdPlain = %q, want %q", got, want)
	}
}

func TestTheFilterKeepsMessagesToAndFromTheAgent(t *testing.T) {
	m := swarmModel()
	m.streamFilter = "gemini"
	for _, it := range m.convItems() {
		if it.msg == nil || (it.msg.FromAgent != "gemini" && it.msg.ToAgent != "gemini") {
			t.Errorf("filter gemini shows %+v", it)
		}
	}
}

// TestAgentIdentityIsUniqueAndChairIsFirst guards the identity table.
func TestAgentIdentityIsUniqueAndChairIsFirst(t *testing.T) {
	m := swarmModel()
	idents := m.identities()
	if len(idents) != len(m.agents) {
		t.Fatalf("identities() returned %d entries for %d agents", len(idents), len(m.agents))
	}
	if !idents[0].chair || idents[0].agent.ID != "chair" || idents[0].sigil != "[C]" {
		t.Errorf("the authority must sort first as [C], got %q %q", idents[0].agent.ID, idents[0].sigil)
	}
	seen := map[string]domain.AgentID{}
	for _, a := range idents {
		if prev, dup := seen[a.sigil]; dup {
			t.Errorf("sigil %s is shared by %s and %s", a.sigil, prev, a.agent.ID)
		}
		seen[a.sigil] = a.agent.ID
	}
}

// TestCommandPaletteCompletesAndRuns covers the ":" line end to end.
func TestCommandPaletteCompletesAndRuns(t *testing.T) {
	m := swarmModel()
	opened, _ := m.dispatch(ActionCommand, 0)
	cm := opened.(Model)
	if cm.mode != modeCommand {
		t.Fatalf("mode = %v, want modeCommand", cm.mode)
	}
	typed, _ := cm.updateCommandLine(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("instr")})
	tm := typed.(Model)
	if got := matchCommands(tm.cmd.input); len(got) != 1 || got[0].name != "instruct" {
		t.Fatalf("completion for %q = %v, want just instruct", tm.cmd.input, got)
	}
	completed, _ := tm.updateCommandLine(tea.KeyMsg{Type: tea.KeyTab})
	if got := completed.(Model).cmd.input; got != "instruct " {
		t.Fatalf("tab completed to %q, want %q", got, "instruct ")
	}

	ran, _ := m.runCommand("filter codex")
	if ran.streamFilter != "codex" {
		t.Errorf("filter command set %q, want codex", ran.streamFilter)
	}
	cleared, _ := ran.runCommand("filter all")
	if cleared.streamFilter != "" {
		t.Errorf("filter all left %q", cleared.streamFilter)
	}
	tab, _ := m.runCommand("tab events")
	if tab.centerOrDefault() != tabEvents {
		t.Errorf(":tab events showed %s", tab.centerOrDefault())
	}
	unknown, _ := m.runCommand("frobnicate")
	if !strings.Contains(unknown.statusLine, "unknown command") {
		t.Errorf("an unknown verb must be reported, got status %q", unknown.statusLine)
	}
}

// TestUnavailableMetricsAreLabelledNotFaked: a plausible fabricated number is
// worse than a blank because it gets believed.
func TestUnavailableMetricsAreLabelledNotFaked(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 44
	out := m.View()
	if !strings.Contains(out, unavailable+" tok") || !strings.Contains(out, "$"+unavailable) {
		t.Error("the header must mark unreported usage as unavailable")
	}
}

func TestHeaderShowsReportedUsage(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 40
	m.tasks[0].InputTokens, m.tasks[0].OutputTokens, m.tasks[0].CostUSD = 11000, 1300, 0.42
	out := stripANSI(m.View())
	if !strings.Contains(out, "12.3k tok") || !strings.Contains(out, "$0.42") {
		t.Errorf("the header should show reported usage:\n%s", strings.Split(out, "\n")[1])
	}
}

func TestTheHeaderSaysWhatTheHumanShouldDo(t *testing.T) {
	m := swarmModel()
	m.width, m.height = 160, 40
	m.tasks[0].State, m.tasks[0].PauseReason = domain.TaskPaused, "STALLED: no progress"
	out := stripANSI(m.View())
	if !strings.Contains(out, "PAUSED") || !strings.Contains(out, "STALLED: no progress") || !strings.Contains(out, "resume") {
		t.Errorf("a paused task must say why and how to resume:\n%s", out)
	}
}

// TestAsciiFallbackDropsEveryNonASCIIGlyph covers NO_COLOR / legacy consoles.
func TestAsciiFallbackDropsEveryNonASCIIGlyph(t *testing.T) {
	saved := asciiOnly
	asciiOnly = true
	defer func() { asciiOnly = saved }()

	m := swarmModel()
	m.width, m.height = 160, 44
	m.agents[4].LastError = "exit 127: cli not on PATH"
	for i := range m.events {
		m.events[i].Summary = "ascii summary"
	}
	m.messages[1].Content = "## Findings\n\n- **bold** item\n\n> quote\n\n| a | b |\n|---|---|\n| 1 | 2 |"
	m.follow, m.convSel = false, 1
	out := m.View()
	for i, r := range out {
		if r > 127 && r != '\n' {
			t.Fatalf("non-ASCII rune %q at offset %d in ASCII mode", r, i)
		}
	}
	for _, want := range []string{"[C]", "chair", "WORKING", "AGENTS", "[conversation]"} {
		if !strings.Contains(out, want) {
			t.Errorf("ASCII mode dropped %q", want)
		}
	}
}

// TestTheTaskInspectorShowsWhoIsWaitingOnWhom: the human asks gemini, gemini
// consults codex; the consultation nests under the human's question.
func TestTheTaskInspectorShowsWhoIsWaitingOnWhom(t *testing.T) {
	m := swarmModel()
	now := time.Now()
	m.messages = append(m.messages,
		&domain.Message{ID: "H1", TaskID: "TASK-001", FromAgent: domain.HumanAgentID, ToAgent: "gemini",
			Type: domain.MsgInformation, RequiresResponse: true, Content: "is the fix safe?", Timestamp: now.Add(-time.Minute)},
		&domain.Message{ID: "Q1", TaskID: "TASK-001", FromAgent: "gemini", ToAgent: "codex",
			Type: domain.MsgQuestion, Content: "run the replay test", Timestamp: now.Add(-30 * time.Second)},
	)
	m.inspect = inspectTask
	_, lines := m.inspectorContent(60)
	text := plainLines(lines)
	if !strings.Contains(text, "you "+gl().arrow+" gemini") {
		t.Errorf("the human's question should be listed:\n%s", text)
	}
	var consult string
	for _, r := range strings.Split(text, "\n") {
		if strings.Contains(r, "gemini "+gl().arrow+" codex") {
			consult = r
		}
	}
	if consult == "" || !strings.HasPrefix(consult, "   ") {
		t.Errorf("gemini's question to codex should be nested under the human's thread:\n%s", text)
	}
	m.width, m.height = 160, 44
	if out := stripANSI(m.View()); !strings.Contains(out, "waiting on gemini to answer you") {
		t.Errorf("the header should say who owes the human an answer:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Markdown
// ---------------------------------------------------------------------------

func renderedMD(src string, width int) string { return plainLines(renderMarkdown(src, width)) }

func TestMarkdownRendersWhatTheModelWrote(t *testing.T) {
	src := "## Verdict\n\n**Both** reviews are *accurate*, see `average()`.\n\n" +
		"- first point\n- second point that is long enough to wrap onto another line here, at sixty columns\n\n" +
		"1. one\n2. two\n\n> quoted\n\n```go\nfunc main() {}\n```\n\n[docs](http://example.com)"
	out := renderedMD(src, 60)
	for _, absent := range []string{"**", "## ", "`average", "```", "](http"} {
		if strings.Contains(out, absent) {
			t.Errorf("markdown marker %q should not be shown:\n%s", absent, out)
		}
	}
	for _, want := range []string{"Verdict", "Both reviews are accurate, see average().", bulletGlyph() + " first point",
		"1. one", "quoted", "  func main() {}", "docs"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered markdown should contain %q:\n%s", want, out)
		}
	}
	// A wrapped list item hangs under its text, not under the bullet.
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.Contains(l, "second point") && i+1 < len(lines) && !strings.HasPrefix(lines[i+1], "  ") {
			t.Errorf("a wrapped list item should indent its continuation: %q", lines[i+1])
		}
	}
}

func TestMarkdownNeverExceedsTheWidth(t *testing.T) {
	src := "# A heading that is far too long to fit on one line of a narrow pane\n\n" +
		strings.Repeat("word ", 50) + "\n\n- " + strings.Repeat("x", 120) + "\n\n```\n" + strings.Repeat("y", 90) +
		"\n```\n\n| col one | col two | col three |\n|---|---|---|\n| " + strings.Repeat("v", 40) + " | b | c |"
	for _, w := range []int{8, 20, 33, 60} {
		for i, l := range renderMarkdown(src, w) {
			if got := l.width(); got > w {
				t.Errorf("width %d: line %d is %d wide: %q", w, i, got, l.plain())
			}
		}
	}
}

func TestMarkdownTablesAlignOrFallBack(t *testing.T) {
	src := "| reviewer | bugs |\n|---|---|\n| codex | 7/7 |\n| scout | 7/7 |"
	wide := renderedMD(src, 40)
	if !strings.Contains(wide, "codex    "+gl().vert+" 7/7") {
		t.Errorf("a table that fits should align its columns:\n%s", wide)
	}
	narrow := renderedMD(src, 16)
	if !strings.Contains(narrow, "reviewer: codex") {
		t.Errorf("a table that does not fit should fall back to label: value:\n%s", narrow)
	}
}

func TestMdSummaryIsOneLineOfProse(t *testing.T) {
	cases := map[string]string{
		"**Both reviews are accurate.** Each found all 7.": "Both reviews are accurate. Each found all 7.",
		"## Findings\n\n1. **Off-by-one** in `average()`":  "Findings: Off-by-one in average()",
		"\n\n- first item\n- second":                       "first item",
		"```go\ncode\n```\nafter":                          "code",
		"":                                                 "",
	}
	for in, want := range cases {
		if got := mdSummary(in); got != want {
			t.Errorf("mdSummary(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestTheConversationWindowMatchesAFullRender: the pane renders only the
// entries its window touches; that must be exactly the same window of the
// full render, wherever the selection is (including a window that starts on
// a message's second line).
func TestTheConversationWindowMatchesAFullRender(t *testing.T) {
	m := busyModel()
	m.width, m.height = 160, 40
	inner, rows := m.paneBox(paneConversation)
	items := m.convItems()
	for _, sel := range []int{0, 1, 5, 17, 150, len(items) - 3, len(items) - 1} {
		for _, stored := range []int{0, 1, 33, 299, 1000} {
			mm := m
			mm.follow = sel == len(items)-1
			mm.convSel = sel
			mm.setScroll(paneConversation, stored)
			full, owners := mm.convLines(items, inner)
			list := rows
			want := topWindow(full, list, mm.convOffset(items, owners, list))
			got := mm.conversationBox(inner+2, rows+2)
			for i := range want {
				if g, w := string([]rune(stripANSI(got[i+1]))[1:inner+1]), stripANSI(want[i].render(inner)); g != w {
					t.Fatalf("sel %d stored %d row %d:\n got  %q\n want %q", sel, stored, i, g, w)
				}
			}
		}
	}
}

// TestYourMessagesStandOut: in a long run the human could not find what they
// had asked. Their own messages are badged, barred and shown in full (up to
// a few lines), orchestrator notices sent in their name are not, and the
// windowed render still matches the full one with these taller entries.
func TestYourMessagesStandOut(t *testing.T) {
	m := busyModel()
	m.width, m.height = 160, 40
	long := strings.Repeat("please make the bugs harder and keep them subtle ", 12)
	for i := 3; i < len(m.messages); i += 25 {
		m.messages[i].FromAgent = domain.HumanAgentID
		m.messages[i].Content = long
		m.messages[i].Metadata = nil
	}
	m.messages[10].FromAgent = domain.HumanAgentID
	m.messages[10].Metadata = map[string]string{"orchestrator": "checkpoint"}

	inner, rows := m.paneBox(paneConversation)
	items := m.convItems()
	full, owners := m.convLines(items, inner)
	text := plainLines(full)
	if !strings.Contains(text, " YOU ") || !strings.Contains(text, gl().youBar+"   please make the bugs harder") {
		t.Fatalf("your messages should be badged and shown:\n%s", text)
	}
	var shown int
	for _, l := range full {
		if strings.HasPrefix(l.plain(), gl().youBar+"   ") {
			shown++
		}
	}
	if per := shown / ((len(m.messages) - 3 + 24) / 25); per != maxYouLines {
		t.Errorf("a long message of yours shows %d lines, want %d", per, maxYouLines)
	}
	if len(owners) != len(convOwners(items, inner)) {
		t.Fatalf("convOwners says %d lines, the render has %d", len(convOwners(items, inner)), len(owners))
	}
	for _, sel := range []int{0, 3, 28, 100, len(items) - 1} {
		mm := m
		mm.convSel = sel
		mm.follow = sel == len(items)-1
		full, owners := mm.convLines(items, inner)
		list := rows
		want := topWindow(full, list, mm.convOffset(items, owners, list))
		got := mm.conversationBox(inner+2, rows+2)
		for i := range want {
			if g, w := string([]rune(stripANSI(got[i+1]))[1:inner+1]), stripANSI(want[i].render(inner)); g != w {
				t.Fatalf("sel %d row %d:\n got  %q\n want %q", sel, i, g, w)
			}
		}
	}
	checkFits(t, "your messages", m.View(), 160)
}

func TestTheInspectorBreaksTheSpendDownByAgent(t *testing.T) {
	m := swarmModel()
	tk := m.currentTask()
	if tk == nil || len(tk.Participants) == 0 {
		t.Fatal("the swarm model needs a task with participants")
	}
	agent := tk.Participants[0]
	tk.AgentUsage = map[domain.AgentID]domain.AgentUsage{
		agent: {Turns: 12, CostUSD: 1.30, InputTokens: 65607, OutputTokens: 33759, CacheReadTokens: 518330},
	}
	m.inspect = inspectTask
	_, lines := m.inspectorContent(60)
	if text := plainLines(lines); !strings.Contains(text, "12 turns "+dot()+" $1.30") {
		t.Errorf("the task's usage should list each agent's spend:\n%s", text)
	}
	m.width, m.height = 200, 50
	checkFits(t, "per-agent usage", m.View(), 200)
}
