package config

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// ReviewConfig says how the agents' tool calls are approved. Every call goes
// through ritam's review (internal/review) before it runs, whatever the mode;
// the mode decides who approves what the rules let through.
type ReviewConfig struct {
	// Mode is auto or manual. Auto: ritam's rules decide; a harmful call is
	// refused, a suspicious one is asked about, the rest run. Manual: on top
	// of that, every command waits for the human's approval. Rules can miss
	// a destructive command, so auto is not a guarantee.
	Mode string `yaml:"mode"`
	// AskTimeout is how long a call waits for the human's answer before it
	// is refused.
	AskTimeout time.Duration `yaml:"askTimeout"`
	// Rules are the human's own additions to ritam's rules. They run no
	// code and can only make the review stricter.
	Rules ReviewRules `yaml:"rules"`
	// Hooks are programs of the human's own that the review runs on every
	// tool call. They can refuse a call or put it to the human, never
	// approve one.
	Hooks []ReviewHook `yaml:"hooks"`
}

// ReviewRules are patterns added to ritam's rules. A command pattern is
// read like a command: "git push --force" matches any git command whose
// subcommand starts with push and that has the --force flag, however it
// is written (git -C repo push --force origin). A protect pattern is a
// path glob, relative to the workspace unless absolute.
type ReviewRules struct {
	// Deny refuses the commands matching these patterns.
	Deny []string `yaml:"deny"`
	// Ask puts the commands matching these to the human.
	Ask []string `yaml:"ask"`
	// Protect refuses changing, deleting or linking these paths.
	Protect []string `yaml:"protect"`
}

// ReviewHook is one program the review runs on tool calls.
type ReviewHook struct {
	// Name says which hook refused a call.
	Name string `yaml:"name"`
	// Run is the program and its arguments; no shell is involved.
	Run []string `yaml:"run"`
	// Tools limits the hook to these tools, by name (Bash, Write) or kind
	// (shell, edit, read, search, web, mcp); empty: every tool.
	Tools []string `yaml:"tools"`
	// Agents limits it to these CLIs (claude-code, codex); empty: both.
	Agents []string `yaml:"agents"`
	// Input is ritam (the same JSON for every CLI: the call and what ritam
	// found in it) or native (the CLI's own hook payload, for scripts
	// written for that CLI's hooks).
	Input string `yaml:"input"`
	// Timeout is how long the hook may take [10s, at most 15s].
	Timeout time.Duration `yaml:"timeout"`
	// OnFailure is block (a hook that fails or times out refuses the
	// call; the default) or ignore.
	OnFailure string `yaml:"onFailure"`
}

const (
	// ReviewAuto lets ritam's rules decide.
	ReviewAuto = "auto"
	// ReviewManual asks the human before every command.
	ReviewManual = "manual"
	// DefaultAskTimeout is how long a call waits for the human by default.
	DefaultAskTimeout = 5 * time.Minute
	// MaxAskTimeout bounds the wait: the agent's CLI waits for the hook for
	// as long, and the turn's own clock keeps running meanwhile.
	MaxAskTimeout = 30 * time.Minute
	minAskTimeout = 10 * time.Second
	// DefaultHookTimeout and MaxHookTimeout bound a hook of the human's:
	// the agent's CLI waits for all of them, and for the human after.
	DefaultHookTimeout = 10 * time.Second
	MaxHookTimeout     = 15 * time.Second
)

// AutoModeWarning is what the human is told about auto mode wherever it can
// be chosen.
const AutoModeWarning = "auto: ritam's rules decide, asking you only about suspicious calls. " +
	"Rules cannot foresee everything, so a destructive command can still get through; " +
	"choose manual to approve every command yourself."

// Manual reports whether every command waits for the human.
func (r ReviewConfig) Manual() bool { return r.Mode == ReviewManual }

// Wait is how long a call waits for the human, the default when unset.
func (r ReviewConfig) Wait() time.Duration {
	if r.AskTimeout <= 0 {
		return DefaultAskTimeout
	}
	return r.AskTimeout
}

// Wait is how long the hook may take, the default when unset.
func (h ReviewHook) Wait() time.Duration {
	if h.Timeout <= 0 {
		return DefaultHookTimeout
	}
	return h.Timeout
}

// Blocks reports whether a failing hook refuses the call.
func (h ReviewHook) Blocks() bool { return h.OnFailure != "ignore" }

// Native reports whether the hook gets the CLI's own payload.
func (h ReviewHook) Native() bool { return h.Input == "native" }

// Same reports whether two hooks are the same entry.
func (h ReviewHook) Same(o ReviewHook) bool {
	return h.Name == o.Name && slices.Equal(h.Run, o.Run) && slices.Equal(h.Tools, o.Tools) &&
		slices.Equal(h.Agents, o.Agents) && h.Input == o.Input && h.Timeout == o.Timeout && h.OnFailure == o.OnFailure
}

func (r ReviewConfig) validate() []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf("config: "+format, a...)) }
	if r.Mode != ReviewAuto && r.Mode != ReviewManual {
		bad("review.mode must be %s or %s, got %q", ReviewAuto, ReviewManual, r.Mode)
	}
	if r.AskTimeout != 0 && (r.AskTimeout < minAskTimeout || r.AskTimeout > MaxAskTimeout) {
		bad("review.askTimeout must be between %s and %s, got %s", minAskTimeout, MaxAskTimeout, r.AskTimeout)
	}
	for _, list := range []struct {
		name string
		v    []string
	}{{"deny", r.Rules.Deny}, {"ask", r.Rules.Ask}, {"protect", r.Rules.Protect}} {
		for _, p := range list.v {
			if strings.TrimSpace(p) == "" {
				bad("review.rules.%s has an empty pattern", list.name)
			}
		}
	}
	names := map[string]bool{}
	for i, h := range r.Hooks {
		where := fmt.Sprintf("review.hooks[%d]", i)
		switch {
		case strings.TrimSpace(h.Name) == "":
			bad("%s needs a name", where)
		case names[h.Name]:
			bad("%s: the name %q is used twice", where, h.Name)
		}
		names[h.Name] = true
		if len(h.Run) == 0 || strings.TrimSpace(h.Run[0]) == "" {
			bad("%s (%s) needs run: the program and its arguments", where, h.Name)
		}
		if h.Input != "" && h.Input != "ritam" && h.Input != "native" {
			bad("%s (%s): input must be ritam or native, got %q", where, h.Name, h.Input)
		}
		if h.OnFailure != "" && h.OnFailure != "block" && h.OnFailure != "ignore" {
			bad("%s (%s): onFailure must be block or ignore, got %q", where, h.Name, h.OnFailure)
		}
		if h.Timeout < 0 || h.Timeout > MaxHookTimeout {
			bad("%s (%s): timeout must be at most %s, got %s", where, h.Name, MaxHookTimeout, h.Timeout)
		}
		for _, a := range h.Agents {
			if a != "claude-code" && a != "codex" {
				bad("%s (%s): agents are claude-code or codex, got %q", where, h.Name, a)
			}
		}
	}
	return errs
}

// SaveReviewMode writes the review mode into the settings file at path,
// creating it if needed. Only review.mode changes: the rest of the review
// section, and of the file, is kept as it was.
func SaveReviewMode(path, mode string) error {
	return saveKey(path, "review", "mode", mode,
		"How the agents' tool calls are approved: auto (ritam's rules decide and ask you about suspicious "+
			"calls) or manual (you approve every command). Set from the settings tab.", "save review mode")
}

// unionStrings is a then b's entries a lacks.
func unionStrings(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, v := range b {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
