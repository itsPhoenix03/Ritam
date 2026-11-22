package facts

import (
	"encoding/base64"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// The PowerShell parser is a tokenizer of ritam's own, not PowerShell's:
// strings ('', "", here-strings), backtick escapes, variables, $(...),
// (...), @(...), script blocks, [Type]::Method(...) calls, pipelines,
// redirections and the keywords' blocks. What it cannot read is a parse
// error, which the rules treat as suspicious. (command-parsing.md would use
// PowerShell's own parser in a helper process; this is its fallback.)

type psKind int

const (
	tWord   psKind = iota
	tStr           // '...' or @'...'@: literal
	tDStr          // "..." or @"..."@: expandable
	tVar           // $name, ${name}, $env:NAME
	tSub           // $( ... )
	tParen         // ( ... )
	tArray         // @( ... )
	tHash          // @{ ... }
	tBlock         // { ... }
	tType          // [ ... ]
	tMember        // .Name or ::Name right after a token
	tOp            // | ; && || & . = += > >> 2> 2>&1 ...
	tNL            // a line end
	tComma
)

type psTok struct {
	kind psKind
	// text is the token as written; inner is a group's inside or a
	// string's content.
	text, inner string
	// glued: no blank before the token.
	glued bool
}

// psLex splits PowerShell code into tokens. ok is false when a quote or a
// bracket is never closed.
func psLex(src string) (toks []psTok, ok bool) {
	i := 0
	n := len(src)
	glued := false
	add := func(t psTok) {
		t.glued = glued
		toks = append(toks, t)
		glued = true
	}
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			i++
			glued = false
			continue
		case c == '`' && i+1 < n && (src[i+1] == '\n' || src[i+1] == '\r'):
			// A line continuation.
			i += 2
			for i < n && (src[i] == '\n' || src[i] == '\r') {
				i++
			}
			glued = false
			continue
		case c == '\n':
			add(psTok{kind: tNL, text: "\n"})
			i++
			glued = false
			continue
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		case c == '<' && i+1 < n && src[i+1] == '#':
			end := strings.Index(src[i+2:], "#>")
			if end < 0 {
				return toks, false
			}
			i += end + 4
			continue
		case c == '\'' || isSmartSingle(src[i:]):
			end, inner, good := psSingle(src, i)
			if !good {
				return toks, false
			}
			add(psTok{kind: tStr, text: src[i:end], inner: inner})
			i = end
			continue
		case c == '"' || isSmartDouble(src[i:]):
			end, good := psDouble(src, i)
			if !good {
				return toks, false
			}
			_, sz := utf8.DecodeRuneInString(src[i:])
			_, esz := utf8.DecodeLastRuneInString(src[:end])
			add(psTok{kind: tDStr, text: src[i:end], inner: src[i+sz : end-esz]})
			i = end
			continue
		case c == '@' && i+1 < n && (src[i+1] == '\'' || src[i+1] == '"'):
			q := src[i+1]
			close := "\n" + string(q) + "@"
			start := strings.IndexByte(src[i:], '\n')
			if start < 0 {
				return toks, false
			}
			end := strings.Index(src[i+start:], close)
			if end < 0 {
				return toks, false
			}
			inner := src[i+start+1 : i+start+end]
			kind := tStr
			if q == '"' {
				kind = tDStr
			}
			stop := i + start + end + len(close)
			add(psTok{kind: kind, text: src[i:stop], inner: strings.TrimSuffix(inner, "\r")})
			i = stop
			continue
		case c == '$' && i+1 < n && src[i+1] == '(':
			end := psMatch(src, i+1, '(', ')')
			if end < 0 {
				return toks, false
			}
			add(psTok{kind: tSub, text: src[i : end+1], inner: src[i+2 : end]})
			i = end + 1
			continue
		case c == '@' && i+1 < n && (src[i+1] == '(' || src[i+1] == '{'):
			open, closeCh, kind := byte('('), byte(')'), tArray
			if src[i+1] == '{' {
				open, closeCh, kind = '{', '}', tHash
			}
			end := psMatch(src, i+1, open, closeCh)
			if end < 0 {
				return toks, false
			}
			add(psTok{kind: kind, text: src[i : end+1], inner: src[i+2 : end]})
			i = end + 1
			continue
		case c == '$':
			j := i + 1
			if j < n && src[j] == '{' {
				end := strings.IndexByte(src[j:], '}')
				if end < 0 {
					return toks, false
				}
				add(psTok{kind: tVar, text: src[i : j+end+1], inner: src[j+1 : j+end]})
				i = j + end + 1
				continue
			}
			for j < n && (isPSNameChar(src[j]) || src[j] == ':' && j+1 < n && isPSNameChar(src[j+1])) {
				j++
			}
			if j == i+1 && j < n && strings.IndexByte("$?^_", src[j]) >= 0 {
				j++
			}
			add(psTok{kind: tVar, text: src[i:j], inner: src[i+1 : j]})
			i = j
			continue
		case c == '(':
			end := psMatch(src, i, '(', ')')
			if end < 0 {
				return toks, false
			}
			add(psTok{kind: tParen, text: src[i : end+1], inner: src[i+1 : end]})
			i = end + 1
			continue
		case c == '{':
			end := psMatch(src, i, '{', '}')
			if end < 0 {
				return toks, false
			}
			add(psTok{kind: tBlock, text: src[i : end+1], inner: src[i+1 : end]})
			i = end + 1
			continue
		case c == '[' && (!glued || len(toks) == 0 || toks[len(toks)-1].kind == tOp):
			end := psMatch(src, i, '[', ']')
			if end < 0 {
				return toks, false
			}
			add(psTok{kind: tType, text: src[i : end+1], inner: src[i+1 : end]})
			i = end + 1
			continue
		case (c == '.' || c == ':' && i+1 < n && src[i+1] == ':') && glued && len(toks) > 0 && memberable(toks[len(toks)-1]):
			j := i + 1
			if c == ':' {
				j = i + 2
			}
			k := j
			for k < n && (isPSNameChar(src[k])) {
				k++
			}
			if k > j {
				add(psTok{kind: tMember, text: src[i:k], inner: src[j:k]})
				i = k
				continue
			}
		case c == ',':
			add(psTok{kind: tComma, text: ","})
			i++
			glued = false
			continue
		case c == ';':
			add(psTok{kind: tOp, text: ";"})
			i++
			glued = false
			continue
		case c == '|' || c == '&':
			op := string(c)
			if i+1 < n && src[i+1] == c {
				op += string(c)
			}
			add(psTok{kind: tOp, text: op})
			i += len(op)
			glued = false
			continue
		case c == '>' || (c >= '1' && c <= '6' || c == '*') && i+1 < n && src[i+1] == '>' && !glued:
			j := i
			if c != '>' {
				j++
			}
			j++
			if j < n && src[j] == '>' {
				j++
			}
			if j+1 < n && src[j] == '&' && src[j+1] >= '1' && src[j+1] <= '6' {
				j += 2
			}
			add(psTok{kind: tOp, text: src[i:j]})
			i = j
			glued = false
			continue
		case c == '=' && !glued || (c == '+' || c == '-' || c == '*' || c == '/') && i+1 < n && src[i+1] == '=' && !glued:
			j := i + 1
			if c != '=' {
				j++
			}
			add(psTok{kind: tOp, text: src[i:j]})
			i = j
			glued = false
			continue
		case c == '.' && !glued && i+1 < n && (src[i+1] == ' ' || src[i+1] == '\t'):
			add(psTok{kind: tOp, text: "."})
			i++
			glued = false
			continue
		}
		// A bare word, up to a blank or an operator; backticks escape.
		j := i
		for j < n {
			ch := src[j]
			if ch == '`' && j+1 < n {
				j += 2
				continue
			}
			if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' || ch == ';' || ch == '|' || ch == '&' ||
				ch == ',' || ch == '(' || ch == ')' || ch == '{' || ch == '}' || ch == '"' || ch == '\'' ||
				ch == '>' && j > i || ch == '$' && j > i && j+1 < n && src[j+1] == '(' {
				break
			}
			if ch == '=' && j > i && j+1 < n && src[j+1] != '=' && strings.HasPrefix(src[i:], "$") {
				break
			}
			if isSmartQuote(src[j:]) {
				break
			}
			j++
		}
		if j == i {
			j = i + 1 // a lone ) or }: take it as a word, the parse will flag it
		}
		add(psTok{kind: tWord, text: src[i:j]})
		i = j
	}
	return toks, true
}

func memberable(t psTok) bool {
	return t.kind == tType || t.kind == tVar || t.kind == tParen || t.kind == tSub || t.kind == tMember ||
		t.kind == tStr || t.kind == tDStr || t.kind == tArray
}

func isPSNameChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// PowerShell accepts typographic quotes as quotes.
func isSmartSingle(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r == '‘' || r == '’' || r == '‚' || r == '‛'
}

func isSmartDouble(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r == '“' || r == '”' || r == '„'
}

func isSmartQuote(s string) bool { return isSmartSingle(s) || isSmartDouble(s) }

// psSingle reads a single-quoted string starting at i; ” is a quote.
func psSingle(src string, i int) (end int, inner string, ok bool) {
	_, sz := utf8.DecodeRuneInString(src[i:])
	var sb strings.Builder
	j := i + sz
	for j < len(src) {
		if src[j] == '\'' || isSmartSingle(src[j:]) {
			_, qs := utf8.DecodeRuneInString(src[j:])
			if j+qs < len(src) && (src[j+qs] == '\'' || isSmartSingle(src[j+qs:])) {
				sb.WriteByte('\'')
				_, qs2 := utf8.DecodeRuneInString(src[j+qs:])
				j += qs + qs2
				continue
			}
			return j + qs, sb.String(), true
		}
		sb.WriteByte(src[j])
		j++
	}
	return len(src), sb.String(), false
}

// psDouble finds the end of a double-quoted string starting at i.
func psDouble(src string, i int) (end int, ok bool) {
	_, sz := utf8.DecodeRuneInString(src[i:])
	j := i + sz
	for j < len(src) {
		switch {
		case src[j] == '`' && j+1 < len(src):
			j += 2
			continue
		case src[j] == '$' && j+1 < len(src) && src[j+1] == '(':
			e := psMatch(src, j+1, '(', ')')
			if e < 0 {
				return len(src), false
			}
			j = e + 1
			continue
		case src[j] == '"' || isSmartDouble(src[j:]):
			_, qs := utf8.DecodeRuneInString(src[j:])
			if j+qs < len(src) && src[j+qs] == '"' {
				j += qs + 1
				continue
			}
			return j + qs, true
		}
		j++
	}
	return len(src), false
}

// psMatch finds the bracket closing the one at i, skipping strings and
// nested brackets.
func psMatch(src string, i int, open, close byte) int {
	depth := 0
	for j := i; j < len(src); j++ {
		switch c := src[j]; {
		case c == '`':
			j++
		case c == '\'':
			e, _, ok := psSingle(src, j)
			if !ok {
				return -1
			}
			j = e - 1
		case c == '"':
			e, ok := psDouble(src, j)
			if !ok {
				return -1
			}
			j = e - 1
		case c == '#' && open != '[' && (j == 0 || src[j-1] == ' ' || src[j-1] == '\n' || src[j-1] == '\t'):
			for j < len(src) && src[j] != '\n' {
				j++
			}
		case c == open:
			depth++
		case c == close:
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

func (b *builder) parsePowerShell(src string, st *sh) {
	toks, ok := psLex(src)
	if !ok {
		b.parseError("powershell: an unclosed quote or bracket")
	}
	p := &psParser{b: b, st: st}
	p.statements(toks)
}

type psParser struct {
	b  *builder
	st *sh
}

var psKeywords = map[string]bool{"if": true, "elseif": true, "else": true, "foreach": true, "for": true,
	"while": true, "do": true, "until": true, "switch": true, "try": true, "catch": true, "finally": true,
	"trap": true, "begin": true, "process": true, "end": true, "param": true, "return": true, "throw": true,
	"exit": true, "break": true, "continue": true, "data": true, "dynamicparam": true, "class": true,
	"enum": true, "using": true}

// statements splits tokens on ; && || and line ends, then on pipes.
func (p *psParser) statements(toks []psTok) {
	var cur []psTok
	flush := func() {
		if len(cur) > 0 {
			p.pipeline(cur)
		}
		cur = nil
	}
	for i, t := range toks {
		if t.kind == tNL || t.kind == tOp && (t.text == ";" || t.text == "&&" || t.text == "||") {
			flush()
			continue
		}
		// & at the end of a statement runs it in the background.
		if t.kind == tOp && t.text == "&" && (i+1 == len(toks) || toks[i+1].kind == tNL) && len(cur) > 0 {
			continue
		}
		cur = append(cur, t)
	}
	flush()
}

func (p *psParser) pipeline(toks []psTok) {
	var elems [][]psTok
	var cur []psTok
	for _, t := range toks {
		if t.kind == tOp && t.text == "|" {
			elems = append(elems, cur)
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	elems = append(elems, cur)
	in := pipeIn{}
	var prev []*Command
	for _, el := range elems {
		cmds, out, roots := p.element(el, in)
		to := ""
		if len(cmds) > 0 {
			to = cmds[0].Program
		}
		for _, c := range prev {
			c.PipeTo = to
		}
		from := ""
		if len(cmds) > 0 {
			from = cmds[0].Program
		}
		in = pipeIn{from: from, text: out, roots: roots, piped: true}
		prev = cmds
	}
}

// element parses one pipeline element: a command, an assignment, a
// keyword statement or an expression. It returns its records, the text it
// outputs when known, and the paths it lists.
func (p *psParser) element(toks []psTok, in pipeIn) ([]*Command, *string, []Path) {
	if len(toks) == 0 {
		return nil, nil, nil
	}
	first := toks[0]
	if first.kind == tWord {
		kw := strings.ToLower(first.text)
		switch {
		case kw == "function" || kw == "filter" || kw == "workflow":
			name := ""
			if len(toks) > 1 {
				name = strings.ToLower(toks[1].text)
			}
			for _, t := range toks[1:] {
				if t.kind == tBlock {
					if p.st.funcs == nil {
						p.st.funcs = map[string]any{}
					}
					p.st.funcs[name] = t.inner
				}
			}
			c := &Command{Program: "define-function", ProgramRaw: first.text, Args: []Arg{{Value: name, Raw: name}},
				Subcommand: name, Raw: psText(toks), Dialect: PowerShell, Depth: p.st.depth}
			p.b.add(c)
			return []*Command{c}, nil, nil
		case psKeywords[kw]:
			for _, t := range toks[1:] {
				switch t.kind {
				case tBlock:
					p.b.nested(PowerShell, t.inner, p.st, false)
				case tParen, tSub:
					p.b.nested(PowerShell, t.inner, p.st, false)
				}
			}
			if kw == "return" || kw == "throw" || kw == "exit" {
				return p.element(toks[1:], in)
			}
			return nil, nil, nil
		}
	}
	// Assignment: $x = ..., $env:X = ...
	if first.kind == tVar && len(toks) >= 2 && toks[1].kind == tOp && strings.HasSuffix(toks[1].text, "=") {
		return p.assignment(first, toks[1].text, toks[2:], in)
	}
	// An expression: a .NET call, a string, a variable, a group.
	if first.kind != tWord && !(first.kind == tOp && (first.text == "&" || first.text == ".")) &&
		first.kind != tDStr && first.kind != tStr || (first.kind == tStr || first.kind == tDStr) && len(toks) == 1 {
		return p.expression(toks, in)
	}
	return p.command(toks, in)
}

func psText(toks []psTok) string {
	var sb strings.Builder
	for i, t := range toks {
		if i > 0 && !t.glued {
			sb.WriteByte(' ')
		}
		sb.WriteString(t.text)
	}
	return sb.String()
}

// assignment records a variable, and an environment variable as an
// assignment; the right side is parsed as the command or expression it is.
func (p *psParser) assignment(v psTok, op string, rhs []psTok, in pipeIn) ([]*Command, *string, []Path) {
	name := strings.ToLower(v.inner)
	cmds, out, _ := p.element(rhs, in)
	val, known := "", false
	if len(rhs) == 1 && (rhs[0].kind == tStr || rhs[0].kind == tDStr || rhs[0].kind == tWord) {
		w := p.word(rhs[0])
		val, known = w.value, !w.dynamic
	} else if out != nil {
		val, known = *out, true
	}
	if p.st.vars == nil {
		p.st.vars = map[string]*string{}
	}
	if known && op == "=" {
		p.st.vars[name] = &val
	} else {
		p.st.vars[name] = nil
	}
	// Remember what a variable holds: New-Object Net.WebClient.
	if len(cmds) > 0 {
		for _, c := range cmds {
			if strings.HasSuffix(c.Program, "::new") {
				if p.st.aliases == nil {
					p.st.aliases = map[string]string{}
				}
				p.st.aliases["\x00type:"+name] = strings.TrimSuffix(c.Program, "::new")
			}
		}
	}
	if env, ok := strings.CutPrefix(name, "env:"); ok {
		c := &Command{Raw: psText(append([]psTok{v}, rhs...)), Dialect: PowerShell, Depth: p.st.depth,
			Assigns: []Assign{{Name: v.inner[4:], Value: val, Dynamic: !known}}}
		_ = env
		p.b.add(c)
		cmds = append([]*Command{c}, cmds...)
	}
	return cmds, nil, nil
}

// command parses a command element: a program (or & / . with one) and
// its arguments and redirections.
func (p *psParser) command(toks []psTok, in pipeIn) ([]*Command, *string, []Path) {
	rc := &rawCmd{raw: psText(toks), pipeFrom: in.from, piped: in.piped, stdin: in.text, pipeRoots: in.roots}
	i := 0
	if toks[0].kind == tOp && (toks[0].text == "&" || toks[0].text == ".") {
		i = 1
		if i < len(toks) && toks[i].kind == tBlock {
			cmds := p.b.nested(PowerShell, toks[i].inner, p.st, false)
			return cmds, nil, nil
		}
		if toks[0].text == "." && i < len(toks) {
			// Dot-sourcing runs a script in this scope.
			rc.words = append(rc.words, lit("."))
		}
	}
	for ; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.kind == tOp && strings.Contains(t.text, ">"):
			if strings.Contains(t.text, "&") {
				continue // 2>&1
			}
			if i+1 < len(toks) {
				i++
				op := ">"
				if strings.Count(t.text, ">") == 2 {
					op = ">>"
				}
				rc.redirs = append(rc.redirs, redirect{op: op, target: p.word(toks[i])})
			}
		case t.kind == tComma:
		case (t.kind == tParen || t.kind == tSub || t.kind == tVar || t.kind == tType) && i+1 < len(toks) &&
			toks[i+1].kind == tMember && toks[i+1].glued:
			// (New-Object Net.WebClient).DownloadString(...) as an argument:
			// evaluated as the expression it is.
			j := i + 1
			for j < len(toks) && toks[j].kind == tMember && toks[j].glued {
				j++
				if j < len(toks) && toks[j].kind == tParen && toks[j].glued {
					j++
				}
			}
			w := p.exprWord(toks[i:j])
			w.raw = psText(toks[i:j])
			rc.words = append(rc.words, w)
			i = j - 1
		case t.kind == tMember && len(rc.words) > 0:
			last := &rc.words[len(rc.words)-1]
			last.value += t.text
			last.raw += t.text
		case t.glued && len(rc.words) > 0 && t.kind != tParen && t.kind != tBlock:
			// Tokens written together are one word: C:\$name\x.
			w := p.word(t)
			last := &rc.words[len(rc.words)-1]
			p.join(last, w)
		default:
			rc.words = append(rc.words, p.word(t))
		}
	}
	if len(rc.words) == 0 {
		return nil, nil, nil
	}
	// & $x, & (...) and & ("a" + "b") build the program's name.
	if toks[0].kind == tOp && len(toks) > 1 && toks[1].kind != tWord && toks[1].kind != tStr {
		rc.words[0].varEnd = len(rc.words[0].value)
	}
	// A function defined earlier runs its body.
	if body, ok := p.st.funcs[strings.ToLower(rc.words[0].value)].(string); ok && !p.st.active[rc.words[0].value] {
		cmds := p.b.command(rc, p.st, nil)
		if p.st.active == nil {
			p.st.active = map[string]bool{}
		}
		p.st.active[rc.words[0].value] = true
		p.b.nested(PowerShell, body, p.st, false)
		delete(p.st.active, rc.words[0].value)
		return cmds, nil, nil
	}
	cmds := p.b.command(rc, p.st, nil)
	if len(cmds) == 0 {
		return nil, nil, nil
	}
	main := cmds[0]
	switch main.Program {
	case "set-alias", "new-alias":
		var name, val string
		for k, a := range main.Args {
			switch strings.ToLower(a.Value) {
			case "-name":
				if k+1 < len(main.Args) {
					name = main.Args[k+1].Value
				}
			case "-value":
				if k+1 < len(main.Args) {
					val = main.Args[k+1].Value
				}
			}
		}
		var pos []string
		for _, a := range main.Args {
			if !strings.HasPrefix(a.Value, "-") {
				pos = append(pos, a.Value)
			}
		}
		if name == "" && len(pos) > 0 {
			name = pos[0]
		}
		if val == "" && len(pos) > 1 {
			val = pos[1]
		}
		if name != "" && val != "" {
			if p.st.aliases == nil {
				p.st.aliases = map[string]string{}
			}
			target, _ := programName(val, PowerShell)
			if a, ok := psAliases[target]; ok {
				target = a
			}
			p.st.aliases[strings.ToLower(name)] = target
		}
	case "new-object":
		for _, a := range main.Args {
			if !strings.HasPrefix(a.Value, "-") && !a.Dynamic {
				typ := psType(a.Value)
				c := &Command{Program: typ + "::new", ProgramRaw: main.ProgramRaw, Raw: main.Raw, Dialect: PowerShell,
					Depth: p.st.depth, WrappedBy: []string{"new-object"}}
				p.b.add(c)
				cmds = append(cmds, c)
				break
			}
		}
	}
	var out *string
	if main.Program == "write-output" || main.Program == "get-content" && in.text != nil {
		if !anyDynamicArg(main.Args) && main.Program == "write-output" {
			s := main.Argline()
			out = &s
		}
	}
	return cmds, out, listRoots(main)
}

// join appends a token written against the previous one to its word.
func (p *psParser) join(last *word, w word) {
	if w.dynamic && !last.dynamic {
		last.prefix = last.value
	}
	last.value += w.value
	last.raw += w.raw
	last.dynamic = last.dynamic || w.dynamic
	if !last.dynamic {
		last.prefix = last.value
	}
	if w.varEnd >= 0 {
		last.varEnd = len(last.value)
	}
	last.from = append(last.from, w.from...)
	last.glob = last.glob || w.glob
}

// expression parses an element that is no command: [T]::M(...), $x.M(),
// (...), a string. A string or a decoded value it outputs is returned.
func (p *psParser) expression(toks []psTok, in pipeIn) ([]*Command, *string, []Path) {
	start := len(p.b.call.Commands)
	w := p.exprWord(toks)
	var out *string
	if !w.dynamic {
		v := w.value
		out = &v
	}
	return p.b.call.Commands[start:], out, nil
}

// exprWord evaluates what it can of an expression: literals, known
// variables, decoded base64; the .NET calls and commands in it are
// reported.
func (p *psParser) exprWord(toks []psTok) word {
	if len(toks) == 0 {
		return word{dynamic: true, varEnd: -1}
	}
	// [Type]::Method(args) and chains of members.
	i := 0
	var cur word
	typ := ""
	switch t := toks[0]; t.kind {
	case tType:
		typ = psType(t.inner)
		cur = word{value: t.text, raw: t.text, dynamic: true, varEnd: -1}
		i = 1
	case tVar:
		name := strings.ToLower(t.inner)
		if tt, ok := p.st.aliases["\x00type:"+name]; ok {
			typ = tt
		}
		cur = p.word(t)
		i = 1
	case tParen, tSub:
		start := len(p.b.call.Commands)
		cur = p.groupWord(t)
		for _, c := range p.b.call.Commands[start:] {
			if strings.HasSuffix(c.Program, "::new") {
				typ = strings.TrimSuffix(c.Program, "::new")
			}
		}
		i = 1
	default:
		cur = p.word(t)
		i = 1
	}
	static := toks[0].kind == tType
	for i < len(toks) {
		t := toks[i]
		if t.kind != tMember {
			break
		}
		member := strings.ToLower(t.inner)
		isCall := i+1 < len(toks) && toks[i+1].kind == tParen && toks[i+1].glued
		if !isCall {
			// A property: [Text.Encoding]::UTF8 is an encoding.
			if typ == "system.text.encoding" || static && strings.HasPrefix(t.text, "::") && typ == "system.text.encoding" {
				cur = word{value: member, raw: cur.raw + t.text, dynamic: true, varEnd: -1}
				static = false
			} else {
				typ = ""
				cur = word{value: cur.value, raw: cur.raw + t.text, dynamic: true, varEnd: -1, from: cur.from}
				static = false
			}
			i++
			continue
		}
		args := p.callArgs(toks[i+1].inner)
		name := "?." + member
		if typ != "" {
			sep := "."
			if static && strings.HasPrefix(t.text, "::") {
				sep = "::"
			}
			name = typ + sep + member
		}
		if member == "new" && typ != "" {
			name = typ + "::new"
		}
		rc := &rawCmd{words: append([]word{lit(name)}, args...), raw: psText(toks[:i+2]), dotnet: true}
		p.b.command(rc, p.st, nil)
		if name == "?."+member && psWatchMethods[member] {
			p.b.note("a ." + member + " call on an object of unknown type")
		}
		cur = p.evalCall(name, cur, args)
		if member == "new" {
			typ = strings.TrimSuffix(name, "::new")
		} else {
			typ = ""
		}
		static = false
		i += 2
	}
	// "a" + "b" builds a string.
	if i+1 < len(toks) && toks[i].kind == tWord && toks[i].text == "+" {
		rest := p.exprWord(toks[i+1:])
		out := word{value: cur.value + rest.value, raw: psText(toks), dynamic: cur.dynamic || rest.dynamic,
			from: append(slices.Clone(cur.from), rest.from...)}
		switch {
		case !cur.dynamic && !rest.dynamic:
			out.prefix = out.value
		case !cur.dynamic:
			out.prefix = cur.value + rest.prefix
		default:
			out.prefix = cur.prefix
		}
		out.varEnd = len(out.value)
		return out
	}
	// Anything after the expression (operators, more values) is checked
	// for groups to parse.
	for _, t := range toks[i:] {
		switch t.kind {
		case tParen, tSub, tArray:
			p.groupWord(t)
		case tBlock:
			p.b.nested(PowerShell, t.inner, p.st, false)
		}
	}
	return cur
}

var psWatchMethods = map[string]bool{"downloadstring": true, "downloadfile": true, "uploadfile": true,
	"uploaddata": true, "invoke": true, "load": true, "getdelegateforfunctionpointer": true}

// evalCall works out the value of the few calls that decode text.
func (p *psParser) evalCall(name string, recv word, args []word) word {
	out := word{value: name + "(...)", raw: name, dynamic: true, varEnd: -1}
	for _, a := range args {
		out.from = append(out.from, a.from...)
	}
	out.from = append(out.from, recv.from...)
	out.from = append(out.from, name)
	switch {
	case name == "system.convert::frombase64string" && len(args) == 1 && !args[0].dynamic:
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(args[0].value))
		if err == nil {
			out.value, out.dynamic = string(capBytes(data)), false
			out.prefix = out.value
		}
	case strings.HasSuffix(name, "encoding.getstring") && len(args) == 1 && !args[0].dynamic:
		text := args[0].value
		if recv.value == "unicode" {
			b := []byte(text)
			if len(b)%2 == 0 {
				u := make([]uint16, len(b)/2)
				for k := range u {
					u[k] = uint16(b[2*k]) | uint16(b[2*k+1])<<8
				}
				text = string(utf16.Decode(u))
			}
		}
		if utf8.ValidString(text) {
			out.value, out.dynamic = text, false
			out.prefix = text
			p.b.call.Contents = append(p.b.call.Contents, Content{Scope: ScopeDecoded, Text: text})
		}
	case name == "system.management.automation.scriptblock::create" && len(args) == 1 && !args[0].dynamic:
		p.b.nested(PowerShell, args[0].value, p.st, false)
	}
	return out
}

// callArgs splits a .NET call's arguments on top-level commas.
func (p *psParser) callArgs(inner string) []word {
	toks, ok := psLex(inner)
	if !ok {
		p.b.parseError("powershell: an unclosed quote or bracket")
	}
	var out []word
	var cur []psTok
	flush := func() {
		if len(cur) > 0 {
			out = append(out, p.exprWord(cur))
		}
		cur = nil
	}
	for _, t := range toks {
		if t.kind == tComma {
			flush()
			continue
		}
		if t.kind == tNL {
			continue
		}
		cur = append(cur, t)
	}
	flush()
	return out
}

// groupWord parses a (...) or $(...) group as code and stands for its
// result: known when it is a known value, else only known at run time.
func (p *psParser) groupWord(t psTok) word {
	inner, ok := psLex(t.inner)
	if !ok {
		p.b.parseError("powershell: an unclosed quote or bracket")
	}
	// A group holding one expression has that expression's value.
	single := true
	for _, x := range inner {
		if x.kind == tNL || x.kind == tOp && (x.text == ";" || x.text == "|") {
			single = false
		}
	}
	if single && len(inner) > 0 && inner[0].kind != tWord {
		w := p.exprWord(inner)
		w.raw = t.text
		if w.dynamic {
			w.varEnd = 0
		}
		return w
	}
	start := len(p.b.call.Commands)
	child := p.st.child()
	if child.depth > maxDepth {
		p.b.limit("nested more than 8 levels deep")
		return word{value: t.text, raw: t.text, dynamic: true, varEnd: 0}
	}
	(&psParser{b: p.b, st: child}).statements(inner)
	return word{value: t.text, raw: t.text, dynamic: true, varEnd: 0, from: programs(p.b.call.Commands[start:])}
}

// word resolves one token as a command argument.
func (p *psParser) word(t psTok) word {
	switch t.kind {
	case tStr:
		return word{value: t.inner, raw: t.text, prefix: t.inner, quoted: true, varEnd: -1}
	case tDStr:
		w := p.expand(t.inner)
		w.raw, w.quoted = t.text, true
		return w
	case tVar:
		if v, ok := p.variable(t.inner); ok {
			return word{value: v, raw: t.text, prefix: v, varEnd: len(v)}
		}
		return word{value: t.text, raw: t.text, dynamic: true, varEnd: len(t.text)}
	case tSub, tParen, tArray:
		return p.groupWord(t)
	case tBlock:
		code := t.inner
		return word{value: t.text, raw: t.text, prefix: t.text, varEnd: -1, block: &code}
	case tHash:
		p.b.nested(PowerShell, t.inner, p.st, false)
		return word{value: t.text, raw: t.text, dynamic: true, varEnd: -1}
	case tType:
		return word{value: t.text, raw: t.text, dynamic: true, varEnd: -1}
	}
	// A bare word: backticks escape; a $ in it expands.
	if strings.Contains(t.text, "$") {
		w := p.expand(t.text)
		w.raw = t.text
		w.glob = hasGlob(w.value)
		return w
	}
	v := unbacktick(t.text, false)
	return word{value: v, raw: t.text, prefix: v, glob: strings.ContainsAny(v, "*?[") && !strings.HasPrefix(v, "-"), varEnd: -1}
}

// unbacktick removes PowerShell's escapes; in a double-quoted string `n
// and friends are control characters.
func unbacktick(s string, dq bool) string {
	if !strings.Contains(s, "`") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '`' || i+1 >= len(s) {
			sb.WriteByte(s[i])
			continue
		}
		i++
		c := s[i]
		if dq {
			switch c {
			case 'n':
				sb.WriteByte('\n')
				continue
			case 't':
				sb.WriteByte('\t')
				continue
			case 'r':
				sb.WriteByte('\r')
				continue
			case '0':
				sb.WriteByte(0)
				continue
			case 'a':
				sb.WriteByte(7)
				continue
			case 'b':
				sb.WriteByte(8)
				continue
			case 'e':
				sb.WriteByte(27)
				continue
			case 'f':
				sb.WriteByte(12)
				continue
			case 'v':
				sb.WriteByte(11)
				continue
			}
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

var psVarInString = regexp.MustCompile(`\$(\{[^}]*\}|[A-Za-z_][A-Za-z0-9_]*(:[A-Za-z_][A-Za-z0-9_]*)?|\$|\?|\^)`)

// expand resolves an expandable string: variables and $(...) inside it.
func (p *psParser) expand(s string) word {
	out := word{varEnd: -1}
	var sb strings.Builder
	known := true
	dyn := func(src string) {
		if known {
			out.prefix = sb.String()
			known = false
		}
		sb.WriteString(src)
		out.varEnd = sb.Len()
	}
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '`' && i+1 < len(s):
			sb.WriteString(unbacktick(s[i:i+2], true))
			i += 2
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			end := psMatch(s, i+1, '(', ')')
			if end < 0 {
				p.b.parseError("powershell: an unclosed $( in a string")
				dyn(s[i:])
				i = len(s)
				continue
			}
			g := p.groupWord(psTok{kind: tSub, text: s[i : end+1], inner: s[i+2 : end]})
			out.from = append(out.from, g.from...)
			if g.dynamic {
				dyn(s[i : end+1])
			} else {
				sb.WriteString(g.value)
			}
			i = end + 1
		case c == '$':
			m := psVarInString.FindString(s[i:])
			if m == "" {
				sb.WriteByte(c)
				i++
				continue
			}
			name := strings.Trim(strings.TrimPrefix(m, "$"), "{}")
			if v, ok := p.variable(name); ok {
				sb.WriteString(v)
				out.varEnd = sb.Len()
			} else {
				dyn(m)
			}
			i += len(m)
		default:
			sb.WriteByte(c)
			i++
		}
	}
	out.value = sb.String()
	out.dynamic = !known
	if known {
		out.prefix = out.value
	}
	return out
}

// psEnv are the environment variables PowerShell code may name that
// resolve to known folders.
var psEnv = map[string]string{"userprofile": "USERPROFILE", "home": "HOME", "appdata": "APPDATA",
	"localappdata": "LOCALAPPDATA", "temp": "TEMP", "tmp": "TMP", "programdata": "PROGRAMDATA",
	"windir": "WINDIR", "systemroot": "WINDIR", "programfiles": "PROGRAMFILES", "username": "USER",
	"allusersprofile": "PROGRAMDATA", "public": "PUBLIC"}

// variable resolves $name: one assigned literally earlier, $HOME, $PWD, a
// well-known $env: variable, $true/$false/$null.
func (p *psParser) variable(name string) (string, bool) {
	n := strings.ToLower(name)
	if v, ok := p.st.vars[n]; ok {
		if v == nil {
			return "", false
		}
		return *v, true
	}
	switch n {
	case "home":
		return p.b.env.variable("HOME")
	case "pwd":
		return p.st.cwd, p.st.cwd != ""
	case "true":
		return "True", true
	case "false":
		return "False", true
	case "null":
		return "", true
	case "profile":
		if h, ok := p.b.env.variable("HOME"); ok {
			return h + "/Documents/WindowsPowerShell/Microsoft.PowerShell_profile.ps1", true
		}
	}
	if e, ok := strings.CutPrefix(n, "env:"); ok {
		if k, ok := psEnv[e]; ok {
			return p.b.env.variable(k)
		}
	}
	return "", false
}

// psAccelerators are PowerShell's type accelerators and the short names the
// rules care about.
var psAccelerators = map[string]string{
	"scriptblock": "system.management.automation.scriptblock", "powershell": "system.management.automation.powershell",
	"convert": "system.convert", "environment": "system.environment", "math": "system.math",
	"activator": "system.activator", "regex": "system.text.regularexpressions.regex", "ipaddress": "system.net.ipaddress",
	"xml": "system.xml.xmldocument", "wmiclass": "system.management.managementclass", "wmi": "system.management.managementobject",
	"adsi": "system.directoryservices.directoryentry", "type": "system.type", "string": "system.string",
	"char": "system.char", "byte": "system.byte", "int": "system.int32", "guid": "system.guid",
	"runspacefactory": "system.management.automation.runspaces.runspacefactory", "uri": "system.uri",
	"psobject": "system.management.automation.psobject", "pscustomobject": "system.management.automation.pscustomobject",
	"diagnostics.process": "system.diagnostics.process", "marshal": "system.runtime.interopservices.marshal",
}

// psType names a .NET type as the rules do: lowercase, the namespace in
// full (IO.File is system.io.file).
func psType(t string) string {
	t = strings.ToLower(strings.TrimSpace(strings.Trim(t, "[]")))
	t = strings.TrimSuffix(t, "[]")
	if a, ok := psAccelerators[t]; ok {
		return a
	}
	if strings.HasPrefix(t, "system.") || strings.HasPrefix(t, "microsoft.") {
		return t
	}
	if strings.Contains(t, ".") {
		return "system." + t
	}
	if strings.IndexFunc(t, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) < 0 {
		return "system." + t
	}
	return t
}
