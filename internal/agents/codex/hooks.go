package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Codex's hooks, from its documentation (learn.chatgpt.com/docs/hooks) and
// codex-cli 0.166.0's own config check. NOT YET RUN LIVE: no Codex account
// was available when this was written. What was checked: `-c
// hooks.PreToolUse=[...]` is read as the hooks setting when its strings are
// TOML literal strings, and `codex exec` and `codex exec resume` both take
// --dangerously-bypass-hook-trust. What was not: which shell runs the
// command, that the hook inherits the CLI's environment, and what Codex does
// when a hook fails (its documentation names no onFailure setting, and says
// a failed hook can let the tool run). The turn's check that its calls went
// through the hook is what will show any of this going wrong.

// UseHooks has every tool call the agent makes go through ritam's hook
// first (see hookArgs), and records each call against its turn.
func (a *CodexAdapter) UseHooks(h ports.HookMonitor) { a.hooks = h }

// hookArgs installs ritam's hook before every tool call Codex makes: shell
// commands, apply_patch and MCP tools (not web searches, which run on
// OpenAI's side). It goes in as a config override, which wins over the
// user's config.toml.
//
// A hook that is not managed by an administrator runs only once a person has
// reviewed and trusted it in Codex's own UI, which a headless turn has no
// way to do; --dangerously-bypass-hook-trust runs it without. That flag
// covers every enabled hook, so it is withheld when the workspace defines
// hooks of its own (projectHooks): those would run unreviewed, outside the
// sandbox. The agent's turn then runs with ritam's hook skipped, and the
// turn's check reports that no call went through it. The guard also takes
// away a .codex/hooks.json an agent writes, after its turn.
func (a *CodexAdapter) hookArgs() []string {
	if a.hooks == nil {
		return nil
	}
	cmd := a.hooks.HookCommand(domain.AgentKindCodex)
	args := []string{"-c", fmt.Sprintf(`hooks.PreToolUse=[{matcher="*",hooks=[{type="command",command=%s,timeout=%d}]}]`,
		tomlString(hookCommandLine(cmd, runtime.GOOS)), cmd.TimeoutSeconds)}
	if p := projectHooks(a.cfg.WorkingDir, a.cfg.WorkspaceRoot); p != "" {
		a.log.ForAgent(a.id).Warn("ritam's hook will not run for this Codex turn: the workspace defines hooks of "+
			"its own, which running hooks without Codex's trust review would run too", "file", p)
		return args
	}
	return append(args, "--dangerously-bypass-hook-trust")
}

// hookCommandLine is the hook as the one command line Codex takes: its
// documentation shows no way to give a program and its arguments apart, and
// its examples rely on a shell. The program's path is quoted for the
// platform's shell; ritam's own arguments are plain words.
func hookCommandLine(cmd ports.HookCommand, goos string) string {
	program := cmd.Program
	if goos == "windows" {
		if strings.ContainsAny(program, " &()[]{}^=;!'+,`~%") {
			program = `"` + program + `"`
		}
	} else {
		program = "'" + strings.ReplaceAll(program, "'", `'\''`) + "'"
	}
	return strings.Join(append([]string{program}, cmd.Args...), " ")
}

// tomlString writes s as a TOML string: a literal string, which keeps a
// Windows path's backslashes as they are, unless s holds what a literal
// string cannot.
func tomlString(s string) string {
	if !strings.ContainsAny(s, "'\r\n") {
		return "'" + s + "'"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", `\r`, "\n", `\n`).Replace(s) + `"`
}

// tomlHooks finds a hooks setting in a config.toml: a [hooks] or
// [[hooks.X]] table, or a hooks key.
var tomlHooks = regexp.MustCompile(`(?m)^\s*(\[+\s*hooks\b|hooks\s*[.=])`)

// projectHooks returns a file in which a workspace folder defines Codex
// hooks of its own -- .codex/hooks.json, or hooks in .codex/config.toml --
// or "" when none of dirs does.
func projectHooks(dirs ...string) string {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if p := filepath.Join(d, ".codex", "hooks.json"); fileExists(p) {
			return p
		}
		p := filepath.Join(d, ".codex", "config.toml")
		if data, err := os.ReadFile(p); err == nil && tomlHooks.Match(data) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
