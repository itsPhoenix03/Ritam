// Package review is ritam's check on what agents do with their tools.
//
// The agent CLIs run a hook before every tool call: a shell command, a file
// edit, a fetch. The hook is ritam's own executable (`ritam hook pretool`),
// which hands the call over HTTP to the review server running inside ritam
// and turns the answer into what the CLI expects. The server knows which
// task and agent a call belongs to from the turn's token, which the CLI
// passes on to its hook in the environment.
//
// With a Policy, every call is decided: parsed into facts (package facts),
// checked against ritam's rules (package rules) and the turn's history, and
// refused, stopped, put to the human or passed (see Policy). Without one,
// calls are only recorded. Either way a passed call still meets the CLI's
// own permission rules, and a call is refused when it cannot be reviewed at
// all -- the hook cannot reach ritam, the call is unreadable or too large,
// or it comes from no running turn -- because a check that cannot happen
// must not let the call through (fail closed).
//
// After each turn the calls the CLI reported in its output are compared with
// the calls the hook saw, so a hook the CLI never ran is noticed: a CLI drops
// a settings file it cannot read without a word, for one.
package review

import (
	"time"

	"ritam/internal/brand"
)

const (
	// MaxPayload is the most the hook reads from the CLI, and the server from
	// the hook. A call bigger than this is refused, not cut short: a check of
	// part of a call says nothing about the rest.
	MaxPayload = 1 << 20

	// cliTimeoutSeconds is how long the CLI waits for the hook when calls
	// are only recorded.
	cliTimeoutSeconds = 30
	// hookTimeout is how long the hook waits for the server, beyond the
	// time the human has to answer: less than the CLI waits for the hook
	// (cliMargin more), so a slow answer is the hook's refusal rather than
	// the CLI's timeout, which may let the call run.
	hookTimeout = 20 * time.Second
	cliMargin   = 40
	// maxHookWait bounds what the hook accepts from its environment.
	maxHookWait = 40 * time.Minute

	// preTool is the hook event checked before a tool runs, as it appears in
	// the hook's arguments and the server's path.
	preTool = "pretool"
	// hookPath prefixes the server's hook endpoints.
	hookPath = "/v1/hook/"
)

// URLEnv names the environment variable holding the review server's address.
func URLEnv() string { return brand.Title() + "_HOOK_URL" }

// TokenEnv names the environment variable holding the turn's token.
func TokenEnv() string { return brand.Title() + "_HOOK_TOKEN" }

// WaitEnv names the environment variable holding how long, in seconds, the
// hook waits for the server: long enough for the human to answer.
func WaitEnv() string { return brand.Title() + "_HOOK_WAIT" }

// Decision is the server's answer to a call.
type Decision string

const (
	// Pass means no objection: the CLI's own permission rules decide.
	Pass Decision = "pass"
	// Deny refuses the call; the reason is shown to the agent.
	Deny Decision = "deny"
)

// Verdict is the server's answer, as the hook receives it.
type Verdict struct {
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
}
