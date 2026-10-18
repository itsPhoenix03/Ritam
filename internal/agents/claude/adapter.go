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

// editTools are what a write-enabled agent may always use.
var editTools = []string{"Read", "Grep", "Glob", "Edit", "Write", "MultiEdit", "NotebookEdit"}

// writeModeTools is editTools plus what the grant lets the agent run: a
// Bash rule per allowed command, or Bash itself at the any-command level
// (the deny rules still refuse the destructive commands: a deny wins).
func writeModeTools(g core.Grant) []string {
	tools := append([]string(nil), editTools...)
	if g.AnyCommand {
		return append(tools, "Bash")
	}
	for _, c := range g.Commands {
		tools = append(tools, "Bash("+c+":*)")
	}
	return tools
}

// RunsInSandbox reports whether the CLI runs the agent's commands in a
// sandbox. Claude Code's commands run directly on the machine here.
func (a *ClaudeCodeAdapter) RunsInSandbox() bool { return false }

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

// deniedInWriteMode refuses every destructive command, in Bash and in
// PowerShell, and the PowerShell tool itself (allowed commands run in Bash).
func deniedInWriteMode() []string {
	out := []string{"PowerShell"}
	for _, c := range core.DangerousCommands {
		out = append(out, "Bash("+c+":*)")
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
		}
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
	if sess, ok := a.sessions.Get(key); ok && sess.Kind == domain.AgentKindClaudeCode && sess.ID != "" {
		return sess, true
	}
	sess := core.NewSession(key, domain.AgentKindClaudeCode, core.NewSessionUUID(),
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
func (a *ClaudeCodeAdapter) buildArgsFor(g core.Grant) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
	}

	if a.cfg.AllowWrites {
		// File edits are accepted (Claude Code keeps them inside the working
		// directory, which is the workspace). Shell commands run only as the
		// access level allows -- none, by name, or any -- and destructive
		// ones are denied outright: a deny rule wins over an allow rule.
		args = append(args,
			"--permission-mode", "acceptEdits",
			"--allowedTools", strings.Join(writeModeTools(g), ","),
			"--disallowedTools", strings.Join(deniedInWriteMode(), ","),
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
	return append(args, a.cfg.Args...)
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
