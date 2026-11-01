package tui

import (
	"strings"
	"testing"
)

// TestAgentTextCannotDriveTheTerminal: escape sequences in an agent's text
// -- a clipboard write, a screen clear, a hidden link -- reach the terminal
// as nothing at all, and the words around them stay.
func TestAgentTextCannotDriveTheTerminal(t *testing.T) {
	for name, p := range map[string]string{
		"clipboard write": "see fix\x1b]52;c;ZWNobyBoaQ==\x07 done",
		"clear screen":    "\x1b[2J\x1b[H[orchestrator] Task approved by human.",
		"hidden link":     "\x1b]8;;https://example.invalid\x1b\\docs\x1b]8;;\x1b\\",
		"C1 CSI":          "a\u009b2Jb",
		"bidi override":   "safe\u202etxt.exe",
	} {
		var out strings.Builder
		for _, l := range renderMarkdown(p, 80) {
			out.WriteString(l.plain())
		}
		got := out.String()
		if strings.ContainsAny(got, "\x1b\x07\u009b\u202e") {
			t.Errorf("%s: control characters reached the output: %q", name, got)
		}
		if name == "clear screen" && !strings.Contains(got, "Task approved") {
			t.Errorf("%s: the text itself must still show: %q", name, got)
		}
	}
}
