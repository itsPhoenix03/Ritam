package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The team tab: who is on the team, and every model you can recruit.
//
// Each provider's catalog lists its models (Claude Code's are what its
// /model picker offers; Codex's come from `codex debug models`). Select one
// and press enter to recruit an agent running it; give it a name and it joins
// the team, available to every new task. Several agents may run the same
// model, and an agent works on each of its tasks in its own session, so the
// same model can work on several tasks at once. :recruit and :dismiss do the
// same from the command line.

type modelsLoadedMsg struct {
	models []ports.ModelInfo
	err    error
}

// modelCheckedMsg reports a model check.
type modelCheckedMsg struct {
	info ports.ModelInfo
	err  error
}

func checkModelCmd(ctx context.Context, svc ports.Service, mi ports.ModelInfo) tea.Cmd {
	return func() tea.Msg {
		info, err := svc.CheckModel(ctx, mi.Provider, mi.ID)
		return modelCheckedMsg{info: info, err: err}
	}
}

// applyModelCheck records a check's result on the model list.
func (m Model) applyModelCheck(msg modelCheckedMsg) Model {
	if msg.err != nil {
		m.setError(msg.err)
		return m
	}
	for i := range m.models {
		if m.models[i].Provider == msg.info.Provider && m.models[i].ID == msg.info.ID {
			m.models[i].Checked, m.models[i].Unavailable, m.models[i].Reason =
				msg.info.Checked, msg.info.Unavailable, msg.info.Reason
			m.models[i].Builds, m.models[i].SandboxNote = msg.info.Builds, msg.info.SandboxNote
		}
	}
	switch {
	case msg.info.Unavailable:
		m.setStatus("%s cannot run on this account: %s", msg.info.ID, msg.info.Reason)
	case msg.info.Builds:
		m.setStatus("%s runs on this account, and its sandbox check passed: it builds & tests at level 1", msg.info.ID)
	case msg.info.SandboxNote != "":
		m.setStatus("%s runs on this account; sandbox check: %s", msg.info.ID, msg.info.SandboxNote)
	default:
		m.setStatus("%s runs on this account", msg.info.ID)
	}
	return m
}

// availabilityCell says what is known about whether this account can run a
// model: unavailable (and why), checked, or nothing yet.
func availabilityCell(mi ports.ModelInfo) cell {
	switch {
	case mi.Unavailable:
		return st("  "+gl().errored+" unavailable: "+mi.Reason, lipgloss.NewStyle().Foreground(colorRed))
	case mi.Checked:
		return st("  "+gl().done+" runs on this account", lipgloss.NewStyle().Foreground(colorGreen))
	}
	return txt("")
}

// buildsCell tags a model whose CLI passed the sandbox check.
func buildsCell() cell {
	return st("  "+gl().done+" builds & tests", lipgloss.NewStyle().Foreground(colorGreen))
}

// checking starts a check of mi.
func (m Model) checking(mi ports.ModelInfo) (Model, tea.Cmd) {
	m.setStatus("checking %s with one short call...", mi.ID)
	return m, checkModelCmd(m.ctx, m.svc, mi)
}

// teamChangedMsg reports a recruit or a dismissal.
type teamChangedMsg struct {
	status string
	err    error
}

func loadModelsCmd(ctx context.Context, svc ports.Service) tea.Cmd {
	return func() tea.Msg {
		ms, err := svc.Models(ctx)
		return modelsLoadedMsg{models: ms, err: err}
	}
}

func recruitCmd(ctx context.Context, svc ports.Service, in ports.RecruitInput) tea.Cmd {
	return func() tea.Msg {
		a, err := svc.Recruit(ctx, in)
		if err != nil {
			return teamChangedMsg{err: err}
		}
		return teamChangedMsg{status: fmt.Sprintf("recruited %s (%s, %s): pick it for a new task with n", a.ID, in.Provider, in.Model)}
	}
}

func dismissCmd(ctx context.Context, svc ports.Service, id domain.AgentID) tea.Cmd {
	return func() tea.Msg {
		if err := svc.Dismiss(ctx, id); err != nil {
			return teamChangedMsg{err: err}
		}
		return teamChangedMsg{status: "dismissed " + string(id)}
	}
}

// modelList is every model, grouped by provider, the ones a CLI hides last
// within their provider.
func (m Model) modelList() []ports.ModelInfo {
	var order []string
	byProvider := map[string][]ports.ModelInfo{}
	for _, mi := range m.models {
		if _, ok := byProvider[mi.Provider]; !ok {
			order = append(order, mi.Provider)
		}
		byProvider[mi.Provider] = append(byProvider[mi.Provider], mi)
	}
	var out []ports.ModelInfo
	for _, p := range order {
		for _, mi := range byProvider[p] {
			if !mi.Hidden { // models a CLI hides are not offered
				out = append(out, mi)
			}
		}
	}
	return out
}

func (m Model) selectedModel() (ports.ModelInfo, bool) {
	list := m.modelList()
	if len(list) == 0 {
		return ports.ModelInfo{}, false
	}
	return list[clampInt(m.modelSel, 0, len(list)-1)], true
}

func (m Model) moveModelSelection(delta int) Model {
	list := m.modelList()
	if len(list) == 0 {
		m.setStatus("no models listed yet")
		return m
	}
	m.modelSel = clampInt(m.modelSel+delta, 0, len(list)-1)
	return m
}

// needsModels reports whether the team tab is showing and the model list
// has not been asked for yet.
func (m Model) needsModels() bool {
	return m.centerOrDefault() == tabTeam && m.models == nil && !m.modelsLoading && m.modelsErr == nil
}

// teamLines builds the team tab and, per line, the model it shows (-1 for
// none).
func (m Model) teamLines(inner int) ([]line, []int) {
	var out []line
	var owners []int
	add := func(l line, owner int) {
		out = append(out, l)
		owners = append(owners, owner)
	}
	idents := m.identities()
	add(line{st("TEAM", sectionHeaderStyle), st("  "+itoa(len(m.agents))+" agents "+dot()+" :dismiss <agent> removes a recruit", dimStyle)}, -1)
	for _, a := range m.agents {
		model := a.Model
		if model == "" {
			model = "(cli default)"
		}
		add(line{txt("  "), agentCell(idents, a.ID), st(strings.Repeat(" ", maxInt(1, 12-len([]rune(string(a.ID))))), dimStyle),
			st(fitWidth(string(a.Kind), 13), dimStyle), st(fitWidth(model, 24), bodyStyle),
			st(shortRole(m.roleOn(a)), dimStyle)}, -1)
	}
	add(blank(), -1)
	add(line{st("MODELS", sectionHeaderStyle), st("  enter recruits the selected model "+dot()+
		" :check checks it runs on your account "+dot()+" :recruit <provider> <model> [name] [role] [writes]", dimStyle)}, -1)
	switch {
	case m.modelsLoading:
		add(line{st("  loading the providers' model lists...", dimStyle)}, -1)
	case m.modelsErr != nil:
		add(line{st("  could not list models: "+collapse(m.modelsErr.Error()), warnStyle)}, -1)
	}
	list := m.modelList()
	sel := clampInt(m.modelSel, 0, maxInt(0, len(list)-1))
	provider := ""
	for i, mi := range list {
		if mi.Provider != provider {
			provider = mi.Provider
			add(line{st("  "+provider, emphStyle)}, -1)
		}
		mark := "    "
		if i == sel && m.paneIsActive(paneConversation) {
			mark = "  " + gl().caret + " "
		} else if i == sel {
			mark = "  " + gl().arrow
			mark = fitWidth(mark, 4)
		}
		label := mi.Label
		if label == "" {
			label = mi.ID
		}
		row := line{txt(mark), st(fitWidth(label, 16), bodyStyle), st(fitWidth(mi.ID, 24), dimStyle)}
		if mi.Unavailable || mi.Checked {
			row = row.join(availabilityCell(mi))
		} else {
			row = row.join(st(mi.Description, dimStyle))
		}
		if mi.Builds {
			row = row.join(buildsCell())
		}
		if i == sel {
			row = line{st(fitEllipsis(row.plain(), inner), reverseStyle)}
		}
		add(row, i)
	}
	return out, owners
}

// teamBox draws the team tab, keeping the selected model in view.
func (m Model) teamBox(width, height int) []string {
	all, owners := m.teamLines(width - 2)
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
	off := followSelection(m.scrollOf(paneConversation), first, last, height-2, len(all))
	return renderBox(boxSpec{title: "team", right: itoa(len(m.models)) + " models",
		active: m.paneIsActive(paneConversation)}, topWindow(all, height-2, off), width, height)
}

// startRecruit opens the prompt naming a recruit of the selected model.
func (m Model) startRecruit() Model {
	mi, ok := m.selectedModel()
	if !ok {
		m.setStatus("no model selected")
		return m
	}
	if mi.Unavailable {
		m.setStatus("%s cannot run on this account: %s (:check to try again)", mi.ID, mi.Reason)
		return m
	}
	m.mode = modePrompt
	m.inputSelected = false
	m.prompt = promptState{action: ActionRecruit, label: "name for a " + mi.ID + " agent",
		input: m.suggestName(mi), recruit: mi}
	return m
}

// suggestName proposes an unused agent name for a model: "opus", then
// "opus-2", and so on.
func (m Model) suggestName(mi ports.ModelInfo) string {
	return m.suggestNameAvoiding(mi, nil)
}

// suggestNameAvoiding is suggestName, also avoiding the names in taken.
func (m Model) suggestNameAvoiding(mi ports.ModelInfo, taken map[string]bool) string {
	base := modelBase(mi.ID)
	name := base
	for n := 2; ; n++ {
		if _, exists := m.agentByID(domain.AgentID(name)); !exists && !taken[name] {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
}

// modelBase turns a model id into the stem of an agent name: lower case,
// dots as dashes, no context-size suffix ("claude-fable-5-1[1m]" gives
// "claude-fable-5-1").
func modelBase(id string) string {
	base := strings.ToLower(id)
	if i := strings.IndexAny(base, "[ "); i > 0 {
		base = base[:i]
	}
	base = strings.Trim(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r == '.':
			return '-'
		}
		return -1
	}, base), "-_")
	if base == "" {
		base = "agent"
	}
	if len(base) > 28 {
		base = base[:28]
	}
	return base
}

// parseRecruit reads ":recruit <provider> <model> [name] [role] [writes]".
func (m Model) parseRecruit(rest string) (ports.RecruitInput, error) {
	f := strings.Fields(rest)
	if len(f) < 2 {
		return ports.RecruitInput{}, fmt.Errorf("usage: recruit <provider> <model> [name] [role] [writes]")
	}
	in := ports.RecruitInput{Provider: f[0], Model: f[1]}
	switch strings.ToLower(in.Provider) {
	case "claude", "claude-code":
		in.Provider = "claude-code"
	case "codex", "openai":
		in.Provider = "codex"
	}
	for _, arg := range f[2:] {
		switch strings.ToLower(arg) {
		case "authority":
			in.Role = domain.RoleAuthority
		case "implementer":
			in.Role = domain.RoleImplementer
		case "reviewer":
			in.Role = domain.RoleReviewer
		case "writes", "write", "allowwrites":
			in.AllowWrites = true
		default:
			if in.Name != "" {
				return in, fmt.Errorf("unexpected %q: usage: recruit <provider> <model> [name] [role] [writes]", arg)
			}
			in.Name = domain.AgentID(arg)
		}
	}
	if in.Name == "" {
		in.Name = domain.AgentID(m.suggestName(ports.ModelInfo{ID: in.Model}))
	}
	return in, nil
}

// agentKindModel describes what an agent runs, e.g. "claude-code · opus".
func agentKindModel(a domain.Agent) string {
	if a.Model == "" {
		return string(a.Kind)
	}
	return string(a.Kind) + " " + dot() + " " + a.Model
}
