package core

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests exercise the real process-spawn path. Rather than depending on a
// shell or a system binary — which differ between Windows and POSIX, and are
// exactly the assumption this package must not make — they re-invoke the test
// binary itself as the child process.

const helperEnv = "RITAM_ONESHOT_HELPER"

// TestHelperHandshake is the child process. It does nothing unless the
// environment variable is set, so it is inert during a normal test run.
func TestHelperHandshake(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("not running as the helper child process")
	}

	var mode string
	args := os.Args
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			mode = args[i+1]
			args = args[i+2:]
			break
		}
	}

	switch mode {
	case "echo":
		os.Stdout.WriteString(strings.Join(args, " "))
		os.Exit(0)
	case "stdin":
		buf := make([]byte, 1024)
		n, _ := os.Stdin.Read(buf)
		os.Stdout.Write(buf[:n])
		os.Exit(0)
	case "fail":
		os.Stderr.WriteString("deliberate failure\nsecond line\n")
		os.Exit(3)
	case "silentfail":
		os.Exit(3)
	case "stdoutfail":
		os.Stdout.WriteString("noise\nrate limited, try later\n\n")
		os.Exit(2)
	case "env":
		os.Stdout.WriteString(os.Getenv("RITAM_TEST_VALUE"))
		os.Exit(0)
	case "stream":
		// Emit lines with gaps, so a buffered reader is distinguishable from a
		// streaming one.
		for i := 0; i < 3; i++ {
			os.Stdout.WriteString("line-" + string(rune('0'+i)) + "\n")
			time.Sleep(150 * time.Millisecond)
		}
		os.Exit(0)
	case "longline":
		os.Stdout.WriteString(strings.Repeat("x", 250_000) + "\n")
		os.Exit(0)
	case "hang":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "spawner":
		// Stands in for a .cmd shim: Windows runs `codex.cmd` as cmd.exe,
		// which starts node as a grandchild that inherits stdout/stderr. If
		// only the direct child is killed, the grandchild keeps the pipes
		// open and the caller blocks until it finishes on its own.
		exe, _ := os.Executable()
		c := exec.Command(exe, "-test.run=TestHelperHandshake", "--", "lingering", args[0])
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Start(); err != nil {
			os.Exit(4)
		}
		os.Stdout.WriteString("spawned\n")
		_ = c.Wait()
		os.Exit(0)
	case "lingering":
		// Proves survival: the marker only appears if this process outlives
		// the cancel by a few seconds.
		time.Sleep(3 * time.Second)
		_ = os.WriteFile(args[0], []byte("survived"), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(0)
}

// TestCancelKillsTheWholeProcessTree is a regression test for a 29-second
// shutdown hang seen with the real Codex CLI. Cancelling killed cmd.exe but
// not the node process it had started, which held the output pipe open: the
// caller sat in the read loop until node finished, and node kept running (and
// spending) after the task had ended.
func TestCancelKillsTheWholeProcessTree(t *testing.T) {
	marker := t.TempDir() + string(os.PathSeparator) + "grandchild-survived"
	r := helperRunner(t, 2*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	spawned := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := r.RunStreaming(ctx, []string{"spawner", marker}, "", func(line string) {
			if line == "spawned" {
				spawned <- struct{}{}
			}
		})
		done <- err
	}()

	select {
	case <-spawned:
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("helper never reported spawning its grandchild")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Errorf("cancelled run returned %v, want ErrCancelled", err)
		}
	case <-time.After(killWait + 5*time.Second):
		t.Fatal("RunStreaming did not return after cancel: a grandchild is holding the pipe open")
	}

	// Give a surviving grandchild time to write its marker.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the grandchild outlived the cancel: only the direct child was killed")
	}
}

// helperRunner builds a runner whose executable is this test binary.
func helperRunner(t *testing.T, timeout time.Duration) *OneShotRunner {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	return &OneShotRunner{
		AgentID:    "helper",
		Executable: exe,
		BaseArgs:   []string{"-test.run=TestHelperHandshake", "--"},
		Env:        map[string]string{helperEnv: "1"},
		Timeout:    timeout,
	}
}

func TestRunCapturesStdout(t *testing.T) {
	r := helperRunner(t, 30*time.Second)
	res, err := r.Run(context.Background(), []string{"echo", "hello", "world"}, "")
	if err != nil {
		t.Fatalf("Run: %v (stderr: %s)", err, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello world") {
		t.Errorf("stdout = %q, want it to contain %q", res.Stdout, "hello world")
	}
	if res.Duration <= 0 {
		t.Error("expected a measured duration")
	}
}

// TestRunPassesArgumentsAsData is the security-relevant case: prompt text is
// full of quotes, newlines and shell metacharacters, and none of it may be
// reinterpreted as a command.
func TestRunPassesArgumentsAsData(t *testing.T) {
	hostile := `"; rm -rf / & echo pwned | cat > out.txt`
	r := helperRunner(t, 30*time.Second)

	res, err := r.Run(context.Background(), []string{"echo", hostile}, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "pwned") {
		t.Fatalf("argument did not arrive intact: %q", res.Stdout)
	}
	// It arrived as one argument, not as a parsed command line.
	if strings.Contains(res.Stdout, "out.txt\n") && !strings.Contains(res.Stdout, hostile) {
		t.Error("argument appears to have been re-parsed rather than passed as data")
	}
}

func TestRunWritesStdin(t *testing.T) {
	r := helperRunner(t, 30*time.Second)
	res, err := r.Run(context.Background(), []string{"stdin"}, "piped payload")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "piped payload") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestRunMergesEnvironment(t *testing.T) {
	r := helperRunner(t, 30*time.Second)
	r.Env["RITAM_TEST_VALUE"] = "from-config"

	res, err := r.Run(context.Background(), []string{"env"}, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "from-config") {
		t.Errorf("stdout = %q, want the merged env var", res.Stdout)
	}
}

func TestRunReportsNonZeroExitWithStderr(t *testing.T) {
	r := helperRunner(t, 30*time.Second)
	res, err := r.Run(context.Background(), []string{"fail"}, "")
	if err == nil {
		t.Fatal("expected a non-zero exit to be reported")
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	// The CLI's own message is far more useful than the exit code alone.
	if !strings.Contains(err.Error(), "deliberate failure") {
		t.Errorf("error should carry the first stderr line, got: %v", err)
	}
}

func TestRunTimesOut(t *testing.T) {
	r := helperRunner(t, 300*time.Millisecond)

	start := time.Now()
	_, err := r.Run(context.Background(), []string{"hang"}, "")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	// A hung agent must not hold the run loop for its full sleep.
	if elapsed > 10*time.Second {
		t.Errorf("timeout took %s; the child was not killed promptly", elapsed)
	}
}

func TestRunHonoursContextCancellation(t *testing.T) {
	r := helperRunner(t, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Run(ctx, []string{"hang"}, "")
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(killWait + 5*time.Second):
		t.Fatal("cancelling the context must kill the child process")
	}
}

func TestResolveReportsMissingExecutable(t *testing.T) {
	r := &OneShotRunner{AgentID: "opus", Executable: "definitely-not-installed-xyz"}

	_, err := r.Resolve()
	if err == nil {
		t.Fatal("expected an error for a missing executable")
	}
	// The message has to tell the operator how to fix it.
	if !strings.Contains(err.Error(), "PATH") || !strings.Contains(err.Error(), "executable") {
		t.Errorf("error should name PATH and the config key, got: %v", err)
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("error should wrap exec.ErrNotFound, got: %v", err)
	}
}

func TestResolveRejectsEmptyExecutable(t *testing.T) {
	r := &OneShotRunner{AgentID: "opus"}
	if _, err := r.Resolve(); err == nil {
		t.Fatal("expected an error when no executable is configured")
	}
}

func TestMergeEnvOverridesParentValues(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2"}, map[string]string{"B": "override", "C": "3"})

	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "A=1") || !strings.Contains(joined, "B=override") || !strings.Contains(joined, "C=3") {
		t.Fatalf("mergeEnv = %v", got)
	}
	if strings.Contains(joined, "B=2") {
		t.Error("the parent value should have been replaced, not duplicated")
	}
}

// TestCancelIsDistinctFromACrash: killing the child because the context was
// cancelled exits non-zero (1 on Windows), which used to be reported as
// "exited with code 1". It must be a distinct, testable sentinel.
func TestCancelIsDistinctFromACrash(t *testing.T) {
	for name, run := range map[string]func(context.Context, *OneShotRunner) error{
		"Run": func(ctx context.Context, r *OneShotRunner) error {
			_, err := r.Run(ctx, []string{"hang"}, "")
			return err
		},
		"RunStreaming": func(ctx context.Context, r *OneShotRunner) error {
			_, err := r.RunStreaming(ctx, []string{"hang"}, "", nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := helperRunner(t, time.Minute)
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(400 * time.Millisecond)
				cancel()
			}()
			err := run(ctx, r)
			if !errors.Is(err, ErrCancelled) {
				t.Fatalf("err = %v, want ErrCancelled", err)
			}
			if errors.Is(err, ErrTimeout) || strings.Contains(err.Error(), "exited with code") {
				t.Errorf("a cancel must not read as a timeout or crash: %v", err)
			}
		})
	}
}

func TestEmptyStderrFallsBackToStdoutThenToNoOutput(t *testing.T) {
	r := helperRunner(t, 30*time.Second)

	_, err := r.Run(context.Background(), []string{"stdoutfail"}, "")
	if err == nil || !strings.HasSuffix(err.Error(), "rate limited, try later") {
		t.Errorf("want last stdout line as reason, got: %v", err)
	}
	_, err = r.RunStreaming(context.Background(), []string{"silentfail"}, "", nil)
	if err == nil || !strings.HasSuffix(err.Error(), "no output") {
		t.Errorf("want explicit 'no output', got: %v", err)
	}
	_, err = r.Run(context.Background(), []string{"fail"}, "")
	if err == nil || !strings.HasSuffix(err.Error(), "deliberate failure") {
		t.Errorf("stderr must still win, got: %v", err)
	}
}

// TestNULInArgumentGivesAnActionableError: Go reports this as the opaque
// "fork/exec <exe>: invalid argument".
func TestNULInArgumentGivesAnActionableError(t *testing.T) {
	r := helperRunner(t, 30*time.Second)
	for name, run := range map[string]func() error{
		"Run": func() error { _, err := r.Run(context.Background(), []string{"echo", "a\x00b"}, ""); return err },
		"RunStreaming": func() error {
			_, err := r.RunStreaming(context.Background(), []string{"echo", "a\x00b"}, "", nil)
			return err
		},
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "NUL byte") {
			t.Errorf("%s: want an error naming the NUL byte, got: %v", name, err)
		}
	}
}

func TestSanitizePromptStripsControlCharacters(t *testing.T) {
	got := SanitizePrompt("a\x00b\x01c\x1bd\x7fe\nf\r\tg")
	if got != "abcde\nf\r\tg" {
		t.Errorf("got %q", got)
	}
}
