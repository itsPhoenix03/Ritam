package rules

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Options fill the placeholders a rules file may use.
type Options struct {
	// Honeytoken is the decoy credential planted for the agents; rules
	// matching {honeytoken} or {honeytoken_path} never fire without it.
	Honeytoken, HoneytokenPath string
}

// The values a rule may name. Anything else is an error.
var (
	knownOS       = []string{"linux", "macos", "windows"}
	knownDialects = []string{"bash", "powershell", "cmd"}
	knownAccess   = []string{"read", "list", "write", "delete", "exec", "link", "meta"}
	knownZones    = []string{"build", "workspace", "protected", "ritam", "temp", "home", "system", "outside", "unc",
		"device", "null", "registry", "unknown"}
	knownTools     = []string{"shell", "read", "edit", "search", "web", "mcp", "other"}
	knownScopes    = []string{"edit", "inline", "heredoc", "decoded", "output", "message"}
	knownActions   = []string{string(Record), string(Ask), string(Deny), string(DenyAndStop)}
	knownDecisions = []string{"pass", "ask", "deny"}
	exampleKinds   = []string{"bash", "ps", "cmd", "write", "read", "edit", "url", "output"}
)

// keysFor lists the match keys each target may use.
var keysFor = map[Target][]string{
	OnCommand: {"program", "program_regex", "program_raw_regex", "program_indirect", "subcommand", "subcommand_regex",
		"flags_any", "flags_all", "args_any", "args_regex", "argline_regex", "argc_max", "args_dynamic",
		"inline_code", "inline_dynamic", "pipe_from", "pipe_to", "arg_from", "wrapped_by", "assigns_any",
		"assigns_regex", "raw_regex", "path", "url"},
	OnPath:     {"access", "zone", "glob", "tool"},
	OnContent:  {"scope", "path_glob", "regex_any"},
	OnURL:      {"host_regex", "host_not", "scheme", "path_regex"},
	OnInput:    {"regex_any"},
	OnSequence: {"steps", "count", "within_calls"},
	OnMeta:     {"parse_error", "limit_exceeded", "unknown_tool", "tool"},
}

// file is the rules file as written.
type file struct {
	Version    int                 `yaml:"version"`
	Actions    map[string]string   `yaml:"actions"`
	Categories map[string]string   `yaml:"categories"`
	Lists      map[string][]string `yaml:"lists"`
	Paths      map[string][]string `yaml:"paths"`
	Rules      []rawRule           `yaml:"rules"`
}

type rawRule struct {
	ID       string    `yaml:"id"`
	Category string    `yaml:"category"`
	Severity string    `yaml:"severity"`
	OS       strList   `yaml:"os"`
	Dialect  strList   `yaml:"dialect"`
	On       string    `yaml:"on"`
	Match    *rawCond  `yaml:"match"`
	Unless   *rawCond  `yaml:"unless"`
	Reason   string    `yaml:"reason"`
	Examples rawExamps `yaml:"examples"`
}

type rawCond struct {
	Program         strList `yaml:"program"`
	ProgramRegex    strList `yaml:"program_regex"`
	ProgramRawRegex strList `yaml:"program_raw_regex"`
	ProgramIndirect *bool   `yaml:"program_indirect"`
	Subcommand      strList `yaml:"subcommand"`
	SubcommandRegex strList `yaml:"subcommand_regex"`
	FlagsAny        strList `yaml:"flags_any"`
	FlagsAll        strList `yaml:"flags_all"`
	ArgsAny         strList `yaml:"args_any"`
	ArgsRegex       strList `yaml:"args_regex"`
	ArglineRegex    strList `yaml:"argline_regex"`
	ArgcMax         *int    `yaml:"argc_max"`
	ArgsDynamic     *bool   `yaml:"args_dynamic"`
	InlineCode      *bool   `yaml:"inline_code"`
	InlineDynamic   *bool   `yaml:"inline_dynamic"`
	PipeFrom        strList `yaml:"pipe_from"`
	PipeTo          strList `yaml:"pipe_to"`
	ArgFrom         strList `yaml:"arg_from"`
	WrappedBy       strList `yaml:"wrapped_by"`
	AssignsAny      strList `yaml:"assigns_any"`
	AssignsRegex    strList `yaml:"assigns_regex"`
	RawRegex        strList `yaml:"raw_regex"`
	Path            *rawSub `yaml:"path"`
	URL             *rawSub `yaml:"url"`

	Access strList `yaml:"access"`
	Zone   strList `yaml:"zone"`
	Glob   strList `yaml:"glob"`
	Tool   strList `yaml:"tool"`

	Scope    strList `yaml:"scope"`
	PathGlob strList `yaml:"path_glob"`
	RegexAny strList `yaml:"regex_any"`

	HostRegex strList `yaml:"host_regex"`
	HostNot   strList `yaml:"host_not"`
	Scheme    strList `yaml:"scheme"`
	PathRegex strList `yaml:"path_regex"`

	ParseError    *bool `yaml:"parse_error"`
	LimitExceeded *bool `yaml:"limit_exceeded"`
	UnknownTool   *bool `yaml:"unknown_tool"`

	Steps       []rawStep `yaml:"steps"`
	Count       *rawCount `yaml:"count"`
	WithinCalls int       `yaml:"within_calls"`
}

// rawSub is the path or url sub-map of a command rule.
type rawSub struct {
	Access    strList `yaml:"access"`
	Zone      strList `yaml:"zone"`
	Glob      strList `yaml:"glob"`
	HostRegex strList `yaml:"host_regex"`
	HostNot   strList `yaml:"host_not"`
	Scheme    strList `yaml:"scheme"`
}

type rawStep struct {
	Category     strList `yaml:"category"`
	Rule         strList `yaml:"rule"`
	Access       strList `yaml:"access"`
	Tool         strList `yaml:"tool"`
	Glob         strList `yaml:"glob"`
	Program      strList `yaml:"program"`
	Decision     strList `yaml:"decision"`
	SamePath     bool    `yaml:"same_path"`
	SameCategory bool    `yaml:"same_category"`
}

type rawCount struct {
	Of  rawStep `yaml:"of"`
	Min int     `yaml:"min"`
}

type rawExamps struct {
	Hit  []rawExample `yaml:"hit"`
	Miss []rawExample `yaml:"miss"`
}

type rawExample struct{ Example }

func (e *rawExample) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		e.Kind, e.Text = "", n.Value
		return nil
	case yaml.MappingNode:
		var m map[string]string
		if err := n.Decode(&m); err != nil {
			return err
		}
		for k, v := range m {
			switch {
			case k == "content":
				e.Content = v
			case slices.Contains(exampleKinds, k):
				if e.Kind != "" {
					return fmt.Errorf("line %d: an example names both %s and %s", n.Line, e.Kind, k)
				}
				e.Kind, e.Text = k, v
			default:
				return fmt.Errorf("line %d: unknown example key %q", n.Line, k)
			}
		}
		if e.Kind == "" {
			return fmt.Errorf("line %d: an example names no command or file", n.Line)
		}
		if e.Content != "" && e.Kind != "edit" {
			return fmt.Errorf("line %d: only an edit example has content", n.Line)
		}
		return nil
	}
	return fmt.Errorf("line %d: an example is a string or a map", n.Line)
}

// strList is a list of strings, or one string standing for a list of one.
type strList []string

func (l *strList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*l = strList{n.Value}
		return nil
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: a list of strings holds only strings", c.Line)
			}
			out = append(out, c.Value)
		}
		*l = out
		return nil
	}
	return fmt.Errorf("line %d: want a string or a list of strings", n.Line)
}

// Parse reads and checks a rules file.
func Parse(data []byte, opts Options) (*Set, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f file
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("rules: version %d is not supported (want 1)", f.Version)
	}
	s := &Set{Actions: map[Severity]Action{}, Categories: f.Categories}
	for name, act := range f.Actions {
		sev, err := ParseSeverity(name)
		if err != nil {
			return nil, fmt.Errorf("rules: actions: %w", err)
		}
		if !slices.Contains(knownActions, act) {
			return nil, fmt.Errorf("rules: actions: unknown action %q for %s", act, name)
		}
		s.Actions[sev] = Action(act)
	}
	c := compiler{lists: f.Lists, paths: f.Paths, opts: opts}
	seen := map[string]bool{}
	var errs []error
	for i := range f.Rules {
		r, err := c.rule(&f.Rules[i], f.Categories)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if seen[r.ID] {
			errs = append(errs, fmt.Errorf("rule %s: the id is used twice", r.ID))
			continue
		}
		seen[r.ID] = true
		s.Rules = append(s.Rules, r)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	for _, r := range s.Rules {
		for _, st := range r.match.steps {
			for _, id := range st.rule {
				if !seen[id] {
					return nil, fmt.Errorf("rules: rule %s: a step names rule %q, which does not exist", r.ID, id)
				}
			}
		}
	}
	s.index()
	return s, nil
}

type compiler struct {
	lists, paths map[string][]string
	opts         Options
	// needs collects a placeholder the current rule uses but has no value.
	needs string
}

func (c *compiler) rule(raw *rawRule, categories map[string]string) (*Rule, error) {
	if raw.ID == "" {
		return nil, errors.New("a rule has no id")
	}
	fail := func(format string, a ...any) (*Rule, error) {
		return nil, fmt.Errorf("rule %s: %s", raw.ID, fmt.Sprintf(format, a...))
	}
	if _, ok := categories[raw.Category]; !ok {
		return fail("unknown category %q", raw.Category)
	}
	sev, err := ParseSeverity(raw.Severity)
	if err != nil {
		return fail("%v", err)
	}
	if strings.TrimSpace(raw.Reason) == "" {
		return fail("no reason: the agent and the human must be told why")
	}
	on := Target(raw.On)
	keys, ok := keysFor[on]
	if !ok {
		return fail("unknown target on: %q", raw.On)
	}
	if err := oneOf("os", raw.OS, knownOS); err != nil {
		return fail("%v", err)
	}
	if err := oneOf("dialect", raw.Dialect, knownDialects); err != nil {
		return fail("%v", err)
	}
	if raw.Match == nil {
		return fail("no match")
	}
	c.needs = ""
	r := &Rule{ID: raw.ID, Category: raw.Category, Severity: sev, OS: raw.OS, Dialect: raw.Dialect, On: on,
		Reason: strings.TrimSpace(raw.Reason)}
	if r.match, err = c.cond(raw.Match, on, keys); err != nil {
		return fail("match: %v", err)
	}
	if r.match.empty() {
		return fail("match: nothing to match")
	}
	if raw.Unless != nil {
		if r.unless, err = c.cond(raw.Unless, on, keys); err != nil {
			return fail("unless: %v", err)
		}
	}
	r.needs = c.needs
	for _, e := range raw.Examples.Hit {
		r.Examples.Hit = append(r.Examples.Hit, c.example(e.Example, raw.Dialect))
	}
	for _, e := range raw.Examples.Miss {
		r.Examples.Miss = append(r.Examples.Miss, c.example(e.Example, raw.Dialect))
	}
	return r, nil
}

// example gives a plain string example the rule's first dialect.
func (c *compiler) example(e Example, dialects []string) Example {
	if e.Kind != "" {
		return e
	}
	e.Kind = "bash"
	if len(dialects) > 0 {
		e.Kind = map[string]string{"bash": "bash", "powershell": "ps", "cmd": "cmd"}[dialects[0]]
	}
	return e
}

func oneOf(what string, got, known []string) error {
	for _, v := range got {
		if !slices.Contains(known, v) {
			return fmt.Errorf("unknown %s %q", what, v)
		}
	}
	return nil
}

// expand replaces "@name" entries by the named list or path group.
func (c *compiler) expand(l strList) (strList, error) {
	var out strList
	for _, v := range l {
		name, ok := strings.CutPrefix(v, "@")
		if !ok || name == "" {
			out = append(out, v)
			continue
		}
		if vs, ok := c.lists[name]; ok {
			out = append(out, vs...)
		} else if vs, ok := c.paths[name]; ok {
			out = append(out, vs...)
		} else {
			return nil, fmt.Errorf("unknown list @%s", name)
		}
	}
	return out, nil
}

// placeholders fills {honeytoken} and {honeytoken_path}; a pattern needing
// one that is not set is dropped, and the rule marked as needing it.
func (c *compiler) placeholders(l strList, quote bool) strList {
	var out strList
	for _, v := range l {
		ok := true
		for name, val := range map[string]string{"{honeytoken}": c.opts.Honeytoken, "{honeytoken_path}": c.opts.HoneytokenPath} {
			if !strings.Contains(v, name) {
				continue
			}
			if val == "" {
				c.needs, ok = strings.Trim(name, "{}"), false
				break
			}
			if quote {
				val = regexp.QuoteMeta(val)
			}
			v = strings.ReplaceAll(v, name, val)
		}
		if ok {
			out = append(out, v)
		}
	}
	return out
}

func (c *compiler) regexes(what string, l strList) ([]*regexp.Regexp, error) {
	l, err := c.expand(l)
	if err != nil {
		return nil, err
	}
	var out []*regexp.Regexp
	for _, p := range c.placeholders(l, true) {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func (c *compiler) words(l strList, known []string, what string) ([]string, error) {
	l, err := c.expand(l)
	if err != nil {
		return nil, err
	}
	if known != nil {
		if err := oneOf(what, l, known); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (c *compiler) lower(l strList) ([]string, error) {
	l, err := c.expand(l)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(l))
	for i, v := range l {
		out[i] = strings.ToLower(v)
	}
	return out, nil
}

func (c *compiler) globs(l strList) ([]string, error) {
	l, err := c.expand(l)
	if err != nil {
		return nil, err
	}
	return c.placeholders(l, false), nil
}

// cond compiles a match or unless block, refusing keys its target does not
// use.
func (c *compiler) cond(raw *rawCond, on Target, keys []string) (*cond, error) {
	used := usedKeys(raw)
	for _, k := range used {
		if !slices.Contains(keys, k) {
			return nil, fmt.Errorf("key %s is not used with on: %s", k, on)
		}
	}
	var (
		out  = &cond{}
		errs []error
		try  = func(err error) {
			if err != nil {
				errs = append(errs, err)
			}
		}
		set = func(dst *[]string, l strList, known []string, what string) {
			v, err := c.words(l, known, what)
			try(err)
			*dst = v
		}
		lower = func(dst *[]string, l strList) {
			v, err := c.lower(l)
			try(err)
			*dst = v
		}
		res = func(dst *[]*regexp.Regexp, l strList, what string) {
			v, err := c.regexes(what, l)
			try(err)
			*dst = v
		}
		glob = func(dst *[]string, l strList) {
			v, err := c.globs(l)
			try(err)
			*dst = v
		}
	)
	lower(&out.program, raw.Program)
	res(&out.programRe, raw.ProgramRegex, "program_regex")
	res(&out.programRawRe, raw.ProgramRawRegex, "program_raw_regex")
	out.programIndirect = raw.ProgramIndirect
	lower(&out.subcommand, raw.Subcommand)
	res(&out.subcommandRe, raw.SubcommandRegex, "subcommand_regex")
	set(&out.flagsAny, raw.FlagsAny, nil, "")
	set(&out.flagsAll, raw.FlagsAll, nil, "")
	set(&out.argsAny, raw.ArgsAny, nil, "")
	res(&out.argsRe, raw.ArgsRegex, "args_regex")
	res(&out.arglineRe, raw.ArglineRegex, "argline_regex")
	out.argcMax = raw.ArgcMax
	out.argsDynamic, out.inlineCode, out.inlineDynamic = raw.ArgsDynamic, raw.InlineCode, raw.InlineDynamic
	lower(&out.pipeFrom, raw.PipeFrom)
	lower(&out.pipeTo, raw.PipeTo)
	lower(&out.argFrom, raw.ArgFrom)
	lower(&out.wrappedBy, raw.WrappedBy)
	set(&out.assignsAny, raw.AssignsAny, nil, "")
	res(&out.assignsRe, raw.AssignsRegex, "assigns_regex")
	res(&out.rawRe, raw.RawRegex, "raw_regex")
	if raw.Path != nil {
		p := &cond{}
		if len(raw.Path.HostRegex)+len(raw.Path.HostNot)+len(raw.Path.Scheme) > 0 {
			try(errors.New("path: host_regex, host_not and scheme belong to url"))
		}
		set(&p.access, raw.Path.Access, knownAccess, "access")
		set(&p.zone, raw.Path.Zone, knownZones, "zone")
		glob(&p.glob, raw.Path.Glob)
		out.path = p
	}
	if raw.URL != nil {
		u := &cond{}
		if len(raw.URL.Access)+len(raw.URL.Zone)+len(raw.URL.Glob) > 0 {
			try(errors.New("url: access, zone and glob belong to path"))
		}
		res(&u.hostRe, raw.URL.HostRegex, "host_regex")
		lower(&u.hostNot, raw.URL.HostNot)
		lower(&u.scheme, raw.URL.Scheme)
		out.url = u
	}
	set(&out.access, raw.Access, knownAccess, "access")
	set(&out.zone, raw.Zone, knownZones, "zone")
	glob(&out.glob, raw.Glob)
	set(&out.tool, raw.Tool, knownTools, "tool")
	set(&out.scope, raw.Scope, knownScopes, "scope")
	glob(&out.pathGlob, raw.PathGlob)
	res(&out.regexAny, raw.RegexAny, "regex_any")
	res(&out.hostRe, raw.HostRegex, "host_regex")
	lower(&out.hostNot, raw.HostNot)
	lower(&out.scheme, raw.Scheme)
	res(&out.pathRe, raw.PathRegex, "path_regex")
	out.parseError, out.limitExceeded, out.unknownTool = raw.ParseError, raw.LimitExceeded, raw.UnknownTool

	for _, rs := range raw.Steps {
		st, err := c.step(rs)
		try(err)
		out.steps = append(out.steps, st)
	}
	if raw.Count != nil {
		st, err := c.step(raw.Count.Of)
		try(err)
		if raw.Count.Min < 1 {
			try(errors.New("count: min must be at least 1"))
		}
		out.count = &countCond{of: st, min: raw.Count.Min}
	}
	out.within = raw.WithinCalls
	if on == OnSequence {
		if (len(out.steps) == 0) == (out.count == nil) {
			try(errors.New("a sequence rule has either steps or a count"))
		}
		if out.within < 1 {
			try(errors.New("a sequence rule needs within_calls"))
		}
	}
	return out, errors.Join(errs...)
}

func (c *compiler) step(raw rawStep) (step, error) {
	var (
		st   step
		errs []error
	)
	add := func(dst *[]string, l strList, known []string, what string) {
		v, err := c.words(l, known, what)
		errs = append(errs, err)
		*dst = v
	}
	add(&st.category, raw.Category, nil, "")
	add(&st.rule, raw.Rule, nil, "")
	add(&st.access, raw.Access, knownAccess, "access")
	add(&st.tool, raw.Tool, knownTools, "tool")
	add(&st.decision, raw.Decision, knownDecisions, "decision")
	g, err := c.globs(raw.Glob)
	errs = append(errs, err)
	st.glob = g
	p, err := c.lower(raw.Program)
	errs = append(errs, err)
	st.program = p
	st.samePath, st.sameCategory = raw.SamePath, raw.SameCategory
	return st, errors.Join(errs...)
}

// usedKeys lists the keys a block sets, by their names in the file.
func usedKeys(r *rawCond) []string {
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}
	add("program", r.Program != nil)
	add("program_regex", r.ProgramRegex != nil)
	add("program_raw_regex", r.ProgramRawRegex != nil)
	add("program_indirect", r.ProgramIndirect != nil)
	add("subcommand", r.Subcommand != nil)
	add("subcommand_regex", r.SubcommandRegex != nil)
	add("flags_any", r.FlagsAny != nil)
	add("flags_all", r.FlagsAll != nil)
	add("args_any", r.ArgsAny != nil)
	add("args_regex", r.ArgsRegex != nil)
	add("argline_regex", r.ArglineRegex != nil)
	add("argc_max", r.ArgcMax != nil)
	add("args_dynamic", r.ArgsDynamic != nil)
	add("inline_code", r.InlineCode != nil)
	add("inline_dynamic", r.InlineDynamic != nil)
	add("pipe_from", r.PipeFrom != nil)
	add("pipe_to", r.PipeTo != nil)
	add("arg_from", r.ArgFrom != nil)
	add("wrapped_by", r.WrappedBy != nil)
	add("assigns_any", r.AssignsAny != nil)
	add("assigns_regex", r.AssignsRegex != nil)
	add("raw_regex", r.RawRegex != nil)
	add("path", r.Path != nil)
	add("url", r.URL != nil)
	add("access", r.Access != nil)
	add("zone", r.Zone != nil)
	add("glob", r.Glob != nil)
	add("tool", r.Tool != nil)
	add("scope", r.Scope != nil)
	add("path_glob", r.PathGlob != nil)
	add("regex_any", r.RegexAny != nil)
	add("host_regex", r.HostRegex != nil)
	add("host_not", r.HostNot != nil)
	add("scheme", r.Scheme != nil)
	add("path_regex", r.PathRegex != nil)
	add("parse_error", r.ParseError != nil)
	add("limit_exceeded", r.LimitExceeded != nil)
	add("unknown_tool", r.UnknownTool != nil)
	add("steps", r.Steps != nil)
	add("count", r.Count != nil)
	add("within_calls", r.WithinCalls != 0)
	return out
}

// cond is a compiled match or unless block. Empty fields are not checked.
type cond struct {
	program                   []string
	programRe, programRawRe   []*regexp.Regexp
	programIndirect           *bool
	subcommand                []string
	subcommandRe              []*regexp.Regexp
	flagsAny, flagsAll        []string
	argsAny                   []string
	argsRe, arglineRe         []*regexp.Regexp
	argcMax                   *int
	argsDynamic, inlineCode   *bool
	inlineDynamic             *bool
	pipeFrom, pipeTo, argFrom []string
	wrappedBy                 []string
	assignsAny                []string
	assignsRe, rawRe          []*regexp.Regexp
	path, url                 *cond

	access, zone, glob, tool []string
	scope, pathGlob          []string
	regexAny                 []*regexp.Regexp
	hostRe                   []*regexp.Regexp
	hostNot, scheme          []string
	pathRe                   []*regexp.Regexp

	parseError, limitExceeded, unknownTool *bool

	steps  []step
	count  *countCond
	within int
}

// empty reports a block that checks nothing, which would fire on every
// call. A pattern dropped for a missing placeholder leaves its rule
// disabled instead (Rule.needs), so it does not count as empty.
func (c *cond) empty() bool {
	return len(c.program)+len(c.programRe)+len(c.programRawRe)+len(c.subcommand)+len(c.subcommandRe)+
		len(c.flagsAny)+len(c.flagsAll)+len(c.argsAny)+len(c.argsRe)+len(c.arglineRe)+len(c.pipeFrom)+
		len(c.pipeTo)+len(c.argFrom)+len(c.wrappedBy)+len(c.assignsAny)+len(c.assignsRe)+len(c.rawRe)+
		len(c.access)+len(c.zone)+len(c.glob)+len(c.tool)+len(c.scope)+len(c.pathGlob)+len(c.regexAny)+
		len(c.hostRe)+len(c.hostNot)+len(c.scheme)+len(c.pathRe)+len(c.steps) == 0 &&
		c.programIndirect == nil && c.argcMax == nil && c.argsDynamic == nil && c.inlineCode == nil &&
		c.inlineDynamic == nil && c.path == nil && c.url == nil && c.parseError == nil &&
		c.limitExceeded == nil && c.unknownTool == nil && c.count == nil
}

// step is one step of a sequence rule, matched against an earlier call.
type step struct {
	category, rule, access, tool, glob, program, decision []string
	samePath, sameCategory                                bool
}

type countCond struct {
	of  step
	min int
}
