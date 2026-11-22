package facts

import (
	"regexp"
	"strings"
)

// cmd.exe has no grammar worth the name, so this follows its order of
// work: %VAR% expanded per line, then ^ escapes, quotes, the operators &,
// &&, ||, | and parenthesised blocks. Anything that hides a word's value
// until run time -- an unknown %var%, !var!, a caret inside a word, a for
// /f -- marks it as indirect or dynamic and lets the rules decide.

// cmdMark wraps the source of a value only known at run time in the
// expanded line, so the token holding it can be marked dynamic.
const cmdMark = "\x00"

var (
	cmdPctRe     = regexp.MustCompile(`%(~[a-zA-Z]*)?[0-9*]|%%~?[a-zA-Z]|%([A-Za-z_][A-Za-z0-9_#$'()*+,\-.?@\[\]` + "`" + `{}~]*)(:[^%]*)?%`)
	cmdBangRe    = regexp.MustCompile(`!([A-Za-z_][A-Za-z0-9_]*)(:[^!]*)?!`)
	cmdKnownVars = map[string]string{"USERPROFILE": "USERPROFILE", "HOME": "HOME", "APPDATA": "APPDATA",
		"LOCALAPPDATA": "LOCALAPPDATA", "TEMP": "TEMP", "TMP": "TMP", "PROGRAMDATA": "PROGRAMDATA",
		"ALLUSERSPROFILE": "PROGRAMDATA", "WINDIR": "WINDIR", "SYSTEMROOT": "WINDIR", "PROGRAMFILES": "PROGRAMFILES",
		"USERNAME": "USER"}
)

func (b *builder) parseCmd(src string, st *sh) {
	p := &cmdParser{b: b, st: st}
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		p.sequence(p.expandPercent(line))
	}
}

type cmdParser struct {
	b  *builder
	st *sh
}

func (p *cmdParser) delayed() bool { return p.st.aliases["\x00delayed"] == "1" }

// expandPercent expands %VAR% the way cmd does before it reads a line.
func (p *cmdParser) expandPercent(line string) string {
	return cmdPctRe.ReplaceAllStringFunc(line, func(m string) string {
		if strings.HasPrefix(m, "%%") || len(m) >= 2 && (m[1] >= '0' && m[1] <= '9' || m[1] == '*' || m[1] == '~') {
			return cmdMark + m + cmdMark
		}
		inner := strings.Trim(m, "%")
		name, mod, _ := strings.Cut(inner, ":")
		if mod != "" {
			return cmdMark + m + cmdMark // %X:~1,2%, %X:a=b%
		}
		if v, ok := p.cmdVar(name); ok {
			return v
		}
		return cmdMark + m + cmdMark
	})
}

func (p *cmdParser) cmdVar(name string) (string, bool) {
	up := strings.ToUpper(name)
	if v, ok := p.st.vars[up]; ok {
		if v == nil {
			return "", false
		}
		return *v, true
	}
	switch up {
	case "CD":
		if p.st.cwd != "" {
			return strings.ReplaceAll(p.st.cwd, "/", `\`), true
		}
		return "", false
	case "HOMEDRIVE":
		if h, ok := p.b.env.variable("HOME"); ok && len(h) >= 2 {
			return h[:2], true
		}
	case "HOMEPATH":
		if h, ok := p.b.env.variable("HOME"); ok && len(h) > 2 {
			return strings.ReplaceAll(h[2:], "/", `\`), true
		}
	}
	if k, ok := cmdKnownVars[up]; ok {
		if v, ok := p.b.env.variable(k); ok {
			return strings.ReplaceAll(v, "/", `\`), true
		}
	}
	return "", false
}

// sequence parses commands joined by &, &&, || and |, and parenthesised
// blocks.
func (p *cmdParser) sequence(s string) {
	type seg struct {
		text  string
		op    string
		block string
	}
	var segs []seg
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQ = !inQ
			cur.WriteByte(c)
		case inQ:
			cur.WriteByte(c)
		case c == '^' && i+1 < len(s):
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
		case c == '(' && strings.TrimSpace(cur.String()) == "" || c == '(' && cmdOpensBlock(cur.String()):
			end := cmdMatch(s, i)
			if end < 0 {
				p.b.parseError("cmd: an unclosed (")
				cur.WriteString(s[i:])
				i = len(s)
				continue
			}
			segs = append(segs, seg{text: cur.String(), block: s[i+1 : end]})
			cur.Reset()
			i = end
		case c == '&' || c == '|':
			op := string(c)
			if i+1 < len(s) && s[i+1] == c {
				op += string(c)
				i++
			}
			// 2>&1 is no operator.
			if op == "&" && cur.Len() > 0 && strings.HasSuffix(cur.String(), ">") {
				cur.WriteByte('&')
				continue
			}
			segs = append(segs, seg{text: cur.String(), op: op})
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	segs = append(segs, seg{text: cur.String()})

	in := pipeIn{}
	var prev []*Command
	for _, sg := range segs {
		var cmds []*Command
		if strings.TrimSpace(sg.text) != "" {
			cmds = p.command(sg.text, in)
		}
		if sg.block != "" {
			b := strings.TrimSpace(sg.block)
			// for /f ... in ('command') runs the command.
			if len(b) >= 2 && (b[0] == '\'' && b[len(b)-1] == '\'' || b[0] == '`' && b[len(b)-1] == '`') {
				b = b[1 : len(b)-1]
			}
			child := p.st.child()
			if child.depth > maxDepth {
				p.b.limit("nested more than 8 levels deep")
			} else {
				(&cmdParser{b: p.b, st: child}).sequence(b)
			}
		}
		if len(prev) > 0 && len(cmds) > 0 {
			for _, c := range prev {
				c.PipeTo = cmds[0].Program
			}
		}
		if sg.op == "|" && len(cmds) > 0 {
			in = pipeIn{from: cmds[0].Program, piped: true, roots: listRoots(cmds[0])}
			prev = cmds
		} else {
			in, prev = pipeIn{}, nil
		}
	}
}

// cmdOpensBlock reports whether a ( after this text opens a block: after
// if's condition, else, do, or in.
func cmdOpensBlock(before string) bool {
	t := strings.ToLower(strings.TrimSpace(before))
	if t == "" {
		return true
	}
	f := strings.Fields(t)
	last := f[len(f)-1]
	if last == "else" || last == "do" || last == "in" {
		return true
	}
	return f[0] == "if" || f[0] == "@if"
}

func cmdMatch(s string, i int) int {
	depth := 0
	inQ := false
	for j := i; j < len(s); j++ {
		switch c := s[j]; {
		case c == '"':
			inQ = !inQ
		case inQ:
		case c == '^':
			j++
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// cmdTokens splits a command on blanks, keeping quoted text together and
// taking redirections apart.
func (p *cmdParser) cmdTokens(s string) (words []word, redirs []redirect) {
	var toks []string
	var cur strings.Builder
	inQ := false
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
		}
		cur.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQ = !inQ
			cur.WriteByte(c)
		case inQ:
			cur.WriteByte(c)
		case c == '^' && i+1 < len(s):
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
		case c == ' ' || c == '\t':
			flush()
		case c == '>' || c == '<':
			// fd>, >, >>, 2>&1
			pre := cur.String()
			fd := ""
			if pre == "1" || pre == "2" || pre == "0" {
				fd = pre
				cur.Reset()
			}
			flush()
			op := string(c)
			if c == '>' && i+1 < len(s) && s[i+1] == '>' {
				op = ">>"
				i++
			}
			if i+1 < len(s) && s[i+1] == '&' {
				i += 2 // >&1
				continue
			}
			toks = append(toks, cmdMark+"redir"+fd+op)
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if strings.HasPrefix(t, cmdMark+"redir") {
			op := strings.TrimLeft(strings.TrimPrefix(t, cmdMark+"redir"), "012")
			if i+1 < len(toks) {
				i++
				redirs = append(redirs, redirect{op: op, target: p.word(toks[i])})
			}
			continue
		}
		words = append(words, p.word(t))
	}
	return words, redirs
}

// word resolves a cmd token: quotes and carets off, delayed !VAR! expanded
// when on, run-time parts marked.
func (p *cmdParser) word(t string) word {
	w := word{raw: strings.ReplaceAll(t, cmdMark, ""), varEnd: -1}
	if p.delayed() {
		t = cmdBangRe.ReplaceAllStringFunc(t, func(m string) string {
			name := strings.Trim(m, "!")
			if v, ok := p.cmdVar(name); ok && !strings.Contains(name, ":") {
				return v
			}
			return cmdMark + m + cmdMark
		})
	}
	var sb strings.Builder
	known := true
	inQ := false
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c == cmdMark[0]:
			end := strings.IndexByte(t[i+1:], cmdMark[0])
			if end < 0 {
				end = len(t) - i - 1
			}
			src := t[i+1 : i+1+end]
			if known {
				w.prefix = sb.String()
				known = false
			}
			sb.WriteString(src)
			w.varEnd = sb.Len()
			i += end + 1
		case c == '"':
			inQ = !inQ
			w.quoted = true
		case c == '^' && !inQ && i+1 < len(t):
			i++
			sb.WriteByte(t[i])
			w.varEnd = sb.Len() // a caret inside a word builds it
		default:
			sb.WriteByte(c)
		}
	}
	w.value = sb.String()
	w.dynamic = !known
	if known {
		w.prefix = w.value
	}
	w.glob = strings.ContainsAny(w.value, "*?")
	return w
}

// command parses one cmd command, its built-ins handled.
func (p *cmdParser) command(text string, in pipeIn) []*Command {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "@")
	lower := strings.ToLower(t)
	if t == "" || strings.HasPrefix(lower, "rem ") || lower == "rem" || strings.HasPrefix(t, "::") ||
		strings.HasPrefix(t, ":") || lower == "echo off" || lower == "echo on" {
		return nil
	}
	words, redirs := p.cmdTokens(t)
	// The text after a block: ") do command", ") else (".
	for len(words) > 0 && (strings.EqualFold(words[0].value, "do") || strings.EqualFold(words[0].value, "else")) {
		words = words[1:]
	}
	if len(words) == 0 && len(redirs) == 0 {
		return nil
	}
	if len(words) == 0 {
		rc := &rawCmd{raw: t, redirs: redirs}
		return p.b.command(rc, p.st, nil)
	}
	first := strings.ToLower(words[0].value)
	switch first {
	case "if":
		return p.ifCommand(t, words, redirs, in)
	case "for":
		return p.forCommand(t, words, redirs, in)
	case "set":
		p.set(words)
	case "setlocal":
		for _, w := range words[1:] {
			if strings.EqualFold(w.value, "enabledelayedexpansion") {
				if p.st.aliases == nil {
					p.st.aliases = map[string]string{}
				}
				p.st.aliases["\x00delayed"] = "1"
			}
		}
	case "call":
		if len(words) > 1 && strings.HasPrefix(words[1].value, ":") {
			return nil // a label in the same script
		}
		rc := &rawCmd{words: words, raw: t, redirs: redirs, pipeFrom: in.from, piped: in.piped, pipeRoots: in.roots}
		cmds := p.b.command(rc, p.st, nil)
		if len(words) > 1 {
			sub := &rawCmd{words: words[1:], raw: t, pipeFrom: in.from, piped: in.piped, pipeRoots: in.roots}
			cmds = append(cmds, p.b.command(sub, p.st, []string{"call"})...)
		}
		return cmds
	case "cmd":
		// cmd /v:on turns on delayed expansion for its commands.
		for _, w := range words[1:] {
			if strings.EqualFold(w.value, "/v:on") || strings.EqualFold(w.value, "/v") {
				if p.st.aliases == nil {
					p.st.aliases = map[string]string{}
				}
				p.st.aliases["\x00delayed"] = "1"
			}
		}
	}
	rc := &rawCmd{words: words, raw: t, redirs: redirs, pipeFrom: in.from, piped: in.piped, pipeRoots: in.roots}
	return p.b.command(rc, p.st, nil)
}

// set records set X=value; set /a and set /p give values only known at
// run time.
func (p *cmdParser) set(words []word) {
	if len(words) < 2 {
		return
	}
	if p.st.vars == nil {
		p.st.vars = map[string]*string{}
	}
	rest := words[1:]
	dynamic := false
	if f := strings.ToLower(rest[0].value); f == "/a" || f == "/p" {
		dynamic = true
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return
	}
	var parts []string
	for _, w := range rest {
		parts = append(parts, w.value)
		dynamic = dynamic || w.dynamic
	}
	name, val, ok := strings.Cut(strings.Join(parts, " "), "=")
	if !ok {
		return
	}
	name = strings.ToUpper(strings.TrimSpace(name))
	if dynamic {
		p.st.vars[name] = nil
		return
	}
	p.st.vars[name] = &val
}

// ifCommand: if [/i] [not] exist X | defined X | errorlevel N | A==B
// command. The condition's file is listed; the command is parsed.
func (p *cmdParser) ifCommand(t string, words []word, redirs []redirect, in pipeIn) []*Command {
	i := 1
	if i < len(words) && strings.EqualFold(words[i].value, "/i") {
		i++
	}
	if i < len(words) && strings.EqualFold(words[i].value, "not") {
		i++
	}
	var cond []*Command
	if i < len(words) {
		switch strings.ToLower(words[i].value) {
		case "exist":
			if i+1 < len(words) {
				c := &Command{Program: "if", ProgramRaw: words[0].raw, Raw: t, Dialect: Cmd, Depth: p.st.depth}
				p.b.add(c)
				p.b.path(c, words[i+1], List, p.st)
				cond = append(cond, c)
			}
			i += 2
		case "defined", "errorlevel", "cmdextversion":
			i += 2
		default:
			// A==B or A op B
			if strings.Contains(words[i].value, "==") {
				i++
			} else {
				i += 3
			}
		}
	}
	if i >= len(words) {
		return cond
	}
	rc := &rawCmd{words: words[i:], raw: t, redirs: redirs, pipeFrom: in.from, piped: in.piped, pipeRoots: in.roots}
	return append(cond, p.b.command(rc, p.st, nil)...)
}

// forCommand: for ... in (set) do command. The command after do is
// parsed; for /f marks its loop variable as only known at run time.
func (p *cmdParser) forCommand(t string, words []word, redirs []redirect, in pipeIn) []*Command {
	for i, w := range words {
		if strings.EqualFold(w.value, "do") && i+1 < len(words) {
			rc := &rawCmd{words: words[i+1:], raw: t, redirs: redirs, pipeFrom: in.from, piped: in.piped}
			return p.b.command(rc, p.st, nil)
		}
	}
	return nil
}
