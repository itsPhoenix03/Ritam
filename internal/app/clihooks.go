package app

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/review"
	"ritam/internal/review/facts"
)

// The human's own Claude Code hooks (review.cliHooks). ritam starts Claude
// Code with --setting-sources "", which keeps the user's settings, hooks
// included, out of the agents' turns. With the switch on, the hooks in the
// user's settings file come back:
//
//	on      every event's hooks run natively in the CLI, except the pre-tool
//	        hooks: the review runs those itself, with the CLI's own payload,
//	        and takes their deny and ask but never their allow or a
//	        rewritten input, which would get a call past ritam's review.
//	native  every hook runs natively in the CLI, after ritam's own pre-tool
//	        hook; a pre-tool hook's allow or rewrite then gets past ritam.
//
// Codex's hooks are not brought back yet: they live in TOML (or a separate
// hooks.json), and Codex could not be run here to test how they merge.

// cliHooks is what review.cliHooks brings back of the human's hooks.
type cliHooks struct {
	// native are the hooks to run in the CLI, by event.
	native map[string][]any
	// review are the pre-tool hooks the review runs itself.
	review []review.Hook
	// names list what came back, for the settings tab.
	names []string
	// err says why the user's settings could not be read.
	err error
}

// claudeSettingsFile is the user's Claude Code settings file.
func claudeSettingsFile() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// loadCLIHooks reads the user's Claude Code hooks for the given mode.
func loadCLIHooks(mode, file, dir string) cliHooks {
	var out cliHooks
	if mode == "" || mode == "off" || file == "" {
		return out
	}
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		out.err = err
		return out
	}
	var s struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		out.err = fmt.Errorf("%s: %w", file, err)
		return out
	}
	out.native = map[string][]any{}
	for event, groups := range s.Hooks {
		if event == "PreToolUse" && mode == "on" {
			for i, g := range groups {
				hs, names := preToolHooks(g, i, dir)
				out.review = append(out.review, hs...)
				out.names = append(out.names, names...)
			}
			continue
		}
		out.native[event] = groups
		out.names = append(out.names, fmt.Sprintf("%s (%d, in the CLI)", event, len(groups)))
	}
	return out
}

// preToolHooks turns one PreToolUse group of Claude Code's settings into
// hooks the review runs: its matcher becomes the tool filter, each command
// a hook with the CLI's own payload. Hooks of other types (prompt) cannot
// be run outside the CLI and are left out.
func preToolHooks(group any, i int, dir string) ([]review.Hook, []string) {
	g, ok := group.(map[string]any)
	if !ok {
		return nil, nil
	}
	matcher, _ := g["matcher"].(string)
	var re *regexp.Regexp
	if matcher != "" && matcher != "*" {
		re, _ = regexp.Compile("^(?:" + matcher + ")$")
	}
	list, _ := g["hooks"].([]any)
	var hooks []review.Hook
	var names []string
	for j, item := range list {
		h, ok := item.(map[string]any)
		if !ok || h["type"] != "command" {
			continue
		}
		command, _ := h["command"].(string)
		if strings.TrimSpace(command) == "" {
			continue
		}
		run := shellLine(command)
		if args, ok := h["args"].([]any); ok {
			run = []string{command}
			for _, a := range args {
				if s, ok := a.(string); ok {
					run = append(run, s)
				}
			}
		}
		timeout := config.MaxHookTimeout
		if secs, ok := h["timeout"].(float64); ok && secs > 0 && time.Duration(secs)*time.Second < timeout {
			timeout = time.Duration(secs) * time.Second
		}
		name := fmt.Sprintf("claude:%d.%d", i+1, j+1)
		names = append(names, fmt.Sprintf("%s %s (PreToolUse, by the review)", name, clipText(command, 60)))
		matchRe := re
		hooks = append(hooks, review.Hook{Name: name, Run: run, Native: true, Timeout: timeout, Dir: dir,
			// Claude Code treats a hook that fails as no objection.
			Block: h["onFailure"] == "block",
			Wants: func(tool string, _ facts.ToolKind, cli domain.AgentKind) bool {
				return cli == domain.AgentKindClaudeCode && (matchRe == nil || matchRe.MatchString(tool))
			}})
	}
	return hooks, names
}

// shellLine runs a hook's command line the way Claude Code would: through
// bash (Git Bash on Windows), else sh, else cmd.
func shellLine(command string) []string {
	for _, sh := range []string{"bash", "sh"} {
		if p, err := exec.LookPath(sh); err == nil {
			return []string{p, "-c", command}
		}
	}
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/C", command}
	}
	return []string{"sh", "-c", command}
}

func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}
