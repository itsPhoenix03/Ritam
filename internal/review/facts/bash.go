package facts

import (
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"
)

// parseBash parses bash code (Claude's Bash tool, Git Bash on Windows,
// bash -c) and reports every command in it, inside if branches, loops,
// functions and substitutions too: what might run is reported, not only
// what will. Nothing is run; a $(...) is parsed, never evaluated.
func (b *builder) parseBash(src string, st *sh) {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(src), "")
	if err != nil {
		b.parseError("bash: " + err.Error())
		b.roughBash(src, st)
		return
	}
	w := &bashWalker{b: b, src: src, st: st}
	w.stmts(f.Stmts)
}

// roughBash splits code that would not parse on blanks and separators, to
// add what it can find. It never removes a finding: the parse error itself
// already makes the call suspicious.
func (b *builder) roughBash(src string, st *sh) {
	for _, part := range strings.FieldsFunc(src, func(r rune) bool { return strings.ContainsRune(";&|\n()`", r) }) {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		var words []word
		for _, f := range fields {
			words = append(words, lit(strings.Trim(f, `"'`)))
		}
		b.command(&rawCmd{words: words, raw: strings.TrimSpace(part)}, st, nil)
	}
}

type bashWalker struct {
	b   *builder
	src string
	st  *sh
}

// result is what a statement passes on to a pipe: its first and last
// commands, the text it writes when known (echo LITERAL), and the paths it
// lists (find src) for a command that reads them from the pipe.
type result struct {
	first, last []*Command
	out         *string
	roots       []Path
}

// pipeIn is what arrives on a statement's stdin from a pipe.
type pipeIn struct {
	from  string
	text  *string
	roots []Path
	piped bool
}

func (w *bashWalker) stmts(list []*syntax.Stmt) result {
	var r result
	for i, s := range list {
		sr := w.stmt(s, pipeIn{})
		if i == 0 {
			r.first = sr.first
		}
		r.last, r.out, r.roots = sr.last, sr.out, sr.roots
	}
	return r
}

func (w *bashWalker) text(n syntax.Node) string {
	start, end := int(n.Pos().Offset()), int(n.End().Offset())
	if start < 0 || end > len(w.src) || start > end {
		return ""
	}
	return w.src[start:end]
}

func (w *bashWalker) stmt(s *syntax.Stmt, in pipeIn) result {
	if s == nil || s.Cmd == nil {
		if s != nil && len(s.Redirs) > 0 {
			w.loose(s.Redirs, w.text(s))
		}
		return result{}
	}
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		return w.call(c, s, in)
	case *syntax.BinaryCmd:
		var r result
		switch c.Op {
		case syntax.Pipe, syntax.PipeAll:
			left := w.stmt(c.X, in)
			from := ""
			if len(left.last) > 0 {
				from = left.last[0].Program
				if len(left.last) > 1 && left.last[len(left.last)-1].Program != "" {
					from = left.last[len(left.last)-1].Program
				}
			}
			right := w.stmt(c.Y, pipeIn{from: from, text: left.out, roots: left.roots, piped: true})
			to := ""
			if len(right.first) > 0 {
				to = right.first[0].Program
			}
			for _, cmd := range left.last {
				cmd.PipeTo = to
			}
			r = result{first: left.first, last: right.last, out: right.out, roots: right.roots}
		default:
			left := w.stmt(c.X, in)
			right := w.stmt(c.Y, pipeIn{})
			r = result{first: left.first, last: right.last, out: right.out}
		}
		w.loose(s.Redirs, w.text(s))
		return r
	case *syntax.Subshell:
		saved := *w.st
		saved.vars = cloneVars(w.st.vars)
		r := w.stmtsIn(c.Stmts, in)
		*w.st = saved
		w.loose(s.Redirs, w.text(s))
		return r
	case *syntax.Block:
		r := w.stmtsIn(c.Stmts, in)
		w.loose(s.Redirs, w.text(s))
		return r
	case *syntax.IfClause:
		for ic := c; ic != nil; ic = ic.Else {
			w.stmts(ic.Cond)
			w.stmts(ic.Then)
		}
	case *syntax.WhileClause:
		w.stmts(c.Cond)
		w.stmts(c.Do)
	case *syntax.ForClause:
		if it, ok := c.Loop.(*syntax.WordIter); ok {
			for _, item := range it.Items {
				w.word(item)
			}
		} else {
			w.walkSubst(c.Loop)
		}
		w.stmts(c.Do)
	case *syntax.CaseClause:
		w.word(c.Word)
		for _, item := range c.Items {
			w.stmts(item.Stmts)
		}
	case *syntax.FuncDecl:
		name := ""
		if c.Name != nil {
			name = c.Name.Value
		}
		if w.st.funcs == nil {
			w.st.funcs = map[string]any{}
		}
		w.st.funcs[name] = c.Body
		cmd := &Command{Program: "define-function", ProgramRaw: "function", Args: []Arg{{Value: name, Raw: name}},
			Subcommand: strings.ToLower(name), Raw: w.text(s), Dialect: Bash, Depth: w.st.depth}
		w.b.add(cmd)
		return result{first: []*Command{cmd}, last: []*Command{cmd}}
	case *syntax.DeclClause:
		return w.decl(c, s)
	case *syntax.TestClause:
		w.walkSubst(c.X)
	case *syntax.ArithmCmd:
		w.walkSubst(c.X)
	case *syntax.LetClause:
		for _, e := range c.Exprs {
			w.walkSubst(e)
		}
	case *syntax.CoprocClause:
		return w.stmt(c.Stmt, in)
	case *syntax.TimeClause:
		return w.stmt(c.Stmt, in)
	default:
		w.walkSubst(s.Cmd)
	}
	w.loose(s.Redirs, w.text(s))
	return result{}
}

// stmtsIn walks a group whose first statement reads the group's stdin.
func (w *bashWalker) stmtsIn(list []*syntax.Stmt, in pipeIn) result {
	var r result
	for i, s := range list {
		pin := pipeIn{}
		if i == 0 {
			pin = in
		}
		sr := w.stmt(s, pin)
		if i == 0 {
			r.first = sr.first
		}
		r.last, r.out = sr.last, sr.out
	}
	return r
}

// loose records the redirections of a compound command ({ ...; } > f) as
// a command of their own, so the files they write are not missed.
func (w *bashWalker) loose(redirs []*syntax.Redirect, raw string) {
	if len(redirs) == 0 {
		return
	}
	rc := &rawCmd{raw: raw}
	for _, r := range redirs {
		rd, _ := w.redirect(r)
		rc.redirs = append(rc.redirs, rd)
	}
	w.b.command(rc, w.st, nil)
}

// walkSubst finds the command substitutions inside an expression and
// reports the commands in them.
func (w *bashWalker) walkSubst(n syntax.Node) {
	if n == nil {
		return
	}
	syntax.Walk(n, func(node syntax.Node) bool {
		switch x := node.(type) {
		case *syntax.CmdSubst:
			w.subst(x.Stmts)
			return false
		case *syntax.ProcSubst:
			w.subst(x.Stmts)
			return false
		}
		return true
	})
}

// subst reports the commands of a substitution, run as a subshell, and
// returns their programs.
func (w *bashWalker) subst(list []*syntax.Stmt) []string {
	child := w.st.child()
	if child.depth > maxDepth {
		w.b.limit("nested more than 8 levels deep")
		return nil
	}
	sub := &bashWalker{b: w.b, src: w.src, st: child}
	start := len(w.b.call.Commands)
	sub.stmts(list)
	return programs(w.b.call.Commands[start:])
}

func (w *bashWalker) redirect(r *syntax.Redirect) (redirect, bool) {
	rd := redirect{op: r.Op.String()}
	if r.N != nil {
		rd.fd = r.N.Value
	}
	if r.Word != nil {
		rd.target = w.word(r.Word)
	}
	switch r.Op {
	case syntax.Hdoc, syntax.DashHdoc:
		if r.Hdoc != nil {
			body := w.word(r.Hdoc)
			rd.body, rd.bodyKnown = body.value, !body.dynamic
			// A quoted delimiter keeps the body literal.
			if rd.target.quoted {
				rd.body, rd.bodyKnown = w.text(r.Hdoc), true
			}
		} else {
			rd.bodyKnown = true
		}
		return rd, true
	case syntax.WordHdoc:
		rd.body, rd.bodyKnown = rd.target.value, !rd.target.dynamic
		return rd, true
	}
	return rd, false
}

func (w *bashWalker) call(c *syntax.CallExpr, s *syntax.Stmt, in pipeIn) result {
	rc := &rawCmd{raw: w.text(s), pipeFrom: in.from, piped: in.piped, stdin: in.text, pipeRoots: in.roots}
	for _, r := range s.Redirs {
		rd, isInput := w.redirect(r)
		rc.redirs = append(rc.redirs, rd)
		if isInput && rd.bodyKnown {
			body := rd.body
			rc.stdin, rc.heredoc = &body, r.Op != syntax.WordHdoc
		} else if isInput {
			rc.stdin, rc.piped = nil, true
		}
	}
	// Prefix assignments set the variables for this command only; alone
	// on a line, they set them for the commands after.
	scoped := len(c.Args) > 0
	for _, a := range c.Assigns {
		as := w.assign(a)
		rc.assigns = append(rc.assigns, as)
		if !scoped {
			w.setVar(as)
		}
	}
	for _, arg := range c.Args {
		for _, wd := range w.expandBraces(arg) {
			rc.words = append(rc.words, w.word(wd))
		}
	}

	// A function defined earlier runs its body.
	var body *syntax.Stmt
	if len(rc.words) > 0 && !rc.words[0].dynamic {
		if fb, ok := w.st.funcs[rc.words[0].value].(*syntax.Stmt); ok && !w.st.active[rc.words[0].value] {
			body = fb
		}
	}
	cmds := w.b.command(rc, w.st, nil)
	if body != nil {
		name := rc.words[0].value
		if w.st.active == nil {
			w.st.active = map[string]bool{}
		}
		w.st.active[name] = true
		w.st.depth++
		w.stmt(body, in)
		w.st.depth--
		delete(w.st.active, name)
	}
	if len(cmds) == 0 {
		return result{}
	}
	main := cmds[0]
	switch main.Program {
	case "unset":
		for _, a := range main.Args {
			delete(w.st.vars, a.Value)
			main.Assigns = append(main.Assigns, Assign{Name: a.Value})
		}
	case "eval":
		if code := main.Argline(); code != "" && !anyDynamicArg(main.Args) {
			w.b.nested(Bash, code, w.st, false)
		} else if code != "" {
			main.Inline = &Inline{Lang: Bash, Code: code, Dynamic: true}
		}
	case "read", "mapfile", "readarray":
		for _, a := range main.Args {
			if !strings.HasPrefix(a.Value, "-") {
				w.st.vars[a.Value] = nil
			}
		}
	}
	return result{first: cmds, last: cmds, out: w.output(main, rc), roots: listRoots(main)}
}

func anyDynamicArg(args []Arg) bool {
	for _, a := range args {
		if a.Dynamic {
			return true
		}
	}
	return false
}

// listRoots are the paths a command lists, which a command after it in a
// pipe may act on (find src | xargs rm).
func listRoots(c *Command) []Path {
	switch c.Program {
	case "find", "ls", "get-childitem", "dir", "fd", "locate", "git":
		var out []Path
		for _, p := range c.Paths {
			if p.Access == List || p.Access == Delete {
				out = append(out, p)
			}
		}
		if len(out) == 0 && c.Program != "git" {
			out = append(out, Path{Raw: "."})
		}
		return out
	}
	return nil
}

// output is the text a command writes when it can be known without
// running it: echo LITERAL, a here-doc through cat, base64 -d of a known
// text. A shell or decoder after it in a pipe gets that text.
func (w *bashWalker) output(c *Command, rc *rawCmd) *string {
	for _, r := range rc.redirs {
		if r.op == ">" || r.op == ">>" || r.op == "&>" {
			return nil
		}
	}
	switch c.Program {
	case "echo", "printf":
		var parts []string
		for _, a := range c.Args {
			if a.Dynamic {
				return nil
			}
			if c.Program == "echo" && (a.Value == "-n" || a.Value == "-e" || a.Value == "-ne" || a.Value == "-en") {
				continue
			}
			parts = append(parts, a.Value)
		}
		s := strings.Join(parts, " ")
		if c.Program == "printf" && len(parts) > 0 {
			s = strings.ReplaceAll(parts[0], `\n`, "\n")
		}
		return &s
	case "cat", "tee":
		return rc.stdin
	case "base64", "base32", "basenc", "openssl":
		if rc.stdin == nil || !hasDecodeFlag(c) {
			return nil
		}
		data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(*rc.stdin), ""))
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(strings.Join(strings.Fields(*rc.stdin), ""))
		}
		if err != nil || !utf8.Valid(data) {
			w.b.note("a base64 payload that does not decode to text")
			return nil
		}
		s := string(capBytes(data))
		w.b.call.Contents = append(w.b.call.Contents, Content{Scope: ScopeDecoded, Text: s})
		return &s
	case "xxd":
		if rc.stdin == nil || !hasFlag(c, "-r") {
			return nil
		}
		data, err := hex.DecodeString(strings.Join(strings.Fields(*rc.stdin), ""))
		if err != nil {
			return nil
		}
		s := string(capBytes(data))
		w.b.call.Contents = append(w.b.call.Contents, Content{Scope: ScopeDecoded, Text: s})
		return &s
	}
	return nil
}

func capBytes(b []byte) []byte {
	if len(b) > maxDecoded {
		return b[:maxDecoded]
	}
	return b
}

func hasDecodeFlag(c *Command) bool {
	for _, f := range c.Flags {
		switch f.Name {
		case "-d", "--decode", "-D", "-di":
			return true
		}
	}
	return c.Program == "openssl" && strings.Contains(c.Subcommand, "base64") && hasFlag(c, "-d")
}

func hasFlag(c *Command, names ...string) bool {
	for _, f := range c.Flags {
		for _, n := range names {
			if f.Name == n {
				return true
			}
		}
	}
	return false
}

// decl handles declare, export, local, readonly and typeset.
func (w *bashWalker) decl(c *syntax.DeclClause, s *syntax.Stmt) result {
	variant := ""
	if c.Variant != nil {
		variant = c.Variant.Value
	}
	rc := &rawCmd{raw: w.text(s), words: []word{lit(variant)}}
	for _, a := range c.Args {
		if a.Naked && a.Name == nil && a.Value != nil {
			rc.words = append(rc.words, w.word(a.Value))
			continue
		}
		as := w.assign(a)
		if a.Naked {
			rc.words = append(rc.words, lit(as.Name))
			continue
		}
		rc.assigns = append(rc.assigns, as)
		w.setVar(as)
		rc.words = append(rc.words, lit(as.Name+"="+as.Value))
	}
	for _, r := range s.Redirs {
		rd, _ := w.redirect(r)
		rc.redirs = append(rc.redirs, rd)
	}
	cmds := w.b.command(rc, w.st, nil)
	return result{first: cmds, last: cmds}
}

func (w *bashWalker) assign(a *syntax.Assign) Assign {
	as := Assign{}
	if a.Name != nil {
		as.Name = a.Name.Value
	}
	switch {
	case a.Value != nil:
		v := w.word(a.Value)
		as.Value, as.Dynamic = v.value, v.dynamic
	case a.Array != nil:
		var parts []string
		for _, el := range a.Array.Elems {
			if el.Value != nil {
				v := w.word(el.Value)
				parts = append(parts, v.value)
				as.Dynamic = as.Dynamic || v.dynamic
			}
		}
		as.Value = strings.Join(parts, " ")
	}
	if a.Index != nil {
		as.Dynamic = true
	}
	return as
}

func (w *bashWalker) setVar(as Assign) {
	if w.st.vars == nil {
		w.st.vars = map[string]*string{}
	}
	if as.Dynamic {
		w.st.vars[as.Name] = nil
		return
	}
	v := as.Value
	w.st.vars[as.Name] = &v
}

// expandBraces expands a word's static brace expansions: {a,b} and {1..3}.
func (w *bashWalker) expandBraces(wd *syntax.Word) []*syntax.Word {
	if !strings.ContainsAny(w.text(wd), "{,}") {
		return []*syntax.Word{wd}
	}
	cp := *wd
	cp.Parts = append([]syntax.WordPart(nil), wd.Parts...)
	if !syntax.SplitBraces(&cp) {
		return []*syntax.Word{wd}
	}
	out := expandBraceWord(&cp)
	if len(out) == 0 || len(out) > 64 {
		return []*syntax.Word{wd}
	}
	return out
}

// expandBraceWord expands the BraceExp parts of a word that SplitBraces
// produced, keeping each result's other parts as they were.
func expandBraceWord(wd *syntax.Word) []*syntax.Word {
	for i, part := range wd.Parts {
		be, ok := part.(*syntax.BraceExp)
		if !ok {
			continue
		}
		var elems []*syntax.Word
		if be.Sequence && len(be.Elems) >= 2 {
			lo, err1 := strconv.Atoi(litOf(be.Elems[0]))
			hi, err2 := strconv.Atoi(litOf(be.Elems[1]))
			if err1 != nil || err2 != nil || abs(hi-lo) > 64 {
				return nil
			}
			step := 1
			if hi < lo {
				step = -1
			}
			for n := lo; ; n += step {
				elems = append(elems, &syntax.Word{Parts: []syntax.WordPart{&syntax.Lit{Value: strconv.Itoa(n)}}})
				if n == hi {
					break
				}
			}
		} else {
			elems = be.Elems
		}
		var out []*syntax.Word
		for _, el := range elems {
			parts := append(append(append([]syntax.WordPart(nil), wd.Parts[:i]...), el.Parts...), wd.Parts[i+1:]...)
			out = append(out, expandBraceWord(&syntax.Word{Parts: parts})...)
		}
		return out
	}
	return []*syntax.Word{wd}
}

func litOf(wd *syntax.Word) string {
	var sb strings.Builder
	for _, p := range wd.Parts {
		if l, ok := p.(*syntax.Lit); ok {
			sb.WriteString(l.Value)
		}
	}
	return sb.String()
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// word resolves a word: quotes and escapes taken off, $'...' decoded,
// variables assigned literally earlier (and a few well-known ones)
// substituted, anything else marked as only known at run time.
func (w *bashWalker) word(wd *syntax.Word) word {
	out := word{raw: w.text(wd), varEnd: -1}
	var val strings.Builder
	known := true
	markDynamic := func(src string) {
		if known {
			out.prefix = val.String()
			known = false
		}
		val.WriteString(src)
		out.varEnd = val.Len()
	}
	for i, part := range wd.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			s := p.Value
			if i == 0 && (s == "~" || strings.HasPrefix(s, "~/")) {
				// ~ is the home folder; done in path resolution, so the
				// value keeps it.
			} else if i == 0 && strings.HasPrefix(s, "~") {
				markDynamic(s)
				continue
			}
			if hasGlob(s) {
				out.glob = true
			}
			val.WriteString(unescapeBash(s))
		case *syntax.SglQuoted:
			out.quoted = true
			s := p.Value
			if p.Dollar {
				s = ansiC(s)
			}
			val.WriteString(s)
		case *syntax.DblQuoted:
			out.quoted = true
			for _, ip := range p.Parts {
				switch q := ip.(type) {
				case *syntax.Lit:
					val.WriteString(unescapeDouble(q.Value))
				case *syntax.ParamExp:
					if v, ok := w.param(q); ok {
						val.WriteString(v)
						out.varEnd = val.Len()
					} else {
						markDynamic(w.text(q))
					}
				case *syntax.CmdSubst:
					out.from = append(out.from, w.subst(q.Stmts)...)
					markDynamic(w.text(q))
				default:
					w.walkSubst(ip)
					markDynamic(w.text(ip))
				}
			}
		case *syntax.ParamExp:
			if v, ok := w.param(p); ok {
				val.WriteString(v)
				out.varEnd = val.Len()
			} else {
				markDynamic(w.text(p))
			}
		case *syntax.CmdSubst:
			out.from = append(out.from, w.subst(p.Stmts)...)
			markDynamic(w.text(p))
		case *syntax.ProcSubst:
			out.from = append(out.from, w.subst(p.Stmts)...)
			markDynamic(w.text(p))
		case *syntax.ArithmExp:
			w.walkSubst(p)
			markDynamic(w.text(p))
		case *syntax.ExtGlob:
			out.glob = true
			val.WriteString(w.text(p))
		default:
			markDynamic(w.text(part))
		}
	}
	out.value = val.String()
	out.dynamic = !known
	if known {
		out.prefix = out.value
	}
	return out
}

// param resolves $X and ${X}: a variable assigned literally earlier in the
// same code, or a well-known one. Anything with an operator, a position
// ($1) or an unknown name is only known at run time.
func (w *bashWalker) param(p *syntax.ParamExp) (string, bool) {
	if p.Param == nil || p.Excl || p.Length || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Exp != nil ||
		p.Names != 0 || p.NestedParam != nil {
		return "", false
	}
	name := p.Param.Value
	if v, ok := w.st.vars[name]; ok {
		if v == nil {
			return "", false
		}
		return *v, true
	}
	switch name {
	case "PWD":
		if w.st.cwd != "" {
			return w.st.cwd, true
		}
		return "", false
	case "HOME", "USERPROFILE", "TMPDIR", "TEMP", "TMP", "APPDATA", "LOCALAPPDATA", "USER", "PROGRAMDATA":
		return w.b.env.variable(name)
	}
	return "", false
}

// hasGlob reports an unescaped wildcard in an unquoted literal.
func hasGlob(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '*', '?', '[':
			return true
		}
	}
	return false
}

// unescapeBash takes the backslashes off an unquoted literal.
func unescapeBash(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// unescapeDouble takes off the backslashes that escape inside double
// quotes: before $, `, ", \ and a newline.
func unescapeDouble(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("$`\"\\\n", s[i+1]) >= 0 {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// ansiC decodes the escapes of $'...': \n, \t, \xHH, \NNN, \uHHHH, ...
func ansiC(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			sb.WriteByte(s[i])
			continue
		}
		i++
		switch c := s[i]; c {
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case 'a':
			sb.WriteByte(7)
		case 'b':
			sb.WriteByte(8)
		case 'e', 'E':
			sb.WriteByte(27)
		case 'f':
			sb.WriteByte(12)
		case 'v':
			sb.WriteByte(11)
		case 'x', 'u', 'U':
			max := map[byte]int{'x': 2, 'u': 4, 'U': 8}[c]
			j := i + 1
			for j < len(s) && j-i-1 < max && isHex(s[j]) {
				j++
			}
			if j == i+1 {
				sb.WriteByte('\\')
				sb.WriteByte(c)
				continue
			}
			n, _ := strconv.ParseUint(s[i+1:j], 16, 32)
			if c == 'x' {
				sb.WriteByte(byte(n))
			} else {
				sb.WriteRune(rune(n))
			}
			i = j - 1
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			for j < len(s) && j-i < 3 && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			n, _ := strconv.ParseUint(s[i:j], 8, 16)
			sb.WriteByte(byte(n))
			i = j - 1
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
