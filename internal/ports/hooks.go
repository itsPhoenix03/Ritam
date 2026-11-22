package ports

import "ritam/internal/domain"

// HookMonitor is ritam's side of the agent CLIs' tool hooks. Before a CLI runs
// a tool (a shell command, a file edit, a fetch), it runs ritam's hook, which
// asks the monitor about the call. An adapter that supports hooks writes the
// hook into its CLI's settings (HookCommand) and wraps each turn in a
// HookTurn, so a call can be traced to its task and agent.
type HookMonitor interface {
	// HookCommand is how a CLI of the given kind runs ritam's hook before a
	// tool call.
	HookCommand(kind domain.AgentKind) HookCommand
	// BeginTurn opens a turn: the hook calls the CLI makes until End carry
	// its credentials (HookTurn.Env) and are reviewed and recorded against
	// it.
	BeginTurn(scope TurnScope) HookTurn
}

// TurnScope is what the review needs to know about a turn.
type TurnScope struct {
	Task  domain.TaskID
	Agent domain.AgentID
	Kind  domain.AgentKind
	// BuildDir is the agent's own build folder, relative to the workspace
	// (build/<agent>), "" when it builds nowhere.
	BuildDir string
}

// HookCommand is the program a CLI runs as its pre-tool hook.
type HookCommand struct {
	// Program is the absolute path of ritam's own executable, so no lookup on
	// PATH (where an agent could put a program of the same name) is involved.
	Program string
	// Args follow Program.
	Args []string
	// TimeoutSeconds is how long the CLI waits for the hook. The hook gives up
	// sooner, so the CLI never times out a hook that is still deciding.
	TimeoutSeconds int
	// Native are the human's own hooks for this CLI, by event, in its own
	// settings form, to run natively beside ritam's (review.cliHooks).
	Native map[string][]any
}

// HookTurn is one CLI invocation as the hook monitor sees it.
type HookTurn interface {
	// Env is what the CLI process needs in its environment for its hook to
	// reach the monitor. The hook inherits it from the CLI.
	Env() map[string]string
	// End closes the turn: its credentials stop working, and what the CLI
	// reported doing is compared with what the hook saw, so a hook that did
	// not run is noticed. Pass a zero ToolCalls when the turn did not finish
	// (killed, failed): a cut-off turn may report calls it never made.
	End(reported ToolCalls)
}

// ToolCalls is what a CLI's own output said it ran in a turn.
type ToolCalls struct {
	// Calls are the tool calls by id, for a CLI whose output uses the same
	// ids as its hook (Claude Code's tool_use ids): each must have reached
	// the hook.
	Calls []ToolCall
	// Count is the number of tool calls, for a CLI whose output does not
	// share ids with its hook; it is used when Calls is empty.
	Count int
}

// ToolCall is one tool call a CLI reported.
type ToolCall struct {
	ID   string
	Tool string
}

// Total is how many tool calls were reported.
func (t ToolCalls) Total() int {
	if len(t.Calls) > 0 {
		return len(t.Calls)
	}
	return t.Count
}
