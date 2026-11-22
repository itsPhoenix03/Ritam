package rules

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"ritam/internal/review/facts"
)

// Finding is one rule that fired on a call.
type Finding struct {
	Rule *Rule
	// Evidence is what matched: the command, path, URL or text.
	Evidence string
}

// maxScan is the most of one piece of text the content rules look at.
const maxScan = 1 << 20

// Check tests a call's facts against every rule except the sequence ones
// (see CheckSequence), and returns the findings, the most severe first.
func (s *Set) Check(call *facts.Call, env facts.Env) []Finding {
	m := matcher{call: call, env: env}
	var out []Finding
	seen := map[string]bool{}
	add := func(r *Rule, evidence string) {
		if seen[r.ID] {
			return
		}
		seen[r.ID] = true
		out = append(out, Finding{Rule: r, Evidence: clipEvidence(evidence)})
	}
	for _, c := range call.Commands {
		dialect := c.Dialect
		for _, r := range append(slices.Clone(s.byProgram[c.Program]), s.anyProg...) {
			if seen[r.ID] || !r.appliesTo(call.OS, dialect) {
				continue
			}
			if m.command(r.match, c) && (r.unless == nil || !m.command(r.unless, c)) {
				add(r, c.Raw)
			}
		}
	}
	for _, r := range s.byTarget[OnPath] {
		if !r.appliesTo(call.OS, call.Dialect) {
			continue
		}
		for _, p := range call.Paths {
			if m.path(r.match, p) && (r.unless == nil || !m.path(r.unless, p)) {
				add(r, p.Raw)
				break
			}
		}
	}
	for _, r := range s.byTarget[OnContent] {
		if !r.appliesTo(call.OS, call.Dialect) {
			continue
		}
		for _, c := range call.Contents {
			if m.content(r.match, c) && (r.unless == nil || !m.content(r.unless, c)) {
				ev := c.Scope
				if c.Path != "" {
					ev += " " + c.Path
				}
				add(r, ev)
				break
			}
		}
	}
	for _, r := range s.byTarget[OnURL] {
		if !r.appliesTo(call.OS, call.Dialect) {
			continue
		}
		for _, u := range call.URLs {
			if m.url(r.match, u) && (r.unless == nil || !m.url(r.unless, u)) {
				add(r, u.Raw)
				break
			}
		}
	}
	for _, r := range s.byTarget[OnInput] {
		if !r.appliesTo(call.OS, call.Dialect) {
			continue
		}
		for _, in := range call.Inputs {
			if anyRegex(r.match.regexAny, in) && (r.unless == nil || !anyRegex(r.unless.regexAny, in)) {
				add(r, in)
				break
			}
		}
	}
	for _, r := range s.byTarget[OnMeta] {
		if !r.appliesTo(call.OS, call.Dialect) {
			continue
		}
		if m.meta(r.match) && (r.unless == nil || !m.meta(r.unless)) {
			add(r, strings.Join(call.Meta.Notes, "; "))
		}
	}
	sortFindings(out)
	return out
}

// sortFindings puts the most severe first, keeping the file's order among
// equals.
func sortFindings(f []Finding) {
	slices.SortStableFunc(f, func(a, b Finding) int { return int(b.Rule.Severity) - int(a.Rule.Severity) })
}

func clipEvidence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 160 {
		return string(r[:157]) + "..."
	}
	return s
}

// matcher tests the keys of a block against one call's facts.
type matcher struct {
	call *facts.Call
	env  facts.Env
}

// fold says whether flags and arguments compare without case: on Windows.
func (m matcher) fold() bool { return m.call.OS == "windows" }

func (m matcher) eq(a, b string) bool {
	if m.fold() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func (m matcher) inList(list []string, v string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return m.eq(x, v) })
}

func anyRegex(res []*regexp.Regexp, s string) bool {
	if len(s) > maxScan {
		s = s[:maxScan]
	}
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func boolIs(want *bool, got bool) bool { return want == nil || *want == got }

// command tests a command rule's block.
func (m matcher) command(k *cond, c *facts.Command) bool {
	if len(k.program) > 0 && !slices.Contains(k.program, c.Program) {
		return false
	}
	if len(k.programRe) > 0 && !anyRegex(k.programRe, c.Program) {
		return false
	}
	if len(k.programRawRe) > 0 && !anyRegex(k.programRawRe, c.ProgramRaw) {
		return false
	}
	if !boolIs(k.programIndirect, c.Indirect) {
		return false
	}
	if len(k.subcommand) > 0 {
		sub := strings.ToLower(c.Subcommand)
		if !slices.ContainsFunc(k.subcommand, func(e string) bool { return sub == e || strings.HasPrefix(sub, e+" ") }) {
			return false
		}
	}
	if len(k.subcommandRe) > 0 && !anyRegex(k.subcommandRe, c.Subcommand) {
		return false
	}
	if len(k.flagsAny) > 0 && !slices.ContainsFunc(c.Flags, func(f facts.Flag) bool { return m.inList(k.flagsAny, f.Name) }) {
		return false
	}
	for _, want := range k.flagsAll {
		if !slices.ContainsFunc(c.Flags, func(f facts.Flag) bool { return m.eq(want, f.Name) }) {
			return false
		}
	}
	if len(k.argsAny) > 0 && !slices.ContainsFunc(c.Args, func(a facts.Arg) bool { return m.inList(k.argsAny, a.Value) }) {
		return false
	}
	if len(k.argsRe) > 0 && !slices.ContainsFunc(c.Args, func(a facts.Arg) bool {
		return anyRegex(k.argsRe, a.Value) || a.Raw != a.Value && anyRegex(k.argsRe, a.Raw)
	}) {
		return false
	}
	if len(k.arglineRe) > 0 && !anyRegex(k.arglineRe, c.Argline()) {
		return false
	}
	if k.argcMax != nil && len(c.Args) > *k.argcMax {
		return false
	}
	if k.argsDynamic != nil && *k.argsDynamic != slices.ContainsFunc(c.Args, func(a facts.Arg) bool { return a.Dynamic }) {
		return false
	}
	if !boolIs(k.inlineCode, c.Inline != nil) {
		return false
	}
	if k.inlineDynamic != nil && (c.Inline == nil || c.Inline.Dynamic != *k.inlineDynamic) {
		return false
	}
	if len(k.pipeFrom) > 0 && !slices.Contains(k.pipeFrom, c.PipeFrom) {
		return false
	}
	if len(k.pipeTo) > 0 && !slices.Contains(k.pipeTo, c.PipeTo) {
		return false
	}
	if len(k.argFrom) > 0 && !slices.ContainsFunc(c.ArgFrom, func(p string) bool { return slices.Contains(k.argFrom, p) }) {
		return false
	}
	if len(k.wrappedBy) > 0 && !slices.ContainsFunc(c.WrappedBy, func(p string) bool { return slices.Contains(k.wrappedBy, p) }) {
		return false
	}
	if len(k.assignsAny) > 0 && !slices.ContainsFunc(c.Assigns, func(a facts.Assign) bool { return m.inList(k.assignsAny, a.Name) }) {
		return false
	}
	if len(k.assignsRe) > 0 && !slices.ContainsFunc(c.Assigns, func(a facts.Assign) bool { return anyRegex(k.assignsRe, a.Name) }) {
		return false
	}
	if len(k.rawRe) > 0 && !anyRegex(k.rawRe, c.Raw) {
		return false
	}
	if k.path != nil && !slices.ContainsFunc(c.Paths, func(p facts.Path) bool { return m.path(k.path, p) }) {
		return false
	}
	if k.url != nil && !slices.ContainsFunc(c.URLs, func(u facts.URL) bool { return m.url(k.url, u) }) {
		return false
	}
	return true
}

func (m matcher) path(k *cond, p facts.Path) bool {
	if len(k.access) > 0 && !slices.Contains(k.access, string(p.Access)) {
		return false
	}
	if len(k.zone) > 0 && !slices.Contains(k.zone, string(p.Zone)) {
		return false
	}
	if len(k.glob) > 0 && !slices.ContainsFunc(k.glob, func(g string) bool { return m.env.MatchGlob(g, p.Path) }) {
		return false
	}
	if len(k.tool) > 0 && !slices.Contains(k.tool, string(p.Tool)) {
		return false
	}
	return true
}

func (m matcher) content(k *cond, c facts.Content) bool {
	if len(k.scope) > 0 && !slices.Contains(k.scope, c.Scope) {
		return false
	}
	if len(k.pathGlob) > 0 && (c.Path == "" || !slices.ContainsFunc(k.pathGlob, func(g string) bool { return m.env.MatchGlob(g, c.Path) })) {
		return false
	}
	return len(k.regexAny) == 0 || anyRegex(k.regexAny, c.Text)
}

func (m matcher) url(k *cond, u facts.URL) bool {
	if len(k.hostRe) > 0 && !anyRegex(k.hostRe, u.Host) {
		return false
	}
	if len(k.hostNot) > 0 && (u.Host == "" || slices.ContainsFunc(k.hostNot, func(h string) bool { return hostMatch(h, u.Host) })) {
		return false
	}
	if len(k.scheme) > 0 && !slices.Contains(k.scheme, strings.ToLower(u.Scheme)) {
		return false
	}
	if len(k.pathRe) > 0 && !anyRegex(k.pathRe, u.Path) {
		return false
	}
	return true
}

// hostMatch matches a host against a pattern: example.com, or *.example.com
// for any of its subdomains.
func hostMatch(pattern, host string) bool {
	if pattern == host {
		return true
	}
	ok, _ := path.Match(pattern, host)
	if ok {
		return true
	}
	if suffix, found := strings.CutPrefix(pattern, "*."); found {
		return strings.HasSuffix(host, "."+suffix)
	}
	return false
}

func (m matcher) meta(k *cond) bool {
	mf := m.call.Meta
	if !boolIs(k.parseError, mf.ParseError) || !boolIs(k.limitExceeded, mf.LimitExceeded) || !boolIs(k.unknownTool, mf.UnknownTool) {
		return false
	}
	if len(k.tool) > 0 && !slices.Contains(k.tool, string(m.call.Kind)) {
		return false
	}
	return true
}

// Fires reports whether one rule fires on a call, sequence rules aside.
func (s *Set) Fires(r *Rule, call *facts.Call, env facts.Env) bool {
	for _, f := range s.Check(call, env) {
		if f.Rule == r {
			return true
		}
	}
	return false
}
