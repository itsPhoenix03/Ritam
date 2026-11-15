package app

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ritam/internal/config"
	"ritam/internal/layout"
	"ritam/internal/logging"
)

// canaryHelperEnv makes the test binary play a CLI for the sandbox check:
// it runs the prompt's two echo commands as a CLI would, both when its model
// is "unsandboxed", only the one inside the workspace otherwise. A model
// with "wait" in it then waits for the test's go-ahead (see awaitCanary).
const canaryHelperEnv = "RITAM_CANARY_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(canaryHelperEnv) == "1" {
		fakeCanaryCLI()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeCanaryCLI() {
	model := ""
	for i, a := range os.Args {
		if a == "--model" && i+1 < len(os.Args) {
			model = os.Args[i+1]
		}
	}
	var targets []string
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "echo ") {
			continue
		}
		if i, j := strings.Index(line, `"`), strings.LastIndex(line, `"`); i >= 0 && j > i {
			targets = append(targets, line[i+1:j])
		}
	}
	if len(targets) != 2 {
		os.Exit(3)
	}
	inside, outside := targets[0], targets[1]
	_ = os.WriteFile(inside, []byte("canary"), 0o600)
	if strings.Contains(model, "unsandboxed") {
		_ = os.WriteFile(outside, []byte("canary"), 0o600)
	}
	if strings.Contains(model, "wait") {
		_ = os.WriteFile(inside+".written", nil, 0o600)
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(inside + ".go"); err == nil {
				break
			}
		}
	}
}

// canaryTeam is a team whose Codex is the test binary playing a CLI, with
// the canary folder in an app home of its own.
func canaryTeam(t *testing.T) (*Team, layout.Home, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(canaryHelperEnv, "1")
	root, home := t.TempDir(), layout.Home{Root: t.TempDir()}
	cfg := config.Default()
	cfg.TUI.UseMockAgents = false
	cfg.Providers["codex"] = config.ProviderConfig{Executable: exe}
	cfg.Runtime.ScratchDir = filepath.Join(root, ".ritam", "tmp")
	cfg.Runtime.CanaryDir = home.CanaryDir()
	ws := config.Workspace{Root: root, Writable: true}
	return &Team{cfg: cfg, ws: ws, log: logging.NewDiscard()}, home, cfg.Runtime.ScratchDir
}

// awaitCanary waits until n checks have run their commands and wait for the
// go-ahead, runs between, then lets them finish.
func awaitCanary(t *testing.T, scratch string, n int, between func()) {
	t.Helper()
	var written []string
	for deadline := time.Now().Add(30 * time.Second); len(written) < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d checks ran their commands", len(written), n)
		}
		written, _ = filepath.Glob(filepath.Join(scratch, "*.written"))
	}
	between()
	for _, w := range written {
		_ = os.WriteFile(strings.TrimSuffix(w, ".written")+".go", nil, 0o600)
	}
}

type canaryResult struct {
	sandboxed bool
	note      string
	err       error
}

// TestAnotherRitamStartingMidCheckCannotPassIt: an unsandboxed CLI writes
// outside the workspace, and before the check judges it another ritam
// starts (any command does, even `where`) and tidies the canary folder. The
// file it wrote stays, and the check still fails.
func TestAnotherRitamStartingMidCheckCannotPassIt(t *testing.T) {
	tm, home, scratch := canaryTeam(t)
	done := make(chan canaryResult, 1)
	go func() {
		ok, note, err := tm.canaryCLI(context.Background(), "codex", "gpt-unsandboxed-wait")
		done <- canaryResult{ok, note, err}
	}()
	awaitCanary(t, scratch, 1, func() {
		if err := layout.PruneCanary(home); err != nil {
			t.Error(err)
		}
	})
	r := <-done
	if r.err != nil || r.sandboxed || !strings.Contains(r.note, "not sandboxed") {
		t.Errorf("a CLI that wrote outside is not sandboxed, whoever started meanwhile: %+v", r)
	}
	if left, _ := os.ReadDir(home.CanaryDir()); len(left) != 0 {
		t.Errorf("the check removes its folder when done: %v", left)
	}
}

// TestACheckWhoseFolderWentAwayProvesNothing: if the check's own folder is
// gone when it is judged, a missing outside file says nothing about the
// sandbox, so the check is an error and never a pass.
func TestACheckWhoseFolderWentAwayProvesNothing(t *testing.T) {
	tm, home, scratch := canaryTeam(t)
	done := make(chan canaryResult, 1)
	go func() {
		ok, note, err := tm.canaryCLI(context.Background(), "codex", "gpt-unsandboxed-wait")
		done <- canaryResult{ok, note, err}
	}()
	awaitCanary(t, scratch, 1, func() {
		entries, _ := os.ReadDir(home.CanaryDir())
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(home.CanaryDir(), e.Name()))
		}
	})
	if r := <-done; r.err == nil || r.sandboxed {
		t.Errorf("a check whose folder was emptied must not pass: %+v", r)
	}
}

// TestTwoSandboxChecksAtOnceDoNotAffectEachOther: two checks run side by
// side, one CLI sandboxed and one not; each writes into its own folder and
// gets its own verdict, and neither's cleanup touches the other's files.
func TestTwoSandboxChecksAtOnceDoNotAffectEachOther(t *testing.T) {
	tm, home, scratch := canaryTeam(t)
	models := []string{"gpt-unsandboxed-wait", "gpt-sandboxed-wait"}
	results := make([]canaryResult, len(models))
	var wg sync.WaitGroup
	for i, m := range models {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, note, err := tm.canaryCLI(context.Background(), "codex", m)
			results[i] = canaryResult{ok, note, err}
		}()
	}
	var folders []string
	awaitCanary(t, scratch, len(models), func() {
		entries, _ := os.ReadDir(home.CanaryDir())
		for _, e := range entries {
			folders = append(folders, e.Name())
		}
	})
	wg.Wait()
	if len(folders) != 2 || folders[0] == folders[1] {
		t.Errorf("each check has a folder of its own: %v", folders)
	}
	if r := results[0]; r.err != nil || r.sandboxed || !strings.Contains(r.note, "not sandboxed") {
		t.Errorf("the CLI that wrote outside is not sandboxed: %+v", r)
	}
	if r := results[1]; r.err != nil || !r.sandboxed {
		t.Errorf("the sandboxed CLI passes, whatever the other wrote: %+v", r)
	}
	if left, _ := os.ReadDir(home.CanaryDir()); len(left) != 0 {
		t.Errorf("both checks remove their folders: %v", left)
	}
	if left, _ := filepath.Glob(filepath.Join(scratch, "canary-*.txt")); len(left) != 0 {
		t.Errorf("both checks remove their files in the workspace: %v", left)
	}
}
