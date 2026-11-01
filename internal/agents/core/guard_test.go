package core

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"ritam/internal/domain"
)

// TestAnAgentsEditToAGuardedFileIsPutBack: an agent's shell can write where
// its file tools may not; after the turn ritam puts its own files back and
// keeps the agent's version for the human.
func TestAnAgentsEditToAGuardedFileIsPutBack(t *testing.T) {
	dir, keep := t.TempDir(), t.TempDir()
	cfg := filepath.Join(dir, "ritam.yaml")
	created := filepath.Join(dir, ".ritam", "ritam.yaml")
	_ = os.WriteFile(cfg, []byte("agents: {}\n"), 0o600)

	g := NewFileGuard(keep, cfg, created)
	var told []string
	g.OnRestore = func(path string, agent domain.AgentID, kept string) {
		told = append(told, string(agent)+" "+filepath.Base(path))
		if kept != "" {
			if data, _ := os.ReadFile(kept); len(data) == 0 {
				t.Errorf("the agent's version must be kept in %s", kept)
			}
		}
	}

	_ = os.WriteFile(cfg, []byte("agents: {opus: {args: [--dangerously-skip-permissions]}}\n"), 0o600)
	_ = os.MkdirAll(filepath.Dir(created), 0o700)
	_ = os.WriteFile(created, []byte("access: {level: 3}\n"), 0o600)
	if got := g.Check("opus"); len(got) != 2 {
		t.Fatalf("both changes must be put back, got %v", got)
	}
	if data, _ := os.ReadFile(cfg); string(data) != "agents: {}\n" {
		t.Errorf("ritam.yaml must be as ritam last saw it, got %q", data)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Error("a guarded file the agent created must be removed")
	}
	if len(told) != 2 {
		t.Errorf("the human must be told of each: %v", told)
	}
	if got := g.Check("opus"); len(got) != 0 {
		t.Errorf("nothing changed since: %v", got)
	}
}

// TestRitamsOwnWritesAreNotTakenForAnAgents: what ritam writes through the
// guard stays, even with checks running beside it.
func TestRitamsOwnWritesAreNotTakenForAnAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	reg, err := OpenSessionRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	g := NewFileGuard("", path)
	reg.UseGuard(g)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if got := g.Check("codex"); len(got) > 0 {
					t.Errorf("ritam's own write was put back: %v", got)
					return
				}
			}
		}
	}()
	for i := 0; i < 50; i++ {
		s := NewSession(SessionKey(string(rune('a'+i%26))), domain.AgentKindCodex, "thread", dir, "")
		if err := reg.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()

	again, err := OpenSessionRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.Get("a"); !ok {
		t.Error("ritam's sessions must survive the checks")
	}
}

// TestANilGuardChecksNothing: adapters built outside the app have none.
func TestANilGuardChecksNothing(t *testing.T) {
	var g *FileGuard
	ran := false
	if err := g.Write("x", func() error { ran = true; return nil }); err != nil || !ran {
		t.Error("a nil guard still writes")
	}
	if g.Check("opus") != nil {
		t.Error("a nil guard checks nothing")
	}
}

// TestUnsandboxedNamesTheWritersWithNoVerifiedSandbox: they are the ones
// the human is warned about at the any-command level.
func TestUnsandboxedNamesTheWritersWithNoVerifiedSandbox(t *testing.T) {
	a := NewAccess(domain.AccessAnyCommand, nil)
	a.SetWrites("claude", true)
	a.SetWrites("codex", true)
	a.SetWrites("reader", false)
	a.SetSandboxed("codex", true)
	if got := a.Unsandboxed(); len(got) != 1 || got[0] != "claude" {
		t.Errorf("only the unsandboxed writer: %v", got)
	}
	if got := a.Unsandboxed("codex", "reader"); len(got) != 0 {
		t.Errorf("among codex and reader nobody: %v", got)
	}
	a.Forget("claude")
	if got := a.Unsandboxed(); len(got) != 0 {
		t.Errorf("a dismissed agent is forgotten: %v", got)
	}
}
