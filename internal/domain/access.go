package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// AccessLevel is how much a workspace lets its agents do beyond editing
// files. It is chosen by the human, per workspace, and the zero value is the
// safe one: files only.
//
// Whatever the level, some commands stay blocked (see AlwaysBlocked), and an
// agent configured read-only stays read-only.
type AccessLevel int

const (
	// AccessFilesOnly: edit files, run read-only commands. The default.
	AccessFilesOnly AccessLevel = iota
	// AccessSandboxedBuilder: only agents whose CLI runs commands in a
	// sandbox may build and test, with the listed commands; the others ask
	// them.
	AccessSandboxedBuilder
	// AccessBuildAndTest: every agent may run the listed commands.
	AccessBuildAndTest
	// AccessAnyCommand: any command inside the workspace except the
	// always-blocked ones.
	AccessAnyCommand
)

// AccessLevels lists every level, lowest first.
var AccessLevels = []AccessLevel{AccessFilesOnly, AccessSandboxedBuilder, AccessBuildAndTest, AccessAnyCommand}

// Valid reports whether l is a known level.
func (l AccessLevel) Valid() bool { return l >= AccessFilesOnly && l <= AccessAnyCommand }

// Name is the level's short name, as the header and the settings tab show it.
func (l AccessLevel) Name() string {
	switch l {
	case AccessFilesOnly:
		return "Files only"
	case AccessSandboxedBuilder:
		return "Sandboxed builder"
	case AccessBuildAndTest:
		return "Build and test"
	case AccessAnyCommand:
		return "Any command"
	}
	return fmt.Sprintf("Level %d", int(l))
}

// Summary says in one line what agents may do at this level.
func (l AccessLevel) Summary() string {
	switch l {
	case AccessFilesOnly:
		return "edit files, run read-only commands"
	case AccessSandboxedBuilder:
		return "only sandboxed agents may build and test; the others ask them"
	case AccessBuildAndTest:
		return "every agent may run the commands you list"
	case AccessAnyCommand:
		return "anything inside the workspace except the always-blocked list"
	}
	return "unknown level"
}

// UsesCommandList reports whether the level grants the listed commands to
// at least some agents.
func (l AccessLevel) UsesCommandList() bool {
	return l == AccessSandboxedBuilder || l == AccessBuildAndTest
}

// String renders the level as "2 (Build and test)".
func (l AccessLevel) String() string { return fmt.Sprintf("%d (%s)", int(l), l.Name()) }

// ParseAccessLevel reads a level from its number or its name ("2",
// "build", "build and test", "any").
func ParseAccessLevel(s string) (AccessLevel, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil {
		if l := AccessLevel(n); l.Valid() {
			return l, nil
		}
		return 0, fmt.Errorf("access level %d: use 0 to 3", n)
	}
	for _, l := range AccessLevels {
		name := strings.ToLower(l.Name())
		if s != "" && (s == name || strings.HasPrefix(name, s)) {
			return l, nil
		}
	}
	return 0, fmt.Errorf("unknown access level %q: use 0 to 3", s)
}

// AlwaysBlocked names what no agent may do at any level, whatever the
// settings say. The commands behind it are core.DangerousCommands.
var AlwaysBlocked = []string{
	"deleting files", "system settings", "permissions", "killing processes",
	"network access", "installing software", "rewriting git history",
}
