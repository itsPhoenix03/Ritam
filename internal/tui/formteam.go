package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The new-task form's team: Recruit, then Participants.
//
// Recruit lists every model the providers offer. Tick the ones the task
// needs and say how many agents of each. Participants is built only from
// that, and every participant is named after its model: opus-1, opus-2,
// sonnet-1, ... The agents defined in ritam.yaml (and any other name) are
// never pulled in; a name of this form already on the team for the same
// model is simply used again, since an agent is only a definition and each
// task gets its own instance of it.
//
// The authority is chosen in Participants with a checkbox, by the human
// only: nobody is pre-selected, and the task cannot start without one. On
// create the agents not yet on the team are recruited, then the task
// starts.

// Form fields, in tab order.
const (
	fieldTitle = iota
	fieldDescription
	fieldRecruit
	fieldParticipants
	fieldBuilder
)

// maxRecruitCount caps how many agents of one model the form takes.
const maxRecruitCount = 9

// formMember is one participant the task would have.
type formMember struct {
	id       domain.AgentID
	what     string // e.g. "claude-code · opus"
	existing bool   // the name is already on the team for this model
	entry    int    // index into recruitEntries()
}

// formNeedsModels reports whether the new-task form is open and the model
// list has not been asked for yet.
func (m Model) formNeedsModels() bool {
	return m.mode == modeCreateForm && m.models == nil && !m.modelsLoading && m.modelsErr == nil
}

// agentRuns reports whether agent a runs model mi. A mock agent stands in
// for any provider, so only its model is compared.
func agentRuns(a domain.Agent, mi ports.ModelInfo) bool {
	if a.Model != mi.ID {
		return false
	}
	return a.Kind == domain.AgentKindMock || string(a.Kind) == mi.Provider
}

// recruitEntries is what Recruit lists: the catalogs' models.
func (m Model) recruitEntries() []ports.ModelInfo {
	return m.modelList()
}

// entryWhat describes an entry, e.g. "claude-code · opus".
func entryWhat(mi ports.ModelInfo) string {
	return mi.Provider + " " + dot() + " " + mi.ID
}

// formMembers lists the task's participants: for each ticked entry, as many
// agents as asked, named <model>-1, <model>-2, ...
func (m Model) formMembers() []formMember {
	entries := m.recruitEntries()
	taken := map[string]bool{}
	var out []formMember
	for i, mi := range entries {
		base := modelBase(mi.ID)
		for k, n := 1, m.form.recruit[i]; n > 0; k++ {
			name := fmt.Sprintf("%s-%d", base, k)
			if taken[name] {
				continue
			}
			a, exists := m.agentByID(domain.AgentID(name))
			if exists && !agentRuns(a, mi) {
				continue // the name belongs to an agent on another model
			}
			taken[name] = true
			out = append(out, formMember{id: domain.AgentID(name), what: entryWhat(mi), existing: exists, entry: i})
			n--
		}
	}
	return out
}

// formAuthority is the participant the human ticked as authority, if it is
// still a participant.
func (m Model) formAuthority() (formMember, bool) {
	for _, fm := range m.formMembers() {
		if fm.id == m.form.authority {
			return fm, true
		}
	}
	return formMember{}, false
}

// formRecruits is the new agents the form will recruit.
func (m Model) formRecruits() []ports.RecruitInput {
	entries := m.recruitEntries()
	var out []ports.RecruitInput
	for _, fm := range m.formMembers() {
		if !fm.existing {
			mi := entries[fm.entry]
			// Writing is still confined to the workspace, and turned off
			// when ritam runs somewhere it may not write.
			out = append(out, ports.RecruitInput{Name: fm.id, Provider: mi.Provider, Model: mi.ID, AllowWrites: true})
		}
	}
	return out
}

// updateFormTeam handles keys on the recruit and participants fields.
func (m Model) updateFormTeam(msg tea.KeyMsg) Model {
	k := msg.String()
	switch m.form.focus {
	case fieldRecruit:
		entries := m.recruitEntries()
		if len(entries) == 0 {
			m.setStatus("no models listed yet")
			return m
		}
		if m.form.recruit == nil {
			m.form.recruit = map[int]int{}
		}
		c := clampInt(m.form.recruitCursor, 0, len(entries)-1)
		if entries[c].Unavailable && (k == " " || k == "right" || k == "+" || k == "=" ||
			(len(k) == 1 && k[0] >= '1' && k[0] <= '9')) {
			m.setStatus("%s cannot run on this account: %s (c checks again)", entries[c].ID, entries[c].Reason)
			return m
		}
		switch {
		case k == "up":
			m.form.recruitCursor = maxInt(0, c-1)
		case k == "down":
			m.form.recruitCursor = minInt(len(entries)-1, c+1)
		case k == " ":
			if m.form.recruit[c] > 0 {
				m.form.recruit[c] = 0
			} else {
				m.form.recruit[c] = 1
			}
		case k == "right" || k == "+" || k == "=":
			m.form.recruit[c] = minInt(maxRecruitCount, m.form.recruit[c]+1)
		case k == "left" || k == "-":
			m.form.recruit[c] = maxInt(0, m.form.recruit[c]-1)
		case len(k) == 1 && k[0] >= '0' && k[0] <= '9':
			m.form.recruit[c] = int(k[0] - '0')
		}

	case fieldBuilder:
		opts := m.builderOptions()
		i := max(0, slices.Index(opts, m.formBuilder()))
		switch k {
		case "right", " ", "down":
			m.form.builder = opts[(i+1)%len(opts)]
		case "left", "up":
			m.form.builder = opts[(i-1+len(opts))%len(opts)]
		}

	case fieldParticipants:
		members := m.formMembers()
		if len(members) == 0 {
			m.setStatus("tick models under Recruit first")
			return m
		}
		c := clampInt(m.form.cursor, 0, len(members)-1)
		switch k {
		case "up":
			m.form.cursor = maxInt(0, c-1)
		case "down":
			m.form.cursor = minInt(len(members)-1, c+1)
		case " ":
			// The authority checkbox: one participant, picked by the human.
			if m.form.authority == members[c].id {
				m.form.authority = ""
			} else {
				m.form.authority = members[c].id
			}
		}
	}
	return m
}

// formTeamLines renders the recruit and participants fields.
func (m Model) formTeamLines(inner int) []line {
	g := gl()
	// head is a field's title, with its keys beside it when they fit and
	// wrapped beneath it when they do not.
	head := func(field int, label, keys string) []line {
		prefix, sty := "  ", dimStyle
		if m.form.focus == field {
			prefix, sty = g.caret+" ", sectionHeaderStyle
		}
		l := line{st(prefix+label, sty)}
		if keys == "" {
			return []line{l}
		}
		if beside := l.join(st("  "+keys, dimStyle)); beside.width() <= inner {
			return []line{beside}
		}
		out := []line{l}
		for _, w := range wrapWords(keys, maxInt(8, inner-4)) {
			out = append(out, line{txt("    "), st(w, dimStyle)})
		}
		return out
	}
	var out []line

	// Recruit: every model; tick the ones wanted and how many.
	out = append(out, blank())
	out = append(out, head(fieldRecruit, "Recruit",
		keyLegend("up/down move", "space select", "left/right or 0-9 how many", "c check it runs on your account"))...)
	entries := m.recruitEntries()
	switch {
	case m.modelsLoading:
		out = append(out, line{st("    loading the providers' model lists...", dimStyle)})
	case m.modelsErr != nil:
		out = append(out, line{st("    could not list models: "+collapse(m.modelsErr.Error()), warnStyle)})
	case len(entries) == 0:
		out = append(out, line{st("    no models listed", dimStyle)})
	}
	cur := clampInt(m.form.recruitCursor, 0, maxInt(0, len(entries)-1))
	for i, mi := range entries {
		focused := m.form.focus == fieldRecruit && i == cur
		mark := "  "
		if focused {
			mark = g.caret + " "
		}
		n := m.form.recruit[i]
		box := "[ ]"
		if n > 0 {
			box = "[x]"
		}
		label := mi.Label
		if label == "" {
			label = mi.ID
		}
		what := entryWhat(mi)
		row := line{txt("  " + mark + box + " "), st(fitWidth(label, 16), bodyStyle), st(fitWidth(what, 40), dimStyle)}
		if mi.Unavailable || mi.Checked {
			row = row.join(availabilityCell(mi))
		}
		if mi.Builds {
			row = row.join(buildsCell())
		}
		if n > 0 {
			row = row.join(st(fmt.Sprintf("x%d", n), emphStyle))
		}
		if focused {
			row = line{st(fitEllipsis(row.plain(), inner), reverseStyle)}
		}
		out = append(out, row)
	}

	// Participants: built from Recruit; the authority is ticked here.
	members := m.formMembers()
	summary := fmt.Sprintf("%d agents", len(members))
	out = append(out, blank())
	out = append(out, head(fieldParticipants, "Participants: "+summary,
		keyLegend("up/down move", "space tick the authority"))...)
	if len(members) == 0 {
		out = append(out, line{st("    none yet: tick the models the task needs under Recruit", dimStyle)})
	}
	authority, hasAuthority := m.formAuthority()
	pc := clampInt(m.form.cursor, 0, maxInt(0, len(members)-1))
	for i, fm := range members {
		mark := " "
		if m.form.focus == fieldParticipants && i == pc {
			mark = g.caret
		}
		box := "[ ]"
		if hasAuthority && fm.id == authority.id {
			box = "[x]"
		}
		row := line{txt("   " + mark + " " + box + " "), st(fitWidth(string(fm.id), 14), bodyStyle), st(fm.what, dimStyle)}
		if hasAuthority && fm.id == authority.id {
			row = row.join(st("  authority", sectionHeaderStyle))
		}
		out = append(out, row)
	}
	switch {
	case len(members) == 1:
		out = append(out, blank(), line{st("  Recruit at least two agents: the authority needs somebody to delegate to.", warnStyle)})
	case len(members) >= 2 && !hasAuthority:
		out = append(out, blank(), line{st("  Tick one participant as the authority.", warnStyle)})
	}

	// Builder: who runs builds and tests.
	b := m.formBuilder()
	out = append(out, blank())
	out = append(out, head(fieldBuilder, "Builder: "+g.arrow+" "+domain.BuilderLabel(b), keyLegend("left/right choose"))...)
	text, warn := m.builderNote(b)
	sty := dimStyle
	if warn {
		sty = warnStyle
	}
	for _, w := range wrapWords(text, maxInt(8, inner-4)) {
		out = append(out, line{txt("    "), st(w, sty)})
	}
	return out
}

// builderOptions are the Builder field's choices: auto, manual, then each
// participant.
func (m Model) builderOptions() []string {
	opts := []string{domain.BuilderAuto, domain.BuilderManual}
	for _, fm := range m.formMembers() {
		opts = append(opts, string(fm.id))
	}
	return opts
}

// formBuilder is the Builder field's choice, auto when the participant
// chosen has since left the form.
func (m Model) formBuilder() string {
	if slices.Contains(m.builderOptions(), m.form.builder) {
		return m.form.builder
	}
	return domain.BuilderAuto
}

// memberBuilds reports whether a participant's model passed the sandbox
// check.
func (m Model) memberBuilds(id domain.AgentID) bool {
	entries := m.recruitEntries()
	for _, fm := range m.formMembers() {
		if fm.id == id && fm.entry < len(entries) {
			return entries[fm.entry].Builds
		}
	}
	return false
}

// builderNote explains a Builder choice under the workspace's access level,
// and whether it will not work as chosen.
func (m Model) builderNote(b string) (string, bool) {
	level := m.access.Level
	switch b {
	case domain.BuilderManual:
		return "You run the builds and tests; the agents ask you, and the task stays open until you answer.", false
	case domain.BuilderAuto:
		switch level {
		case domain.AccessFilesOnly:
			return "Access level 0: no agent may build, so you do (the task stays open until you answer). Raise it in :settings.", false
		case domain.AccessSandboxedBuilder:
			var who []string
			for _, fm := range m.formMembers() {
				if m.memberBuilds(fm.id) {
					who = append(who, string(fm.id))
				}
			}
			if len(who) == 0 {
				return "Participants whose sandbox check passed build; others declaring a sandbox are checked when the task starts (one short call). With none, you build.", false
			}
			return strings.Join(who, ", ") + " build, each in build/<agent>/; the others ask them.", false
		}
		return fmt.Sprintf("Access level %d: every agent may build, each in its own build/<agent>/ folder.", int(level)), false
	}
	switch {
	case level == domain.AccessFilesOnly:
		return "Access level 0: no agent may build. Choose Manual, or raise the level in :settings.", true
	case level == domain.AccessSandboxedBuilder && !m.memberBuilds(domain.AgentID(b)):
		return b + " has not passed the sandbox check, and at level 1 only sandboxed agents build: it is checked when the task starts.", true
	}
	return "Only " + b + " runs builds and tests, in build/" + b + "/; the others ask it.", false
}

// formSubmission is the task the form describes: its authority, the
// existing agents taking part, and the agents to recruit for it.
func (m Model) formSubmission() (domain.AgentID, []domain.AgentID, []ports.RecruitInput) {
	authority, ok := m.formAuthority()
	if !ok {
		return "", nil, nil
	}
	var existing []domain.AgentID
	for _, fm := range m.formMembers() {
		if fm.existing {
			existing = append(existing, fm.id)
		}
	}
	return authority.id, existing, m.formRecruits()
}
