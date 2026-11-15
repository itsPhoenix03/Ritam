package tui

import (
	"strings"
	"time"
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

// The paste defence.
//
// Where the terminal supports bracketed paste, a paste arrives as one key
// message and is typed as text. The native Windows console does not: it
// delivers a paste as one key event per character, each newline an Enter, so
// a multi-line paste into the instruction prompt used to send its first line
// and run the rest as hotkeys (q quits, c cancels). Two rules stop that:
//
//   - keys arriving closer together than burstGap are a paste, not typing:
//     in a text field Enter and Tab type a space instead of submitting or
//     moving on, and destructive commands are ignored;
//   - for textGuard after a text mode is left, the destructive single-key
//     commands (quit, cancel, approve, reject) are ignored, in case the text
//     ended early and what follows was meant as text.
const (
	// burstGap is far below the gap between two keys typed by hand (tens of
	// milliseconds at the fastest) and well above the gap between the key
	// events of one paste, which are handled back to back: only an Update
	// and a View (a few milliseconds on a busy screen) separate them.
	burstGap = 15 * time.Millisecond
	// textGuard is about the time it takes to notice a prompt has closed.
	textGuard = 150 * time.Millisecond
)

// clock is the time the paste defence reads.
func (m Model) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// noteKey stamps the key about to be handled and decides whether it is part
// of a burst.
func (m Model) noteKey() Model {
	now := m.clock()
	m.burst = !m.lastKeyAt.IsZero() && now.Sub(m.lastKeyAt) >= 0 && now.Sub(m.lastKeyAt) < burstGap
	m.lastKeyAt, m.keyAt = now, now
	return m
}

// guarded reports whether a destructive single-key command arriving now must
// be ignored: it came within a paste, or just after a text field closed.
func (m Model) guarded() bool {
	if m.burst {
		return true
	}
	return !m.leftTextAt.IsZero() && m.keyAt.Sub(m.leftTextAt) < textGuard
}

// isDestructive reports whether an action ends or decides something that
// cannot be taken back with a key.
func isDestructive(a Action) bool {
	switch a {
	case ActionQuit, ActionCancelTask, ActionApprove, ActionReject:
		return true
	}
	return false
}

// pastedBreak reports whether a key that would submit or leave a text field
// (Enter, Tab) is part of a paste, and so is text: a line break or a tab in
// what was pasted.
func (m Model) pastedBreak(msg tea.KeyMsg) bool {
	if !m.burst {
		return false
	}
	switch msg.String() {
	case "enter", "tab", "shift+tab", "ctrl+j":
		return true
	}
	return false
}

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
// Widths are in columns, so a row of CJK text holds half as many characters.
func wrapKeepingSpaces(s string, width int) []string {
	if s == "" {
		return []string{""}
	}
	var rows []string
	for textWidth(s) > width {
		head, _ := splitWidth(s, width)
		// Break after the last space in the second half of the row, if any.
		if i := strings.LastIndexByte(head, ' '); i >= 0 && textWidth(head[:i+1]) > width/2 {
			head = head[:i+1]
		}
		rows = append(rows, head)
		s = s[len(head):]
	}
	return append(rows, s)
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
	if m.mode != modeCommand && m.mode != modePrompt && m.mode != modeConfirm {
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
	case modeConfirm:
		var out []line
		for i, l := range wrapWords(m.confirm.label, maxInt(8, inner-2)) {
			p := txt("  ")
			if i == 0 {
				p = st(g.caret+" ", warnStyle)
			}
			out = append(out, line{p, st(l, warnStyle)})
		}
		return out
	}
	return nil
}
