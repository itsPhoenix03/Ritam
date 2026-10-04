package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// Markdown.
//
// The agents write markdown: headings, bold claims, bullet lists, numbered
// findings, fenced code. Shown raw -- or worse, collapsed onto one line, as
// the stream used to -- a careful report reads as a wall of asterisks and
// backticks. renderMarkdown draws it the way the model meant it to look,
// within this package's rules: plain text is measured and wrapped, styles are
// attached per run, every line fits the width exactly, and nothing depends on
// colour (headings and emphasis stay distinguishable under NO_COLOR because
// their markers are turned into layout, not just into colour).
//
// It is deliberately a small subset of CommonMark: the constructs agents
// actually produce. Anything it does not recognise is shown as text, which is
// the right failure for a renderer whose input is arbitrary model output.

var (
	mdHeading1Style = lipgloss.NewStyle().Bold(true).Foreground(colorCyan)
	mdHeading2Style = lipgloss.NewStyle().Bold(true).Foreground(colorBright)
	mdBoldStyle     = lipgloss.NewStyle().Bold(true).Foreground(colorBright)
	mdItalicStyle   = lipgloss.NewStyle().Italic(true).Foreground(colorWhite)
	mdCodeStyle     = lipgloss.NewStyle().Foreground(colorYellow)
	mdLinkStyle     = lipgloss.NewStyle().Underline(true).Foreground(colorCyan)
	mdQuoteStyle    = lipgloss.NewStyle().Italic(true).Foreground(colorGray)
)

// mdSpan is a styled run of inline text.
type mdSpan struct {
	text  string
	style lipgloss.Style
}

// renderMarkdown renders src to lines no wider than width.
func renderMarkdown(src string, width int) []line {
	if width < 8 {
		width = 8
	}
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\t", "    ")
	lines := strings.Split(src, "\n")

	var out []line
	// blankBefore adds one separating blank line between blocks, never two
	// and never at the top.
	blankBefore := func() {
		if len(out) > 0 && len(out[len(out)-1]) > 0 {
			out = append(out, blank())
		}
	}

	for i := 0; i < len(lines); {
		raw := lines[i]
		trimmed := strings.TrimSpace(raw)

		switch {
		case trimmed == "":
			i++

		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fence := trimmed[:3]
			lang := strings.TrimSpace(trimmed[3:])
			blankBefore()
			if lang != "" {
				out = append(out, line{st(lang, dimStyle)})
			}
			i++
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), fence) {
				out = append(out, codeLines(lines[i], width)...)
				i++
			}
			i++ // the closing fence (or the end)
			out = append(out, blank())

		case isRule(trimmed):
			blankBefore()
			out = append(out, line{st(strings.Repeat(gl().horiz, width), dimStyle)})
			i++

		case headingLevel(trimmed) > 0:
			level := headingLevel(trimmed)
			text := strings.TrimSpace(strings.TrimRight(trimmed[level:], "#"))
			blankBefore()
			sty := mdHeading2Style
			if level == 1 {
				sty = mdHeading1Style
				text = strings.ToUpper(text)
			}
			for _, l := range wrapSpans(restyle(parseInline(text), sty), width, "", "") {
				out = append(out, l)
			}
			if level <= 2 {
				// Under NO_COLOR a heading is still a heading: it gets a rule.
				out = append(out, line{st(strings.Repeat(gl().horiz, minInt(width, utf8.RuneCountInString(text))), dimStyle)})
			}
			i++

		case strings.HasPrefix(trimmed, ">"):
			blankBefore()
			var para []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				para = append(para, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), ">")))
				i++
			}
			bar := gl().vert + " "
			for _, l := range wrapSpans(restyle(parseInline(strings.Join(para, " ")), mdQuoteStyle), width, bar, bar) {
				out = append(out, l)
			}

		case isTableRow(trimmed):
			blankBefore()
			var rows []string
			for i < len(lines) && isTableRow(strings.TrimSpace(lines[i])) {
				rows = append(rows, strings.TrimSpace(lines[i]))
				i++
			}
			out = append(out, tableLines(rows, width)...)

		case listMarker(raw) != "":
			// A run of list items, each with its continuation lines.
			if len(out) > 0 && !isListLine(out) {
				blankBefore()
			}
			for i < len(lines) {
				marker := listMarker(lines[i])
				if marker == "" {
					break
				}
				indent := leadingSpaces(lines[i])
				item := strings.TrimSpace(lines[i])[len(marker):]
				i++
				// Continuation: indented, non-blank, not a new item.
				for i < len(lines) && strings.TrimSpace(lines[i]) != "" && listMarker(lines[i]) == "" &&
					leadingSpaces(lines[i]) > indent && !isFenceOrBlock(lines[i]) {
					item += " " + strings.TrimSpace(lines[i])
					i++
				}
				pad := strings.Repeat(" ", minInt(indent, 8))
				bullet := marker
				if marker == "- " || marker == "* " || marker == "+ " {
					bullet = bulletGlyph() + " "
				}
				first := pad + bullet
				rest := strings.Repeat(" ", utf8.RuneCountInString(first))
				out = append(out, wrapSpans(parseInline(strings.TrimSpace(item)), width, first, rest)...)
			}

		default:
			// A paragraph: consecutive plain lines, joined.
			blankBefore()
			var para []string
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				if t == "" || headingLevel(t) > 0 || listMarker(lines[i]) != "" || isFenceOrBlock(lines[i]) ||
					isRule(t) || isTableRow(t) {
					break
				}
				para = append(para, t)
				i++
			}
			out = append(out, wrapSpans(parseInline(strings.Join(para, " ")), width, "", "")...)
		}
	}
	for len(out) > 0 && len(out[len(out)-1]) == 0 {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return []line{{st("(empty)", dimStyle)}}
	}
	return out
}

// mdPlain is a whole message as one run of plain prose: every meaningful
// line with its markdown markers removed, code fences and rules dropped.
func mdPlain(src string) string {
	var parts []string
	for _, l := range strings.Split(src, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || isRule(t) || strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			continue
		}
		if n := headingLevel(t); n > 0 {
			t = strings.TrimSpace(strings.TrimRight(t[n:], "#"))
		}
		t = strings.TrimSpace(strings.TrimPrefix(t, ">"))
		if m := listMarker(t); m != "" {
			t = strings.TrimSpace(t[len(m):])
		}
		if s := strings.TrimSpace(plainInline(t)); s != "" {
			parts = append(parts, s)
		}
	}
	return collapse(strings.Join(parts, " "))
}

// mdSummary is a message reduced to one line of plain prose, for lists: the
// first meaningful line, with markdown markers removed rather than shown. A
// heading alone says little ("Findings"), so it leads into the line after it.
func mdSummary(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if n := headingLevel(t); n > 0 {
			head := plainInline(strings.TrimSpace(strings.TrimRight(t[n:], "#")))
			if rest := mdSummary(strings.Join(lines[i+1:], "\n")); rest != "" {
				return head + ": " + rest
			}
			return head
		}
		if t != "" && !isRule(t) {
			break
		}
	}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || isRule(t) || strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			continue
		}
		if n := headingLevel(t); n > 0 {
			t = strings.TrimSpace(strings.TrimRight(t[n:], "#"))
		}
		t = strings.TrimSpace(strings.TrimPrefix(t, ">"))
		if m := listMarker(t); m != "" {
			t = strings.TrimSpace(t[len(m):])
		}
		var b strings.Builder
		for _, s := range parseInline(t) {
			b.WriteString(s.text)
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			return s
		}
	}
	return ""
}

func bulletGlyph() string {
	if asciiOnly {
		return "-"
	}
	return "•"
}

func headingLevel(t string) int {
	n := 0
	for n < len(t) && n < 6 && t[n] == '#' {
		n++
	}
	if n == 0 || n >= len(t) || t[n] != ' ' {
		return 0
	}
	return n
}

func isRule(t string) bool {
	if len(t) < 3 {
		return false
	}
	c := t[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] != c && t[i] != ' ' {
			return false
		}
	}
	return true
}

func isTableRow(t string) bool {
	return len(t) > 1 && strings.HasPrefix(t, "|") && strings.Count(t, "|") >= 2
}

func isFenceOrBlock(l string) bool {
	t := strings.TrimSpace(l)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") || strings.HasPrefix(t, ">")
}

func leadingSpaces(l string) int {
	return len(l) - len(strings.TrimLeft(l, " "))
}

// listMarker returns the item marker ("- ", "* ", "+ ", "12. ", "3) ") that
// starts l, or "".
func listMarker(l string) string {
	t := strings.TrimLeft(l, " ")
	if len(t) >= 2 && (t[0] == '-' || t[0] == '*' || t[0] == '+') && t[1] == ' ' {
		if isRule(strings.TrimSpace(t)) {
			return ""
		}
		return t[:2]
	}
	n := 0
	for n < len(t) && n < 3 && t[n] >= '0' && t[n] <= '9' {
		n++
	}
	if n > 0 && n+1 < len(t) && (t[n] == '.' || t[n] == ')') && t[n+1] == ' ' {
		return t[:n+2]
	}
	return ""
}

// isListLine reports whether the last rendered line belongs to a list, so
// consecutive list blocks are not split by a blank line.
func isListLine(out []line) bool {
	if len(out) == 0 || len(out[len(out)-1]) == 0 {
		return false
	}
	p := strings.TrimLeft(out[len(out)-1].plain(), " ")
	return strings.HasPrefix(p, bulletGlyph()+" ") || listMarker(p) != ""
}

// codeLines renders one line of a fenced block: indented, verbatim, hard
// wrapped rather than word wrapped (code has no word boundaries to trust).
func codeLines(l string, width int) []line {
	inner := width - 2
	r := []rune(strings.TrimRight(l, " "))
	if len(r) == 0 {
		return []line{{txt("  ")}}
	}
	var out []line
	for len(r) > 0 {
		n := minInt(inner, len(r))
		out = append(out, line{txt("  "), st(string(r[:n]), mdCodeStyle)})
		r = r[n:]
	}
	return out
}

// tableLines draws a pipe table with aligned columns, or, when it cannot fit,
// each row as "header: value" pairs.
func tableLines(rows []string, width int) []line {
	var cells [][]string
	for _, r := range rows {
		parts := strings.Split(strings.Trim(r, "|"), "|")
		row := make([]string, len(parts))
		sep := true
		for i, p := range parts {
			row[i] = plainInline(strings.TrimSpace(p))
			if strings.Trim(row[i], "-: ") != "" {
				sep = false
			}
		}
		if sep {
			continue // the |---|---| delimiter row
		}
		cells = append(cells, row)
	}
	if len(cells) == 0 {
		return nil
	}
	cols := 0
	for _, r := range cells {
		cols = maxInt(cols, len(r))
	}
	widths := make([]int, cols)
	for _, r := range cells {
		for i, c := range r {
			widths[i] = maxInt(widths[i], utf8.RuneCountInString(c))
		}
	}
	total := 0
	for _, w := range widths {
		total += w + 3
	}
	var out []line
	if total-1 <= width {
		for ri, r := range cells {
			l := line{}
			for i := 0; i < cols; i++ {
				c := ""
				if i < len(r) {
					c = r[i]
				}
				sty := bodyStyle
				if ri == 0 {
					sty = mdBoldStyle
				}
				if i > 0 {
					l = l.join(st(" "+gl().vert+" ", dimStyle))
				}
				l = l.join(st(fitWidth(c, widths[i]), sty))
			}
			out = append(out, l)
			if ri == 0 {
				out = append(out, line{st(strings.Repeat(gl().horiz, minInt(width, total-1)), dimStyle)})
			}
		}
		return out
	}
	head := cells[0]
	for _, r := range cells[1:] {
		for i, c := range r {
			label := ""
			if i < len(head) {
				label = head[i] + ": "
			}
			out = append(out, wrapSpans([]mdSpan{{label, mdBoldStyle}, {c, bodyStyle}}, width, "", "  ")...)
		}
		out = append(out, blank())
	}
	return out
}

func plainInline(s string) string {
	var b strings.Builder
	for _, sp := range parseInline(s) {
		b.WriteString(sp.text)
	}
	return b.String()
}

// parseInline splits text into styled spans: **bold**, __bold__, *italic*,
// _italic_, `code`, [label](url). Unclosed markers are kept as text.
func parseInline(s string) []mdSpan {
	var out []mdSpan
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			out = append(out, mdSpan{buf.String(), bodyStyle})
			buf.Reset()
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && strings.IndexByte("\\`*_[]()#+-.!|>", s[i+1]) >= 0:
			buf.WriteByte(s[i+1])
			i += 2
			continue
		case c == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				flush()
				out = append(out, mdSpan{s[i+1 : i+1+j], mdCodeStyle})
				i += j + 2
				continue
			}
		case (c == '*' || c == '_') && i+1 < len(s) && s[i+1] == c:
			delim := s[i : i+2]
			if j := strings.Index(s[i+2:], delim); j > 0 {
				flush()
				for _, sp := range parseInline(s[i+2 : i+2+j]) {
					if sp.style.GetForeground() == mdCodeStyle.GetForeground() {
						out = append(out, sp)
						continue
					}
					out = append(out, mdSpan{sp.text, mdBoldStyle})
				}
				i += j + 4
				continue
			}
		case c == '*' || (c == '_' && (i == 0 || !isWordByte(s[i-1]))):
			if j := strings.IndexByte(s[i+1:], c); j > 0 && s[i+1] != ' ' &&
				(c == '*' || i+2+j >= len(s) || !isWordByte(s[i+2+j])) {
				flush()
				out = append(out, mdSpan{s[i+1 : i+1+j], mdItalicStyle})
				i += j + 2
				continue
			}
		case c == '[':
			if j := strings.Index(s[i:], "]("); j > 0 {
				if k := strings.IndexByte(s[i+j+2:], ')'); k >= 0 {
					flush()
					out = append(out, mdSpan{s[i+1 : i+j], mdLinkStyle})
					i += j + 2 + k + 1
					continue
				}
			}
		}
		buf.WriteByte(c)
		i++
	}
	flush()
	return out
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// restyle paints spans with sty except code, which keeps its own style.
func restyle(spans []mdSpan, sty lipgloss.Style) []mdSpan {
	for i := range spans {
		if spans[i].style.GetForeground() != mdCodeStyle.GetForeground() {
			spans[i].style = sty
		}
	}
	return spans
}

// wrapSpans word-wraps styled spans to width. first prefixes the first line,
// rest every following one (a hanging indent for list items and quotes).
func wrapSpans(spans []mdSpan, width int, first, rest string) []line {
	type word struct {
		parts []mdSpan // a word can change style mid-way, e.g. **bold**,
		w     int
	}
	// Split into words, keeping each word's styled pieces together. A word
	// boundary is a space; spans glued together without one form one word.
	var words []word
	cur := word{}
	push := func() {
		if cur.w > 0 {
			words = append(words, cur)
		}
		cur = word{}
	}
	for _, sp := range spans {
		start := 0
		for i, r := range sp.text {
			if r == ' ' {
				if i > start {
					seg := sp.text[start:i]
					cur.parts = append(cur.parts, mdSpan{seg, sp.style})
					cur.w += utf8.RuneCountInString(seg)
				}
				push()
				start = i + 1
			}
		}
		if start < len(sp.text) {
			seg := sp.text[start:]
			cur.parts = append(cur.parts, mdSpan{seg, sp.style})
			cur.w += utf8.RuneCountInString(seg)
		}
	}
	push()

	var out []line
	prefix := first
	l := line{txt(prefix)}
	used := utf8.RuneCountInString(prefix)
	avail := func() int { return width - utf8.RuneCountInString(prefix) }
	empty := true
	newLine := func() {
		out = append(out, l)
		prefix = rest
		l = line{txt(prefix)}
		used = utf8.RuneCountInString(prefix)
		empty = true
	}
	for _, w := range words {
		need := w.w
		if !empty {
			need++
		}
		if used+need > width && !empty {
			newLine()
		}
		if w.w > avail() {
			// A single word wider than the line (a path, a URL): hard break.
			for _, p := range w.parts {
				r := []rune(p.text)
				for len(r) > 0 {
					space := width - used
					if space <= 0 {
						newLine()
						space = width - used
					}
					n := minInt(space, len(r))
					l = l.join(st(string(r[:n]), p.style))
					used += n
					empty = false
					r = r[n:]
				}
			}
			continue
		}
		if !empty {
			l = l.join(txt(" "))
			used++
		}
		for _, p := range w.parts {
			l = l.join(st(p.text, p.style))
		}
		used += w.w
		empty = false
	}
	if !empty || len(out) == 0 {
		out = append(out, l)
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
