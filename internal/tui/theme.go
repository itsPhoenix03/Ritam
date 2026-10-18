package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"ritam/internal/domain"
)

// This file is the single source of truth for the TUI's visual language:
// which colours exist, which glyphs exist, and how both degrade when the
// terminal cannot render them.
//
// Two rules drive everything here.
//
//  1. Sixteen colours only. The orchestrator is watched from plain Windows
//     Terminal / PowerShell / ssh sessions, so every colour is an ANSI index
//     (Campbell values quoted in comments) rather than a hex triple. A design
//     that only reads correctly under truecolor is a design that breaks on the
//     machine the tool is actually run from.
//
//  2. Colour is never the only carrier of meaning. Six agents exhaust the
//     distinguishable palette long before a real config does, so identity is
//     "sigil + colour" and state is "glyph + colour". With NO_COLOR set the
//     screen loses hue and loses nothing else.

// Campbell 16-colour palette, by ANSI index.
const (
	ansiBlack     = "0"  // #0c0c0c
	ansiRed       = "1"  // #c50f1f
	ansiGreen     = "2"  // #13a10e
	ansiYellow    = "3"  // #c19c00
	ansiBlue      = "4"  // #0037da
	ansiMagenta   = "5"  // #881798 (design uses this slot for agent 2)
	ansiCyan      = "6"  // #3a96dd
	ansiWhite     = "7"  // #cccccc -- body text
	ansiBrBlack   = "8"  // #767676 -- chrome, labels, thinking
	ansiBrRed     = "9"  // #e74856 -- ERROR / reject
	ansiBrGreen   = "10" // #16c60c -- WORKING / approve / cursor
	ansiBrYellow  = "11" // #f9f1a5 -- usage, pending, write-enabled
	ansiBrBlue    = "12" // #3b78ff
	ansiBrMagenta = "13" // #b4009e
	ansiBrCyan    = "14" // #61d6d6 -- chair / focus / active borders
	ansiBrWhite   = "15" // #f2f2f2 -- emphasis
)

// asciiOnly reports whether the UI must avoid box-drawing and block glyphs.
//
// It is read once at startup rather than per frame: the environment does not
// change mid-run, and View() is called many times a second.
var asciiOnly = os.Getenv("RITAM_ASCII") != "" || os.Getenv("NO_COLOR") != ""

// glyphs bundles every non-ASCII character the UI draws, so the fallback is a
// single table swap instead of conditionals scattered through the renderers.
type glyphSet struct {
	topLeft, topRight, botLeft, botRight string
	horiz, vert                          string
	teeLeft, teeRight                    string

	working, idle, errored, stopped, done string

	treeMid, treeEnd, treeOpen string
	arrow, caret, cursor       string
	ellipsis                   string
	sparks                     []string
	barFull, barEmpty          string
	// youBar marks the human's own messages down their left edge.
	youBar string
}

var unicodeGlyphs = glyphSet{
	topLeft: "┌", topRight: "┐", botLeft: "└", botRight: "┘",
	horiz: "─", vert: "│", teeLeft: "├", teeRight: "┤",
	working: "●", idle: "◐", errored: "✗", stopped: "○", done: "✓",
	treeMid: "│", treeEnd: "└", treeOpen: "▾",
	arrow: "↳", caret: "❯", cursor: "▌", ellipsis: "…",
	sparks:  []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"},
	barFull: "█", barEmpty: "░",
	youBar: "▌",
}

var asciiGlyphs = glyphSet{
	topLeft: "+", topRight: "+", botLeft: "+", botRight: "+",
	horiz: "-", vert: "|", teeLeft: "+", teeRight: "+",
	working: "*", idle: "o", errored: "x", stopped: ".", done: "v",
	treeMid: "|", treeEnd: "\\", treeOpen: "v",
	arrow: "->", caret: ">", cursor: "_", ellipsis: "~",
	sparks:  []string{".", ".", ":", "-", "=", "+", "*", "#"},
	barFull: "#", barEmpty: "-",
	youBar: "|",
}

// gl returns the active glyph set.
func gl() glyphSet {
	if asciiOnly {
		return asciiGlyphs
	}
	return unicodeGlyphs
}

// dot is the inline separator used between facts on one row. It is a function
// rather than a constant for the same reason the rest of this file is a table
// lookup: ASCII mode has to be able to swap it out.
func dot() string {
	if asciiOnly {
		return "-"
	}
	return "·"
}

// ---------------------------------------------------------------------------
// Agent identity: sigil + colour
// ---------------------------------------------------------------------------

// agentSlots is the six-colour identity rotation, in the order the design
// assigns it. Slot 0 is reserved for the first AUTHORITY-role agent, which is
// why it is bright cyan -- the same colour the UI uses for focus.
var agentSlots = []lipgloss.Color{
	lipgloss.Color(ansiBrCyan),
	lipgloss.Color(ansiMagenta),
	lipgloss.Color(ansiBrBlue),
	lipgloss.Color(ansiBrYellow),
	lipgloss.Color(ansiBrRed),
	lipgloss.Color(ansiBrGreen),
}

// agentIdent is one agent's stable on-screen identity for a single frame.
type agentIdent struct {
	agent domain.Agent
	// index is the agent's position in Model.agents, i.e. the number the
	// 1..9 focus keys use.
	index int
	// sigil is the bracketed letter that identifies the agent when colour is
	// unavailable or when two agents share a colour, e.g. "[C]".
	sigil string
	color lipgloss.Color
	// chair marks the authority of the task on screen. It does not influence
	// sigil or colour, which are stable for the session.
	chair bool
}

// style returns the agent's identity style.
func (a agentIdent) style() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(a.color)
}

// label renders "[X] codex" -- the canonical way to name an agent on screen.
func (a agentIdent) label() line {
	return line{st(a.sigil, a.style()), txt(" " + string(a.agent.ID))}
}

// identities assigns every agent a sigil and a colour.
//
// Identity is a property of the session, not of the task on screen: the reader
// learns "[X] is codex, cyan is the chair" once, and that must still hold after
// switching tasks. So sigils and colours are derived from the agent roster
// alone -- config order, with agents whose role is AUTHORITY listed first --
// and never from the selected task's authority. The first AUTHORITY-role agent
// keeps the reserved [C] sigil and slot 0 colour (bright cyan, the focus
// colour). Which agent chairs the *current* task is a separate fact, carried by
// agentIdent.chair and drawn as the tree marker, so it can change with the task
// without anything else on screen moving.
//
// Sigils are derived from the agent id and deduplicated, so two agents never
// collide even once the six colour slots wrap around.
func (m Model) identities() []agentIdent {
	if m.frameIdents != nil {
		return m.frameIdents
	}
	return m.computeIdentities()
}

func (m Model) computeIdentities() []agentIdent {
	if len(m.agents) == 0 {
		return nil
	}
	authority := domain.AgentID("")
	if t := m.currentTask(); t != nil {
		authority = t.AuthorityAgent
	}

	order := make([]int, 0, len(m.agents))
	for i, a := range m.agents {
		if a.Role == domain.RoleAuthority {
			order = append(order, i)
		}
	}
	reserved := len(order) > 0 // the first of them takes [C]
	for i, a := range m.agents {
		if a.Role != domain.RoleAuthority {
			order = append(order, i)
		}
	}

	used := map[string]bool{}
	if reserved {
		used["C"] = true
	}
	out := make([]agentIdent, 0, len(order))
	for slot, idx := range order {
		a := m.agents[idx]

		letter := "C"
		if !reserved || slot != 0 {
			letter = pickSigil(string(a.ID), used)
		}
		used[letter] = true

		out = append(out, agentIdent{
			agent: a,
			index: idx,
			sigil: "[" + letter + "]",
			color: agentSlots[slot%len(agentSlots)],
			chair: a.ID == authority,
		})
	}
	return out
}

// pickSigil chooses a single unused uppercase character for an agent id,
// preferring letters that actually occur in the name so the sigil is
// mnemonic ("codex" -> X once C is taken by the chair).
func pickSigil(id string, used map[string]bool) string {
	for _, r := range strings.ToUpper(id) {
		if r < 'A' || r > 'Z' {
			continue
		}
		if s := string(r); !used[s] {
			return s
		}
	}
	for r := 'A'; r <= 'Z'; r++ {
		if s := string(r); !used[s] {
			return s
		}
	}
	return "?"
}

// rosterIdents is who the agents pane lists: with a task selected, only its
// participants (the rest of the team is in the team tab); otherwise the
// whole team. Identities themselves stay team-wide, so an agent keeps its
// sigil and colour from task to task.
func (m Model) rosterIdents() []agentIdent {
	all := m.identities()
	t := m.currentTask()
	if t == nil || (t.AuthorityAgent == "" && len(t.Participants) == 0) {
		return all
	}
	out := make([]agentIdent, 0, len(all))
	for _, a := range all {
		if a.agent.ID == t.AuthorityAgent || containsAgent(t.Participants, a.agent.ID) {
			out = append(out, a)
		}
	}
	return out
}

// identFor looks up one agent's identity within a precomputed frame set.
func identFor(idents []agentIdent, id domain.AgentID) (agentIdent, bool) {
	for _, a := range idents {
		if a.agent.ID == id {
			return a, true
		}
	}
	return agentIdent{}, false
}

// ---------------------------------------------------------------------------
// State glyphs
// ---------------------------------------------------------------------------

// stateGlyph returns the glyph marking an agent's lifecycle state. The glyph
// carries the state on its own: with NO_COLOR the dot still differs between
// WORKING, IDLE, ERROR and STOPPED.
func stateGlyph(s domain.AgentState) string {
	g := gl()
	switch s {
	case domain.AgentStateWorking:
		return g.working
	case domain.AgentStateStarting, domain.AgentStateIdle, domain.AgentStateUnavailable:
		return g.idle
	case domain.AgentStateError:
		return g.errored
	default:
		return g.stopped
	}
}

// ---------------------------------------------------------------------------
// Sparklines and bars
// ---------------------------------------------------------------------------

// sparkline renders values as a fixed-width bar strip, scaled to the largest
// value in the window. An all-zero window renders as the lowest glyph rather
// than as blanks, so the reader can tell "nothing happened" apart from "this
// metric is not wired up".
func sparkline(values []int, width int) string {
	g := gl()
	if width < 1 {
		return ""
	}
	if len(values) == 0 {
		return strings.Repeat(g.sparks[0], width)
	}
	// Show the most recent `width` samples.
	if len(values) > width {
		values = values[len(values)-width:]
	}
	max := 0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	for _, v := range values {
		idx := 0
		if max > 0 {
			idx = v * (len(g.sparks) - 1) / max
		}
		b.WriteString(g.sparks[idx])
	}
	// Left-pad so the newest sample stays flush right as the window fills.
	if pad := width - len(values); pad > 0 {
		return strings.Repeat(" ", pad) + b.String()
	}
	return b.String()
}

// meter renders a proportional bar, e.g. "██░░" for 2 of 4.
func meter(n, total, width int) string {
	g := gl()
	if width < 1 || total <= 0 {
		return ""
	}
	filled := n * width / total
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat(g.barFull, filled) + strings.Repeat(g.barEmpty, width-filled)
}
