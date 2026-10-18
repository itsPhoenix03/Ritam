package core

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// ProcessRuntime implements ports.AgentRuntime over a long-lived child
// process with piped stdin/stdout/stderr. It is the shared plumbing that both
// the Claude Code and Codex adapters build on; it knows nothing about the
// protocol those CLIs speak, only about running a process and shuttling
// bytes.
type ProcessRuntime struct {
	// AgentID tags every RuntimeOutput/error this runtime produces.
	AgentID domain.AgentID
	// Executable is resolved via exec.LookPath, never a hard-coded path, so
	// that Claude Code / Codex installs anywhere on PATH (including Windows
	// shims) work without configuration.
	Executable string
	Args       []string
	// WorkingDir is the directory the process runs in; empty means inherit.
	WorkingDir string
	// ExtraEnv is appended to the process's environment (e.g. proxy base-url
	// vars). It never carries credentials placed here by this package.
	ExtraEnv []string
	// StopGrace bounds how long Stop waits after closing stdin before it kills
	// the process. Windows has no SIGTERM equivalent for arbitrary processes,
	// so graceful shutdown here means "close stdin and hope the child notices
	// EOF and exits on its own"; Kill is the only reliable way to guarantee
	// termination cross-platform.
	StopGrace time.Duration

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	out     chan ports.RuntimeOutput
	status  domain.AgentState
	started bool
	stopped bool
	done    chan struct{} // closed when the process's wait goroutine finishes
}

// NewProcessRuntime constructs a runtime for the given executable name/args.
// The executable is looked up on PATH at Start time via exec.LookPath.
func NewProcessRuntime(id domain.AgentID, executable string, args []string, workingDir string, extraEnv []string) *ProcessRuntime {
	return &ProcessRuntime{
		AgentID:    id,
		Executable: executable,
		Args:       args,
		WorkingDir: workingDir,
		ExtraEnv:   extraEnv,
		StopGrace:  5 * time.Second,
		status:     domain.AgentStateStopped,
		out:        make(chan ports.RuntimeOutput, 64),
	}
}

// Start resolves the executable on PATH and launches it with piped
// stdin/stdout/stderr. Output from both stdout and stderr is streamed
// line-by-line onto Output().
func (p *ProcessRuntime) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return fmt.Errorf("core: process runtime for %q already started", p.AgentID)
	}

	path, err := exec.LookPath(p.Executable)
	if err != nil {
		p.status = domain.AgentStateUnavailable
		return fmt.Errorf("core: resolve executable %q: %w", p.Executable, err)
	}

	// exec.CommandContext with an explicit argv slice, never a shell string:
	// this avoids shell-quoting bugs and injection risk on both POSIX and
	// Windows (cmd.exe has its own, different quoting rules we sidestep
	// entirely by never invoking it).
	cmd := exec.CommandContext(ctx, path, p.Args...)
	if p.WorkingDir != "" {
		cmd.Dir = p.WorkingDir
	}
	if len(p.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), p.ExtraEnv...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("core: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("core: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("core: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		p.status = domain.AgentStateError
		return fmt.Errorf("core: start %q: %w", p.Executable, err)
	}

	p.cmd = cmd
	p.stdin = stdin
	p.started = true
	p.status = domain.AgentStateIdle
	p.done = make(chan struct{})

	var readers sync.WaitGroup
	readers.Add(2)
	go p.pump(stdout, &readers)
	go p.pump(stderr, &readers)

	go func() {
		readers.Wait()
		waitErr := cmd.Wait()
		p.mu.Lock()
		if !p.stopped {
			if waitErr != nil {
				p.status = domain.AgentStateError
			} else {
				p.status = domain.AgentStateStopped
			}
		}
		p.mu.Unlock()
		p.out <- ports.RuntimeOutput{AgentID: p.AgentID, Err: waitErr, Exited: true}
		close(p.done)
	}()

	return nil
}

// pump reads r line-by-line and forwards each line as a RuntimeOutput.
func (p *ProcessRuntime) pump(r io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	// Agent CLIs may emit long lines (e.g. rendered diffs); grow the buffer
	// well past bufio's 64KiB default rather than silently truncating output.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		p.out <- ports.RuntimeOutput{AgentID: p.AgentID, Text: scanner.Text()}
	}
	// scanner.Err() is intentionally not surfaced as a fatal RuntimeOutput
	// here: a closed pipe at process exit is the normal case and is already
	// reported by the Wait goroutine via Exited/Err.
}

// Stop attempts a graceful shutdown by closing stdin (many CLI agents exit on
// stdin EOF), then kills the process if it has not exited within StopGrace.
// Safe to call more than once.
func (p *ProcessRuntime) Stop(ctx context.Context) error {
	p.mu.Lock()
	if !p.started || p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.status = domain.AgentStateStopped
	stdin := p.stdin
	cmd := p.cmd
	done := p.done
	p.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}

	grace := p.StopGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	select {
	case <-done:
		return nil
	case <-time.After(grace):
		// Windows has no SIGTERM for arbitrary processes (os.Process.Signal is
		// effectively Kill-only there), so a graceful "ask nicely" step isn't
		// available cross-platform; killing after a grace period is the only
		// portable way to guarantee the child actually stops.
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(grace):
			// Process refused to die even after Kill (rare); give up waiting
			// so Stop never blocks forever.
		}
		return nil
	case <-ctx.Done():
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return ctx.Err()
	}
}

// Restart stops the current process (if any) and starts a fresh one.
func (p *ProcessRuntime) Restart(ctx context.Context) error {
	_ = p.Stop(ctx)
	p.mu.Lock()
	p.started = false
	p.stopped = false
	p.mu.Unlock()
	return p.Start(ctx)
}

// Send writes payload to the process's stdin, followed by a newline so
// line-oriented CLIs treat it as a complete input.
func (p *ProcessRuntime) Send(ctx context.Context, payload string) error {
	p.mu.Lock()
	stdin := p.stdin
	started := p.started
	stopped := p.stopped
	p.mu.Unlock()
	if !started || stopped || stdin == nil {
		return fmt.Errorf("core: process runtime for %q is not running", p.AgentID)
	}
	if _, err := io.WriteString(stdin, payload+"\n"); err != nil {
		return fmt.Errorf("core: write to %q stdin: %w", p.AgentID, err)
	}
	return nil
}

// Interrupt has no portable "pause" signal for an arbitrary child process, so
// this scaffold treats it as a documented no-op rather than pretending to
// support it. Adapters that can express interruption via the protocol (e.g.
// a control message on stdin) should do so themselves before/instead of
// calling this.
func (p *ProcessRuntime) Interrupt(ctx context.Context) error {
	return nil
}

// Status returns the runtime's current lifecycle state.
func (p *ProcessRuntime) Status() domain.AgentState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// Output yields raw runtime output. Contents are untrusted.
func (p *ProcessRuntime) Output() <-chan ports.RuntimeOutput {
	return p.out
}

var _ ports.AgentRuntime = (*ProcessRuntime)(nil)
