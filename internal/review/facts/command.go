package facts

import (
	"encoding/base64"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
)

// Limits on what the parser takes on. Past them the call is reported as
// too large to check (Meta.LimitExceeded), never passed.
const (
	maxInput   = 256 << 10
	maxNodes   = 2000
	maxDepth   = 8
	maxDecoded = 64 << 10
)

// word is one word of a command as a dialect's parser resolved it.
type word struct {
	// value is the word's value: when part of it is only known at run time
	// (dynamic), the known parts with the source text of the rest.
	value string
	raw   string
	// prefix is the value known before the first run-time part.
	prefix  string
	dynamic bool
	quoted  bool
	glob    bool
	// varEnd is where in value the last text from a variable or a
	// substitution ends, -1 if none: a program whose basename came from
	// one is indirect.
	varEnd int
	// from are the programs whose output feeds the word.
	from []string
	// block is the code of a PowerShell script block { ... }.
	block *string
}

func lit(s string) word { return word{value: s, raw: s, prefix: s, varEnd: -1} }

// redirect is one redirection of a command.
type redirect struct {
	// op is >, >>, <, <>, <<, <<<, &>, &>>, >&, <&.
	op     string
	fd     string
	target word
	// body is a here-doc's or here-string's text.
	body      string
	bodyKnown bool
}

// rawCmd is one simple command as a dialect's parser found it, before the
// shared normalization.
type rawCmd struct {
	words   []word
	assigns []Assign
	redirs  []redirect
	raw     string
	// dotnet: words[0] is already a normalized .NET call name.
	dotnet   bool
	pipeFrom string
	// piped: stdin comes from a pipe. stdin, when known, is the text that
	// arrives on it (a here-doc, a here-string, echo LITERAL |).
	piped     bool
	stdin     *string
	heredoc   bool
	pipeRoots []Path
}

// sh is the state of one piece of shell code being parsed.
type sh struct {
	dialect string
	// cwd is the folder commands run in, "" once it cannot be known.
	cwd string
	// vars are variables assigned earlier; a nil value is one assigned
	// something only known at run time.
	vars    map[string]*string
	funcs   map[string]any
	aliases map[string]string
	depth   int
	remote  bool
	// active guards against a function calling itself.
	active map[string]bool
}

func (s *sh) child() *sh {
	c := *s
	c.depth++
	c.vars = cloneVars(s.vars)
	return &c
}

func cloneVars(m map[string]*string) map[string]*string {
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// builder collects the facts of one call.
type builder struct {
	env   Env
	call  *Call
	nodes int
}

func (b *builder) limit(note string) {
	if !b.call.Meta.LimitExceeded {
		b.call.Meta.LimitExceeded = true
	}
	b.note(note)
}

func (b *builder) parseError(note string) {
	b.call.Meta.ParseError = true
	b.note(note)
}

func (b *builder) note(n string) {
	if n != "" && !slices.Contains(b.call.Meta.Notes, n) {
		b.call.Meta.Notes = append(b.call.Meta.Notes, n)
	}
}

// budget counts a node and says whether parsing may go on.
func (b *builder) budget(st *sh) bool {
	b.nodes++
	if b.nodes > maxNodes {
		b.limit("more than 2000 commands")
		return false
	}
	if st.depth > maxDepth {
		b.limit("nested more than 8 levels deep")
		return false
	}
	return true
}

// nested parses code in a dialect, one level deeper, as code that runs
// from the current folder. It returns the commands found.
func (b *builder) nested(dialect, code string, st *sh, remote bool) []*Command {
	c := st.child()
	c.dialect, c.remote = dialect, st.remote || remote
	if c.depth > maxDepth {
		b.limit("nested more than 8 levels deep")
		return nil
	}
	if dialect == Bash || dialect == PowerShell || dialect == Cmd {
		b.call.Inputs = append(b.call.Inputs, code)
	}
	start := len(b.call.Commands)
	b.parse(dialect, code, c)
	return b.call.Commands[start:]
}

// parse dispatches code to its dialect's parser.
func (b *builder) parse(dialect, code string, st *sh) {
	if len(code) > maxInput {
		b.limit("the command is over 256 KiB")
		return
	}
	switch dialect {
	case Bash:
		b.parseBash(code, st)
	case PowerShell:
		b.parsePowerShell(code, st)
	case Cmd:
		b.parseCmd(code, st)
	}
}

func (b *builder) add(c *Command) {
	b.call.Commands = append(b.call.Commands, c)
}

// programs lists the program names of commands.
func programs(cmds []*Command) []string {
	var out []string
	for _, c := range cmds {
		if c.Program != "" && !slices.Contains(out, c.Program) {
			out = append(out, c.Program)
		}
	}
	return out
}

var (
	versionRe = regexp.MustCompile(`^(python|pip|pypy|ruby|node|perl|php|lua)([0-9]+)\.[0-9.]+$`)
	extRe     = regexp.MustCompile(`(?i)\.(exe|com|cmd|bat|ps1)$`)
)

// programName normalizes a program word: the basename without folder or
// extension, lowercased, versions folded (python3.12 is python3).
func programName(value, dialect string) (name, dir string) {
	v := slash(strings.TrimSpace(value))
	if i := strings.LastIndex(v, "/"); i >= 0 {
		dir, v = v[:i], v[i+1:]
		if dir == "" {
			dir = "/"
		}
	}
	// A PowerShell module-qualified cmdlet is no path:
	// Microsoft.PowerShell.Management\Remove-Item.
	if dialect == PowerShell && dir != "" && !strings.ContainsAny(dir, "/:~") && strings.Contains(dir, ".") &&
		!strings.HasPrefix(dir, ".") {
		dir = ""
	}
	v = extRe.ReplaceAllString(v, "")
	v = strings.ToLower(v)
	v = versionRe.ReplaceAllString(v, "$1$2")
	return v, dir
}

// command normalizes one simple command, reports it and the commands it
// wraps, and returns their records: the command itself first.
func (b *builder) command(rc *rawCmd, st *sh, wrapped []string) []*Command {
	if !b.budget(st) {
		return nil
	}
	c := &Command{Raw: rc.raw, Dialect: st.dialect, Depth: st.depth, Assigns: rc.assigns, PipeFrom: rc.pipeFrom,
		WrappedBy: slices.Clone(wrapped), Remote: st.remote}
	out := []*Command{c}
	b.add(c)
	if len(rc.words) == 0 {
		b.redirects(c, rc, st)
		return out
	}

	pw := rc.words[0]
	c.ProgramRaw = pw.raw
	switch {
	case rc.dotnet:
		c.Program = pw.value
	case pw.dynamic && pw.prefix == "":
		c.Dynamic, c.Indirect = true, true
	default:
		val := pw.value
		if pw.dynamic {
			val = pw.prefix // the known part: "$HOME/bin/$X" has no name
		}
		c.Program, c.ProgramPath = programName(val, st.dialect)
		c.Indirect = pw.varEnd > strings.LastIndex(slash(pw.value), "/")
		if pw.dynamic && pw.varEnd >= len(pw.prefix) && c.Indirect {
			c.Program, c.Dynamic = "", true
		}
	}
	if st.dialect == PowerShell && !rc.dotnet && c.ProgramPath == "" {
		if a, ok := st.aliases[c.Program]; ok {
			c.Program = a
		} else if a, ok := psAliases[c.Program]; ok {
			c.Program = a
		}
	}
	if st.dialect == Cmd && c.ProgramPath == "" && strings.HasSuffix(c.Program, ".") {
		c.Program = strings.TrimSuffix(c.Program, ".")
	}
	c.ArgFrom = append(c.ArgFrom, pw.from...)
	args := rc.words[1:]
	for _, a := range args {
		c.Args = append(c.Args, Arg{Value: a.value, Raw: a.raw, Dynamic: a.dynamic, Quoted: a.quoted})
		for _, p := range a.from {
			if !slices.Contains(c.ArgFrom, p) {
				c.ArgFrom = append(c.ArgFrom, p)
			}
		}
	}
	pf := parseFlags(c.Program, args, st.dialect)
	c.Flags, c.Subcommand = pf.flags, pf.subcommand

	// A program named with a folder is a file being run.
	if c.ProgramPath != "" && !c.Dynamic && !rc.dotnet && !st.remote {
		b.path(c, pw, Exec, st)
	}
	b.redirects(c, rc, st)
	inner := b.inline(c, rc, args, pf, st)
	b.paths(c, rc, args, pf, st)
	b.urls(c, args, pf, st)
	b.writes(c, rc, args, pf, st)
	out = append(out, inner...)
	out = append(out, b.unwrap(c, rc, args, pf, st, wrapped)...)
	return out
}

// path resolves a word as a path with an access and records it.
func (b *builder) path(c *Command, w word, acc Access, st *sh) {
	if st.remote {
		return
	}
	text := w.value
	if w.raw == "~" || strings.HasPrefix(w.raw, "~/") || strings.HasPrefix(w.raw, `~\`) {
		if h := b.env.Home; h != "" && !w.quoted {
			text = slash(h) + text[1:]
		}
	}
	if strings.HasPrefix(strings.ToLower(text), "file://") {
		text = strings.TrimPrefix(text[7:], "localhost")
		if b.env.OS == "windows" && len(text) > 2 && text[0] == '/' && driveRe.MatchString(text[1:]) {
			text = text[1:]
		}
	}
	text = stripPSProvider(text)
	prefix := w.prefix
	if w.dynamic && strings.HasPrefix(prefix, "~") && b.env.Home != "" {
		prefix = slash(b.env.Home) + prefix[1:]
		text = slash(b.env.Home) + text[1:]
	}
	r := b.env.resolve(text, prefix, w.dynamic, w.glob, st.cwd, st.dialect)
	p := Path{Path: r.path, Raw: w.raw, Access: acc, Zone: r.zone, Glob: r.glob, Dynamic: r.dynamic, Tool: b.call.Kind}
	b.addPath(c, p)
	if strings.HasPrefix(r.path, "/dev/tcp/") || strings.HasPrefix(r.path, "/dev/udp/") {
		parts := strings.Split(r.path, "/")
		if len(parts) >= 5 {
			b.addURL(c, URL{Raw: w.raw, Scheme: parts[2], Host: normalizeHost(parts[3]), Path: "/" + strings.Join(parts[4:], "/")})
		}
	}
}

func (b *builder) addPath(c *Command, p Path) {
	if c != nil {
		c.Paths = append(c.Paths, p)
	}
	b.call.Paths = append(b.call.Paths, p)
}

var psProviderRe = regexp.MustCompile(`(?i)^(microsoft\.powershell\.core\\)?filesystem::`)

func stripPSProvider(s string) string { return psProviderRe.ReplaceAllString(s, "") }

// redirects records the files a command's redirections read and write.
func (b *builder) redirects(c *Command, rc *rawCmd, st *sh) {
	for _, r := range rc.redirs {
		switch r.op {
		case ">", ">>", ">|", "&>", "&>>", "<>":
			b.path(c, r.target, Write, st)
		case ">&", "<&":
			if t := r.target.value; t != "-" && !isNumber(t) {
				b.path(c, r.target, Write, st)
			}
		case "<":
			b.path(c, r.target, Read, st)
		}
	}
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// parsed is a command's arguments sorted into flags and positionals.
type parsed struct {
	flags []Flag
	// pos are the indices in args of the positional words.
	pos []int
	// vals are the indices of each flag's values, by flag name.
	vals       map[string][]int
	subcommand string
}

func (p *parsed) has(names ...string) bool {
	for _, f := range p.flags {
		if slices.Contains(names, f.Name) {
			return true
		}
	}
	return false
}

func (p *parsed) value(names ...string) (int, bool) {
	for _, n := range names {
		if v := p.vals[n]; len(v) > 0 {
			return v[0], true
		}
	}
	return 0, false
}

// isCmdlet reports whether a program is a PowerShell cmdlet, whose
// parameters are written -Name.
func isCmdlet(prog string) bool {
	if _, ok := psCmdlets[prog]; ok {
		return true
	}
	verb, _, ok := strings.Cut(prog, "-")
	return ok && psVerbs[verb]
}

var psVerbs = map[string]bool{"add": true, "clear": true, "close": true, "copy": true, "enter": true, "exit": true,
	"find": true, "format": true, "get": true, "hide": true, "join": true, "lock": true, "move": true, "new": true,
	"open": true, "optimize": true, "pop": true, "push": true, "redo": true, "remove": true, "rename": true,
	"reset": true, "resize": true, "search": true, "select": true, "set": true, "show": true, "skip": true,
	"split": true, "step": true, "switch": true, "undo": true, "unlock": true, "watch": true, "connect": true,
	"disconnect": true, "read": true, "receive": true, "send": true, "write": true, "backup": true,
	"checkpoint": true, "compare": true, "compress": true, "convert": true, "convertfrom": true, "convertto": true,
	"dismount": true, "edit": true, "expand": true, "export": true, "group": true, "import": true,
	"initialize": true, "limit": true, "merge": true, "mount": true, "out": true, "publish": true, "restore": true,
	"save": true, "sync": true, "unpublish": true, "update": true, "approve": true, "assert": true, "build": true,
	"complete": true, "confirm": true, "deny": true, "deploy": true, "disable": true, "enable": true,
	"install": true, "invoke": true, "register": true, "request": true, "restart": true, "resume": true,
	"start": true, "stop": true, "submit": true, "suspend": true, "uninstall": true, "unregister": true,
	"wait": true, "debug": true, "measure": true, "ping": true, "repair": true, "resolve": true, "test": true,
	"trace": true, "block": true, "grant": true, "protect": true, "revoke": true, "unblock": true,
	"unprotect": true, "use": true, "foreach": true, "where": true, "sort": true, "tee": true}

// parseFlags sorts a command's arguments into flags and positionals, the
// way the program reads them.
func parseFlags(prog string, args []word, dialect string) parsed {
	switch {
	case prog == "powershell" || prog == "pwsh" || prog == "powershell_ise":
		return psExeFlags(args)
	case dialect == PowerShell && isCmdlet(prog):
		return psFlags(prog, args)
	case winSwitches[prog] || dialect == Cmd:
		return winFlags(prog, args, dialect)
	}
	return unixFlags(prog, args)
}

func unixFlags(prog string, args []word) parsed {
	sp := specs[prog]
	takes := func(name string) bool { return slices.Contains(sp.values, name) || slices.Contains(sp.global, name) }
	p := parsed{vals: map[string][]int{}}
	var sub []string
	end := false
	addVal := func(name string, i int) { p.vals[name] = append(p.vals[name], i) }
	for i := 0; i < len(args); i++ {
		a := args[i]
		v := a.value
		if end || a.dynamic && !strings.HasPrefix(a.prefix, "-") || !strings.HasPrefix(v, "-") || v == "-" || isNumber(v) {
			p.pos = append(p.pos, i)
			if !a.dynamic || a.prefix != "" {
				sub = append(sub, v)
			}
			continue
		}
		if v == "--" {
			end = true
			continue
		}
		if strings.HasPrefix(v, "--") {
			name, val, eq := strings.Cut(v, "=")
			f := Flag{Name: name, Value: val}
			if !eq && takes(name) && i+1 < len(args) {
				i++
				f.Value = args[i].value
				addVal(name, i)
			} else if eq {
				addVal(name, -1)
			}
			p.flags = append(p.flags, f)
			continue
		}
		// -name=value (terraform -chdir=dir, dd-like)
		if name, val, eq := strings.Cut(v, "="); eq && takes(name) {
			p.flags = append(p.flags, Flag{Name: name, Value: val})
			addVal(name, -1)
			continue
		}
		if takes(v) || len(v) == 2 {
			f := Flag{Name: v}
			if takes(v) && i+1 < len(args) {
				i++
				f.Value = args[i].value
				addVal(v, i)
			}
			p.flags = append(p.flags, f)
			continue
		}
		// A group of short flags: -rf is -r -f; with a spec, -ofile is -o
		// with "file". Without one, only all-letter groups are split. The
		// group itself stays too, for single-dash long options (-delete).
		body := v[1:]
		split := sp.values != nil || allLetters(body)
		p.flags = append(p.flags, Flag{Name: v})
		if !split {
			continue
		}
		for j := 0; j < len(body); j++ {
			ch := "-" + string(body[j])
			if takes(ch) {
				f := Flag{Name: ch}
				if rest := body[j+1:]; rest != "" {
					f.Value = rest
				} else if i+1 < len(args) {
					i++
					f.Value = args[i].value
					addVal(ch, i)
				}
				p.flags = append(p.flags, f)
				break
			}
			p.flags = append(p.flags, Flag{Name: ch})
		}
	}
	p.subcommand = strings.ToLower(strings.Join(sub, " "))
	return p
}

func allLetters(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return s != ""
}

// winValues are the /switches of Windows programs that take the next word
// as their value.
var winValues = map[string][]string{
	"schtasks":  {"/tr", "/tn", "/sc", "/ru", "/rp", "/s", "/u", "/p", "/mo", "/d", "/m", "/st", "/sd", "/ed", "/et", "/ri", "/du", "/xml", "/i", "/rl", "/delay"},
	"reg":       {"/v", "/d", "/t", "/s", "/se", "/reg:32", "/reg:64"},
	"icacls":    {"/grant", "/deny", "/remove", "/setowner", "/save", "/restore", "/setintegritylevel", "/inheritance", "/grant:r", "/remove:g", "/remove:d"},
	"takeown":   {"/f", "/s", "/u", "/p", "/d"},
	"forfiles":  {"/p", "/m", "/c", "/d"},
	"runas":     {},
	"start":     {"/d"},
	"wmic":      {"/node", "/user", "/password", "/namespace"},
	"bitsadmin": {"/transfer", "/create", "/addfile", "/setnotifycmdline", "/resume", "/complete"},
	"net":       {},
	"sc":        {},
	"shutdown":  {"/t", "/c", "/d", "/m"},
	"taskkill":  {"/im", "/pid", "/fi", "/s", "/u", "/p"},
	"cipher":    {"/w"},
	"subst":     {},
	"setx":      {"/s", "/u", "/p", "/f", "/a", "/r", "/d"},
}

func winFlags(prog string, args []word, dialect string) parsed {
	p := parsed{vals: map[string][]int{}}
	values := winValues[prog]
	var sub []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		v := a.value
		isSwitch := strings.HasPrefix(v, "/") && len(v) > 1 && !strings.Contains(v[1:], "/") ||
			strings.HasPrefix(v, "/") && strings.Count(v, "/") > 1 && allSwitches(v) ||
			strings.HasPrefix(v, "-") && len(v) > 1 && !isNumber(v)
		if dialect != Cmd && strings.HasPrefix(v, "/") && len(v) > 3 && !allSwitches(v) {
			isSwitch = false // a path written /like/this
		}
		if a.dynamic || !isSwitch {
			p.pos = append(p.pos, i)
			sub = append(sub, v)
			continue
		}
		lower := strings.ToLower(v)
		// /s/q is /s and /q.
		if strings.HasPrefix(lower, "/") && strings.Count(lower, "/") > 1 && allSwitches(lower) {
			for _, s := range strings.Split(lower[1:], "/") {
				p.flags = append(p.flags, Flag{Name: "/" + s})
			}
			continue
		}
		name, val, inline := lower, "", false
		if j := strings.IndexAny(lower[1:], ":="); j >= 0 && !slices.Contains(values, lower) {
			name, val, inline = lower[:j+1], v[j+2:], true
		}
		f := Flag{Name: name, Value: val}
		if inline {
			p.vals[name] = append(p.vals[name], -1)
		} else if slices.Contains(values, name) && i+1 < len(args) {
			i++
			f.Value = args[i].value
			p.vals[name] = append(p.vals[name], i)
		}
		p.flags = append(p.flags, f)
	}
	p.subcommand = strings.ToLower(strings.Join(sub, " "))
	return p
}

func allSwitches(v string) bool {
	for _, s := range strings.Split(strings.TrimPrefix(v, "/"), "/") {
		if s == "" || len(s) > 3 {
			return false
		}
	}
	return true
}

// psExeParams are powershell.exe's and pwsh's own parameters; the short
// forms are the ones they accept besides unique prefixes.
var (
	psExeParams = []string{"-command", "-encodedcommand", "-encodedarguments", "-executionpolicy", "-file",
		"-noprofile", "-noninteractive", "-nologo", "-noexit", "-windowstyle", "-inputformat", "-outputformat",
		"-version", "-workingdirectory", "-sta", "-mta", "-configurationname", "-settingsfile", "-login",
		"-custompipename", "-commandwithargs", "-psconsolefile", "-help"}
	psExeShort = map[string]string{"-c": "-command", "-e": "-encodedcommand", "-ec": "-encodedcommand",
		"-en": "-encodedcommand", "-enc": "-encodedcommand", "-ea": "-encodedarguments", "-ex": "-executionpolicy",
		"-ep": "-executionpolicy", "-exec": "-executionpolicy", "-f": "-file", "-w": "-windowstyle",
		"-nop": "-noprofile", "-noni": "-noninteractive", "-nol": "-nologo", "-noe": "-noexit", "-v": "-version",
		"-wd": "-workingdirectory", "-l": "-login", "-cwa": "-commandwithargs", "-o": "-outputformat",
		"-of": "-outputformat", "-i": "-inputformat", "-if": "-inputformat"}
	psExeValues = []string{"-encodedcommand", "-encodedarguments", "-executionpolicy", "-windowstyle",
		"-inputformat", "-outputformat", "-version", "-workingdirectory", "-configurationname", "-settingsfile",
		"-custompipename", "-psconsolefile"}
)

// psExeFlags reads powershell.exe's parameters, written - or /, in full or
// abbreviated: -enc is -encodedcommand, -w is -windowstyle. After -Command
// or -File the rest is the command and its arguments.
func psExeFlags(args []word) parsed {
	p := parsed{vals: map[string][]int{}}
	var sub []string
	for i := 0; i < len(args); i++ {
		v := args[i].value
		if args[i].dynamic || len(v) < 2 || v[0] != '-' && v[0] != '/' {
			p.pos = append(p.pos, i)
			sub = append(sub, v)
			continue
		}
		raw, inline, hasInline := strings.Cut(strings.ToLower("-"+v[1:]), ":")
		name := raw
		if s, ok := psExeShort[raw]; ok {
			name = s
		} else {
			var hits []string
			for _, x := range psExeParams {
				if strings.HasPrefix(x, raw) {
					hits = append(hits, x)
				}
			}
			if len(hits) == 1 {
				name = hits[0]
			}
		}
		f := Flag{Name: name}
		switch {
		case hasInline:
			f.Value = inline
		case slices.Contains(psExeValues, name) && i+1 < len(args):
			i++
			f.Value = args[i].value
			p.vals[name] = append(p.vals[name], i)
		}
		p.flags = append(p.flags, f)
		if name == "-command" || name == "-file" || name == "-commandwithargs" {
			for j := i + 1; j < len(args); j++ {
				p.pos = append(p.pos, j)
			}
			break
		}
	}
	p.subcommand = strings.ToLower(strings.Join(sub, " "))
	return p
}

// psFlags reads a cmdlet's parameters: -Name, -Name:value, abbreviations
// and aliases resolved, positional words bound to the cmdlet's positional
// parameters.
func psFlags(prog string, args []word) parsed {
	cl := psCmdlets[prog]
	all := append(append(append([]string(nil), cl.switches...), cl.values...), psCommon...)
	takes := func(n string) bool { return slices.Contains(cl.values, n) || slices.Contains(psCommonValues, n) }
	resolve := func(n string) string {
		if a, ok := cl.aliases[n]; ok {
			return a
		}
		if slices.Contains(all, n) {
			return n
		}
		var hits []string
		for _, x := range all {
			if strings.HasPrefix(x, n) {
				hits = append(hits, x)
			}
		}
		if len(hits) == 1 {
			return hits[0]
		}
		return n
	}
	p := parsed{vals: map[string][]int{}}
	var sub []string
	nextPos := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		v := a.value
		if !a.quoted && strings.HasPrefix(v, "-") && len(v) > 1 && !isNumber(v) && !a.dynamic {
			raw, _, inline := strings.Cut(strings.ToLower(v), ":")
			name := resolve(raw)
			f := Flag{Name: name}
			if inline {
				f.Value = v[len(raw)+1:]
				p.vals[name] = append(p.vals[name], -1)
			} else if takes(name) && i+1 < len(args) {
				i++
				f.Value = args[i].value
				p.vals[name] = append(p.vals[name], i)
			}
			p.flags = append(p.flags, f)
			continue
		}
		// Bind the word to the next positional parameter not given by name.
		for nextPos < len(cl.positional) && len(p.vals[cl.positional[nextPos]]) > 0 {
			nextPos++
		}
		if nextPos < len(cl.positional) {
			name := cl.positional[nextPos]
			p.vals[name] = append(p.vals[name], i)
			nextPos++
		}
		p.pos = append(p.pos, i)
		sub = append(sub, v)
	}
	p.subcommand = strings.ToLower(strings.Join(sub, " "))
	return p
}

// pathLike reports whether a word with no known meaning looks like a path.
func pathLike(v string) bool {
	if v == "" || strings.Contains(v, "://") {
		return false
	}
	return strings.ContainsAny(v, `/\`) || strings.HasPrefix(v, "~") || strings.HasPrefix(v, ".") ||
		driveRe.MatchString(v)
}

// paths records the files and folders a command's arguments name.
func (b *builder) paths(c *Command, rc *rawCmd, args []word, pf parsed, st *sh) {
	prog := c.Program
	pos := func(k int) word { return args[pf.pos[k]] }
	switch prog {
	case "cd", "pushd", "chdir", "set-location", "push-location", "sl":
		b.chdir(c, args, pf, st)
		return
	case "popd", "pop-location":
		st.cwd = ""
		return
	case "git":
		b.gitPaths(c, args, pf, st)
		return
	case "find":
		b.findPaths(c, args, pf, st)
		return
	case "tar", "bsdtar":
		b.tarPaths(c, args, pf, st)
		return
	case "dd":
		for _, a := range args {
			if v, ok := strings.CutPrefix(a.value, "of="); ok {
				w := a
				w.value, w.prefix = v, strings.TrimPrefix(a.prefix, "of=")
				b.path(c, w, Write, st)
			} else if v, ok := strings.CutPrefix(a.value, "if="); ok {
				w := a
				w.value, w.prefix = v, strings.TrimPrefix(a.prefix, "if=")
				b.path(c, w, Read, st)
			}
		}
		return
	case "docker", "podman":
		for _, n := range []string{"-v", "--volume", "--mount"} {
			for _, i := range pf.vals[n] {
				if i < 0 {
					continue
				}
				v := args[i].value
				host := v
				if n == "--mount" {
					host = ""
					for _, kv := range strings.Split(v, ",") {
						if s, ok := strings.CutPrefix(kv, "source="); ok {
							host = s
						} else if s, ok := strings.CutPrefix(kv, "src="); ok {
							host = s
						}
					}
				} else if j := hostVolume(v); j > 0 {
					host = v[:j]
				}
				if host != "" && pathLike(host) {
					b.path(c, lit(host), Link, st)
				}
			}
		}
		return
	case "mount", "bindfs":
		if pf.has("--bind", "-B", "--rbind") && len(pf.pos) >= 1 {
			b.path(c, pos(0), Link, st)
			if len(pf.pos) >= 2 {
				b.path(c, pos(1), Write, st)
			}
		}
		return
	case "ln":
		b.lnPaths(c, args, pf, st)
		return
	case "sshfs":
		if len(pf.pos) >= 2 {
			b.path(c, pos(1), Link, st)
		}
		return
	case "reg":
		b.regPaths(c, args, pf, st)
		return
	case "set-itemproperty", "new-itemproperty", "remove-itemproperty", "rename-itemproperty", "copy-itemproperty",
		"set-item", "clear-itemproperty":
		acc := Write
		if prog == "remove-itemproperty" {
			acc = Delete
		}
		for _, i := range pf.vals["-path"] {
			if i >= 0 {
				b.path(c, args[i], acc, st)
			}
		}
		for _, i := range pf.vals["-literalpath"] {
			if i >= 0 {
				b.path(c, args[i], acc, st)
			}
		}
		return
	case "new-item":
		typ := ""
		if i, ok := pf.value("-itemtype"); ok && i >= 0 {
			typ = strings.ToLower(args[i].value)
		}
		for _, n := range []string{"-path", "-name"} {
			for _, i := range pf.vals[n] {
				if i >= 0 {
					b.path(c, args[i], Write, st)
				}
			}
		}
		if typ == "symboliclink" || typ == "junction" || typ == "hardlink" {
			for _, n := range []string{"-target", "-value"} {
				for _, i := range pf.vals[n] {
					if i >= 0 {
						b.path(c, args[i], Link, st)
					}
				}
			}
		}
		return
	}

	acc, known := accessByProgram[prog]
	switch {
	case interpreters[prog] && c.Inline == nil:
		if prog == "source" || prog == "." {
			break
		}
		if !hasInlineFlag(prog, args, pf) && len(pf.pos) > 0 {
			if first := pos(0); first.value != "-" && !awks[prog] {
				b.path(c, first, Exec, st)
			}
		}
		return
	case !known:
		if st.dialect == PowerShell && isCmdlet(prog) {
			for _, n := range []string{"-path", "-literalpath", "-filepath"} {
				for _, i := range pf.vals[n] {
					if i >= 0 {
						b.path(c, args[i], Read, st)
					}
				}
			}
			return
		}
		for _, k := range pf.pos {
			if a := args[k]; !a.dynamic && pathLike(a.value) && !strings.Contains(a.value, "@") {
				b.path(c, a, Read, st)
			}
		}
		return
	}

	positional := pf.pos
	if st.dialect == PowerShell && isCmdlet(prog) {
		positional = nil // bound to named parameters below
	}
	skip := acc.skip
	if (prog == "grep" || prog == "egrep" || prog == "fgrep" || prog == "rg" || prog == "ag") && pf.has("-e", "--regexp", "-f", "--file") {
		skip = 0
	}
	if prog == "sed" && pf.has("-e", "--expression", "-f", "--file") {
		skip = 0
	}
	if (prog == "awk" || prog == "gawk") && pf.has("-f", "--file") {
		skip = 0
	}
	all := acc.all
	if prog == "sed" && pf.has("-i", "--in-place") || prog == "sed" && hasGroupFlag(pf, 'i') {
		all = Write
	}
	if all != "" {
		n := len(positional) - skip
		for k, i := range positional {
			if k < skip {
				continue
			}
			a := all
			if acc.last != "" && k == len(positional)-1 && n >= 2 && !pf.has("-t", "--target-directory") {
				a = acc.last
			}
			b.path(c, args[i], a, st)
		}
	}
	if acc.first != "" && len(positional) > 0 {
		b.path(c, args[positional[0]], acc.first, st)
	}
	if acc.second != "" && len(positional) > 1 {
		b.path(c, args[positional[1]], acc.second, st)
	}
	for name, a := range acc.flags {
		for _, i := range pf.vals[name] {
			if i >= 0 {
				b.path(c, args[i], a, st)
			} else {
				for _, f := range c.Flags {
					if f.Name == name && f.Value != "" {
						b.path(c, lit(f.Value), a, st)
					}
				}
			}
		}
	}
	if prog == "curl" && (pf.has("-O", "--remote-name", "-J", "--remote-name-all") || hasGroupFlag(pf, 'O')) {
		b.path(c, lit("."), Write, st)
	}
	if (prog == "wget") && !pf.has("-O", "--output-document") && !pf.has("--spider") {
		b.path(c, lit("."), Write, st)
	}
	if prog == "unzip" && !pf.has("-d", "-l", "-t") {
		b.path(c, lit("."), Write, st)
	}
}

func hasGroupFlag(pf parsed, ch byte) bool {
	for _, f := range pf.flags {
		if len(f.Name) > 2 && f.Name[0] == '-' && f.Name[1] != '-' && strings.IndexByte(f.Name[1:], ch) >= 0 {
			return true
		}
	}
	return false
}

// hostVolume finds where a -v HOST:CTR value's host part ends, skipping a
// Windows drive letter.
func hostVolume(v string) int {
	start := 0
	if driveRe.MatchString(v) {
		start = 2
	}
	if j := strings.Index(v[start:], ":"); j >= 0 {
		return start + j
	}
	return -1
}

func hasInlineFlag(prog string, args []word, pf parsed) bool {
	spec, ok := inlineFlags[prog]
	if !ok {
		return false
	}
	if shells[prog] {
		return hasGroupFlag(pf, 'c') || pf.has("-c") || pf.has("-s")
	}
	return pf.has(spec.flags...)
}

// chdir follows cd: later relative paths resolve against the new folder.
func (b *builder) chdir(c *Command, args []word, pf parsed, st *sh) {
	var target *word
	if i, ok := pf.value("-path", "-literalpath"); ok && i >= 0 {
		target = &args[i]
	} else if len(pf.pos) > 0 {
		target = &args[pf.pos[0]]
	}
	switch {
	case target == nil:
		if b.env.Home != "" {
			st.cwd = b.env.clean(slash(b.env.Home))
		}
		return
	case target.value == "-" || target.dynamic && target.prefix == "":
		st.cwd = ""
		return
	}
	b.path(c, *target, List, st)
	p := c.Paths[len(c.Paths)-1]
	if p.Dynamic || p.Zone == ZoneUnknown || st.cwd == "" {
		st.cwd = ""
		return
	}
	st.cwd = p.Path
}

// gitPaths: -C moves the folder; clean deletes in it; clone writes its
// destination; worktrees and submodules write theirs.
func (b *builder) gitPaths(c *Command, args []word, pf parsed, st *sh) {
	gst := *st
	for _, i := range pf.vals["-C"] {
		if i >= 0 {
			w := args[i]
			b.path(c, w, List, &gst)
			gst.cwd = c.Paths[len(c.Paths)-1].Path
		}
	}
	sub := strings.Fields(c.Subcommand)
	if len(sub) == 0 {
		return
	}
	positional := pf.pos
	// Drop the subcommand word itself.
	rest := []int{}
	for _, i := range positional {
		if strings.ToLower(args[i].value) == sub[0] && len(rest) == 0 && len(positional) > 0 && i == positional[0] {
			continue
		}
		rest = append(rest, i)
	}
	switch sub[0] {
	case "clean":
		b.path(c, lit("."), Delete, &gst)
		for _, i := range rest {
			b.path(c, args[i], Delete, &gst)
		}
	case "clone":
		if len(rest) >= 2 {
			b.path(c, args[rest[1]], Write, &gst)
		} else if len(rest) == 1 {
			b.path(c, lit("."), Write, &gst)
		}
	case "rm":
		for _, i := range rest {
			b.path(c, args[i], Delete, &gst)
		}
	case "checkout", "restore", "reset", "stash":
		b.path(c, lit("."), Write, &gst)
	case "init":
		if len(rest) > 0 {
			b.path(c, args[rest[0]], Write, &gst)
		} else {
			b.path(c, lit("."), Write, &gst)
		}
	case "apply", "am":
		for _, i := range rest {
			b.path(c, args[i], Read, &gst)
		}
	}
}

// findPaths: the roots are listed, and deleted with -delete.
func (b *builder) findPaths(c *Command, args []word, pf parsed, st *sh) {
	roots := findRoots(args)
	acc := List
	for _, a := range args {
		if a.value == "-delete" {
			acc = Delete
		}
	}
	if len(roots) == 0 {
		roots = []word{lit(".")}
	}
	for _, r := range roots {
		b.path(c, r, acc, st)
	}
	for _, flag := range []string{"-fprint", "-fprintf", "-fls"} {
		for _, i := range pf.vals[flag] {
			if i >= 0 {
				b.path(c, args[i], Write, st)
			}
		}
	}
}

// findRoots are find's arguments before the first expression word.
func findRoots(args []word) []word {
	var out []word
	for _, a := range args {
		v := a.value
		if strings.HasPrefix(v, "-") && v != "-" || v == "(" || v == "!" || v == `\(` {
			break
		}
		out = append(out, a)
	}
	return out
}

func (b *builder) tarPaths(c *Command, args []word, pf parsed, st *sh) {
	extract := pf.has("-x", "--extract", "--get") || hasGroupFlag(pf, 'x') ||
		len(args) > 0 && !strings.HasPrefix(args[0].value, "-") && strings.ContainsRune(args[0].value, 'x')
	create := pf.has("-c", "--create", "-r", "--append", "-u", "--update") || hasGroupFlag(pf, 'c') ||
		len(args) > 0 && !strings.HasPrefix(args[0].value, "-") && strings.ContainsRune(args[0].value, 'c')
	archive := Read
	if create {
		archive = Write
	}
	for _, n := range []string{"-f", "--file"} {
		for _, i := range pf.vals[n] {
			if i >= 0 {
				b.path(c, args[i], archive, st)
			}
		}
	}
	for _, f := range pf.flags {
		if len(f.Name) > 2 && f.Name[0] == '-' && f.Name[1] != '-' && strings.HasSuffix(f.Name, "f") && f.Value == "" {
			continue
		}
	}
	dir := Read
	if extract {
		dir = Write
	}
	gotDir := false
	for _, n := range []string{"-C", "--directory"} {
		for _, i := range pf.vals[n] {
			if i >= 0 {
				b.path(c, args[i], dir, st)
				gotDir = true
			}
		}
	}
	if extract && !gotDir {
		b.path(c, lit("."), Write, st)
	}
	for k, i := range pf.pos {
		if k == 0 && !strings.HasPrefix(args[0].value, "-") && i == 0 {
			continue // the old-style option letters: tar xzf
		}
		b.path(c, args[i], Read, st)
	}
}

// lnPaths: ln [-s] TARGET... LINK makes LINK, pointing at TARGET. A
// symbolic link's relative target is relative to the link's folder, not
// to the current one.
func (b *builder) lnPaths(c *Command, args []word, pf parsed, st *sh) {
	symbolic := pf.has("-s", "--symbolic") || hasGroupFlag(pf, 's')
	targets := pf.pos
	var linkDir string
	if i, ok := pf.value("-t", "--target-directory"); ok && i >= 0 {
		b.path(c, args[i], Write, st)
		linkDir = c.Paths[len(c.Paths)-1].Path
	} else if len(targets) >= 2 {
		link := args[targets[len(targets)-1]]
		targets = targets[:len(targets)-1]
		b.path(c, link, Write, st)
		linkDir = path.Dir(c.Paths[len(c.Paths)-1].Path)
	}
	for _, i := range targets {
		t := args[i]
		if symbolic && linkDir != "" && !t.dynamic && !b.env.isAbs(slash(t.value)) && !strings.HasPrefix(t.value, "~") {
			abs := lit(b.env.join(linkDir, slash(t.value)))
			abs.raw = t.raw
			b.path(c, abs, Link, st)
			continue
		}
		b.path(c, t, Link, st)
	}
}

// regPaths: reg add/delete/import KEY changes the registry.
func (b *builder) regPaths(c *Command, args []word, pf parsed, st *sh) {
	if len(pf.pos) < 2 {
		return
	}
	op := strings.ToLower(args[pf.pos[0]].value)
	acc := Read
	switch op {
	case "add", "import", "copy", "restore", "load", "unload":
		acc = Write
	case "delete":
		acc = Delete
	}
	key := args[pf.pos[1]]
	if k, ok := registryKey(key.value); ok {
		b.addPath(c, Path{Path: k, Raw: key.raw, Access: acc, Zone: ZoneRegistry, Tool: b.call.Kind})
	}
	if op == "copy" && len(pf.pos) > 2 {
		if k, ok := registryKey(args[pf.pos[2]].value); ok {
			b.addPath(c, Path{Path: k, Raw: args[pf.pos[2]].raw, Access: Write, Zone: ZoneRegistry, Tool: b.call.Kind})
		}
	}
	if (op == "save" || op == "export") && len(pf.pos) > 2 {
		b.path(c, args[pf.pos[2]], Write, st)
	}
}

var (
	schemeRe  = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://([^\s'"<>()]+)`)
	scpHostRe = regexp.MustCompile(`^(?:([^@\s/:]+)@)?([A-Za-z0-9][A-Za-z0-9.\-]*|\[[0-9a-fA-F:.]+\]):(.*)$`)
)

// remoteHosts are the programs whose first positional is a host.
var remoteHosts = map[string]bool{"ssh": true, "mosh": true, "autossh": true, "telnet": true, "nc": true, "ncat": true,
	"netcat": true, "sftp": true, "ftp": true, "lftp": true, "tftp": true, "rsh": true, "rlogin": true,
	"plink": true, "sshpass": false, "socat": false, "ping": false}

// urls records the URLs and remote hosts a command names.
func (b *builder) urls(c *Command, args []word, pf parsed, st *sh) {
	for _, a := range args {
		for _, m := range schemeRe.FindAllStringSubmatch(a.value, -1) {
			b.addURL(c, parseURL(m[0]))
		}
	}
	switch c.Program {
	case "scp", "rsync", "git", "sftp", "pscp":
		for _, k := range pf.pos {
			v := args[k].value
			if strings.Contains(v, "://") || driveRe.MatchString(v) && len(v) > 2 && (v[2] == '/' || v[2] == '\\') {
				continue
			}
			if m := scpHostRe.FindStringSubmatch(v); m != nil && (m[1] != "" || c.Program != "git") {
				b.addURL(c, URL{Raw: v, Scheme: "ssh", Host: normalizeHost(m[2]), Path: m[3]})
			}
		}
	}
	if remoteHosts[c.Program] && len(pf.pos) > 0 {
		h := args[pf.pos[0]].value
		if !strings.Contains(h, "://") {
			if i := strings.LastIndex(h, "@"); i >= 0 {
				h = h[i+1:]
			}
			if h != "" {
				b.addURL(c, URL{Raw: args[pf.pos[0]].raw, Scheme: c.Program, Host: normalizeHost(h)})
			}
		}
	}
	if i, ok := pf.value("-uri", "--url"); ok && i >= 0 && !strings.Contains(args[i].value, "://") {
		b.addURL(c, parseURL("https://"+args[i].value))
	}
	// curl ifconfig.me fetches http://ifconfig.me.
	if bareHostTools[c.Program] {
		for _, k := range pf.pos {
			if v := args[k].value; !args[k].dynamic && !strings.Contains(v, "://") && hostLikeRe.MatchString(v) {
				b.addURL(c, parseURL("http://"+v))
			}
		}
	}
}

var (
	bareHostTools = map[string]bool{"curl": true, "wget": true, "wget2": true, "http": true, "https": true,
		"xh": true, "aria2c": true, "axel": true, "lwp-request": true, "lwp-download": true}
	hostLikeRe = regexp.MustCompile(`^(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}(?::[0-9]+)?(?:/\S*)?$|^[0-9]{1,3}(?:\.[0-9]{1,3}){3}(?::[0-9]+)?(?:/\S*)?$`)
)

func (b *builder) addURL(c *Command, u URL) {
	if c != nil {
		c.URLs = append(c.URLs, u)
	}
	b.call.URLs = append(b.call.URLs, u)
}

// writes records text written to a file from the command line itself, so
// the content rules see it as they would an edit: echo ... > file, a here-
// doc into cat or tee, Set-Content -Value.
func (b *builder) writes(c *Command, rc *rawCmd, args []word, pf parsed, st *sh) {
	var targets []string
	for _, r := range rc.redirs {
		if r.op == ">" || r.op == ">>" || r.op == ">|" || r.op == "&>" || r.op == "&>>" {
			targets = append(targets, r.target.value)
		}
	}
	text := ""
	switch c.Program {
	case "echo", "printf", "write-output", "write-host":
		var parts []string
		for _, i := range pf.pos {
			parts = append(parts, args[i].value)
		}
		text = strings.Join(parts, " ")
	case "cat", "tee", "tee-object":
		if rc.stdin != nil {
			text = *rc.stdin
		}
		if c.Program != "cat" {
			for _, i := range pf.pos {
				targets = append(targets, args[i].value)
			}
		}
	case "set-content", "add-content", "out-file":
		if i, ok := pf.value("-value", "-inputobject"); ok && i >= 0 {
			text = args[i].value
		}
		for _, n := range []string{"-path", "-literalpath", "-filepath"} {
			for _, i := range pf.vals[n] {
				if i >= 0 {
					targets = append(targets, args[i].value)
				}
			}
		}
	}
	if text == "" {
		return
	}
	for _, t := range targets {
		r := b.env.resolve(t, t, false, false, st.cwd, st.dialect)
		if t == "~" || strings.HasPrefix(t, "~/") {
			r.path = slash(b.env.Home) + t[1:]
		}
		b.call.Contents = append(b.call.Contents, Content{Scope: ScopeEdit, Path: r.path, Text: text})
	}
}

// inline handles code a command carries: bash -c, python -c, a here-doc
// into a shell, powershell -Command, cmd /c. Shell code is parsed again.
func (b *builder) inline(c *Command, rc *rawCmd, args []word, pf parsed, st *sh) []*Command {
	prog := c.Program
	spec, ok := inlineFlags[prog]
	if !ok && !awks[prog] && prog != "deno" {
		return nil
	}
	var code *word
	switch {
	case awks[prog]:
		if !pf.has("-f", "--file") && len(pf.pos) > 0 {
			code = &args[pf.pos[0]]
		}
		spec.lang = "awk"
	case prog == "deno":
		if len(pf.pos) > 1 && strings.ToLower(args[pf.pos[0]].value) == "eval" {
			code = &args[pf.pos[1]]
		}
		spec.lang = "javascript"
	case prog == "powershell" || prog == "pwsh":
		return nil // unwrap handles it, with its own parameters
	case prog == "cmd":
		return nil
	case shells[prog] || prog == "fish" || prog == "csh" || prog == "tcsh":
		if hasGroupFlag(pf, 'c') || pf.has("-c") {
			// The script is the first positional after the flags.
			if len(pf.pos) > 0 {
				code = &args[pf.pos[0]]
			}
		}
	default:
		for _, f := range spec.flags {
			if i, ok := pf.value(f); ok && i >= 0 {
				code = &args[i]
				break
			}
		}
		if code == nil {
			for _, f := range pf.flags {
				if slices.Contains(spec.flags, f.Name) && f.Value != "" {
					w := lit(f.Value)
					code = &w
				}
			}
		}
	}
	scope := ScopeInline
	if code == nil && (shells[prog] || spec.lang == "python" || spec.lang == "javascript" || spec.lang == "perl" ||
		spec.lang == "ruby" || spec.lang == "php") {
		// No inline flag and no script file: the code comes on stdin.
		noFile := len(pf.pos) == 0 || args[pf.pos[0]].value == "-"
		if shells[prog] && pf.has("-s") {
			noFile = true
		}
		if noFile && (rc.stdin != nil || rc.piped) {
			if rc.stdin != nil {
				w := lit(*rc.stdin)
				code = &w
				if rc.heredoc {
					scope = ScopeHeredoc
				}
			} else {
				c.Inline = &Inline{Lang: spec.lang, Dynamic: true}
				return nil
			}
		}
	}
	if code == nil {
		return nil
	}
	c.Inline = &Inline{Lang: spec.lang, Code: code.value, Dynamic: code.dynamic}
	if code.dynamic {
		return nil
	}
	if spec.lang == Bash {
		return b.nested(Bash, code.value, st, false)
	}
	b.call.Contents = append(b.call.Contents, Content{Scope: scope, Text: code.value})
	return nil
}

// unwrap finds the command a wrapper runs and reports it, wrapped_by the
// wrapper.
func (b *builder) unwrap(c *Command, rc *rawCmd, args []word, pf parsed, st *sh, wrapped []string) []*Command {
	kind, ok := wrappers[c.Program]
	if !ok {
		return nil
	}
	inner := func(words []word) []*Command {
		if len(words) == 0 {
			return nil
		}
		sub := &rawCmd{words: words, raw: rc.raw, pipeFrom: rc.pipeFrom, piped: rc.piped, stdin: rc.stdin,
			heredoc: rc.heredoc, pipeRoots: rc.pipeRoots}
		return b.command(sub, st, append(slices.Clone(wrapped), c.Program))
	}
	firstPos := func() int {
		if len(pf.pos) == 0 {
			return -1
		}
		return pf.pos[0]
	}
	switch kind {
	case wrapOpts:
		if c.Program == "exec" && len(pf.pos) == 0 {
			return nil
		}
		if i := firstPos(); i >= 0 {
			return inner(args[i:])
		}
	case wrapEnv:
		var assigns []Assign
		for i := 0; i < len(args); i++ {
			v := args[i].value
			switch {
			case v == "-u" || v == "--unset" || v == "-C" || v == "--chdir":
				if v == "-u" || v == "--unset" {
					if i+1 < len(args) {
						assigns = append(assigns, Assign{Name: args[i+1].value})
					}
				}
				i++
			case v == "-S" || v == "--split-string":
				if i+1 < len(args) {
					return b.nested(Bash, args[i+1].value, st, false)
				}
			case strings.HasPrefix(v, "-"):
			case strings.Contains(v, "=") && !strings.HasPrefix(v, "="):
				name, val, _ := strings.Cut(v, "=")
				assigns = append(assigns, Assign{Name: name, Value: val, Dynamic: args[i].dynamic})
			default:
				c.Assigns = append(c.Assigns, assigns...)
				out := inner(args[i:])
				if len(out) > 0 {
					out[0].Assigns = append(out[0].Assigns, assigns...)
				}
				return out
			}
		}
		c.Assigns = append(c.Assigns, assigns...)
	case wrapTimeout:
		if len(pf.pos) > 1 {
			return inner(args[pf.pos[1]:])
		}
	case wrapCommand:
		if pf.has("-v", "-V") {
			return nil
		}
		if i := firstPos(); i >= 0 {
			return inner(args[i:])
		}
	case wrapXargs:
		i := firstPos()
		if i < 0 {
			return inner([]word{lit("echo")})
		}
		words := slices.Clone(args[i:])
		repl := ""
		for _, f := range pf.flags {
			if f.Name == "-I" || f.Name == "--replace" || f.Name == "-i" {
				repl = f.Value
				if repl == "" {
					repl = "{}"
				}
			}
		}
		stdinWords := pipeWords(rc.pipeRoots)
		if repl != "" {
			var out []word
			for _, w := range words {
				if strings.Contains(w.value, repl) {
					out = append(out, stdinWords...)
					continue
				}
				out = append(out, w)
			}
			words = out
		} else {
			words = append(words, stdinWords...)
		}
		return inner(words)
	case wrapFind:
		var out []*Command
		roots := findRoots(args)
		for i := 0; i < len(args); i++ {
			v := args[i].value
			if v != "-exec" && v != "-execdir" && v != "-ok" && v != "-okdir" {
				continue
			}
			var words []word
			j := i + 1
			for ; j < len(args); j++ {
				if t := args[j].value; t == ";" || t == `\;` || t == "+" {
					break
				}
				if strings.Contains(args[j].value, "{}") {
					words = append(words, pipeWords(rootPaths(b, roots, st))...)
					continue
				}
				words = append(words, args[j])
			}
			out = append(out, inner(words)...)
			i = j
		}
		return out
	case wrapInline:
		switch c.Program {
		case "su", "script":
			if i, ok := pf.value("-c", "--command"); ok && i >= 0 {
				c.Inline = &Inline{Lang: Bash, Code: args[i].value, Dynamic: args[i].dynamic}
				if !args[i].dynamic {
					return b.nested(Bash, args[i].value, st, false)
				}
			}
		case "flock":
			if i, ok := pf.value("-c", "--command"); ok && i >= 0 {
				return b.nested(Bash, args[i].value, st, false)
			}
			if len(pf.pos) > 1 {
				return inner(args[pf.pos[1]:])
			}
		case "watch":
			if i := firstPos(); i >= 0 {
				var parts []string
				for _, w := range args[i:] {
					parts = append(parts, w.value)
				}
				return b.nested(Bash, strings.Join(parts, " "), st, false)
			}
		}
	case wrapParallel:
		if i := firstPos(); i >= 0 {
			var words []word
			for _, w := range args[i:] {
				if w.value == ":::" || w.value == "::::" {
					break
				}
				words = append(words, w)
			}
			return inner(words)
		}
	case wrapBusybox:
		if i := firstPos(); i >= 0 {
			return inner(args[i:])
		}
	case wrapPythonM:
		if i, ok := pf.value("-m"); ok && i >= 0 {
			return inner(args[i:])
		}
	case wrapPackage:
		if i := firstPos(); i >= 0 {
			words := slices.Clone(args[i:])
			words[0] = lit(packageCommand(words[0].value))
			return inner(words)
		}
	case wrapSub:
		return b.unwrapSub(c, args, pf, inner)
	case wrapCmdC:
		for k, a := range args {
			v := strings.ToLower(a.value)
			if v == "/c" || v == "/k" || v == "/r" || strings.HasPrefix(v, "/c ") {
				var parts []string
				for _, w := range args[k+1:] {
					parts = append(parts, w.value)
				}
				code := strings.Join(parts, " ")
				c.Inline = &Inline{Lang: Cmd, Code: code}
				return b.nested(Cmd, code, st, false)
			}
		}
	case wrapStart:
		if st.dialect == PowerShell {
			return nil
		}
		for k := 0; k < len(args); k++ {
			v := args[k].value
			switch {
			case k == 0 && args[k].quoted:
				continue // the window title
			case strings.EqualFold(v, "/d"):
				k++
				continue
			case strings.HasPrefix(v, "/"):
				continue
			}
			return inner(args[k:])
		}
	case wrapPowerShell:
		return b.unwrapPowerShell(c, args, st)
	case wrapWSL:
		for k := 0; k < len(args); k++ {
			v := args[k].value
			switch {
			case v == "-e" || v == "--exec":
				return inner(args[k+1:])
			case v == "--":
				k++
				fallthrough
			case !strings.HasPrefix(v, "-"):
				var parts []string
				for _, w := range args[k:] {
					parts = append(parts, w.value)
				}
				return b.nested(Bash, strings.Join(parts, " "), st, false)
			case slices.Contains(specs["wsl"].values, v):
				k++
			}
		}
	case wrapRemote:
		return b.unwrapRemote(c, args, pf, st)
	case wrapCmdString:
		code := ""
		switch c.Program {
		case "forfiles":
			if i, ok := pf.value("/c"); ok && i >= 0 {
				code = args[i].value
			}
		case "schtasks":
			if i, ok := pf.value("/tr"); ok && i >= 0 {
				code = args[i].value
			}
		case "wmic":
			if strings.Contains(c.Subcommand, "call create") && len(pf.pos) > 0 {
				code = args[pf.pos[len(pf.pos)-1]].value
			}
		case "runas":
			if len(pf.pos) > 0 {
				code = args[pf.pos[len(pf.pos)-1]].value
			}
		}
		if code != "" {
			return b.nested(Cmd, code, st, false)
		}
	case wrapStartProcess:
		return b.unwrapStartProcess(c, args, pf, st, inner)
	case wrapInvokeExpression:
		var code *word
		if i, ok := pf.value("-command"); ok && i >= 0 {
			code = &args[i]
		} else if len(args) > 0 {
			code = &args[0]
		}
		if code == nil {
			if rc.stdin != nil {
				w := lit(*rc.stdin)
				code = &w
			} else if rc.piped {
				c.Inline = &Inline{Lang: PowerShell, Dynamic: true}
				return nil
			}
		}
		if code != nil {
			c.Inline = &Inline{Lang: PowerShell, Code: code.value, Dynamic: code.dynamic}
			if !code.dynamic {
				return b.nested(PowerShell, code.value, st, false)
			}
		}
	case wrapScriptBlock:
		var out []*Command
		for _, a := range args {
			if a.block != nil {
				out = append(out, b.nested(PowerShell, *a.block, st, c.Program == "invoke-command" && pf.has("-computername", "-session"))...)
			}
		}
		return out
	}
	return nil
}

// pipeWords stands for arguments that come from a pipe or find's matches:
// unknown values, under the given roots when known.
func pipeWords(roots []Path) []word {
	if len(roots) == 0 {
		return []word{{value: "<stdin>", raw: "<stdin>", dynamic: true, varEnd: -1}}
	}
	var out []word
	for _, r := range roots {
		out = append(out, word{value: r.Raw + "/<item>", raw: "<from " + r.Raw + ">", prefix: r.Raw, dynamic: true, varEnd: -1})
	}
	return out
}

func rootPaths(b *builder, roots []word, st *sh) []Path {
	if len(roots) == 0 {
		roots = []word{lit(".")}
	}
	var out []Path
	for _, r := range roots {
		out = append(out, Path{Raw: r.value})
	}
	return out
}

// packageCommand is the command a package runner runs for a package spec:
// @scope/name@1.2 runs name.
func packageCommand(spec string) string {
	s := spec
	if strings.HasPrefix(s, "@") {
		if i := strings.Index(s, "/"); i >= 0 {
			s = s[i+1:]
		}
	}
	if i := strings.Index(s, "@"); i > 0 {
		s = s[:i]
	}
	if i := strings.IndexAny(s, "=<>~"); i > 0 {
		s = s[:i]
	}
	return s
}

func (b *builder) unwrapSub(c *Command, args []word, pf parsed, inner func([]word) []*Command) []*Command {
	sr, ok := subRunners[c.Program]
	if !ok {
		return nil
	}
	sub := c.Subcommand
	for _, s := range sr.sub {
		if !(sub == s || strings.HasPrefix(sub, s+" ")) {
			continue
		}
		words := strings.Fields(s)
		// Skip the subcommand words and the options, to the command.
		k, matched := 0, 0
		for ; k < len(args) && matched < len(words); k++ {
			if strings.EqualFold(args[k].value, words[matched]) {
				matched++
			}
		}
		for ; k < len(args); k++ {
			v := args[k].value
			if v == "--" {
				k++
				break
			}
			if !strings.HasPrefix(v, "-") {
				break
			}
			if slices.Contains(sr.values, v) {
				k++
			}
		}
		if k >= len(args) {
			return nil
		}
		rest := slices.Clone(args[k:])
		if packageRunners[c.Program+" "+s] {
			rest[0] = lit(packageCommand(rest[0].value))
		}
		return inner(rest)
	}
	return nil
}

// unwrapPowerShell finds the code powershell.exe or pwsh is given.
func (b *builder) unwrapPowerShell(c *Command, args []word, st *sh) []*Command {
	matches := func(v, full string, min int) bool {
		v = strings.ToLower(strings.TrimLeft(v, "-/"))
		return len(v) >= min && strings.HasPrefix(full, v)
	}
	for k := 0; k < len(args); k++ {
		v := args[k].value
		if !strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "/") {
			// The rest of the line is a command.
			return b.psCode(c, joinWords(args[k:]), args[k].dynamic, st)
		}
		switch {
		case matches(v, "command", 1) && !matches(v, "configurationname", 2) || strings.EqualFold(v, "-c"):
			if k+1 < len(args) {
				if args[k+1].value == "-" {
					c.Inline = &Inline{Lang: PowerShell, Dynamic: true}
					return nil
				}
				return b.psCode(c, joinWords(args[k+1:]), anyDynamic(args[k+1:]), st)
			}
			return nil
		case matches(v, "encodedcommand", 1) || strings.EqualFold(v, "-ec") || strings.EqualFold(v, "-enc"):
			if k+1 < len(args) {
				code, ok := decodeUTF16Base64(args[k+1].value)
				if !ok {
					b.note("an -EncodedCommand that is not base64")
					c.Inline = &Inline{Lang: PowerShell, Dynamic: true}
					return nil
				}
				b.call.Contents = append(b.call.Contents, Content{Scope: ScopeDecoded, Text: code})
				c.Inline = &Inline{Lang: PowerShell, Code: code}
				return b.nested(PowerShell, code, st, false)
			}
			return nil
		case matches(v, "file", 1):
			if k+1 < len(args) {
				b.path(c, args[k+1], Exec, st)
			}
			return nil
		case matches(v, "executionpolicy", 2) || matches(v, "windowstyle", 1) || matches(v, "workingdirectory", 2) ||
			matches(v, "outputformat", 1) || matches(v, "inputformat", 2) || matches(v, "version", 1) ||
			matches(v, "psconsolefile", 2) || matches(v, "configurationname", 2) || matches(v, "settingsfile", 2):
			k++
		}
	}
	return nil
}

func (b *builder) psCode(c *Command, code string, dynamic bool, st *sh) []*Command {
	c.Inline = &Inline{Lang: PowerShell, Code: code, Dynamic: dynamic}
	if dynamic {
		return nil
	}
	return b.nested(PowerShell, code, st, false)
}

func joinWords(ws []word) string {
	parts := make([]string, len(ws))
	for i, w := range ws {
		parts[i] = w.value
	}
	return strings.Join(parts, " ")
}

func anyDynamic(ws []word) bool {
	for _, w := range ws {
		if w.dynamic {
			return true
		}
	}
	return false
}

// decodeUTF16Base64 decodes -EncodedCommand: base64 of UTF-16LE text.
func decodeUTF16Base64(s string) (string, bool) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(data)%2 != 0 {
		return "", false
	}
	if len(data) > 2*maxDecoded {
		data = data[:2*maxDecoded]
	}
	u := make([]uint16, len(data)/2)
	for i := range u {
		u[i] = uint16(data[2*i]) | uint16(data[2*i+1])<<8
	}
	return string(utf16.Decode(u)), true
}

// unwrapRemote: ssh host cmd, docker exec ctr cmd, kubectl exec pod -- cmd
// run a command elsewhere. Its paths are not this machine's.
func (b *builder) unwrapRemote(c *Command, args []word, pf parsed, st *sh) []*Command {
	switch c.Program {
	case "ssh":
		if len(pf.pos) > 1 {
			return b.nested(Bash, joinWords(args[pf.pos[1]:]), st, true)
		}
	case "docker", "podman":
		sub := strings.Fields(c.Subcommand)
		if len(sub) == 0 || sub[0] != "exec" && sub[0] != "run" {
			return nil
		}
		// docker exec [opts] CTR CMD...: the command starts at the third
		// positional (exec, CTR, CMD).
		if len(pf.pos) > 2 {
			words := args[pf.pos[2]:]
			sub := &rawCmd{words: words, raw: c.Raw}
			rst := st.child()
			rst.remote = true
			return b.command(sub, rst, append(slices.Clone(c.WrappedBy), c.Program))
		}
	case "kubectl", "oc":
		for k, a := range args {
			if a.value == "--" && strings.HasPrefix(c.Subcommand, "exec") && k+1 < len(args) {
				sub := &rawCmd{words: args[k+1:], raw: c.Raw}
				rst := st.child()
				rst.remote = true
				return b.command(sub, rst, append(slices.Clone(c.WrappedBy), c.Program))
			}
		}
	}
	return nil
}

func (b *builder) unwrapStartProcess(c *Command, args []word, pf parsed, st *sh, inner func([]word) []*Command) []*Command {
	i, ok := pf.value("-filepath")
	if !ok || i < 0 {
		return nil
	}
	words := []word{args[i]}
	var argText []string
	for _, j := range pf.vals["-argumentlist"] {
		if j >= 0 {
			argText = append(argText, args[j].value)
		}
	}
	prog, _ := programName(args[i].value, PowerShell)
	line := strings.Join(argText, " ")
	switch {
	case shells[prog]:
		return append(inner(words), b.nested(Bash, line, st, false)...)
	case prog == "cmd":
		code := line
		if l := strings.ToLower(code); strings.HasPrefix(l, "/c ") || strings.HasPrefix(l, "/k ") {
			code = code[3:]
		}
		return append(inner(words), b.nested(Cmd, code, st, false)...)
	case prog == "powershell" || prog == "pwsh":
		return b.nested(PowerShell, args[i].value+" "+line, st, false)
	}
	for _, a := range strings.Fields(line) {
		words = append(words, lit(strings.Trim(a, `"',`)))
	}
	return inner(words)
}

// parseURL splits a URL into scheme, host and path, the host normalized.
func parseURL(raw string) URL {
	u := URL{Raw: raw}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return u
	}
	u.Scheme = strings.ToLower(scheme)
	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host, u.Path = rest[:i], rest[i:]
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if strings.HasPrefix(host, "[") {
		if i := strings.Index(host, "]"); i > 0 {
			host = host[1:i]
		}
	} else if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	u.Host = normalizeHost(host)
	return u
}

// normalizeHost lowercases a host, drops a trailing dot, and writes odd
// IPv4 forms (2130706433, 0x7f000001, 0177.0.0.1, 127.1) as dotted
// decimal; an IPv4-mapped IPv6 address becomes its IPv4 address.
func normalizeHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if v4, ok := strings.CutPrefix(h, "::ffff:"); ok && strings.Count(v4, ".") == 3 {
		h = v4
	}
	if ip, ok := oddIPv4(h); ok {
		return ip
	}
	return h
}

func oddIPv4(h string) (string, bool) {
	if h == "" || strings.Trim(h, "0123456789abcdefx.") != "" {
		return "", false
	}
	parts := strings.Split(h, ".")
	if len(parts) > 4 {
		return "", false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		base := 10
		switch {
		case strings.HasPrefix(p, "0x"):
			base, p = 16, p[2:]
		case len(p) > 1 && strings.HasPrefix(p, "0"):
			base = 8
		}
		n, err := strconv.ParseUint(p, base, 32)
		if err != nil {
			return "", false
		}
		nums[i] = n
	}
	var v uint64
	switch len(nums) {
	case 1:
		v = nums[0]
	case 2:
		v = nums[0]<<24 | nums[1]
	case 3:
		v = nums[0]<<24 | nums[1]<<16 | nums[2]
	case 4:
		v = nums[0]<<24 | nums[1]<<16 | nums[2]<<8 | nums[3]
	}
	if v > 0xffffffff || len(nums) == 4 && (nums[0] > 255 || nums[1] > 255 || nums[2] > 255 || nums[3] > 255) {
		return "", false
	}
	return strconv.Itoa(int(v>>24)) + "." + strconv.Itoa(int(v>>16&255)) + "." + strconv.Itoa(int(v>>8&255)) + "." +
		strconv.Itoa(int(v&255)), true
}
