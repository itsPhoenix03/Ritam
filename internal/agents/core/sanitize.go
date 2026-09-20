package core

import "strings"

// SanitizePrompt removes control characters that have no place in prompt text:
// NUL and the other C0 controls, keeping newline, carriage return and tab (and
// DEL).
//
// Prompts embed free text a human typed or an agent produced, and a stray NUL
// in it is enough to break things downstream: it cannot be passed in a process
// argument at all (the OS reports "invalid argument" and the invocation fails
// before the CLI starts), and other CLIs and terminals treat control bytes as
// escape or truncation signals. Text that arrives on stdin is safe from the
// argument problem, but stripping here means neither adapter depends on that.
func SanitizePrompt(s string) string {
	clean := func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}
	return strings.Map(clean, s)
}
