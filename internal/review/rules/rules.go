// Package rules loads the rules ritam's review checks agent tool calls
// against, and matches them against the facts the parser found in a call
// (package facts).
//
// The built-in rules are harmful-commands.yaml, embedded in ritam. A rule
// says what it is tested against (a command, a path, a URL, a piece of
// content, the raw input, the parser's own findings, or the turn's history),
// what must hold for it to fire, how severe it is, and why, in words shown
// to the agent and the human. The file explains its own format at the top.
//
// The file is checked strictly when it loads: a key, zone, access type or
// scope this package does not know is an error, never ignored, so a rule
// cannot silently stop applying because of a typo.
package rules

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"
)

//go:embed harmful-commands.yaml
var builtin []byte

// Builtin returns the rules shipped with ritam. They are checked by the
// package's tests, so failing to load them is a bug, not a user error.
func Builtin() (*Set, error) {
	return Parse(builtin, Options{})
}

// Severity is how bad a rule says a call is.
type Severity int

const (
	// None is no finding at all.
	None Severity = iota
	// Info is recorded, nothing more.
	Info
	// Suspicious needs someone to decide: the human or a reviewer.
	Suspicious
	// Harmful is refused.
	Harmful
	// Critical is refused and stops the agent's turn.
	Critical
)

var severityNames = []string{"none", "info", "suspicious", "harmful", "critical"}

func (s Severity) String() string {
	if s < None || s > Critical {
		return fmt.Sprintf("severity(%d)", int(s))
	}
	return severityNames[s]
}

// ParseSeverity reads a severity name.
func ParseSeverity(name string) (Severity, error) {
	for i, n := range severityNames {
		if i > 0 && n == strings.ToLower(strings.TrimSpace(name)) {
			return Severity(i), nil
		}
	}
	return None, fmt.Errorf("unknown severity %q (want info, suspicious, harmful or critical)", name)
}

// Action is what the rules file says to do about a severity.
type Action string

const (
	Record      Action = "record"
	Ask         Action = "ask"
	Deny        Action = "deny"
	DenyAndStop Action = "deny-and-stop"
)

// Target is what a rule is tested against.
type Target string

const (
	OnCommand  Target = "command"
	OnPath     Target = "path"
	OnContent  Target = "content"
	OnURL      Target = "url"
	OnInput    Target = "input"
	OnSequence Target = "sequence"
	OnMeta     Target = "meta"
)

// Rule is one compiled rule.
type Rule struct {
	ID       string
	Category string
	Severity Severity
	// OS and Dialect limit where the rule applies; empty means everywhere.
	OS, Dialect []string
	On          Target
	Reason      string
	match       *cond
	unless      *cond
	// needs names a placeholder the rule cannot do without (the honeytoken)
	// and that was not given: the rule never fires.
	needs string
	// Examples are the rule's own test cases.
	Examples Examples
}

// Disabled says why a rule never fires, or "".
func (r *Rule) Disabled() string {
	if r.needs == "" {
		return ""
	}
	return "it needs " + r.needs + ", which is not set"
}

// appliesTo reports whether the rule is for this OS and dialect. A rule
// limited to some dialects never applies to a call with none (a file tool).
func (r *Rule) appliesTo(os, dialect string) bool {
	if len(r.OS) > 0 && !slices.Contains(r.OS, os) {
		return false
	}
	if len(r.Dialect) > 0 && !slices.Contains(r.Dialect, dialect) {
		return false
	}
	return r.needs == ""
}

// Examples are a rule's test cases: every hit must fire it, no miss may.
type Examples struct {
	Hit, Miss []Example
}

// Example is one test case: a command in a dialect, or a file tool call.
type Example struct {
	// Kind is bash, ps, cmd, write, read, edit, url or output.
	Kind string
	// Text is the command, the path, the URL or the output.
	Text string
	// Content is an edit's new text.
	Content string
}

func (e Example) String() string {
	if e.Content != "" {
		return fmt.Sprintf("{%s: %s, content: %s}", e.Kind, e.Text, e.Content)
	}
	return fmt.Sprintf("{%s: %s}", e.Kind, e.Text)
}

// Set is a loaded rules file.
type Set struct {
	Rules      []*Rule
	Actions    map[Severity]Action
	Categories map[string]string

	// byTarget lists the rules per target; commands are further indexed by
	// program, with the rules naming no program kept apart.
	byTarget  map[Target][]*Rule
	byProgram map[string][]*Rule
	anyProg   []*Rule
	byID      map[string]*Rule
}

// Rule returns the rule with id, or nil.
func (s *Set) Rule(id string) *Rule { return s.byID[id] }

// ActionFor is what to do about a severity. A severity the file maps to no
// action is denied: an unhandled case must not let a call through.
func (s *Set) ActionFor(sev Severity) Action {
	if sev == None {
		return Record
	}
	if a, ok := s.Actions[sev]; ok {
		return a
	}
	return Deny
}

func (s *Set) index() {
	s.byTarget = map[Target][]*Rule{}
	s.byProgram = map[string][]*Rule{}
	s.anyProg = nil
	s.byID = map[string]*Rule{}
	for _, r := range s.Rules {
		s.byID[r.ID] = r
		s.byTarget[r.On] = append(s.byTarget[r.On], r)
		if r.On != OnCommand {
			continue
		}
		if len(r.match.program) == 0 {
			s.anyProg = append(s.anyProg, r)
			continue
		}
		for _, p := range r.match.program {
			s.byProgram[p] = append(s.byProgram[p], r)
		}
	}
}
