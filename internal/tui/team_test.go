package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// run feeds a command's result back into the model, like the program does,
// following only the team's own messages (a repaint ticker would never end).
func run(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			m = run(t, m, c)
		}
	case modelsLoadedMsg, teamChangedMsg:
		next, more := m.Update(msg)
		m = run(t, next.(Model), more)
	}
	return m
}

func teamService() *fakeService {
	svc := newFakeService()
	svc.models = []ports.ModelInfo{
		{Provider: "claude-code", ID: "opus", Label: "Opus", Description: "Most capable for ambitious work"},
		{Provider: "claude-code", ID: "sonnet", Label: "Sonnet", Description: "Most efficient for everyday tasks"},
		{Provider: "codex", ID: "gpt-5.5", Label: "GPT-5.5", Description: "Legacy coding model.", Hidden: true},
		{Provider: "codex", ID: "gpt-6.1-sol", Label: "GPT-6.1-Sol", Description: "Latest workhorse model"},
	}
	return svc
}

// TestRecruitingFromTheModelList: open the team tab, see every provider's
// models, pick one, name the recruit, and it is added.
func TestRecruitingFromTheModelList(t *testing.T) {
	svc := teamService()
	m := populatedModel(svc)
	m.width, m.height = 160, 44

	next, cmd := m.runCommand("models")
	m = run(t, next, cmd)
	next2, cmd2 := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}) // any key: the tab now asks for models
	m = run(t, next2.(Model), cmd2)
	if m.centerOrDefault() != tabTeam || len(m.models) != 4 {
		t.Fatalf("the team tab should list the models: tab %s, %d models", m.centerOrDefault(), len(m.models))
	}
	view := stripANSI(m.View())
	for _, want := range []string{"TEAM", "MODELS", "claude-code", "codex", "GPT-6.1-Sol"} {
		if !strings.Contains(view, want) {
			t.Errorf("the team tab is missing %q", want)
		}
	}
	if strings.Contains(view, "GPT-5.5") {
		t.Error("a model the CLI hides must not be offered")
	}
	if list := m.modelList(); len(list) != 3 || list[2].ID != "gpt-6.1-sol" {
		t.Errorf("hidden models are left out: %+v", list)
	}

	m.modelSel = 0 // opus
	m.activePane = paneConversation
	m, _ = key(m, "enter")
	if m.mode != modePrompt || m.prompt.action != ActionRecruit {
		t.Fatalf("enter on a model should ask for the recruit's name, mode %v", m.mode)
	}
	if m.prompt.input != "opus-2" {
		t.Errorf("an unused name should be suggested, got %q", m.prompt.input)
	}
	next3, cmd3 := key(m, "enter")
	m = run(t, next3, cmd3)
	if svc.lastRecruit.Name != "opus-2" || svc.lastRecruit.Provider != "claude-code" || svc.lastRecruit.Model != "opus" {
		t.Errorf("recruited %+v", svc.lastRecruit)
	}
	checkFits(t, "team tab", m.View(), 160)
}

func TestTheRecruitCommand(t *testing.T) {
	m := populatedModel(teamService())
	in, err := m.parseRecruit("claude opus reviewer writes")
	if err != nil {
		t.Fatal(err)
	}
	if in.Provider != "claude-code" || in.Model != "opus" || in.Role != domain.RoleReviewer || !in.AllowWrites || in.Name != "opus-2" {
		t.Errorf("parsed %+v", in)
	}
	in, err = m.parseRecruit("codex gpt-6.1-sol solver")
	if err != nil || in.Name != "solver" || in.Provider != "codex" {
		t.Errorf("parsed %+v %v", in, err)
	}
	if _, err := m.parseRecruit("codex"); err == nil {
		t.Error("a model is required")
	}
	if got := m.suggestName(ports.ModelInfo{ID: "claude-fable-5-1[1m]"}); got != "claude-fable-5-1" {
		t.Errorf("suggested %q", got)
	}
}

// TestTheTaskFormRecruitsAgents: tick models under Recruit and say how
// many; Participants is built from that, every agent named after its model
// (an agent already on the team under another name is never pulled in, and
// one already called opus-1 on the opus model is used again); the human
// ticks the authority.
func TestTheTaskFormRecruitsAgents(t *testing.T) {
	svc := teamService()
	m := populatedModel(svc)
	m.agents[0].Model = "opus" // "opus" runs the opus model, but is not model-named
	m.agents = append(m.agents, domain.Agent{ID: "opus-1", Kind: domain.AgentKindClaudeCode, Model: "opus"},
		domain.Agent{ID: "sonnet-1", Kind: domain.AgentKindClaudeCode, Model: "haiku"})
	m.width, m.height = 140, 60
	next, cmd := key(m, "n")
	m = run(t, next, cmd)
	if m.mode != modeCreateForm || len(m.models) != 4 {
		t.Fatalf("the form should open with the model list loaded: mode %v, %d models", m.mode, len(m.models))
	}
	for _, r := range "Build the ledger" {
		m, _ = key(m, string(r))
	}
	if view := stripANSI(m.View()); !strings.Contains(view, "Participants: 0 agents") {
		t.Fatalf("nothing is a participant before Recruit:\n%s", view)
	}

	// Recruit: three opus (entry 0), one sonnet (entry 1, whose first name
	// is taken by an agent on another model), one gpt-6.1-sol (entry 2).
	m.form.focus = fieldRecruit
	m, _ = key(m, " ")
	m, _ = key(m, "3")
	m, _ = key(m, "down")
	m, _ = key(m, " ")
	m, _ = key(m, "down")
	m, _ = key(m, " ")

	var names []string
	for _, fm := range m.formMembers() {
		tag := "new"
		if fm.existing {
			tag = "existing"
		}
		names = append(names, string(fm.id)+"="+tag)
	}
	if got, want := strings.Join(names, " "), "opus-1=existing opus-2=new opus-3=new sonnet-2=new gpt-6-1-sol-1=new"; got != want {
		t.Fatalf("participants:\n got  %s\n want %s", got, want)
	}
	view := stripANSI(m.View())
	for _, want := range []string{"[x] Opus", "x3", "[x] Sonnet", "Participants: 5 agents",
		"opus-2", "Tick one participant as the authority"} {
		if !strings.Contains(view, want) {
			t.Errorf("the form is missing %q:\n%s", want, view)
		}
	}
	if strings.Index(view, "Recruit") > strings.Index(view, "Participants:") {
		t.Error("Recruit comes before Participants")
	}
	for _, unwanted := range []string{"existing", ", 3 new", "  new"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("the form should not label participants %q:\n%s", unwanted, view)
		}
	}
	checkFits(t, "task form with recruits", m.View(), 140)

	// The human ticks a new recruit as the authority.
	m.form.focus = fieldParticipants
	m.form.cursor = 1 // opus-2
	m, _ = key(m, " ")
	if a, _ := m.formAuthority(); a.id != "opus-2" {
		t.Fatalf("authority = %q, want opus-2", a.id)
	}

	next, cmd = key(m, "enter")
	if cmd == nil {
		t.Fatalf("submitting should create the task: %s", next.statusLine)
	}
	cmd()
	if len(svc.recruits) != 4 {
		t.Fatalf("four agents should be recruited, got %d: %+v", len(svc.recruits), svc.recruits)
	}
	for _, r := range svc.recruits {
		if !r.AllowWrites {
			t.Errorf("%s should be recruited able to write (within the workspace)", r.Name)
		}
	}
	if svc.created.AuthorityAgent != "opus-2" {
		t.Errorf("authority = %s", svc.created.AuthorityAgent)
	}
	if got := fmt.Sprint(svc.created.Participants); got != "[opus-1 opus-2 opus-3 sonnet-2 gpt-6-1-sol-1]" {
		t.Errorf("participants = %s", got)
	}
}

// TestALongFormKeepsTheFocusedFieldInView: with a long model list the form
// is taller than the screen; the field being edited must stay visible.
func TestALongFormKeepsTheFocusedFieldInView(t *testing.T) {
	svc := teamService()
	for i := 0; i < 20; i++ {
		svc.models = append(svc.models, ports.ModelInfo{Provider: "codex", ID: fmt.Sprintf("model-%02d", i)})
	}
	m := populatedModel(svc)
	m.width, m.height = 100, 30
	next, cmd := key(m, "n")
	m = run(t, next, cmd)
	m.form.focus = fieldParticipants
	m.form.cursor = 1
	if view := stripANSI(m.View()); !strings.Contains(view, "Participants:") || !strings.Contains(view, "codex") {
		t.Errorf("the focused participants field must be on screen:\n%s", view)
	}
	m.form.focus = fieldRecruit
	m.form.recruitCursor = 22 // model-19: claude models, gpt-6.1-sol, then model-00..19
	if view := stripANSI(m.View()); !strings.Contains(view, "model-19") {
		t.Errorf("the selected model must be on screen:\n%s", view)
	}
	checkFits(t, "long form", m.View(), 100)
}

// TestAnUnavailableModelCannotBePicked: a model this account cannot run
// (learned from a failed turn or a check) says why and cannot be ticked;
// c checks it again.
func TestAnUnavailableModelCannotBePicked(t *testing.T) {
	svc := teamService()
	svc.models[0].Unavailable, svc.models[0].Reason = true, "this model is not included in your plan"
	m := populatedModel(svc)
	m.width, m.height = 160, 60
	next, cmd := key(m, "n")
	m = run(t, next, cmd)
	m.form.focus = fieldRecruit
	m, _ = key(m, " ")
	if m.form.recruit[0] != 0 {
		t.Fatal("an unavailable model must not be ticked")
	}
	if !strings.Contains(m.statusLine, "not included in your plan") {
		t.Errorf("the status should say why: %q", m.statusLine)
	}
	if view := stripANSI(m.View()); !strings.Contains(view, "unavailable: this model is not included in your plan") {
		t.Errorf("the form should mark it:\n%s", view)
	}

	// c checks again; here the plan now includes it.
	next2, cmd2 := m.updateCreateForm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	m = next2.(Model)
	if cmd2 == nil {
		t.Fatal("c should start a check")
	}
	n3, _ := m.Update(cmd2())
	m = n3.(Model)
	if m.models[0].Unavailable || !m.models[0].Checked {
		t.Fatalf("a successful check clears the mark: %+v", m.models[0])
	}
	m, _ = key(m, " ")
	if m.form.recruit[0] != 1 {
		t.Error("once it runs, the model can be ticked")
	}
	if svc.callCount("CheckModel") != 1 {
		t.Errorf("CheckModel calls = %d", svc.callCount("CheckModel"))
	}
}
