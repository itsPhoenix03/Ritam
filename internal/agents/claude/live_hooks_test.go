package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/review"
)

// TestLiveEveryToolCallGoesThroughTheHook runs one real Claude Code turn
// through the adapter, with ritam's own binary, freshly built, as the hook
// and a real review server behind it: the settings ritam writes must make
// the CLI run the hook, the hook must reach the server with the turn's
// token, and every tool call the CLI reports must have gone through it.
//
// It costs a few cents (the haiku model) and is skipped unless RITAM_LIVE=1:
//
//	$env:RITAM_LIVE="1"; go test -run TestLiveEveryToolCallGoesThroughTheHook ./internal/agents/claude/ -v
func TestLiveEveryToolCallGoesThroughTheHook(t *testing.T) {
	if os.Getenv("RITAM_LIVE") != "1" {
		t.Skip("set RITAM_LIVE=1 to run against the real claude CLI")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not installed")
	}

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
	rec := &hookRecord{}
	srv.OnCall = func(c review.Call) { rec.mu.Lock(); rec.calls = append(rec.calls, c); rec.mu.Unlock() }
	srv.OnTurnEnd = func(r review.Report) { rec.mu.Lock(); rec.reports = append(rec.reports, r); rec.mu.Unlock() }

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hello from the file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := New("opus", config.AgentConfig{
		Executable: "claude", Model: "haiku", AllowWrites: true, AllowedCommands: []string{"echo"},
		WorkingDir: ws, WorkspaceRoot: ws, ScratchDir: filepath.Join(ws, ".ritam", "tmp"),
		ResponseTimeout: 3 * time.Minute, WriteResponseTimeout: 3 * time.Minute,
	}, nil)
	a.UseHooks(srv)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })

	task := testTask()
	m := domain.Message{TaskID: task.ID, FromAgent: "human", ToAgent: "opus", Type: domain.MsgTaskAssignment,
		Content: "Read hello.txt with the Read tool, then run the shell command `echo hi`. " +
			"Then answer the human with the file's text."}
	if err := a.SendTask(context.Background(), task, m); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(4 * time.Minute)
	for {
		_, reps := rec.snapshot()
		if len(reps) > 0 {
			break
		}
		select {
		case o := <-a.Outbound():
			if o.Err != nil {
				t.Logf("turn reported: %v", o.Err)
			}
		case <-deadline:
			t.Fatal("no turn report within 4 minutes")
		case <-time.After(200 * time.Millisecond):
		}
	}

	calls, reps := rec.snapshot()
	for _, c := range calls {
		t.Logf("hook saw: %s %s (%s) decision=%s", c.Tool, c.Summary, c.ToolUseID, c.Decision)
	}
	rep := reps[0]
	t.Logf("report: seen=%d reported=%d missed=%v", rep.Seen, rep.Reported, rep.Missed)
	if rep.Seen == 0 {
		t.Fatal("the hook saw no call: the CLI did not run it")
	}
	if p := rep.Problem(); p != "" {
		t.Errorf("every reported call must go through the hook: %s", p)
	}
	for _, c := range calls {
		if c.Task != task.ID || c.Agent != "opus" || c.Decision != review.Pass {
			t.Errorf("call recorded wrongly: %+v", c)
		}
	}
}
