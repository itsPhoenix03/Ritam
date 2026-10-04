package tui

import (
	"strings"
	"time"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// Live agent activity (ports.AgentActivity, delivered as a domain.Event with
// Type == domain.EvtAgentActivity) is the one piece of state this package
// keeps that is never persisted anywhere else: if the Model does not retain
// it, it is gone. This file is the retention and layout logic for it, kept
// separate from view.go because it has its own invariants (bounded size,
// wrap caching) that are worth reasoning about in isolation.

// maxActivityEntries and maxActivityBytes bound one agent's retained
// activity so a long streaming run cannot grow the Model without bound.
// Entries are dropped oldest-first once either limit is exceeded.
const (
	maxActivityEntries = 200
	maxActivityBytes   = 8 * 1024
	// maxEntryRunes bounds a single entry. Without this, an agent that only
	// ever emits "text" fragments would concatenate into one entry that grows
	// forever between discrete events (tool calls, status lines), defeating
	// the byte cap above, which only fires once an entry boundary exists.
	maxEntryRunes = 4000
)

// activityEntry is one discrete unit of agent output: either a merged run of
// consecutive text/thinking fragments, or a single tool/status event.
type activityEntry struct {
	kind ports.ActivityKind
	text string
	// at is when the entry started. Fragments merged into an existing entry
	// keep the original time so the merged stream orders an agent's
	// paragraph by when it began speaking, not by when it stopped.
	at time.Time
	// last is when the entry was most recently extended. For a discrete entry it
	// equals at; for a merged text run it is the newest fragment, which is what
	// "how long ago did this agent last say something" needs.
	last time.Time
	// task is the task the activity belonged to, as stamped on the event that
	// carried it. Agents are shared across tasks, so without it a second task's
	// stream would show the first task's tool calls. Empty means the adapter did
	// not say (see Model.inScope for how that is treated).
	task domain.TaskID
}

// isFragmentKind reports whether activity of this kind arrives as partial
// sentences that should be concatenated onto the previous entry of the same
// kind, rather than starting a new line. Per ports.AgentActivity's contract,
// only "thinking" and "text" behave this way; "tool" and "status" are always
// discrete.
func isFragmentKind(k ports.ActivityKind) bool {
	return k == ports.ActivityText || k == ports.ActivityThinking
}

// wrappedLine is one already-width-fit, unstyled display line derived from
// an activityEntry. Keeping the source kind alongside the text lets the view
// apply per-kind styling (e.g. dimming "thinking") after wrapping without
// re-deriving which entry a line came from.
type wrappedLine struct {
	kind ports.ActivityKind
	text string
}

// agentActivity is one agent's bounded activity buffer plus a memoised wrap
// of it. Wrapping every entry is trivial per call but not free, and View()
// runs on every tea.Msg -- including an unrelated agent's 80ms text chunk,
// or a spinner tick -- so re-wrapping unconditionally would mean doing this
// work many times a second for content that has not changed. The cache is
// invalidated only by new content (append) or a pane-width change.
type agentActivity struct {
	entries []activityEntry

	// updated is when this agent last produced anything. The agents pane
	// shows "how long has it been doing that", which is the single most
	// useful signal for spotting a wedged CLI.
	updated time.Time

	cacheWidth int
	cacheTask  domain.TaskID
	cacheLines []wrappedLine
	cacheDirty bool
}

// append adds one activity chunk to the buffer, stamped with the current time.
func (a *agentActivity) append(kind ports.ActivityKind, text string) {
	a.appendAt(kind, text, time.Now())
}

// appendAt adds one activity chunk to the buffer, merging it into the running
// entry when it is a fragment continuation of the same kind.
func (a *agentActivity) appendAt(kind ports.ActivityKind, text string, at time.Time) {
	a.appendTask(kind, text, at, "")
}

// appendTask is appendAt for activity that knows which task it belongs to. A
// fragment only merges into the running entry when both are from the same task:
// gluing the tail of one task's sentence onto another's would misattribute it.
func (a *agentActivity) appendTask(kind ports.ActivityKind, text string, at time.Time, task domain.TaskID) {
	if text == "" {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	if n := len(a.entries); n > 0 && a.entries[n-1].kind == kind && isFragmentKind(kind) && a.entries[n-1].task == task {
		a.entries[n-1].text = trimRunesFront(a.entries[n-1].text+text, maxEntryRunes)
		a.entries[n-1].last = at
	} else {
		a.entries = append(a.entries, activityEntry{kind: kind, text: text, at: at, last: at, task: task})
	}
	a.updated = at
	a.cacheDirty = true
	a.trim()
}

// latest returns the newest entry, if any.
func (a *agentActivity) latest() (activityEntry, bool) {
	if a == nil || len(a.entries) == 0 {
		return activityEntry{}, false
	}
	return a.entries[len(a.entries)-1], true
}

// countKind reports how many entries of a given kind the buffer holds. It is
// what the inspector's per-agent tool count is derived from -- an approximation
// bounded by the ring, and labelled as such on screen.
func (a *agentActivity) countKind(k ports.ActivityKind) int {
	if a == nil {
		return 0
	}
	n := 0
	for _, e := range a.entries {
		if e.kind == k {
			n++
		}
	}
	return n
}

// trim enforces maxActivityEntries and maxActivityBytes, oldest entry first.
func (a *agentActivity) trim() {
	if len(a.entries) > maxActivityEntries {
		a.entries = a.entries[len(a.entries)-maxActivityEntries:]
		a.cacheDirty = true
	}
	total := 0
	for _, e := range a.entries {
		total += len(e.text)
	}
	for total > maxActivityBytes && len(a.entries) > 1 {
		total -= len(a.entries[0].text)
		a.entries = a.entries[1:]
		a.cacheDirty = true
	}
}

// entriesOrNil is the buffer's entries, tolerating a missing buffer.
func (a *agentActivity) entriesOrNil() []activityEntry {
	if a == nil {
		return nil
	}
	return a.entries
}

// latestIn returns the newest entry the scope admits, if any.
func (a *agentActivity) latestIn(sc taskScope) (activityEntry, bool) {
	if a == nil {
		return activityEntry{}, false
	}
	for i := len(a.entries) - 1; i >= 0; i-- {
		if sc.has(a.entries[i].task) {
			return a.entries[i], true
		}
	}
	return activityEntry{}, false
}

// countKindIn is countKind restricted to a scope.
func (a *agentActivity) countKindIn(k ports.ActivityKind, sc taskScope) int {
	if a == nil {
		return 0
	}
	n := 0
	for _, e := range a.entries {
		if e.kind == k && sc.has(e.task) {
			n++
		}
	}
	return n
}

// countIn counts the entries a scope admits.
func (a *agentActivity) countIn(sc taskScope) int {
	if a == nil {
		return 0
	}
	n := 0
	for _, e := range a.entries {
		if sc.has(e.task) {
			n++
		}
	}
	return n
}

// wrappedLines returns the whole buffer wrapped to width, using the cache when
// nothing has changed.
func (a *agentActivity) wrappedLines(width int) []wrappedLine {
	return a.wrappedLinesIn(width, taskScope{all: true})
}

// wrappedLinesIn is wrappedLines restricted to the entries a scope admits. The
// memo is keyed on the scope's task as well as the width, because the same
// buffer is asked for under a different task the moment the human switches.
func (a *agentActivity) wrappedLinesIn(width int, sc taskScope) []wrappedLine {
	if width < 1 {
		width = 1
	}
	key := sc.id
	if sc.all {
		key = "(all tasks)"
	}
	if !a.cacheDirty && a.cacheWidth == width && a.cacheTask == key && a.cacheLines != nil {
		return a.cacheLines
	}
	var out []wrappedLine
	for _, e := range a.entries {
		if !sc.has(e.task) {
			continue
		}
		prefix := kindPrefix(e.kind)
		pw := len([]rune(prefix))
		bodyWidth := width - pw
		if bodyWidth < 1 {
			bodyWidth = 1
		}
		for i, wl := range wrapWords(e.text, bodyWidth) {
			var text string
			if i == 0 {
				text = prefix + wl
			} else {
				text = strings.Repeat(" ", pw) + wl
			}
			out = append(out, wrappedLine{kind: e.kind, text: fitWidth(text, width)})
		}
	}
	a.cacheLines = out
	a.cacheWidth = width
	a.cacheTask = key
	a.cacheDirty = false
	return out
}

// kindPrefix returns the ASCII-safe glyph that marks an activity kind on
// screen. Kept short and distinct so a dense, fast-scrolling pane still
// reads at a glance.
func kindPrefix(k ports.ActivityKind) string {
	switch k {
	case ports.ActivityThinking:
		return "~ "
	case ports.ActivityTool:
		return "@ "
	case ports.ActivityStatus:
		return "> "
	default: // ports.ActivityText and anything unrecognised
		return dot() + " "
	}
}

// trimRunesFront keeps at most max runes of s, dropping from the front (the
// oldest content) so the most recent text -- what a human watching the
// stream actually cares about -- survives.
func trimRunesFront(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[len(r)-max:])
}

// wrapWords greedily wraps s to at most width runes per output line,
// breaking on spaces where possible and hard-breaking any single word (e.g.
// a long file path) that alone exceeds width. This is a small, dependency-
// free wrapper by design: no bubbles/lipgloss word-wrap helper is available
// under this package's dependency constraints.
func wrapWords(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, word := range words {
			wr := []rune(word)
			for len(wr) > width {
				if line != "" {
					out = append(out, line)
					line = ""
				}
				out = append(out, string(wr[:width]))
				wr = wr[width:]
			}
			word = string(wr)
			cand := word
			if line != "" {
				cand = line + " " + word
			}
			if len([]rune(cand)) > width {
				out = append(out, line)
				line = word
			} else {
				line = cand
			}
		}
		out = append(out, line)
	}
	return out
}

// fitWidth pads or truncates s (by rune count) to exactly width runes. It
// must be called on plain, unstyled text: truncating a string that already
// carries lipgloss/ANSI escapes would corrupt them. Callers style *after*
// fitting, never before.
func fitWidth(s string, width int) string {
	if width < 0 {
		width = 0
	}
	r := []rune(s)
	if len(r) > width {
		return string(r[:width])
	}
	if len(r) < width {
		return s + strings.Repeat(" ", width-len(r))
	}
	return s
}
