package core

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"ritam/internal/domain"
)

// OneShotResult is the outcome of a single CLI invocation.
type OneShotResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// OneShotRunner invokes an agent CLI once per protocol message and waits for
// it to exit.
//
// This is the transport both real adapters use. A fresh process per message is
// deliberate: Claude Code and Codex are built for a human at a terminal, so a
// long-lived session can block forever on an interactive permission prompt
// that nobody is there to answer, and a wedged session is hard to tell apart
// from a slow one. A process that must exit before its reply counts gives a
// hard timeout, a clean retry boundary, and a crash that costs one message
// rather than the whole task. The cost is that conversational context must be
// re-supplied each call, which is what the shared knowledge store is for.
type OneShotRunner struct {
	AgentID domain.AgentID
	// Executable is resolved on PATH unless it is an absolute path. It is
	// never hard-coded: these CLIs are installed differently per machine, and
	// on Windows they are usually shims (.cmd) rather than real binaries.
	Executable string
	// BaseArgs precede the per-call arguments on every invocation.
	BaseArgs []string
	// WorkingDir is the directory the CLI runs in. Empty means the
	// orchestrator's own working directory.
	WorkingDir string
	// Env holds extra environment variables merged over the parent
	// environment. Credentials never belong here: each CLI authenticates
	// through its own mechanism.
	Env map[string]string
	// Timeout bounds a single invocation.
	Timeout time.Duration
}

// ErrTimeout reports that a CLI invocation exceeded its configured timeout.
var ErrTimeout = errors.New("agent CLI timed out")

// ErrCancelled reports that a CLI invocation was stopped on purpose because
// its context was cancelled (the task ended, or the adapter was stopped).
//
// It is distinct from a failure. exec.CommandContext kills the child on
// cancel, and on Windows a killed process exits with code 1, which used to be
// reported as the CLI crashing. Callers test for it with errors.Is and treat
// it as "nobody is waiting for this answer any more", not as an agent error.
var ErrCancelled = errors.New("agent CLI invocation cancelled")

// Resolve locates the executable, returning a clear error when the CLI is not
// installed. Callers use it at Start so a missing CLI is reported up front
// rather than on the first message.
func (r *OneShotRunner) Resolve() (string, error) {
	exe := r.Executable
	if exe == "" {
		return "", fmt.Errorf("no executable configured for agent %s", r.AgentID)
	}
	path, err := exec.LookPath(exe)
	if err != nil {
		return "", fmt.Errorf("agent %s: cannot find %q on PATH (set agents.%s.executable in the config): %w",
			r.AgentID, exe, r.AgentID, err)
	}
	return path, nil
}

// Run invokes the CLI once with BaseArgs followed by args, optionally writing
// stdin, and returns once the process exits.
//
// Arguments are passed as a slice and never assembled into a shell string, so
// prompt text containing quotes, newlines or shell metacharacters cannot be
// reinterpreted as a command. Agent-controlled text reaches the CLI as data.
func (r *OneShotRunner) Run(ctx context.Context, args []string, stdin string) (*OneShotResult, error) {
	path, err := r.Resolve()
	if err != nil {
		return nil, err
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append(append([]string(nil), r.BaseArgs...), args...)
	cmd := exec.CommandContext(runCtx, path, full...)
	cmd.Dir = r.WorkingDir
	if len(r.Env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), r.Env)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	killWholeTreeOnCancel(cmd)

	start := time.Now()
	runErr := cmd.Run()
	res := &OneShotResult{
		Stdout:   out.String(),
		Stderr:   errBuf.String(),
		Duration: time.Since(start),
	}

	if err := r.classify(ctx, runCtx, runErr, res, full, timeout); err != nil {
		return res, err
	}
	return res, nil
}

// RunStreaming invokes the CLI and delivers stdout to onLine as each line
// arrives, instead of only after the process exits.
//
// Both CLIs can emit newline-delimited JSON events while they work, which is
// what lets the UI show an agent thinking rather than a frozen pane for the
// minutes a real call takes. The full stdout is still accumulated and returned
// so the caller can extract a final result exactly as it would from Run.
func (r *OneShotRunner) RunStreaming(ctx context.Context, args []string, stdin string, onLine func(string)) (*OneShotResult, error) {
	path, err := r.Resolve()
	if err != nil {
		return nil, err
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append(append([]string(nil), r.BaseArgs...), args...)
	cmd := exec.CommandContext(runCtx, path, full...)
	cmd.Dir = r.WorkingDir
	if len(r.Env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), r.Env)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("agent %s: stdout pipe: %w", r.AgentID, err)
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	killWholeTreeOnCancel(cmd)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("agent %s: %w", r.AgentID, ErrCancelled)
		}
		return nil, r.startError(err, full)
	}

	stdoutText, scanErr := readLines(runCtx, stdout, onLine)

	runErr := cmd.Wait()
	res := &OneShotResult{
		Stdout:   stdoutText,
		Stderr:   errBuf.String(),
		Duration: time.Since(start),
	}

	if err := r.classify(ctx, runCtx, runErr, res, full, timeout); err != nil {
		return res, err
	}
	if scanErr != nil {
		return res, fmt.Errorf("agent %s: reading output: %w", r.AgentID, scanErr)
	}
	return res, nil
}

// killWait bounds how long a cancelled invocation may keep its caller waiting
// after the kill has been sent.
const killWait = 3 * time.Second

// killWholeTreeOnCancel makes a cancelled cmd take its descendants with it,
// and bounds how long Wait can block on pipes they hold. See killTree for why
// killing only the direct child is not enough.
func killWholeTreeOnCancel(cmd *exec.Cmd) {
	prepareTree(cmd)
	cmd.Cancel = func() error { return killTree(cmd.Process) }
	// WaitDelay is the backstop for a descendant that escaped the tree kill:
	// Wait stops waiting for its pipes and returns instead of hanging.
	cmd.WaitDelay = killWait
}

// readLines drains r line by line, calling onLine for each, and returns the
// accumulated text.
//
// The scan runs on its own goroutine so a cancel can stop waiting for it.
// Normally killTree closes the pipe and the scan ends at once, but a
// descendant that escaped the kill would hold the pipe open, blocking the
// scan -- and the agent's Stop with it -- until that process exited. After
// killWait the scan is abandoned instead: onLine is never called again, and
// the goroutine ends whenever the pipe finally closes.
func readLines(ctx context.Context, r io.Reader, onLine func(string)) (string, error) {
	var (
		mu        sync.Mutex
		out       strings.Builder
		abandoned bool
	)
	done := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(r)
		// Streamed JSON events routinely exceed the 64 KB default, and a
		// split line would be unparseable rather than merely truncated.
		scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			if abandoned {
				mu.Unlock()
				continue
			}
			out.WriteString(line)
			out.WriteString("\n")
			mu.Unlock()
			if onLine != nil {
				onLine(line)
			}
		}
		done <- scanner.Err()
	}()

	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		select {
		case err = <-done:
		case <-time.After(killWait):
		}
	}
	mu.Lock()
	abandoned = true
	text := out.String()
	mu.Unlock()
	return text, err
}

// mergeEnv overlays extra onto base, replacing matching keys.
func mergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, override := extra[k]; override {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// firstLine returns the first non-empty line of s, for compact error text.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// classify turns the end of a process into the error the caller should see,
// or nil for a clean exit. It is shared by Run and RunStreaming so the two
// can never disagree about what a cancel, a timeout or a crash looks like.
func (r *OneShotRunner) classify(parent, runCtx context.Context, runErr error, res *OneShotResult, args []string, timeout time.Duration) error {
	if runCtx.Err() == context.DeadlineExceeded && parent.Err() == nil {
		return fmt.Errorf("agent %s: %w after %s", r.AgentID, ErrTimeout, timeout)
	}
	if runErr == nil {
		return nil
	}
	// Checked before the exit code is interpreted: a process we killed on
	// purpose exits non-zero, and that must not read as a crash.
	if parent.Err() != nil || runCtx.Err() == context.Canceled {
		return fmt.Errorf("agent %s: %w", r.AgentID, ErrCancelled)
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		// A non-zero exit is data, not a transport failure: the CLI may have
		// printed a usable error. Report it, but let the adapter decide what
		// it means.
		res.ExitCode = ee.ExitCode()
		return fmt.Errorf("agent %s: %s exited with code %d: %s",
			r.AgentID, r.Executable, res.ExitCode, failureReason(res))
	}
	return r.startError(runErr, args)
}

// startError explains a process that could not be started.
//
// Go reports a NUL byte inside an argument as the opaque
// "fork/exec <exe>: invalid argument", which sends people hunting for a bad
// executable. Naming the real cause makes it fixable.
func (r *OneShotRunner) startError(err error, args []string) error {
	for i, a := range args {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("agent %s: cannot start %s: argument %d contains a NUL byte, which the OS cannot pass to a process (%w)",
				r.AgentID, r.Executable, i, err)
		}
	}
	if errors.Is(err, exec.ErrDot) {
		return fmt.Errorf("agent %s: cannot start %s: it resolves to the current directory, which Go refuses to run implicitly; use an absolute path in the config (%w)",
			r.AgentID, r.Executable, err)
	}
	return fmt.Errorf("agent %s: running %s: %w", r.AgentID, r.Executable, err)
}

// failureReason picks the most useful one-line explanation of a failed run:
// stderr first, then the last line of stdout (some CLIs, Claude Code among
// them, report errors there), and an explicit "no output" rather than leaving
// the message ending in a bare colon.
func failureReason(res *OneShotResult) string {
	if s := firstLine(res.Stderr); s != "" {
		return s
	}
	lines := strings.Split(res.Stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			if len(t) > 300 {
				t = t[:300] + "..."
			}
			return t
		}
	}
	return "no output"
}
