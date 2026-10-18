package tui

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Text fields: the instruction prompt, the ":" command line and the new-task
// form.
//
// What the human types is shown in full, wrapped over as many rows as it
// needs (up to a cap, past which it scrolls with the end in view), rather
// than clipped to one line: an instruction is often a paragraph, and you
// should be able to read all of it before sending it. Editing is append-only
// at the end, plus three whole-field keys so a long text never needs a
// hundred backspaces:
//
//	ctrl+a  select all: typing replaces it, backspace clears it
//	ctrl+u  clear the field
//	ctrl+w  delete the last word

// keyLegend joins key hints with the separator dot (ASCII under RITAM_ASCII).
func keyLegend(hints ...string) string {
	return strings.Join(hints, " "+dot()+" ")
}

// inputHelp is the key legend shown beside a text field.
func inputHelp() string {
	return keyLegend("ctrl+a select all", "ctrl+u clear", "ctrl+w word")
}

// editField applies one key press to a text field holding value. selected is
// whether the whole value is selected (ctrl+a). It returns the new value and
// selection, and whether the key was a text-editing key at all; any other
// key leaves the value alone and clears the selection.
func editField(value string, selected bool, msg tea.KeyMsg) (string, bool, bool) {
	switch msg.String() {
	case "ctrl+a":
		return value, value != "", true
	case "ctrl+u":
		return "", false, true
	case "ctrl+w":
		if selected {
			return "", false, true
		}
		return deleteLastWord(value), false, true
	case "backspace", "delete":
		if selected {
			return "", false, true
		}
		return trimLastRune(value), false, true
	}
	if text := typedText(msg); text != "" {
		if selected {
			return text, false, true
		}
		return value + text, false, true
	}
	return value, false, false
}

// deleteLastWord removes the last word of s and the spaces after it.
func deleteLastWord(s string) string {
	r := []rune(s)
	i := len(r)
	for i > 0 && unicode.IsSpace(r[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(r[i-1]) {
		i--
	}
	return string(r[:i])
}

// wrapInput lays value out over rows of at most width columns, breaking at
// spaces where it can, with the caret at the end when focused. It returns at
// most maxRows rows -- the last ones, where the caret is -- and how many
// earlier rows were left out.
func wrapInput(value string, width, maxRows int, focused bool) ([]string, int) {
	if width < 2 {
		width = 2
	}
	caret := ""
	if focused {
		caret = gl().cursor
	}
	rows := wrapKeepingSpaces(value+caret, width)
	if maxRows > 0 && len(rows) > maxRows {
		hidden := len(rows) - maxRows
		return rows[hidden:], hidden
	}
	return rows, 0
}

// wrapKeepingSpaces wraps s at width, preferring to break after a space, and
// never drops or merges characters: what is shown is exactly what was typed.
func wrapKeepingSpaces(s string, width int) []string {
	r := []rune(s)
	if len(r) == 0 {
		return []string{""}
	}
	var rows []string
	for len(r) > width {
		cut := width
		for i := width; i > width/2; i-- {
			if r[i-1] == ' ' {
				cut = i
				break
			}
		}
		rows = append(rows, string(r[:cut]))
		r = r[cut:]
	}
	return append(rows, string(r))
}

// inputLines renders a text field: prefix on the first row, the wrapped value
// beside it, continuation rows indented to line up under the value. When the
// value needs more than maxRows, the first row says how many are hidden
// above and the rest show the end, where the caret is.
func inputLines(prefix line, value string, width, maxRows int, focused, selected bool) []line {
	indent := prefix.width()
	rows, hidden := wrapInput(value, width-indent, maxRows, focused)
	if hidden > 0 && maxRows > 1 {
		rows, hidden = wrapInput(value, width-indent, maxRows-1, focused)
	}
	sty := bodyStyle
	if selected {
		sty = reverseStyle
	}
	pad := line{txt(strings.Repeat(" ", indent))}
	out := make([]line, 0, len(rows)+1)
	if hidden > 0 {
		out = append(out, append(append(line{}, prefix...), st(gl().ellipsis+" "+itoa(hidden)+" earlier line(s)", dimStyle)))
		prefix = pad
	}
	for i, r := range rows {
		p := pad
		if i == 0 {
			p = prefix
		}
		out = append(out, append(append(line{}, p...), st(r, sty)))
	}
	return out
}

// inputBoxHeight is the bottom strip's height: the box around however many
// rows the text being typed needs.
func (m Model) inputBoxHeight() int {
	if m.mode != modeCommand && m.mode != modePrompt {
		return promptHeight
	}
	w, _ := m.screenSize()
	return maxInt(promptHeight, len(m.inputBodyLines(w-2))+2)
}

// maxInputRows caps how tall the bottom input strip may grow: a third of the
// screen, so the conversation stays in view while you write.
func (m Model) maxInputRows() int {
	_, h := m.screenSize()
	return maxInt(1, minInt(12, h/3))
}

// inputBodyLines is the bottom strip's content in prompt and command modes.
func (m Model) inputBodyLines(inner int) []line {
	g := gl()
	caret := st(g.caret+" ", lipgloss.NewStyle().Foreground(colorGreen))
	switch m.mode {
	case modeCommand:
		return inputLines(line{caret, st(":", sectionHeaderStyle)}, m.cmd.input, inner, m.maxInputRows(), true, m.inputSelected)
	case modePrompt:
		head := m.prompt.label
		if m.prompt.action == ActionSendInstruction && len(m.prompt.participants) > 0 {
			head = "to " + string(m.prompt.participants[m.prompt.targetIdx])
		}
		return inputLines(line{caret, st(head+" ", dimStyle)}, m.prompt.input, inner, m.maxInputRows(), true, m.inputSelected)
	}
	return nil
}
