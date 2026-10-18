// Package codex contains the Codex CLI adapter.
//
// It drives the real `codex` CLI in one-shot headless mode:
// `codex exec -s <sandbox> --skip-git-repo-check --color never -o <file> <prompt>`,
// one process per protocol message. The agent's final message is read from the
// `-o` file rather than scraped from stdout, because stdout also carries the
// banner, reasoning summaries and progress output.
//
// This is a SEPARATE implementation from the Claude Code adapter on purpose.
// The two CLIs are different applications with different flags, different
// sandbox models and different output shapes; sharing an assumed protocol
// between them would be wrong the moment either changes.
//
// TODO(phase-next): still missing for a richer integration:
//   - Session continuity via `codex exec resume`, as with Claude's --resume.
//   - Streaming progress via `--json` (JSONL events) so the TUI can show work
//     in flight instead of waiting for process exit.
//   - Structured replies via `--output-schema`, which would remove the need to
//     parse a header out of free text entirely. This is the most promising
//     next step for this adapter specifically.
//   - Write access is config-gated (agents.<id>.allowWrites) and maps to the
//     workspace-write sandbox; it should stay off until IMPLEMENT has a patch
//     review step.
//
// Auth is the CLI's own (`codex login`). This package never reads an API key
// and never accepts one from configuration.
package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ritam/internal/agents/core"
	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// defaultExecutable is looked up on PATH; never a hard-coded install path. On
// Windows this typically resolves to a `codex.cmd` shim.
const defaultExecutable = "codex"

// CodexAdapter normalises the Codex CLI into the internal protocol.
type CodexAdapter struct {
	id     domain.AgentID
	cfg    config.AgentConfig
	log    *logging.Logger
	runner *core.OneShotRunner

	out chan ports.Outbound
	// activity carries live output for the UI. Presentation only: it never
	// influences routing or task state.
	activity *core.ActivityEmitter

	mu      sync.Mutex
	state   domain.AgentState
	stopped bool
	task    *domain.Task
	// queue serialises invocations: one CLI process per agent at a time.
	queue *core.InvocationQueue
	// cancelAll kills every in-flight CLI invocation. Stop must actually stop
	// work: a call whose answer nobody will read still bills for its tokens.
	cancelAll context.CancelFunc
	// lifetime parents every invocation so cancelAll reaches all of them.
	lifetime context.Context
	// sessions maps (task, agent) to the Codex thread holding this agent's
	// conversation, so each turn resumes it instead of starting from nothing.
	sessions *core.SessionRegistry
	// access is the workspace's access policy; nil means the agent's own
	// command list (see core.Access.GrantFor).
	access *core.Access
}

// New builds an adapter for id using cfg. When cfg.Executable is empty, the
// "codex" executable is resolved on PATH at Start time.
func New(id domain.AgentID, cfg config.AgentConfig, log *logging.Logger) *CodexAdapter {
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
	a := &CodexAdapter{
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
		// zero value of a bool is false, so this has to be set explicitly.
		stopped: true,
	}
	// Until UseSessions supplies the shared, persistent registry, threads are
	// tracked in memory: they resume within a run, but not across restarts.
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
func (a *CodexAdapter) UseSessions(r *core.SessionRegistry) {
	if r != nil {
		a.sessions = r
	}
}

// RunsInSandbox reports whether the CLI runs the agent's commands in a
// sandbox. Codex does: a write-enabled agent runs in workspace-write, which
// keeps writes inside the workspace, with the network and temp folders off
// (see writeConfinement).
func (a *CodexAdapter) RunsInSandbox() bool { return true }

// UseAccess points the adapter at the workspace's access policy, read on
// every turn. Codex's sandbox is the same at every level; what the level
// changes is what the brief allows the agent to run.
func (a *CodexAdapter) UseAccess(acc *core.Access) { a.access = acc }

// ID returns the agent's identity.
func (a *CodexAdapter) ID() domain.AgentID { return a.id }

// Kind reports this adapter's backing kind.
func (a *CodexAdapter) Kind() domain.AgentKind { return domain.AgentKindCodex }

// Start verifies the CLI is installed. Nothing is spawned until a message
// arrives; each message gets its own process.
func (a *CodexAdapter) Start(_ context.Context) error {
	path, err := a.runner.Resolve()
	if err != nil {
		a.setState(domain.AgentStateUnavailable)
		return fmt.Errorf("codex adapter: %w", err)
	}
	a.mu.Lock()
	a.stopped = false
	a.lifetime, a.cancelAll = context.WithCancel(context.Background())
	a.mu.Unlock()
	a.queue.Reopen()
	a.activity.Reopen()
	a.setState(domain.AgentStateIdle)
	a.log.ForAgent(a.id).Info("codex adapter ready", "executable", path,
		"allowWrites", a.cfg.AllowWrites)
	return nil
}

// Stop marks the adapter stopped and kills any in-flight CLI invocation.
//
// Killing rather than waiting is deliberate: once the task is over the answer
// will never be read, but it keeps billing tokens until it finishes, and
// waiting for it stalls shutdown for as long as responseTimeout allows.
func (a *CodexAdapter) Stop(_ context.Context) error {
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
func (a *CodexAdapter) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	a.mu.Lock()
	a.task = t.Clone()
	a.mu.Unlock()
	return a.dispatch(ctx, m)
}

// SendMessage delivers a routed protocol message to the agent, refreshing the
// cached task. A supporting agent is never given an opening assignment, so its
// first delivery is typically a DELEGATE — this is where it learns the task.
func (a *CodexAdapter) SendMessage(ctx context.Context, t *domain.Task, m domain.Message) error {
	if t != nil {
		a.mu.Lock()
		a.task = t.Clone()
		a.mu.Unlock()
	}
	return a.dispatch(ctx, m)
}

// dispatch queues the invocation and returns immediately, so the
// orchestrator's run loop keeps its idle timer and cancellation responsive
// during a long call. A message that arrives while the agent is busy waits its
// turn instead of running a second CLI process beside the first.
func (a *CodexAdapter) dispatch(ctx context.Context, m domain.Message) error {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return fmt.Errorf("codex adapter: agent %s is not running", a.id)
	}
	task := a.task
	lifetime := a.lifetime
	a.mu.Unlock()

	if !a.queue.Enqueue(func() {
		// Parented on the adapter's lifetime so Stop kills it, and bound to
		// the caller's context so a cancelled task does too.
		invokeCtx, cancelInvoke := context.WithCancel(lifetime)
		defer cancelInvoke()
		defer context.AfterFunc(ctx, cancelInvoke)()
		if invokeCtx.Err() != nil {
			return // cancelled while queued: never start the process
		}
		a.invoke(invokeCtx, task, m)
	}) {
		return fmt.Errorf("codex adapter: agent %s is not running", a.id)
	}
	return nil
}

// invoke runs one CLI call and publishes whatever it produced.
func (a *CodexAdapter) invoke(ctx context.Context, t *domain.Task, m domain.Message) {
	if t != nil {
		a.activity.SetTask(t.ID)
	}
	a.activity.Emit(ports.ActivityStatus, core.WorkingOn(m))

	key := core.SessionKeyFor(t, a.id)
	sess, resuming := a.sessions.Get(key)
	resuming = resuming && sess.Kind == domain.AgentKindCodex && sess.ID != ""

	res, reply, readErr, runErr, thread := a.runTurn(ctx, t, m, sess, resuming)

	if runErr != nil && resuming && isMissingSession(runErr, res) {
		// Codex no longer has this thread. Start a new one and re-brief the
		// agent from the orchestrator's own record of the task.
		a.log.ForAgent(a.id).Warn("codex thread not found; starting a new one", "thread", sess.ID)
		a.activity.Emit(ports.ActivityStatus, "previous session lost; re-briefing in a new one")
		resuming = false
		res, reply, readErr, runErr, thread = a.runTurn(ctx, t, m, sess, false)
		if thread != "" {
			sess.Replace(thread, core.SessionRebuilt)
		}
	} else if !resuming && thread != "" {
		sess = core.NewSession(key, domain.AgentKindCodex, thread,
			core.ResolveWorkingDir(a.cfg.WorkingDir), a.cfg.Model)
	}

	// Spending is reported whatever the turn's outcome: a failed turn still
	// cost what it cost. It is reported last, after the turn's messages: a
	// turn that crosses the budget pauses the task, and the reply already
	// paid for has to reach it first. The session saved below keeps the
	// thread totals the next turn's share is measured from.
	if res != nil {
		if tot := parseUsage(res.Stdout); tot != nil {
			defer a.emit(ports.Outbound{AgentID: a.id, Usage: turnUsage(&sess, *tot)})
		}
	}

	// Record the thread whenever Codex created one, even if the turn then
	// failed: the conversation exists on disk and the next turn should
	// continue it rather than start yet another.
	if sess.ID != "" {
		if runErr == nil {
			sess.Turns++
		}
		sess.LastUsed = time.Now()
		sess.Model = a.cfg.Model
		if err := a.sessions.Save(sess); err != nil {
			a.log.ForAgent(a.id).Error("saving agent session", "error", err)
		}
	}

	if errors.Is(runErr, core.ErrCancelled) {
		// The task ended or the adapter stopped; nobody is waiting for this
		// answer, so it is neither a failure nor an outbound error.
		a.activity.Emit(ports.ActivityStatus, "cancelled")
		return
	}
	if runErr != nil {
		// Prefer the CLI's own stderr message: "out of credits", "not logged
		// in" and sandbox refusals are all far more useful than an exit code.
		a.fail(fmt.Errorf("codex adapter: %w%s", runErr, stderrHint(res)))
		return
	}
	if readErr != nil {
		a.fail(fmt.Errorf(
			"codex adapter: agent %s produced no final message%s", a.id, stderrHint(res)))
		return
	}
	if strings.TrimSpace(reply) == "" {
		// The CLI finished cleanly and the agent chose to say nothing --
		// right when it was told no reply is needed. That is not a failure.
		a.activity.Emit(ports.ActivityStatus, "nothing to reply")
		return
	}

	a.log.ForAgent(a.id).Debug("codex reply",
		"duration", res.Duration.String(), "text", logging.Redact(truncate(reply, 400)))

	msgs, err := parseOutput(reply)
	if err != nil {
		a.fail(err)
		return
	}
	if len(msgs) == 0 {
		a.fail(fmt.Errorf(
			"codex adapter: agent %s replied without a %s header", a.id, headerMarker))
		return
	}
	for _, msg := range msgs {
		a.emit(ports.Outbound{AgentID: a.id, Message: msg})
	}
}

// runTurn sends one message: a new thread with the full brief, or a resumed
// one with the short standing brief. It returns the thread id Codex announced
// (only a new thread announces one).
//
// The prompt goes in on stdin, not as an argument. On Windows the `codex`
// executable on PATH is a .cmd shim, so Windows runs it through cmd.exe, which
// re-parses the command line and truncates a multi-line argument at its first
// newline. Codex reads the prompt from stdin when the prompt argument is `-`.
func (a *CodexAdapter) runTurn(ctx context.Context, t *domain.Task, m domain.Message, sess core.Session, resuming bool) (res *core.OneShotResult, reply string, readErr, runErr error, thread string) {
	lastMessage, cleanup, err := newLastMessageFile(a.cfg.ScratchDir, a.id)
	if err != nil {
		return nil, "", nil, fmt.Errorf("codex adapter: %w", err), ""
	}
	defer cleanup()

	runner := *a.runner
	prompt := renderPrompt(t, m, a.cfg.AllowWrites)
	args := a.buildArgs(lastMessage)
	if resuming {
		prompt = core.RenderTurn(t, m, a.cfg.AllowWrites, headerMarker)
		args = a.buildResumeArgs(sess.ID, lastMessage)
		// `exec resume` has no --cd, so the turn runs where the thread lives.
		if sess.WorkingDir != "" {
			runner.WorkingDir = sess.WorkingDir
		}
	}
	if a.cfg.AllowWrites {
		prompt += core.WriteRules(core.ResolveWorkingDir(a.cfg.WorkingDir), a.access.GrantFor(t, a.id, a.cfg.AllowedCommands))
	}

	res, runErr = runner.RunStreaming(ctx, args, prompt, func(line string) {
		if id, ok := threadStarted(line); ok {
			thread = id
		}
		consumeStreamLine(line, a.activity)
	})
	a.activity.Flush()

	text, readErr := os.ReadFile(lastMessage)
	return res, strings.TrimSpace(string(text)), readErr, runErr, thread
}

// isMissingSession reports whether a failed resume means Codex could not find
// the thread, as opposed to any other failure.
func isMissingSession(err error, res *core.OneShotResult) bool {
	if strings.Contains(err.Error(), "no rollout found") {
		return true
	}
	return res != nil && strings.Contains(res.Stderr, "no rollout found")
}

// sandboxEscapeFlags defeat the read-only sandbox or the approval policy. They
// are refused in configured Args regardless of ordering.
//
// This gap was found by a Codex agent auditing this very function through the
// orchestrator: extra args were appended after `--sandbox read-only`, so a
// config could append `--dangerously-bypass-approvals-and-sandbox` and a
// nominally read-only agent would run unsandboxed. A later flag wins, so
// placing the sandbox flag first is not by itself a defence.
var sandboxEscapeFlags = []string{
	"--dangerously-bypass-approvals-and-sandbox",
	"--dangerously-bypass-hook-trust",
	"--approve-for-me",
	"--sandbox",
	"-s",
	"--cd",
	"-C",
	"--output-last-message",
	"-o",
}

// safeExtraArgs drops configured arguments that would change the sandbox, the
// approval policy, the working root or the reply file. Everything the adapter
// relies on for containment is orchestrator-controlled, not config-controlled.
//
// Bare positional arguments are dropped too, whatever they are. Codex reads
// the first positional as the prompt, so a stray token — including the
// now-orphaned value of a flag that was just rejected — would silently replace
// the agent's brief with something from a config file.
func (a *CodexAdapter) safeExtraArgs() []string {
	args := a.cfg.Args
	out := make([]string, 0, len(args))

	// takesValue reports whether the flag at i has a separate value after it.
	takesValue := func(i int, hasInline bool) bool {
		return !hasInline && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-")
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			a.log.ForAgent(a.id).Warn(
				"ignoring configured positional argument: Codex would read it as the prompt",
				"argument", arg)
			continue
		}

		// Match `--flag` and `--flag=value` alike. Flags are compared
		// case-sensitively: Codex's `-c` (config override) and `-C` (change
		// directory) differ only in case, and a case-insensitive match once
		// silently dropped every configured `-c key=value`.
		name, inline, hasInline := strings.Cut(arg, "=")
		blocked := false
		for _, bad := range sandboxEscapeFlags {
			if name == bad {
				blocked = true
				break
			}
		}
		if blocked {
			a.log.ForAgent(a.id).Warn(
				"ignoring configured argument: it would override the orchestrator's sandbox or output settings",
				"argument", name)
			if takesValue(i, hasInline) {
				i++ // drop its value too
			}
			continue
		}

		// A config override can reach the same settings as the blocked
		// flags, so the setting it names is checked as well.
		if name == "-c" || name == "--config" {
			val := inline
			if !hasInline {
				if i+1 >= len(args) {
					continue
				}
				i++
				val = args[i]
			}
			if unsafeConfigOverride(val) {
				a.log.ForAgent(a.id).Warn(
					"ignoring configured override: it would change the orchestrator's sandbox or approval policy",
					"override", val)
				continue
			}
			out = append(out, "-c", val)
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

// unsafeConfigOverride reports whether a `-c key=value` override would change
// the sandbox or the approval policy, which the orchestrator alone decides.
func unsafeConfigOverride(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.HasPrefix(key, "sandbox") ||
		strings.HasPrefix(key, "approval") ||
		strings.HasPrefix(key, "dangerously")
}

// buildArgs assembles the CLI invocation. The prompt is not included: it is
// written to the process's stdin via the `-` positional argument.
func (a *CodexAdapter) buildArgs(lastMessageFile string) []string {
	sandbox := "read-only"
	if a.cfg.AllowWrites {
		sandbox = "workspace-write"
	}

	// Caller-supplied args go first so that the orchestrator's own flags are
	// appended last and win any conflict, on top of being filtered above.
	args := []string{"exec"}
	args = append(args, a.safeExtraArgs()...)
	if a.cfg.Model != "" {
		args = append(args, "--model", a.cfg.Model)
	}
	args = append(args,
		// JSONL events on stdout, so the UI can show work in flight. The
		// authoritative reply is still read from --output-last-message.
		"--json",
		"--sandbox", sandbox,
		// The orchestrator does not require the target to be a git repository,
		// and refuses to depend on one, so this check must be skipped.
		"--skip-git-repo-check",
		"--color", "never",
		"--output-last-message", lastMessageFile,
	)
	args = append(args, writeConfinement(a.cfg.AllowWrites)...)
	if a.cfg.WorkingDir != "" {
		args = append(args, "--cd", a.cfg.WorkingDir)
	}
	// `-` tells Codex to read the prompt from stdin.
	return append(args, "-")
}

// writeConfinement keeps a write-enabled Codex inside the workspace. Its
// workspace-write sandbox also lets commands write to temp folders and,
// depending on the user's config, reach the network; both are turned off,
// so the workspace is the only place anything is written.
func writeConfinement(allowWrites bool) []string {
	if !allowWrites {
		return nil
	}
	return []string{
		"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=true",
		"-c", "sandbox_workspace_write.exclude_slash_tmp=true",
		"-c", "sandbox_workspace_write.network_access=false",
	}
}

// buildResumeArgs assembles `codex exec resume` for an existing thread.
//
// `exec resume` accepts a different flag set from `exec`: there is no
// --sandbox, --cd or --color. The sandbox is therefore forced through a config
// override instead, on every resume, and placed after any configured
// overrides so it wins. This was checked live: with the override, a resumed
// turn asked to write a file could not.
func (a *CodexAdapter) buildResumeArgs(thread, lastMessageFile string) []string {
	sandbox := "read-only"
	if a.cfg.AllowWrites {
		sandbox = "workspace-write"
	}
	args := []string{"exec", "resume"}
	args = append(args, a.resumeSafeExtraArgs()...)
	if a.cfg.Model != "" {
		args = append(args, "--model", a.cfg.Model)
	}
	args = append(args,
		"--json",
		"--skip-git-repo-check",
		"-c", fmt.Sprintf("sandbox_mode=%q", sandbox),
	)
	args = append(args, writeConfinement(a.cfg.AllowWrites)...)
	return append(args,
		"--output-last-message", lastMessageFile,
		thread,
		// `-` tells Codex to read the prompt from stdin.
		"-",
	)
}

// resumeSafeExtraArgs is the subset of the configured args that `exec resume`
// accepts: config overrides and feature toggles. Anything else (a --cd, say)
// would make the resume fail outright.
func (a *CodexAdapter) resumeSafeExtraArgs() []string {
	extra := a.safeExtraArgs()
	var out []string
	for i := 0; i < len(extra); i++ {
		arg := extra[i]
		switch arg {
		case "-c", "--config", "--enable", "--disable":
			if i+1 >= len(extra) {
				continue
			}
			// safeExtraArgs has already refused sandbox and approval
			// overrides; the orchestrator's own -c is appended after these.
			out = append(out, arg, extra[i+1])
			i++
		}
	}
	return out
}

// newLastMessageFile creates the file Codex writes its final message into.
// newLastMessageFile creates the file Codex writes its final reply to, in dir
// (the workspace's scratch folder) so that nothing is written outside the
// workspace; "" falls back to the system temp folder, which only tests use.
func newLastMessageFile(dir string, id domain.AgentID) (string, func(), error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", func() {}, fmt.Errorf("creating output folder: %w", err)
		}
	}
	f, err := os.CreateTemp(dir, fmt.Sprintf("ritam-%s-*.txt", id))
	if err != nil {
		return "", func() {}, fmt.Errorf("creating output file: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	return name, func() { _ = os.Remove(filepath.Clean(name)) }, nil
}

// stderrHint appends the CLI's own first stderr line when there is one.
func stderrHint(res *core.OneShotResult) string {
	if res == nil {
		return ""
	}
	for _, line := range strings.Split(res.Stderr, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return ": " + logging.Redact(truncate(t, 200))
		}
	}
	return ""
}

// Interrupt is a no-op for one-shot invocations: cancelling the context passed
// to Send kills the child process.
func (a *CodexAdapter) Interrupt(_ context.Context) error { return nil }

// Status returns the adapter's current lifecycle state.
func (a *CodexAdapter) Status() domain.AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// Outbound yields protocol messages parsed from the CLI's output.
func (a *CodexAdapter) Outbound() <-chan ports.Outbound { return a.out }

// Activity yields the agent's output as it is generated.
func (a *CodexAdapter) Activity() <-chan ports.AgentActivity { return a.activity.Channel() }

func (a *CodexAdapter) setState(s domain.AgentState) {
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
func (a *CodexAdapter) fail(err error) {
	a.log.ForAgent(a.id).Error("codex invocation failed", "error", logging.Redact(err.Error()))
	a.activity.Emit(ports.ActivityStatus, "failed: "+err.Error())
	a.emit(ports.Outbound{AgentID: a.id, Err: err})
}

// emit publishes an outbound value, dropping it once the adapter has stopped.
func (a *CodexAdapter) emit(o ports.Outbound) {
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// AcceptsBatches reports that several waiting messages may be combined into
// one turn: the agent reads them all in one prompt.
func (a *CodexAdapter) AcceptsBatches() bool { return true }
