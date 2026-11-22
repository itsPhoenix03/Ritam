package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/review"
)

// The live tests are skipped unless RITAM_LIVE=1, so the normal suite stays
// offline and free:
//
//	$env:RITAM_LIVE="1"; go test -run TestLiveCodex ./internal/agents/codex/ -v

// TestLiveCodexAcceptsTheHookConfig: the installed codex reads ritam's hook
// override as its hooks setting. It needs no account: `features list` loads
// the configuration and stops, failing on a hooks value it cannot read.
func TestLiveCodexAcceptsTheHookConfig(t *testing.T) {
	codexOnPath(t)
	program := `C:\Program Files\ritam\ritam.exe`
	if runtime.GOOS != "windows" {
		program = "/opt/my tools/ritam"
	}
	a := New("x", config.AgentConfig{AllowWrites: true, WorkingDir: t.TempDir()}, nil)
	a.UseHooks(stubHooks{program: program})
	args := append([]string{"features", "list"}, slices.DeleteFunc(a.hookArgs(), func(s string) bool {
		return s == "--dangerously-bypass-hook-trust" // an exec flag, not a features one
	})...)
	out, err := exec.Command("codex", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("codex refused the hook override %v: %v\n%s", args, err, out)
	}
}

// TestLiveCodexToolCallsGoThroughTheHook runs one real Codex turn through
// the adapter, with ritam's own binary, freshly built, as the hook. It needs
// a Codex account, and checks what could not be checked without one: that
// Codex runs the hook from the override (through its shell, with the trust
// bypass) and passes on the turn's environment.
func TestLiveCodexToolCallsGoThroughTheHook(t *testing.T) {
	codexOnPath(t)
	bin := filepath.Join(t.TempDir(), "ritam")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "ritam/cmd/ritam").CombinedOutput(); err != nil {
		t.Fatalf("building ritam: %v\n%s", err, out)
	}
	srv, err := review.Start(bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	var (
		mu      sync.Mutex
		calls   []review.Call
		reports []review.Report
	)
	srv.OnCall = func(c review.Call) { mu.Lock(); calls = append(calls, c); mu.Unlock() }
	srv.OnTurnEnd = func(r review.Report) { mu.Lock(); reports = append(reports, r); mu.Unlock() }

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hello from the file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New("x", config.AgentConfig{
		Executable: "codex", AllowWrites: true, WorkingDir: ws, WorkspaceRoot: ws,
		ScratchDir: filepath.Join(ws, ".ritam", "tmp"), ResponseTimeout: 4 * time.Minute, WriteResponseTimeout: 4 * time.Minute,
	}, nil)
	a.UseHooks(srv)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	task := testTask()
	m := domain.Message{TaskID: task.ID, FromAgent: "human", ToAgent: "x", Type: domain.MsgTaskAssignment,
		Content: "Run a shell command that prints the contents of hello.txt, then answer the human with them."}
	if err := a.SendTask(context.Background(), task, m); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		mu.Lock()
		done := len(reports) > 0
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no turn report within 5 minutes")
		}
		select {
		case o := <-a.Outbound():
			if o.Err != nil {
				t.Logf("turn reported: %v", o.Err)
			}
		case <-time.After(200 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		t.Logf("hook saw: %s %s decision=%s", c.Tool, c.Summary, c.Decision)
	}
	rep := reports[0]
	t.Logf("report: seen=%d reported=%d", rep.Seen, rep.Reported)
	if rep.Seen == 0 {
		t.Fatalf("the hook saw no call: Codex did not run it (%s)", rep.Problem())
	}
	if p := rep.Problem(); p != "" {
		t.Error(p)
	}
}

func codexOnPath(t *testing.T) {
	t.Helper()
	if os.Getenv("RITAM_LIVE") != "1" {
		t.Skip("set RITAM_LIVE=1 to run against the real codex CLI")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex is not installed")
	}
}
