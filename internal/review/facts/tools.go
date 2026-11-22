package facts

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Input is one tool call as a CLI's hook reported it.
type Input struct {
	// Provider is claude-code or codex.
	Provider  string
	Tool      string
	ToolInput json.RawMessage
	// Cwd is the folder the CLI says the call runs in; the workspace when
	// empty.
	Cwd string
}

// harmless are tools that touch no file, run nothing and contact nothing,
// so an unknown-tool finding would only be noise.
var harmless = map[string]bool{
	"TodoWrite": true, "TodoRead": true, "ExitPlanMode": true, "EnterPlanMode": true, "BashOutput": true,
	"KillShell": true, "KillBash": true, "AskUserQuestion": true, "TaskOutput": true, "TaskStop": true,
	"TaskCreate": true, "TaskUpdate": true, "TaskList": true, "TaskGet": true, "ToolSearch": true,
	"update_plan": true, "ListMcpResourcesTool": true, "ReadMcpResourceTool": true, "SlashCommand": true,
	"Skill": true, "Monitor": true, "ScheduleWakeup": true, "SendMessage": true, "ListAgents": true,
	"list_mcp_resources": true, "list_mcp_resource_templates": true, "read_mcp_resource": true,
}

// Build works out the facts of a tool call.
func Build(in Input, env Env) *Call {
	call := &Call{Provider: in.Provider, Tool: in.Tool, OS: env.OS, Cwd: in.Cwd}
	if call.Cwd == "" {
		call.Cwd = env.Workspace
	}
	b := &builder{env: env, call: call}
	if len(in.ToolInput) > MaxInput {
		b.limit("the tool input is over 1 MiB")
		return call
	}
	var fields map[string]any
	if len(in.ToolInput) > 0 && json.Unmarshal(in.ToolInput, &fields) != nil {
		b.parseError("the tool input is not a JSON object")
		return call
	}
	str := func(k string) string { s, _ := fields[k].(string); return s }
	st := &sh{cwd: env.clean(slash(call.Cwd)), vars: map[string]*string{}}
	if call.Cwd == "" {
		st.cwd = ""
	}

	switch t := in.Tool; {
	case t == "Bash":
		b.shell(Bash, str("command"), st)
	case t == "PowerShell":
		b.shell(PowerShell, str("command"), st)
	case t == "Read" || t == "view_image" || t == "NotebookRead":
		call.Kind = ToolRead
		b.toolPath(firstOf(str("file_path"), str("path"), str("notebook_path")), Read, st)
	case t == "Write":
		call.Kind = ToolEdit
		b.edit(str("file_path"), str("content"), st)
	case t == "Edit":
		call.Kind = ToolEdit
		b.edit(str("file_path"), str("new_string"), st)
	case t == "MultiEdit":
		call.Kind = ToolEdit
		var texts []string
		if edits, ok := fields["edits"].([]any); ok {
			for _, e := range edits {
				if m, ok := e.(map[string]any); ok {
					if s, ok := m["new_string"].(string); ok {
						texts = append(texts, s)
					}
				}
			}
		}
		b.edit(str("file_path"), strings.Join(texts, "\n"), st)
	case t == "NotebookEdit":
		call.Kind = ToolEdit
		b.edit(str("notebook_path"), str("new_source"), st)
	case t == "Glob":
		call.Kind = ToolSearch
		dir := str("path")
		if dir == "" {
			dir = "."
		}
		b.toolPath(dir, List, st)
		if pat := str("pattern"); pat != "" {
			b.toolGlob(dir, pat, st)
		}
	case t == "Grep" || t == "LS":
		call.Kind = ToolSearch
		dir := str("path")
		if dir == "" {
			dir = "."
		}
		b.toolPath(dir, Read, st)
	case t == "WebFetch":
		call.Kind = ToolWeb
		if u := str("url"); u != "" {
			b.addURL(nil, parseURL(u))
		}
	case t == "WebSearch" || t == "web_search" || t == "web_search_preview":
		call.Kind = ToolWeb
	case t == "Task" || t == "Agent":
		call.Kind = ToolOther
		if p := str("prompt"); p != "" {
			call.Contents = append(call.Contents, Content{Scope: ScopeMessage, Text: p})
		}
	case t == "apply_patch":
		call.Kind = ToolEdit
		patch := firstOf(str("input"), str("patch"), str("content"))
		if patch == "" {
			patch = argvPatch(fields["command"])
		}
		b.patch(patch, st)
	case t == "shell" || t == "local_shell" || t == "exec_command" || t == "container.exec" ||
		t == "unified_exec" || t == "shell_command":
		b.codexShell(fields, st)
	case t == "write_stdin":
		// Typing into a running command: what is typed runs as commands.
		b.shell(platformShell(env.OS), str("chars"), st)
	case strings.HasPrefix(t, "mcp__") || strings.Contains(t, "/") && !strings.Contains(t, " "):
		call.Kind = ToolMCP
		b.mcp(fields, st)
	case harmless[t]:
		call.Kind = ToolOther
	default:
		call.Kind = ToolOther
		call.Meta.UnknownTool = true
		b.mcp(fields, st)
	}
	return call
}

// MaxInput is the most tool input the parser reads.
const MaxInput = 1 << 20

// Shell works out the facts of shell code in a dialect, as if a shell tool
// ran it in cwd (the workspace when empty).
func Shell(dialect, code, cwd string, env Env) *Call {
	if cwd == "" {
		cwd = env.Workspace
	}
	call := &Call{Tool: "shell", OS: env.OS, Cwd: cwd}
	b := &builder{env: env, call: call}
	b.shell(dialect, code, &sh{cwd: env.clean(slash(cwd)), vars: map[string]*string{}})
	return call
}

// Output works out the facts of a tool's output, for the content rules.
func Output(text string, env Env) *Call {
	return &Call{Tool: "output", Kind: ToolOther, OS: env.OS, Contents: []Content{{Scope: ScopeOutput, Text: text}}}
}

func (b *builder) shell(dialect, code string, st *sh) {
	b.call.Kind, b.call.Dialect, b.call.Input = ToolShell, dialect, code
	b.call.Inputs = append(b.call.Inputs, code)
	st.dialect = dialect
	if strings.TrimSpace(code) == "" {
		return
	}
	b.parse(dialect, code, st)
}

func platformShell(os string) string {
	if os == "windows" {
		return PowerShell
	}
	return Bash
}

// codexShell reads a Codex command: an argv list (["bash", "-lc", "..."],
// ["powershell.exe", "-Command", "..."], ["git", "status"]) or one string
// for the platform's shell.
func (b *builder) codexShell(fields map[string]any, st *sh) {
	if wd, ok := fields["workdir"].(string); ok && wd != "" {
		st.cwd = b.env.join(st.cwd, slash(wd))
	}
	raw := fields["command"]
	if raw == nil {
		raw = fields["cmd"]
	}
	switch v := raw.(type) {
	case string:
		b.shell(platformShell(b.env.OS), v, st)
	case []any:
		var argv []string
		for _, a := range v {
			if s, ok := a.(string); ok {
				argv = append(argv, s)
			}
		}
		b.argv(argv, st)
	default:
		b.call.Kind = ToolShell
		b.parseError("a shell call with no command")
	}
}

// argv reports a command given as an argument list: no shell parses it,
// unless its program is one and it carries code.
func (b *builder) argv(argv []string, st *sh) {
	b.call.Kind = ToolShell
	b.call.Input = strings.Join(argv, " ")
	b.call.Inputs = append(b.call.Inputs, b.call.Input)
	if len(argv) == 0 {
		b.parseError("an empty command")
		return
	}
	prog, _ := programName(argv[0], "")
	switch {
	case shells[prog] && len(argv) >= 3 && strings.HasPrefix(argv[1], "-") && strings.Contains(argv[1], "c"):
		b.call.Dialect = Bash
	case prog == "powershell" || prog == "pwsh":
		b.call.Dialect = PowerShell
	case prog == "cmd":
		b.call.Dialect = Cmd
	}
	if prog == "apply_patch" && len(argv) >= 2 {
		b.call.Kind = ToolEdit
		b.patch(argv[1], st)
		return
	}
	st.dialect = Bash
	words := make([]word, len(argv))
	for i, a := range argv {
		words[i] = lit(a)
		words[i].quoted = strings.ContainsAny(a, " \t")
	}
	b.command(&rawCmd{words: words, raw: b.call.Input}, st, nil)
}

func argvPatch(v any) string {
	list, ok := v.([]any)
	if !ok || len(list) < 2 {
		return ""
	}
	if s, ok := list[0].(string); !ok || !strings.HasSuffix(s, "apply_patch") {
		return ""
	}
	s, _ := list[1].(string)
	return s
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// toolPath records the path a file tool names.
func (b *builder) toolPath(p string, acc Access, st *sh) {
	if p == "" {
		return
	}
	w := lit(p)
	w.glob = false
	b.path(nil, w, acc, st)
}

func (b *builder) toolGlob(dir, pattern string, st *sh) {
	full := pattern
	if !b.env.isAbs(slash(pattern)) {
		full = strings.TrimSuffix(slash(dir), "/") + "/" + pattern
	}
	w := lit(full)
	w.glob = true
	b.path(nil, w, List, st)
}

// edit records a file tool's write and the text it writes.
func (b *builder) edit(p, text string, st *sh) {
	if p == "" {
		b.parseError("an edit names no file")
		return
	}
	b.toolPath(p, Write, st)
	last := b.call.Paths[len(b.call.Paths)-1]
	b.call.Contents = append(b.call.Contents, Content{Scope: ScopeEdit, Path: last.Path, Text: text})
}

// patch reads an apply_patch patch: the files it adds, updates, deletes
// and moves, and the lines it adds.
func (b *builder) patch(text string, st *sh) {
	if !strings.Contains(text, "*** Begin Patch") {
		b.parseError("apply_patch: no patch")
		return
	}
	var cur string
	var added []string
	flush := func() {
		if cur != "" && len(added) > 0 {
			b.call.Contents = append(b.call.Contents, Content{Scope: ScopeEdit, Path: cur, Text: strings.Join(added, "\n")})
		}
		added = nil
	}
	ok := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "*** Add File: "), strings.HasPrefix(line, "*** Update File: "):
			flush()
			name := strings.TrimSpace(line[strings.Index(line, ":")+1:])
			b.toolPath(name, Write, st)
			cur = b.call.Paths[len(b.call.Paths)-1].Path
		case strings.HasPrefix(line, "*** Delete File: "):
			flush()
			cur = ""
			b.toolPath(strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: ")), Delete, st)
		case strings.HasPrefix(line, "*** Move to: "):
			if n := len(b.call.Paths); n > 0 {
				b.call.Paths[n-1].Access = Delete
			}
			flush()
			b.toolPath(strings.TrimSpace(strings.TrimPrefix(line, "*** Move to: ")), Write, st)
			cur = b.call.Paths[len(b.call.Paths)-1].Path
		case strings.HasPrefix(line, "*** End Patch"):
			ok = true
		case strings.HasPrefix(line, "+") && cur != "":
			added = append(added, line[1:])
		}
	}
	flush()
	if !ok {
		b.parseError("apply_patch: the patch does not end")
	}
}

var (
	looksURL     = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://\S+$`)
	looksCommand = regexp.MustCompile(`(?i)^\s*(sudo|rm|curl|wget|bash|sh|powershell|pwsh|cmd|python3?|node|git|chmod|del|Remove-Item|iex|Invoke-Expression)\b.*\s`)
)

// mcp walks an MCP tool's (or an unknown tool's) arguments for strings
// that look like paths, URLs or shell commands.
func (b *builder) mcp(fields map[string]any, st *sh) {
	var walk func(v any, depth int)
	walk = func(v any, depth int) {
		if depth > 8 {
			return
		}
		switch x := v.(type) {
		case string:
			switch {
			case looksURL.MatchString(strings.TrimSpace(x)):
				b.addURL(nil, parseURL(strings.TrimSpace(x)))
			case looksCommand.MatchString(x):
				b.call.Inputs = append(b.call.Inputs, x)
				b.nested(platformShell(b.env.OS), x, st, false)
			case !strings.ContainsAny(x, "\n") && len(x) < 1024 && (b.env.isAbs(slash(x)) || strings.HasPrefix(x, "~/")):
				b.toolPath(x, Read, st)
			}
		case []any:
			for _, e := range x {
				walk(e, depth+1)
			}
		case map[string]any:
			for _, e := range x {
				walk(e, depth+1)
			}
		}
	}
	walk(fields, 0)
}
