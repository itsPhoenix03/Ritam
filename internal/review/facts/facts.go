// Package facts works out what an agent's tool call would do, without
// running anything: which programs it starts and with which arguments, which
// files it reads, writes or deletes and where they are, which hosts it
// contacts, and what text it writes or runs. The rules in package rules are
// checked against these facts.
//
// A shell command is parsed, not split on spaces: bash with mvdan.cc/sh,
// PowerShell and cmd with small parsers of their own. Wrappers (sudo, env,
// xargs, find -exec, bash -c, ...) are unwrapped, so the command they run is
// reported as well as the wrapper. Values only known at run time -- a
// variable from the environment, the output of $(...) -- are marked as such
// rather than guessed.
//
// The parser fails closed: what it cannot parse, or that is too large or
// too deeply nested to parse, is reported in Meta, and the rules treat that
// as suspicious. It never decides that a call is harmless.
//
// The design follows command-parsing.md in the review plan.
package facts

import "strings"

// ToolKind sorts tools by what they do.
type ToolKind string

const (
	ToolShell  ToolKind = "shell"
	ToolRead   ToolKind = "read"
	ToolEdit   ToolKind = "edit"
	ToolSearch ToolKind = "search"
	ToolWeb    ToolKind = "web"
	ToolMCP    ToolKind = "mcp"
	ToolOther  ToolKind = "other"
)

// Dialects of shell code.
const (
	Bash       = "bash"
	PowerShell = "powershell"
	Cmd        = "cmd"
)

// Access is what a call does to a path.
type Access string

const (
	Read   Access = "read"
	List   Access = "list"
	Write  Access = "write"
	Delete Access = "delete"
	Exec   Access = "exec"
	// Link is the target of a link or bind mount being made.
	Link Access = "link"
	// Meta is a change of permissions, owner or attributes.
	Meta Access = "meta"
)

// Zone is where a path is, as far as the review is concerned.
type Zone string

const (
	ZoneNull      Zone = "null"
	ZoneDevice    Zone = "device"
	ZoneRegistry  Zone = "registry"
	ZoneUNC       Zone = "unc"
	ZoneRitam     Zone = "ritam"
	ZoneProtected Zone = "protected"
	ZoneBuild     Zone = "build"
	ZoneWorkspace Zone = "workspace"
	ZoneTemp      Zone = "temp"
	ZoneHome      Zone = "home"
	ZoneSystem    Zone = "system"
	ZoneOutside   Zone = "outside"
	ZoneUnknown   Zone = "unknown"
)

// Content scopes: where a piece of text to scan came from.
const (
	ScopeEdit    = "edit"
	ScopeInline  = "inline"
	ScopeHeredoc = "heredoc"
	ScopeDecoded = "decoded"
	ScopeOutput  = "output"
	ScopeMessage = "message"
)

// Call is everything the parser found in one tool call.
type Call struct {
	Provider string
	// Tool is the CLI's name for the tool; Kind sorts it.
	Tool string
	Kind ToolKind
	// Dialect is the shell dialect of a shell call, "" otherwise.
	Dialect string
	OS      string
	Cwd     string
	// Input is the raw command text of a shell call.
	Input string
	// Inputs are Input plus any shell code nested inside it (bash -c ...),
	// for the rules that look at the raw text.
	Inputs   []string
	Commands []*Command
	// Paths are every path the call touches, its commands' included.
	Paths    []Path
	URLs     []URL
	Contents []Content
	Meta     MetaFacts
}

// MetaFacts are the parser's findings about the call itself.
type MetaFacts struct {
	ParseError    bool
	LimitExceeded bool
	UnknownTool   bool
	Notes         []string
}

// Command is one simple command: a program with its arguments.
type Command struct {
	// Program is the normalized name: the basename without its extension,
	// lowercased, with aliases resolved (PowerShell's ri is remove-item).
	// .NET calls are namespace.type::method. Empty for a command that only
	// sets variables.
	Program string
	// ProgramRaw is the program word as written.
	ProgramRaw string
	// ProgramPath is the folder the program was named with, if any.
	ProgramPath string
	// Indirect: the name came from a variable, a substitution or string
	// building. Dynamic: it could not be worked out at all.
	Indirect, Dynamic bool
	Args              []Arg
	Flags             []Flag
	// Subcommand is the non-flag words, flag values skipped.
	Subcommand string
	Assigns    []Assign
	Paths      []Path
	URLs       []URL
	// PipeFrom and PipeTo are the programs on either side in a pipeline.
	PipeFrom, PipeTo string
	// ArgFrom are the programs whose output becomes part of an argument.
	ArgFrom []string
	// WrappedBy are the wrappers removed to reach this command (sudo, env).
	WrappedBy []string
	Inline    *Inline
	Raw       string
	Dialect   string
	Depth     int
	// Remote: the command runs on another machine (ssh host cmd), so its
	// paths are not this machine's.
	Remote bool
}

// Argline is the argument values joined by single spaces, a value only
// known at run time written as its source text.
func (c *Command) Argline() string {
	parts := make([]string, len(c.Args))
	for i, a := range c.Args {
		if a.Dynamic {
			parts[i] = a.Raw
		} else {
			parts[i] = a.Value
		}
	}
	return strings.Join(parts, " ")
}

// Arg is one argument after the program.
type Arg struct {
	Value, Raw      string
	Dynamic, Quoted bool
}

// Flag is one normalized flag: -r, --force, /s, -recurse.
type Flag struct {
	Name, Value string
}

// Assign is an environment variable set (or, with an empty value and
// Unset, removed).
type Assign struct {
	Name, Value string
	Dynamic     bool
}

// Inline is code a command carries: bash -c '...', python -c '...'.
type Inline struct {
	Lang string
	Code string
	// Dynamic: the code is only known at run time (piped in, a variable).
	Dynamic bool
}

// Path is one path a call touches.
type Path struct {
	// Path is the resolved path, with forward slashes. Registry keys are
	// HKLM/..., HKCU/..., HKU/..., HKCR/....
	Path string
	// Raw is the path as written.
	Raw    string
	Access Access
	Zone   Zone
	// Glob: a wildcard pattern, its zone that of its fixed prefix.
	Glob bool
	// Dynamic: part of the path is only known at run time.
	Dynamic bool
	Tool    ToolKind
}

// URL is a URL or remote host a call contacts.
type URL struct {
	Raw, Scheme, Host, Path string
}

// Content is text to scan: an edit's new text, inline code, a here-doc.
type Content struct {
	Scope string
	// Path is the file written, for an edit.
	Path string
	Text string
}
