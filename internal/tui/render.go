package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// Layout primitives.
//
// Every pane in this UI is a fixed-size rectangle: exactly H rows of exactly W
// visible columns. That is not fussiness -- a three-column layout is built by
// zipping columns row by row, so one row that renders a single column too wide
// shears the whole screen. lipgloss cannot be trusted to do the fitting for us
// either, because it measures styled strings and silently wraps rather than
// truncating.
//
// The rule this file enforces is the same one the rest of the package already
// followed: measure and truncate *plain* text, apply style afterwards. A
// `line` is a sequence of plain runs each tagged with a style, so a row can be
// multi-coloured and still be fitted to an exact width without ever slicing
// through an ANSI escape.

// cell is one plain-text run plus the style to paint it with.
type cell struct {
	text  string
	style lipgloss.Style
	plain bool
	// w overrides the measured width. It is set only by raw(), for content
	// that already carries ANSI escapes and therefore cannot be measured (or
	// cut) by rune count.
	w int
}

// txt is an unstyled run.
func txt(s string) cell { return cell{text: s, plain: true} }

// st is a styled run.
func st(s string, style lipgloss.Style) cell { return cell{text: s, style: style} }

// raw wraps an already-styled, already-width-fitted string, declaring the
// visible width it occupies.
//
// It exists for the one case the measure-then-style rule cannot cover:
// embedding a block another renderer has already produced (the per-agent
// columns). Slicing such a string by rune count would cut through an escape
// sequence and corrupt every colour that follows, so raw content is never cut
// -- callers must pass something that already fits.
func raw(s string, visible int) cell {
	return cell{text: s, plain: true, w: visible}
}

// width reports the cell's visible width.
func (c cell) width() int {
	if c.w > 0 {
		return c.w
	}
	return utf8.RuneCountInString(c.text)
}

// line is one row of mixed-style content, not yet fitted to any width.
type line []cell

// join concatenates runs onto a line.
func (l line) join(more ...cell) line { return append(l, more...) }

// width reports the visible width of the line, in runes.
func (l line) width() int {
	n := 0
	for _, c := range l {
		n += c.width()
	}
	return n
}

// visibleRunes counts the display columns of a string that may already carry
// ANSI escape sequences, which len([]rune) would count as content.
func visibleRunes(s string) int {
	n, inEscape := 0, false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEscape = true
		case inEscape:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
		default:
			n++
		}
	}
	return n
}

// plain returns the line's text with all styling dropped. It is what the
// selected-row path renders (selection is reverse video over the whole row,
// never a recolouring of its parts) and what tests can assert against.
func (l line) plain() string {
	var b strings.Builder
	for _, c := range l {
		b.WriteString(c.text)
	}
	return b.String()
}

// render fits the line to exactly width visible columns -- truncating with an
// ellipsis, or padding with spaces -- and paints each run.
func (l line) render(width int) string {
	if width < 1 {
		return ""
	}
	total := l.width()
	if total <= width {
		return l.paint(width, -1)
	}
	// Reserve the last column for the ellipsis marker.
	return l.paint(width, width-len([]rune(gl().ellipsis)))
}

// paint writes the runs, cutting at `cut` runes (negative = no cut) and
// padding out to width.
func (l line) paint(width, cut int) string {
	var b strings.Builder
	used := 0
	limit := width
	if cut >= 0 {
		limit = cut
	}
	for _, c := range l {
		if used >= limit {
			break
		}
		if c.w > 0 {
			// Pre-rendered content: emit whole or not at all. See raw().
			if used+c.w > limit {
				break
			}
			b.WriteString(c.text)
			used += c.w
			continue
		}
		// Count runes without materialising a []rune: only the one cell that
		// straddles the cut needs slicing, and most cells fit whole.
		text, n := c.text, utf8.RuneCountInString(c.text)
		if used+n > limit {
			n = limit - used
			text = cutRunes(text, n)
		}
		used += n
		if c.plain {
			b.WriteString(text)
		} else {
			b.WriteString(c.style.Render(text))
		}
	}
	if cut >= 0 {
		b.WriteString(dimStyle.Render(gl().ellipsis))
		used += len([]rune(gl().ellipsis))
	}
	if used < width {
		b.WriteString(strings.Repeat(" ", width-used))
	}
	return b.String()
}

// cutRunes returns the first n runes of s.
func cutRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// renderSelected paints the whole row as reverse video. Per the design,
// selection is never signalled by introducing a new colour -- with six agent
// colours already in play a seventh would be unreadable -- so the row's own
// styling is dropped for the duration.
func (l line) renderSelected(width int) string {
	return reverseStyle.Render(fitWidth(fitEllipsis(l.plain(), width), width))
}

// fitEllipsis truncates plain text to width, marking the cut so a clipped
// value is never mistaken for a short one.
func fitEllipsis(s string, width int) string {
	if width < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	e := []rune(gl().ellipsis)
	if width <= len(e) {
		return string(r[:width])
	}
	return string(r[:width-len(e)]) + gl().ellipsis
}

// spread lays left-aligned and right-aligned content on one row.
func spread(left, right line, width int) line {
	gap := width - left.width() - right.width()
	if gap < 1 {
		gap = 1
	}
	out := append(line{}, left...)
	out = append(out, txt(strings.Repeat(" ", gap)))
	return append(out, right...)
}

// blank is an empty row, used to pad a pane to its height budget.
func blank() line { return line{} }

// ---------------------------------------------------------------------------
// Boxes
// ---------------------------------------------------------------------------

// boxSpec describes one bordered pane.
type boxSpec struct {
	title string
	// right is the small label in the top-right of the border, e.g. a count
	// or a hotkey hint.
	right string
	// active marks the pane the scroll keys act on; it is drawn in the focus
	// colour so the reader can see where j/k will go.
	active bool
	// accent overrides the border colour (used by the decision gate, which is
	// a pending human action rather than a focus target).
	accent *lipgloss.Color
}

// renderBox draws a bordered pane of exactly `height` rows and `width`
// columns, with body lines fitted into the interior. Body content shorter
// than the interior is padded; longer content is truncated by the caller
// (which owns scrolling) rather than silently dropped here.
//
// The title lives in the top border rather than on its own row: at 24-row
// terminals with three stacked panes, a separate header row per pane costs
// more than it communicates.
func renderBox(spec boxSpec, body []line, width, height int) []string {
	g := gl()
	if width < 4 {
		width = 4
	}
	if height < 2 {
		height = 2
	}

	border := dimStyle
	switch {
	case spec.accent != nil:
		border = lipgloss.NewStyle().Foreground(*spec.accent)
	case spec.active:
		border = lipgloss.NewStyle().Foreground(colorCyan)
	}

	inner := width - 2
	out := make([]string, 0, height)
	out = append(out, borderRow(spec, border, inner, g))

	// The side border is identical on every row, so it is styled once. lipgloss
	// Render is the dearest thing in a frame (about a microsecond and a handful
	// of allocations per call) and a tall pane used to pay it twice per row.
	side := border.Render(g.vert)
	blankRow := strings.Repeat(" ", inner)
	bodyRows := height - 2
	for i := 0; i < bodyRows; i++ {
		content := blankRow
		if i < len(body) {
			content = body[i].render(inner)
		}
		out = append(out, side+content+side)
	}
	out = append(out, border.Render(g.botLeft+strings.Repeat(g.horiz, inner)+g.botRight))
	return out
}

// borderRow draws the top border with the title and the right-hand label
// inlaid: "┌─ AGENTS ──────────── [1] ─┐".
func borderRow(spec boxSpec, border lipgloss.Style, inner int, g glyphSet) string {
	titleStyleFor := labelStyle.Foreground(colorGray)
	if spec.active {
		titleStyleFor = labelStyle.Foreground(colorCyan)
	}
	if spec.accent != nil {
		titleStyleFor = labelStyle.Foreground(*spec.accent)
	}

	l := line{st(g.horiz, border)}
	if spec.title != "" {
		l = l.join(txt(" "), st(strings.ToUpper(spec.title), titleStyleFor), txt(" "))
	}
	right := line{}
	if spec.right != "" {
		right = line{txt(" "), st(spec.right, dimStyle), st(" "+g.horiz, border)}
	}

	used := l.width() + right.width()
	fill := inner - used
	if fill < 0 {
		// Title alone does not fit: drop the right label, then truncate.
		right = line{}
		fill = inner - l.width()
	}
	if fill > 0 {
		l = l.join(st(strings.Repeat(g.horiz, fill), border))
	}
	l = append(l, right...)

	return border.Render(g.topLeft) + l.render(inner) + border.Render(g.topRight)
}

// ---------------------------------------------------------------------------
// Columns
// ---------------------------------------------------------------------------

// column is a vertical stack of already-rendered, fixed-width rows.
type column struct {
	width int
	rows  []string
}

// joinColumns zips columns side by side into `height` rows, separated by a
// single space. Columns shorter than height are padded with blank rows of
// their own width so the separators stay aligned all the way down.
func joinColumns(height int, cols ...column) []string {
	out := make([]string, 0, height)
	for r := 0; r < height; r++ {
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			if c.width <= 0 {
				continue
			}
			if r < len(c.rows) {
				parts = append(parts, c.rows[r])
			} else {
				parts = append(parts, strings.Repeat(" ", c.width))
			}
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

// windowLines selects the visible slice of a pane's content for a given
// height and scroll offset (0 = pinned to the newest line), padding short
// content so the pane keeps its shape.
func windowLines(all []line, height, scroll int) []line {
	if height < 1 {
		height = 1
	}
	if len(all) <= height {
		out := append([]line{}, all...)
		for len(out) < height {
			out = append(out, blank())
		}
		return out
	}
	maxScroll := len(all) - height
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	end := len(all) - scroll
	return all[end-height : end]
}

// topWindow is windowLines for content read from the top rather than tailed
// like a log: a roster, a round history, an inspector. Its scroll offset
// counts *down from the first line*, the opposite of a log's, because the
// first line is the one that must never scroll away on its own.
func topWindow(all []line, height, scroll int) []line {
	if height < 1 {
		height = 1
	}
	if scroll < 0 {
		scroll = 0
	}
	if max := len(all) - height; scroll > max {
		scroll = max
	}
	if scroll < 0 {
		scroll = 0
	}
	out := append([]line{}, all[scroll:]...)
	if len(out) > height {
		out = out[:height]
	}
	for len(out) < height {
		out = append(out, blank())
	}
	return out
}

// topLines is topWindow pinned to the first line, for content that is never
// scrolled (menus, forms).
func topLines(all []line, height int) []line { return topWindow(all, height, 0) }

// hintRow renders the compact "N more above / below" marker used at the foot
// of a windowed pane.
func hintRow(all []line, height, scroll, hiddenAbove int) (line, bool) {
	if len(all) <= height && hiddenAbove == 0 {
		return nil, false
	}
	below := scroll
	above := len(all) - height - scroll
	if above < 0 {
		above = 0
	}
	above += hiddenAbove
	switch {
	case above > 0 && below > 0:
		return line{st("  ^"+itoa(above)+" more   v"+itoa(below)+" more", dimStyle)}, true
	case above > 0:
		return line{st("  ^"+itoa(above)+" more", dimStyle)}, true
	case below > 0:
		return line{st("  v"+itoa(below)+" more", dimStyle)}, true
	}
	return nil, false
}
