// Package claude contains the Claude Code adapter.
//
// It drives the real `claude` CLI in one-shot headless mode: one process per
// protocol message, `-p --output-format stream-json` with the prompt on stdin, read until exit, parse
// the JSON envelope, pull the `@@ORCH` reply header out of the result text.
//
// What is implemented and exercised against the real CLI: executable
// resolution, invocation, the read-only tool policy, JSON envelope parsing,
// prompt rendering and reply parsing.
//
// TODO(phase-next): still missing for a richer integration:
//   - Session continuity. Each message is an independent run, so Claude keeps
//     no memory between them; continuity comes from the orchestrator injecting
//     shared knowledge into each prompt. Using `--resume` with the session id
//     returned in the JSON envelope would preserve native context and cut
//     token use, at the cost of the isolation one-shot runs buy.
//   - Streaming. `--output-format stream-json` would let the TUI show progress
//     mid-turn instead of waiting for the process to exit.
//   - Write access. Enabled by config (agents.<id>.allowWrites), but the
//     IMPLEMENT flow has no patch review step yet, so writes should stay off
//     until one exists.
//   - Cost/token accounting. The envelope carries usage and total_cost_usd;
//     they are logged but not yet aggregated per task.
//
// Auth is the CLI's own (`claude /login`). This package never reads an API key
// and never accepts one from configuration.
package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ritam/internal/agents/core"
	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// defaultExecutable is looked up on PATH; it is never a hard-coded install
// path, since Claude Code may be installed anywhere (and is often a shim on
// PATH on Windows).
const defaultExecutable = "claude"

// readOnlyTools is the allowlist used when the agent may not modify anything.
var readOnlyTools = []string{"Read", "Grep", "Glob"}

// writeTools are denied explicitly alongside the allowlist. Belt and braces:
// an explicit deny cannot be misread, and in headless mode a tool that is
// merely "not allowed" would otherwise surface as a permission prompt with
// nobody present to answer it.
var writeTools = []string{"Bash", "Edit", "Write", "MultiEdit", "NotebookEdit"}

// writeModeTools is what a write-enabled agent is allowed outright: the
// read tools, and what the grant lets it run -- a Bash rule per allowed
// command, or Bash itself at the any-command level (the deny rules still
// refuse the destructive commands: a deny wins).
//
// The edit tools are deliberately not in the list. acceptEdits accepts an
// edit inside the working folder without asking, and asks for one anywhere
// else, which in headless mode means it is refused. An allow rule for Edit
// or Write would accept edits anywhere, and with no sandbox (native
// Windows) nothing else keeps them in the workspace.
func writeModeTools(g core.Grant) []string {
	tools := append([]string(nil), readOnlyTools...)
	if g.AnyCommand {
		return append(tools, "Bash")
	}
	for _, c := range g.Commands {
		tools = append(tools, "Bash("+c+":*)")
	}
	return tools
}

// RunsInSandbox reports whether the CLI runs the agent's commands in a
// sandbox. Every invocation turns Claude Code's own sandbox on (see
// settings), but it exists only on macOS, Linux and WSL2: on native Windows
// the commands run directly on the machine. Where it is declared, the canary
// check still verifies it before the agent may build.
func (a *ClaudeCodeAdapter) RunsInSandbox() bool { return runtime.GOOS != "windows" }

// UseAccess points the adapter at the workspace's access policy, read on
// every turn.
func (a *ClaudeCodeAdapter) UseAccess(acc *core.Access) { a.access = acc }

// CanaryArgs is how the sandbox check runs this CLI: as a builder at the
// any-command level, the most a builder is ever allowed.
func (a *ClaudeCodeAdapter) CanaryArgs(string) []string {
	return a.buildArgsFor(core.Grant{Level: domain.AccessAnyCommand, AnyCommand: true})
}

// grant is what the agent may run on task t under the current policy.
func (a *ClaudeCodeAdapter) grant(t *domain.Task) core.Grant {
	return a.access.GrantFor(t, a.id, a.cfg.AllowedCommands)
}

// deniedInWriteMode refuses every refused command (core.RefusedCommands) in
// Bash, and the PowerShell tool itself (allowed commands run in Bash). A
// pattern with a "*" keeps it: Claude Code reads one as any text, so "find *
// -delete" denies find with -delete anywhere after it.
//
// Claude Code matches the words as written, where IsDangerousCommand first
// drops git's global options: so the options that point git at another
// folder or program are refused outright here, or "git -C . push" would get
// past "git push".
func deniedInWriteMode() []string {
	out := []string{"PowerShell", "Bash(git -C:*)", "Bash(git --git-dir:*)", "Bash(git --work-tree:*)",
		"Bash(git --exec-path:*)"}
	for _, c := range core.RefusedCommands() {
		if strings.Contains(c, "*") {
			out = append(out, "Bash("+c+"*)")
		} else {
			out = append(out, "Bash("+c+":*)")
		}
	}
	return out
}

// ClaudeCodeAdapter normalises the Claude Code CLI into the internal protocol.
type ClaudeCodeAdapter struct {
	id     domain.AgentID
	cfg    config.AgentConfig
	log    *logging.Logger
	runner *core.OneShotRunner

	out chan ports.Outbound
	// activity carries live output for the UI. It is presentation only and
	// never influences routing or task state.
	activity *core.ActivityEmitter

	mu      sync.Mutex
	state   domain.AgentState
	stopped bool
	// task is remembered from the opening assignment so later messages can
	// still tell the agent who its fellow participants are.
	task *domain.Task
	// queue serialises invocations: one CLI process per agent at a time.
	queue *core.InvocationQueue
	// cancelAll kills every in-flight CLI invocation. Stop must actually stop
	// work: a call whose answer nobody will read still bills for its tokens.
	cancelAll context.CancelFunc
	// lifetime parents every invocation so cancelAll reaches all of them.
	lifetime context.Context
	// sessions maps (task, agent) to the Claude session holding this agent's
	// conversation, so each turn resumes it instead of starting from nothing.
	sessions *core.SessionRegistry
	// access is the workspace's access policy; nil means the agent's own
	// command list (see core.Access.GrantFor).
	access *core.Access
}

// New builds an adapter for id using cfg. When cfg.Executable is empty, the
// "claude" executable is resolved on PATH at Start time.
func New(id domain.AgentID, cfg config.AgentConfig, log *logging.Logger) *ClaudeCodeAdapter {
	if log == nil {
		log = logging.NewDiscard()
	}
	exe := cfg.Executable
	if exe == "" {
		exe = defaultExecutable
	}
	timeout := cfg.ResponseTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	a := &ClaudeCodeAdapter{
		id:  id,
		cfg: cfg,
		log: log,
		runner: &core.OneShotRunner{
			AgentID:    id,
			Executable: exe,
			WorkingDir: cfg.WorkingDir,
			Timeout:    timeout,
		},
		out:      make(chan ports.Outbound, 64),
		activity: core.NewActivityEmitter(id, 512),
		state:    domain.AgentStateStopped,
		// An adapter that has never been started must refuse to send. The
		// zero value of a bool is false, so this has to be set explicitly:
		// otherwise a never-started adapter would spawn a process.
		stopped: true,
	}
	// Until UseSessions supplies the shared, persistent registry, sessions
	// are tracked in memory: they still resume within a run, but not across
	// restarts.
	a.sessions, _ = core.OpenSessionRegistry("")
	// WORKING while anything is running or queued; IDLE only once it drains.
	a.queue = core.NewInvocationQueue(func(busy bool) {
		if busy {
			a.setState(domain.AgentStateWorking)
		} else {
			a.setState(domain.AgentStateIdle)
		}
	})
	return a
}

// UseGuard has ritam's own files checked after every CLI invocation, and
// put back if the agent changed them (see core.FileGuard).
func (a *ClaudeCodeAdapter) UseGuard(g *core.FileGuard) { a.runner.Guard = g }

// UseSessions points the adapter at the shared, persistent session registry.
func (a *ClaudeCodeAdapter) UseSessions(r *core.SessionRegistry) {
	if r != nil {
		a.sessions = r
	}
}

// ID returns the agent's identity.
func (a *ClaudeCodeAdapter) ID() domain.AgentID { return a.id }

// Kind reports this adapter's backing kind.
func (a *ClaudeCodeAdapter) Kind() domain.AgentKind { return domain.AgentKindClaudeCode }

// Start verifies the CLI is actually installed. Nothing is spawned yet: each
// message spawns its own process. Resolving here means a missing CLI is
// reported when the task starts rather than halfway through a deliberation.
func (a *ClaudeCodeAdapter) Start(_ context.Context) error {
	path, err := a.runner.Resolve()
	if err != nil {
		a.setState(domain.AgentStateUnavailable)
		return fmt.Errorf("claude-code adapter: %w", err)
	}
	a.mu.Lock()
	a.stopped = false
	a.lifetime, a.cancelAll = context.WithCancel(context.Background())
	a.mu.Unlock()
	a.queue.Reopen()
	a.activity.Reopen()
	a.setState(domain.AgentStateIdle)
	a.log.ForAgent(a.id).Info("claude-code adapter ready", "executable", path,
		"allowWrites", a.cfg.AllowWrites)
	return nil
}

// Stop marks the adapter stopped and kills any in-flight CLI invocation.
//
// Killing rather than waiting is deliberate. Once a task is over — completed,
// cancelled or failed — an answer still being generated will never be read,
// but it keeps billing tokens until it finishes. Waiting politely for it also
// stalled shutdown for as long as responseTimeout allows, which was observed
// holding a finished task open for 15 seconds while two agents talked to
// nobody.
func (a *ClaudeCodeAdapter) Stop(_ context.Context) error {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.stopped = true
	cancel := a.cancelAll
	a.mu.Unlock()

	// Drop queued invocations first so none starts once the running one dies.
	a.queue.Close()
	if cancel != nil {
		cancel()
	}
	a.queue.Wait()
	a.activity.Close()
	a.setState(domain.AgentStateStopped)
	return nil
}

// SendTask hands the agent its opening assignment and remembers the task.
func (a *ClaudeCodeAdapter) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	a.mu.Lock()
	a.task = t.Clone()
	a.mu.Unlock()
	return a.dispatch(ctx, m)
}

// SendMessage delivers a routed protocol message to the agent, refreshing the
// cached task. A supporting agent is never given an opening assignment, so its
// first delivery is typically a DELEGATE — this is where it learns the task.
func (a *ClaudeCodeAdapter) SendMessage(ctx context.Context, t *domain.Task, m domain.Message) error {
	if t != nil {
		a.mu.Lock()
		a.task = t.Clone()
		a.mu.Unlock()
	}
	return a.dispatch(ctx, m)
}

// dispatch queues the invocation and returns immediately.
//
// Returning without waiting keeps the orchestrator's run loop free: it stays
// on its select, so its idle timer and the human's cancel both keep working
// while a CLI call that may take minutes is in flight. A message that arrives
// while the agent is busy waits its turn in the queue rather than running a
// second CLI process beside the first.
func (a *ClaudeCodeAdapter) dispatch(ctx context.Context, m domain.Message) error {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return fmt.Errorf("claude-code adapter: agent %s is not running", a.id)
	}
	task := a.task
	lifetime := a.lifetime
	a.mu.Unlock()

	if !a.queue.Enqueue(func() {
		// The invocation is parented on the adapter's lifetime so Stop kills
		// it, and separately bound to the caller's context so a cancelled task
		// does too. Whichever fires first takes the child process with it.
		invokeCtx, cancelInvoke := context.WithCancel(lifetime)
		defer cancelInvoke()
		defer context.AfterFunc(ctx, cancelInvoke)()
		if invokeCtx.Err() != nil {
			return // cancelled while queued: never start the process
		}
		a.invoke(invokeCtx, task, m)
	}) {
		return fmt.Errorf("claude-code adapter: agent %s is not running", a.id)
	}
	return nil
}

// invoke runs one CLI call and publishes whatever it produced, streaming the
// agent's output as activity while it works.
func (a *ClaudeCodeAdapter) invoke(ctx context.Context, t *domain.Task, m domain.Message) {
	if t != nil {
		a.activity.SetTask(t.ID)
	}
	a.activity.Emit(ports.ActivityStatus, core.WorkingOn(m))

	sess, resuming := a.openSession(t)
	res, streamed, streamErr, err := a.runTurn(ctx, t, m, sess, resuming)

	if err != nil && resuming && isMissingSession(err) {
		// The CLI no longer has this conversation (deleted, or recorded
		// from another directory). Start a new one and re-brief the agent
		// from the orchestrator's own record of the task.
		a.log.ForAgent(a.id).Warn("claude session not found; starting a new one",
			"session", sess.ID, "workingDir", sess.WorkingDir)
		a.activity.Emit(ports.ActivityStatus, "previous session lost; re-briefing in a new one")
		sess.Replace(core.NewSessionUUID(), core.SessionRebuilt)
		a.saveSession(sess)
		res, streamed, streamErr, err = a.runTurn(ctx, t, m, sess, false)
	}

	// Spending is reported whatever the turn's outcome: a failed turn still
	// cost what it cost. It is reported last, after the turn's messages: a
	// turn that crosses the budget pauses the task, and the reply already
	// paid for has to reach it first.
	//
	// Claude reports total_cost_usd for the whole session -- it restores
	// the running total when a session is resumed -- so the turn's cost is
	// how far that total grew. The token counts are the turn's own.
	if res != nil {
		if u := parseUsage(res.Stdout); u != nil {
			u.CostUSD = sess.CostSince(u.CostUSD)
			a.saveSession(sess)
			defer a.emit(ports.Outbound{AgentID: a.id, Usage: u})
		} else if u := partialUsage(res.Stdout); u != nil {
			// Killed before the result: its tokens, cost unknown. The
			// session's running cost is left as it was, so the next turn's
			// result is not charged twice.
			defer a.emit(ports.Outbound{AgentID: a.id, Usage: u})
		}
	}

	if errors.Is(streamErr, errBudgetStopped) {
		// Not a failure, though the CLI exits non-zero: the turn reached the
		// task's spending limit (budgetArgs). The usage reported on return
		// pauses the task for the human.
		a.log.ForAgent(a.id).Warn("claude-code turn stopped at the task's spending limit")
		a.activity.Emit(ports.ActivityStatus, "stopped: the task's spending limit was reached")
		return
	}

	if err != nil {
		if errors.Is(err, core.ErrCancelled) {
			// The task ended or the adapter stopped; nobody is waiting for
			// this answer, so it is neither a failure nor an outbound error.
			a.activity.Emit(ports.ActivityStatus, "cancelled")
			return
		}
		a.fail(fmt.Errorf("claude-code adapter: %w", err))
		return
	}

	a.recordTurn(sess, res.Stdout)

	// Prefer the result carried by the stream; fall back to scanning the whole
	// output, which also covers a CLI that ignored the streaming flags.
	text, err := streamed, streamErr
	if text == "" && err == nil {
		text, err = extractResult(res.Stdout)
	}
	if err != nil {
		a.log.ForAgent(a.id).Error("claude-code output not understood",
			"error", err, "stdout", logging.Redact(truncate(res.Stdout, 400)))
		a.activity.Emit(ports.ActivityStatus, "reply not understood")
		a.emit(ports.Outbound{AgentID: a.id, Err: err})
		return
	}

	a.log.ForAgent(a.id).Debug("claude-code reply",
		"duration", res.Duration.String(), "text", logging.Redact(truncate(text, 400)))

	if strings.TrimSpace(text) == "" {
		// The turn finished and the agent chose to say nothing -- right when
		// it was told no reply is needed. That is not a failure.
		a.activity.Emit(ports.ActivityStatus, "nothing to reply")
		return
	}
	msgs, err := parseOutput(text)
	if err != nil {
		a.fail(err)
		return
	}
	if len(msgs) == 0 {
		// The CLI answered but not in the protocol. Report it as an error so
		// the orchestrator surfaces a stuck agent instead of waiting out its
		// idle timeout in silence.
		a.fail(fmt.Errorf(
			"claude-code adapter: agent %s replied without an @@ORCH header", a.id))
		return
	}
	for _, msg := range msgs {
		a.emit(ports.Outbound{AgentID: a.id, Message: msg})
	}
}

// openSession returns the session this turn should use, and whether it is a
// resumption. A first turn gets a new session whose id the orchestrator
// chooses -- Claude accepts one with --session-id -- and it is recorded before
// the CLI runs, so a turn cancelled half-way through still leaves the agent
// pointed at whatever the CLI managed to save.
func (a *ClaudeCodeAdapter) openSession(t *domain.Task) (core.Session, bool) {
	key := core.SessionKeyFor(t, a.id)
	sess, ok := a.sessions.Get(key)
	if ok && sess.Kind == domain.AgentKindClaudeCode && sess.ID != "" {
		if sess.ResumableIn(core.WorkspaceRoot(a.cfg)) {
			return sess, true
		}
		a.log.ForAgent(a.id).Warn("claude session was made outside the workspace; starting a new one",
			"session", sess.ID, "workingDir", sess.WorkingDir)
		a.activity.Emit(ports.ActivityStatus, "previous session is outside the workspace; re-briefing in a new one")
		sess.Replace(core.NewSessionUUID(), core.SessionRebuilt)
		sess.WorkingDir = core.ResolveWorkingDir(a.cfg.WorkingDir)
		a.saveSession(sess)
		return sess, false
	}
	sess = core.NewSession(key, domain.AgentKindClaudeCode, core.NewSessionUUID(),
		core.ResolveWorkingDir(a.cfg.WorkingDir), a.cfg.Model)
	a.saveSession(sess)
	return sess, false
}

// runTurn sends one message in sess: the full brief when the session is new,
// the short standing brief when it is being resumed.
func (a *ClaudeCodeAdapter) runTurn(ctx context.Context, t *domain.Task, m domain.Message, sess core.Session, resuming bool) (*core.OneShotResult, string, error, error) {
	prompt := renderPrompt(t, m, a.cfg.AllowWrites)
	if resuming {
		prompt = core.RenderTurn(t, m, a.cfg.AllowWrites, headerMarker)
	}
	g := a.grant(t)
	if a.cfg.AllowWrites {
		prompt += core.WriteRules(core.ResolveWorkingDir(a.cfg.WorkingDir), g)
	}
	args := append(a.buildArgsFor(g), a.sessionArgs(t, sess, resuming)...)
	args = append(args, budgetArgs(t)...)

	// Claude files sessions by working directory and cannot resume one from
	// anywhere else, so every turn runs where the session was created.
	runner := *a.runner
	if sess.WorkingDir != "" {
		runner.WorkingDir = sess.WorkingDir
	}

	var (
		streamed  string
		streamErr error
	)
	res, err := runner.RunStreaming(ctx, args, prompt, func(line string) {
		result, done, lineErr := consumeStreamLine(line, a.activity)
		if done {
			streamed, streamErr = result, lineErr
		}
	})
	a.activity.Flush()
	return res, streamed, streamErr, err
}

// sessionArgs selects the session: --session-id to create it on the first
// turn (named so a human browsing Claude's own /resume list can tell what it
// is), --resume to continue it afterwards. Never --continue: "the most recent
// session" may be someone else's.
func (a *ClaudeCodeAdapter) sessionArgs(t *domain.Task, sess core.Session, resuming bool) []string {
	if resuming {
		return []string{"--resume", sess.ID}
	}
	name := "ritam " + string(a.id)
	if t != nil {
		name = fmt.Sprintf("ritam %s %s", t.ID, a.id)
	}
	return []string{"--session-id", sess.ID, "-n", name}
}

// recordTurn notes a completed turn. If the CLI reports a different session
// id than the one requested (a future version forking on resume, say), the
// reported one becomes current, so the next turn continues what Claude
// actually saved.
func (a *ClaudeCodeAdapter) recordTurn(sess core.Session, stdout string) {
	if reported := reportedSessionID(stdout); reported != "" && reported != sess.ID {
		a.log.ForAgent(a.id).Info("claude reported a different session id; following it",
			"requested", sess.ID, "reported", reported)
		kept := sess
		sess.Replace(reported, sess.State)
		sess.Turns = kept.Turns
		// The CLI carries the running cost over into the session it
		// continued in, so the baseline carries over too.
		sess.ReportedCostUSD = kept.ReportedCostUSD
	}
	sess.Turns++
	sess.LastUsed = time.Now()
	sess.Model = a.cfg.Model
	a.saveSession(sess)
}

func (a *ClaudeCodeAdapter) saveSession(sess core.Session) {
	if err := a.sessions.Save(sess); err != nil {
		// Not fatal to the turn, but the agent will not find this session
		// next time, so it must be visible.
		a.log.ForAgent(a.id).Error("saving agent session", "error", err)
	}
}

// isMissingSession reports whether a failed resume means the CLI could not
// find the conversation, as opposed to any other failure.
func isMissingSession(err error) bool {
	return strings.Contains(err.Error(), "No conversation found")
}

// reportedSessionID returns the session_id the CLI's final result envelope
// carries, or "".
func reportedSessionID(stdout string) string {
	lines := strings.Split(stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, "{") || !strings.Contains(l, `"session_id"`) {
			continue
		}
		var env claudeEnvelope
		if json.Unmarshal([]byte(l), &env) == nil && env.SessionID != "" {
			return env.SessionID
		}
	}
	return ""
}

// buildArgs assembles the CLI invocation. The prompt is not included: it is
// written to the process's stdin, which print mode (-p with no positional
// prompt) reads.
//
// It used to be passed as an argument, which broke twice over: a NUL byte
// anywhere in it (say, typed into a task title) made the process fail to start
// with "invalid argument", and Windows caps a command line at 32,767
// characters while prompts grow with the conversation. Stdin has neither limit.
//
// stream-json is used rather than plain json so the UI can show the agent
// working; --verbose is required by the CLI alongside it, and
// --include-partial-messages is what turns whole messages into token deltas.
func (a *ClaudeCodeAdapter) buildArgs() []string { return a.buildArgsFor(a.grant(nil)) }

// buildArgsFor is buildArgs under a given grant.
//
// The configured args go first and ritam's own flags after them, so a later
// flag of ritam's wins any conflict; the configured ones are filtered as
// well (safeExtraArgs). --verbose leads ritam's flags: should a configured
// flag at the end be waiting for a value, it is the flag swallowed, which
// fails the turn visibly instead of loosening anything.
func (a *ClaudeCodeAdapter) buildArgsFor(g core.Grant) []string {
	args := append(a.safeExtraArgs(),
		"--verbose",
		"-p",
		"--output-format", "stream-json",
		"--include-partial-messages",
		// Only ritam's settings and MCP servers: none from the user's or the
		// project's settings files, whose allow rules, hooks and servers
		// would widen what the grant lets the agent do.
		"--setting-sources", "",
		"--strict-mcp-config",
	)
	if settings := a.settingsArg(); settings != "" {
		args = append(args, "--settings", settings)
	}

	if a.cfg.AllowWrites {
		// File edits inside the working directory, which is the workspace,
		// are accepted without asking; anywhere else they are refused (see
		// writeModeTools), and so are edits to ritam's own files and the
		// other protected paths. Shell commands run only as the access level
		// allows -- none, by name, or any -- and refused ones are denied
		// outright: a deny rule wins over an allow rule.
		args = append(args,
			"--permission-mode", "acceptEdits",
			"--allowedTools", strings.Join(writeModeTools(g), ","),
			"--disallowedTools", strings.Join(append(deniedInWriteMode(), protectedRules(a.cfg.Protected)...), ","),
		)
	} else {
		args = append(args,
			"--allowedTools", strings.Join(readOnlyTools, ","),
			"--disallowedTools", strings.Join(writeTools, ","),
			"--permission-mode", "dontAsk",
		)
	}
	if a.cfg.Model != "" {
		args = append(args, "--model", a.cfg.Model)
	}
	return args
}

// budgetStopped is the result subtype of a turn the CLI stopped at
// --max-budget-usd, and errBudgetStopped what the adapter makes of it.
const budgetStopped = "error_max_budget_usd"

var errBudgetStopped = errors.New("claude-code adapter: turn stopped at the spending limit")

// budgetArgs caps one turn's spending at what is left of the task's budget.
// The orchestrator checks the budget between turns; without this, one turn
// -- minutes of a model calling tools -- could run far past it before the
// check. The CLI stops a turn once a call takes it over, so a task ends at
// most one API call over its budget per agent at work.
//
// The CLI counts the cap per invocation, not over the resumed session, so
// what is left of the task's budget is the right figure for every turn.
func budgetArgs(t *domain.Task) []string {
	if t == nil || t.Limits.MaxCostUSD <= 0 {
		return nil
	}
	left := max(t.Limits.MaxCostUSD-t.CostUSD, 0.01)
	return []string{"--max-budget-usd", strconv.FormatFloat(left, 'f', 4, 64)}
}

// protectedRules deny the file tools ritam's own files and folders: its
// settings, database, sessions and team. An Edit rule covers every tool that
// edits files. Without them an agent could rewrite the settings ritam starts
// its agents with next time, or the folder a session resumes in.
func protectedRules(paths []string) []string {
	var out []string
	for _, p := range paths {
		r := rulePath(p)
		out = append(out, "Edit("+r+")", "Edit("+r+"/**)")
	}
	return out
}

// rulePath writes an absolute path the way Claude Code's permission rules
// take one: "//" then the path in POSIX form, with a Windows drive letter as
// a lower-case first folder (D:\proj becomes //d/proj).
func rulePath(p string) string {
	p = filepath.ToSlash(filepath.Clean(p))
	if len(p) >= 2 && p[1] == ':' {
		return "//" + strings.ToLower(p[:1]) + p[2:]
	}
	return "/" + p
}

// settings is what ritam sets through --settings: Claude Code's sandbox for
// the agent's commands where the platform has one, and the file tools held
// to the working folder when reads are confined.
func (a *ClaudeCodeAdapter) settings() map[string]any {
	out := map[string]any{}
	if a.RunsInSandbox() {
		out["sandbox"] = map[string]any{
			"enabled": true,
			// Bash still needs ritam's allow rules: being sandboxed does not
			// stand in for the grant.
			"autoAllowBashIfSandboxed": false,
			// A command the sandbox refuses is refused, not retried outside.
			"allowUnsandboxedCommands": false,
			// No network: no domain is allowed, and none can be asked for.
			"network": map[string]any{"allowedDomains": []string{}, "strictAllowlist": true},
		}
	}
	if a.cfg.ConfineReads {
		out["permissions"] = map[string]any{"blockReadsOutsideWorkingDirectories": true}
	}
	return out
}

// settingsArg is the --settings value: a file in the workspace's scratch
// folder, rewritten when it changes, or "" when there is nothing to set.
//
// A file rather than inline JSON: on Windows an npm-installed CLI is a .cmd
// shim, and cmd.exe re-parses the command line, quotes included. Without a
// scratch folder (tests) the JSON is passed inline.
func (a *ClaudeCodeAdapter) settingsArg() string {
	s := a.settings()
	if len(s) == 0 {
		return ""
	}
	data, _ := json.Marshal(s)
	if a.cfg.ScratchDir == "" {
		return string(data)
	}
	path := filepath.Join(a.cfg.ScratchDir, "claude-"+string(a.id)+".settings.json")
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return path
	}
	err := os.MkdirAll(a.cfg.ScratchDir, 0o700)
	if err == nil {
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			if err = os.Rename(tmp, path); err == nil {
				return path
			}
			_ = os.Remove(tmp)
		}
	}
	a.log.ForAgent(a.id).Warn("writing the agent's settings file; passing them inline", "error", err)
	return string(data)
}

// allowedFlags are the only flags configured args may pass: the model,
// how hard it thinks, and extra instructions. Everything else is refused --
// flags that change the permissions, tools, settings, MCP servers, session
// or output ritam parses, and any the CLI adds later -- whatever their
// order: a later flag wins, so appending ritam's own is not by itself a
// defence. Compared without regard to case, --flag and --flag=value alike.
var allowedFlags = []string{"--effort", "--model", "--fallback-model", "--append-system-prompt", "--betas"}

// safeExtraArgs keeps the configured arguments ritam allows (see
// allowedFlags), the same way the Codex adapter does. Short flags are
// dropped: they combine (-cp is -c -p). Bare positional arguments are
// dropped, since Claude would read one as the prompt -- the orphaned value
// of a refused flag included.
func (a *ClaudeCodeAdapter) safeExtraArgs() []string {
	args := a.cfg.Args
	out := make([]string, 0, len(args))
	takesValue := func(i int, hasInline bool) bool {
		return !hasInline && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-")
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			a.log.ForAgent(a.id).Warn(
				"ignoring configured positional argument: Claude would read it as the prompt", "argument", arg)
			continue
		}
		name, _, hasInline := strings.Cut(arg, "=")
		if !slices.Contains(allowedFlags, strings.ToLower(name)) {
			a.log.ForAgent(a.id).Warn(
				"ignoring configured argument: only the model, its effort and extra instructions may be configured",
				"argument", name)
			if takesValue(i, hasInline) {
				i++ // drop its value too
			}
			continue
		}
		out = append(out, arg)
		if takesValue(i, hasInline) {
			i++
			out = append(out, args[i])
		}
	}
	return out
}

// Interrupt is a no-op for one-shot invocations: cancelling the context passed
// to Send kills the child process, which is the real interrupt.
func (a *ClaudeCodeAdapter) Interrupt(_ context.Context) error { return nil }

// Status returns the adapter's current lifecycle state.
func (a *ClaudeCodeAdapter) Status() domain.AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// Outbound yields protocol messages parsed from the CLI's output.
func (a *ClaudeCodeAdapter) Outbound() <-chan ports.Outbound { return a.out }

// Activity yields the agent's output as it is generated.
func (a *ClaudeCodeAdapter) Activity() <-chan ports.AgentActivity { return a.activity.Channel() }

func (a *ClaudeCodeAdapter) setState(s domain.AgentState) {
	a.mu.Lock()
	a.state = s
	a.mu.Unlock()
}

// fail reports a genuine invocation failure.
//
// The adapter logs it itself at ERROR, so a failure that lands after the
// orchestrator's run loop has exited (and nobody reads Outbound any more) still
// reaches the log. The orchestrator logs agent errors too, but only while it is
// listening; this is the copy that cannot be lost.
func (a *ClaudeCodeAdapter) fail(err error) {
	a.log.ForAgent(a.id).Error("claude-code invocation failed", "error", logging.Redact(err.Error()))
	a.activity.Emit(ports.ActivityStatus, "failed: "+err.Error())
	a.emit(ports.Outbound{AgentID: a.id, Err: err})
}

// emit publishes an outbound value, dropping it if the adapter has stopped so
// a late-finishing invocation cannot block forever on an unread channel.
func (a *ClaudeCodeAdapter) emit(o ports.Outbound) {
	a.mu.Lock()
	stopped := a.stopped
	a.mu.Unlock()
	if stopped {
		return
	}
	select {
	case a.out <- o:
	default:
		a.log.ForAgent(a.id).Warn("outbound buffer full; dropping agent message")
	}
}

// claudeEnvelope is the `--output-format json` result object. Only the fields
// the adapter actually uses are declared.
type claudeEnvelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
}

// extractResult pulls the assistant's text out of the JSON envelope.
//
// Claude Code prints one JSON object in this mode. Treating the output as
// untrusted matters: a CLI upgrade, a crash banner or an auth prompt can all
// put non-JSON on stdout, and that must be a clear error rather than being
// fed onward as if it were an agent's considered reply.
func extractResult(stdout string) (string, error) {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return "", fmt.Errorf("claude-code adapter: empty output (is the CLI logged in? try `claude /login`)")
	}

	var env claudeEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		// Streamed output is one JSON event per line; the result is the
		// last "result" event.
		found := false
		lines := strings.Split(trimmed, "\n")
		for i := len(lines) - 1; i >= 0 && !found; i-- {
			l := strings.TrimSpace(lines[i])
			if strings.HasPrefix(l, "{") && strings.Contains(l, `"result"`) &&
				json.Unmarshal([]byte(l), &env) == nil && env.Type == "result" {
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("claude-code adapter: output was not the expected JSON envelope: %w", err)
		}
	}
	if env.Subtype == budgetStopped {
		return "", errBudgetStopped
	}
	if env.IsError || env.Subtype == "error" {
		return "", fmt.Errorf("claude-code adapter: CLI reported an error: %s", truncate(env.Result, 300))
	}
	// A successful turn with no text is the agent saying nothing.
	return env.Result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// AcceptsBatches reports that several waiting messages may be combined into
// one turn: the agent reads them all in one prompt.
func (a *ClaudeCodeAdapter) AcceptsBatches() bool { return true }
