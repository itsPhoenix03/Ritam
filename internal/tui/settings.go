package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The settings tab: the workspace's access level, which says what agents may
// run beyond editing files, and the command list the middle levels use.
//
// Select a level and press enter (or :access <0-3>). Lowering applies at
// once; raising asks you to type yes first, because more access is more risk
// and the tab says plainly how much. :allow and :disallow edit the command
// list. Every change is saved to the project's settings and recorded in the
// events of every open task; it applies from each agent's next turn.

// accessLoadedMsg carries the workspace's access settings.
type accessLoadedMsg struct {
	info ports.AccessInfo
	err  error
}

// accessChangedMsg reports a change of the access settings.
type accessChangedMsg struct {
	info ports.AccessInfo
	err  error
}

func loadAccessCmd(ctx context.Context, svc ports.Service) tea.Cmd {
	return func() tea.Msg {
		info, err := svc.Access(ctx)
		return accessLoadedMsg{info: info, err: err}
	}
}

func setAccessCmd(ctx context.Context, svc ports.Service, in ports.AccessChange) tea.Cmd {
	return func() tea.Msg {
		info, err := svc.SetAccess(ctx, in)
		return accessChangedMsg{info: info, err: err}
	}
}

// applyAccess records loaded or changed access settings.
func (m Model) applyAccess(info ports.AccessInfo) Model {
	m.access, m.accessLoaded = info, true
	return m
}

// listOrNone renders a command list for the status line.
func listOrNone(cmds []string) string {
	if len(cmds) == 0 {
		return "none"
	}
	return strings.Join(cmds, ", ")
}

// accessRisk says plainly what raising to a level hands the agents.
func accessRisk(l domain.AccessLevel, cmds []string) string {
	list := "nothing (no commands listed yet)"
	if len(cmds) > 0 {
		list = strings.Join(cmds, ", ")
	}
	switch l {
	case domain.AccessSandboxedBuilder:
		return "agents whose CLI runs in a sandbox (Codex) will run: " + list
	case domain.AccessBuildAndTest:
		return "every agent will run: " + list + "; Claude Code runs them with no sandbox, directly on this machine"
	case domain.AccessAnyCommand:
		return "agents may run any command but the always-blocked ones; Claude Code has no sandbox here, so this means you trust these agents"
	}
	return "agents edit files and run read-only commands"
}

// chooseLevel applies a level: at once when lowering, after a typed yes when
// raising.
func (m Model) chooseLevel(l domain.AccessLevel) (Model, tea.Cmd) {
	if !m.accessLoaded {
		m.setStatus("the access settings have not loaded yet")
		return m, nil
	}
	cur := m.access.Level
	switch {
	case m.access.Locked != "" && l != cur:
		m.setStatus("%s", m.access.Locked)
		return m, nil
	case l == cur:
		m.setStatus("already at access level %s", l)
		return m, nil
	case l > cur:
		m.mode = modePrompt
		m.inputSelected = false
		m.prompt = promptState{action: ActionSetAccess, access: l,
			label: fmt.Sprintf("raise access to level %s? %s. Type yes to confirm:", l, accessRisk(l, m.access.Commands))}
		return m, nil
	}
	m.setStatus("lowering access to level %s...", l)
	return m, setAccessCmd(m.ctx, m.svc, ports.AccessChange{Level: l, Commands: m.access.Commands})
}

// confirmLevel handles the typed answer to a raise.
func (m Model) confirmLevel(p promptState) (Model, tea.Cmd) {
	switch strings.ToLower(strings.TrimSpace(p.input)) {
	case "yes", "y":
		m.setStatus("raising access to level %s...", p.access)
		return m, setAccessCmd(m.ctx, m.svc, ports.AccessChange{Level: p.access, Commands: m.access.Commands})
	}
	m.setStatus("access unchanged: type yes to raise it")
	return m, nil
}

// editCommands adds a command to the list or removes one.
func (m Model) editCommands(cmd string, add bool) (Model, tea.Cmd) {
	cmd = strings.Join(strings.Fields(unquote(cmd)), " ")
	verb := map[bool]string{true: "allow", false: "disallow"}[add]
	if cmd == "" {
		m.setStatus("usage: %s <command>, e.g. %s cmake", verb, verb)
		return m, nil
	}
	if !m.accessLoaded {
		m.setStatus("the access settings have not loaded yet")
		return m, nil
	}
	cmds := slices.Clone(m.access.Commands)
	i := slices.Index(cmds, cmd)
	switch {
	case add && i >= 0:
		m.setStatus("%s is already listed", cmd)
		return m, nil
	case !add && i < 0:
		m.setStatus("%s is not listed", cmd)
		return m, nil
	case add:
		cmds = append(cmds, cmd)
	default:
		cmds = slices.Delete(cmds, i, i+1)
	}
	m.setStatus("saving the command list...")
	return m, setAccessCmd(m.ctx, m.svc, ports.AccessChange{Level: m.access.Level, Commands: cmds})
}

// moveLevelSelection moves the settings tab's level cursor.
func (m Model) moveLevelSelection(delta int) Model {
	m.levelSel = clampInt(m.levelSel+delta, 0, len(domain.AccessLevels)-1)
	return m
}

// accessChip is the header's access chip, nothing until the settings load.
func (m Model) accessChip() line {
	if !m.accessLoaded {
		return nil
	}
	return line{m.accessCell(), txt(" ")}
}

// accessCell shows the access level, coloured by how much it allows.
func (m Model) accessCell() cell {
	l := m.access.Level
	text := fmt.Sprintf(" L%d %s ", int(l), strings.ToLower(l.Name()))
	switch l {
	case domain.AccessFilesOnly:
		return st(text, badgeQuietStyle)
	case domain.AccessAnyCommand:
		return st(text, badgeErrStyle)
	}
	return st(text, badgeWarnStyle)
}

// settingsLines builds the settings tab and, per line, the level it shows
// (-1 for none).
func (m Model) settingsLines(inner int) ([]line, []int) {
	var out []line
	var owners []int
	add := func(l line, owner int) {
		out = append(out, l)
		owners = append(owners, owner)
	}
	para := func(text string, style lipgloss.Style) {
		for _, w := range wrapWords(text, maxInt(10, inner-2)) {
			add(line{txt("  "), st(w, style)}, -1)
		}
	}
	a := m.access
	where := ""
	if a.File != "" {
		where = " " + dot() + " saved to " + a.File
	}
	add(line{st("ACCESS", sectionHeaderStyle), st("  what agents may run in this workspace"+where, dimStyle)}, -1)
	if !m.accessLoaded {
		add(line{st("  loading...", dimStyle)}, -1)
		return out, owners
	}
	if a.Locked != "" {
		para(a.Locked, warnStyle)
	}
	if a.HeldBack != "" {
		para("held back: "+a.HeldBack+". Choose the level here (and :allow the commands) to approve it for this folder.", warnStyle)
	}
	sel := clampInt(m.levelSel, 0, len(domain.AccessLevels)-1)
	for i, l := range domain.AccessLevels {
		mark := "    "
		if i == sel {
			mark = "  " + gl().caret + " "
		}
		row := line{txt(mark), st(fmt.Sprintf("%d  ", int(l)), emphStyle), st(fitWidth(l.Name(), 19), bodyStyle),
			st(l.Summary(), dimStyle)}
		if l == a.Level {
			row = row.join(st("  "+gl().done+" current", lipgloss.NewStyle().Foreground(colorGreen)))
		}
		if i == sel && m.paneIsActive(paneConversation) {
			row = line{st(fitEllipsis(row.plain(), inner), reverseStyle)}
		}
		add(row, i)
	}
	add(line{st("  enter applies the selected level (raising it asks you to confirm) "+dot()+" :access <0-3>", dimStyle)}, -1)

	add(blank(), -1)
	add(line{st("COMMANDS", sectionHeaderStyle), st("  run by the sandboxed builders at level 1, by every agent at level 2", dimStyle)}, -1)
	if len(a.Commands) == 0 {
		para("none listed: at levels 1 and 2 agents can run nothing yet. :allow cmake adds one.", warnStyle)
	} else {
		para(strings.Join(a.Commands, "  "+dot()+"  "), bodyStyle)
	}
	if len(a.Blocked) > 0 {
		para("always blocked, never run: "+strings.Join(a.Blocked, ", "), warnStyle)
	}
	add(line{st("  :allow <command> adds one "+dot()+" :disallow <command> removes one", dimStyle)}, -1)

	add(blank(), -1)
	add(line{st("BUILDERS", sectionHeaderStyle), st("  agents whose CLI runs commands in a sandbox", dimStyle)}, -1)
	if len(a.Builders) == 0 {
		para("none on the team: at level 1 nobody can build, and agents ask you instead. Recruit a Codex agent to have one.", dimStyle)
	} else {
		names := make([]string, len(a.Builders))
		for i, id := range a.Builders {
			names[i] = string(id)
		}
		para(strings.Join(names, ", "), bodyStyle)
	}

	add(blank(), -1)
	add(line{st("ALWAYS BLOCKED", sectionHeaderStyle), st("  at every level, whatever the settings say", dimStyle)}, -1)
	para(strings.Join(domain.AlwaysBlocked, "  "+dot()+"  "), bodyStyle)

	add(blank(), -1)
	add(line{st("RISK", sectionHeaderStyle)}, -1)
	para("Codex runs its commands in a sandbox: writes stay inside the workspace, the network is off.", dimStyle)
	para("Claude Code has no sandbox here. At level 2 it runs the listed commands directly on this machine, "+
		"and level 3 means \"I trust these agents\": the blocked list is a deny list, not a sandbox.", warnStyle)
	return out, owners
}

// settingsBox draws the settings tab, keeping the selected level in view.
func (m Model) settingsBox(width, height int) []string {
	all, owners := m.settingsLines(width - 2)
	first, last := -1, -1
	for i, o := range owners {
		if o == m.levelSel && o >= 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	off := followSelection(m.scrollOf(paneConversation), first, last, height-2, len(all))
	right := ""
	if m.accessLoaded {
		right = "level " + itoa(int(m.access.Level))
	}
	return renderBox(boxSpec{title: "settings", right: right, active: m.paneIsActive(paneConversation)},
		topWindow(all, height-2, off), width, height)
}
