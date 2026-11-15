package core

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ritam/internal/domain"
)

func task(id string, created time.Time) *domain.Task {
	return &domain.Task{ID: domain.TaskID(id), CreatedAt: created}
}

func TestSessionsAreKeyedByTaskAndAgent(t *testing.T) {
	r, err := OpenSessionRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	t1 := task("TASK-001", time.Now())

	opus := NewSession(SessionKeyFor(t1, "opus"), domain.AgentKindClaudeCode, "id-opus", `C:\w`, "opus")
	scout := NewSession(SessionKeyFor(t1, "scout"), domain.AgentKindClaudeCode, "id-scout", `C:\w`, "sonnet")
	if err := r.Save(opus); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(scout); err != nil {
		t.Fatal(err)
	}

	got, ok := r.Get(SessionKeyFor(t1, "opus"))
	if !ok || got.ID != "id-opus" {
		t.Fatalf("opus resolved to %+v (ok=%v)", got, ok)
	}
	got, ok = r.Get(SessionKeyFor(t1, "scout"))
	if !ok || got.ID != "id-scout" {
		t.Fatalf("two agents on the same CLI must not share a session, scout got %+v", got)
	}
}

// TestARestartedTaskNumberDoesNotResumeAnOldSession is the reason the key
// carries the task's creation time. Task ids restart at TASK-001 on every
// launch, so keying by id alone would hand a brand-new task an old task's
// conversation.
func TestARestartedTaskNumberDoesNotResumeAnOldSession(t *testing.T) {
	r, _ := OpenSessionRegistry("")
	old := task("TASK-001", time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	fresh := task("TASK-001", time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC))

	_ = r.Save(NewSession(SessionKeyFor(old, "opus"), domain.AgentKindClaudeCode, "old-id", "", ""))
	if s, ok := r.Get(SessionKeyFor(fresh, "opus")); ok {
		t.Fatalf("a new TASK-001 resolved to the old task's session %q", s.ID)
	}
}

func TestRegistrySurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ritam", "sessions.json")
	t1 := task("TASK-001", time.Now())

	r, err := OpenSessionRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(SessionKeyFor(t1, "codex"), domain.AgentKindCodex, "thread-1", `C:\w`, "")
	s.Turns = 3
	if err := r.Save(s); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSessionRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get(SessionKeyFor(t1, "codex"))
	if !ok || got.ID != "thread-1" || got.Turns != 3 || got.WorkingDir != `C:\w` {
		t.Fatalf("after reopening: %+v (ok=%v)", got, ok)
	}
}

// TestSaveIsAtomic checks a save never leaves a half-written file behind: the
// registry is written to a temporary file and renamed into place.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	r, _ := OpenSessionRegistry(path)
	for i := 0; i < 20; i++ {
		_ = r.Save(NewSession(SessionKeyFor(task("TASK-001", time.Now()), domain.AgentID("a")), domain.AgentKindCodex, "x", "", ""))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "sessions.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected only sessions.json, found %v", names)
	}
}

func TestReplaceKeepsTheOldIDInHistory(t *testing.T) {
	r, _ := OpenSessionRegistry("")
	key := SessionKeyFor(task("TASK-001", time.Now()), "opus")
	s := NewSession(key, domain.AgentKindClaudeCode, "first", "", "")
	_ = r.Save(s)

	s.Replace("second", SessionRebuilt)
	_ = r.Save(s)

	got, _ := r.Get(key)
	if got.ID != "second" || got.State != SessionRebuilt {
		t.Fatalf("got %+v", got)
	}
	if len(got.Previous) != 1 || got.Previous[0] != "first" {
		t.Errorf("the replaced id must stay in history, got %v", got.Previous)
	}
}

// TestAnEmptyOrCorruptRegistryIsKeptAsideAndStartsEmpty: an empty file (what
// a power cut mid-write can leave) or a corrupt one does not stop start-up.
// It is kept for a human to read, the registry starts empty, and the warning
// says where the file went.
func TestAnEmptyOrCorruptRegistryIsKeptAsideAndStartsEmpty(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "corrupt": "{not json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sessions.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := OpenSessionRegistry(path)
			if err != nil {
				t.Fatalf("an unreadable registry must not stop start-up: %v", err)
			}
			kept, _ := filepath.Glob(path + ".bad-*")
			if len(kept) != 1 {
				t.Fatalf("the unreadable file must be kept aside: %v", kept)
			}
			if data, _ := os.ReadFile(kept[0]); string(data) != content {
				t.Errorf("the kept file is the unreadable one, unchanged: %q", data)
			}
			if w := r.Recovered(); !strings.Contains(w, kept[0]) {
				t.Errorf("the warning names where the file was kept: %q", w)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the registry starts with no file: %v", err)
			}

			key := SessionKeyFor(task("TASK-001", time.Now()), domain.AgentID("a"))
			if err := r.Save(NewSession(key, domain.AgentKindCodex, "x", "", "")); err != nil {
				t.Fatal(err)
			}
			again, err := OpenSessionRegistry(path)
			if err != nil || again.Recovered() != "" {
				t.Fatalf("the new registry reads back cleanly: %v %q", err, again.Recovered())
			}
			if _, ok := again.Get(key); !ok {
				t.Error("what is saved after recovery is kept")
			}
		})
	}
}

// TestCheckSessionsFileWarnsOnlyAboutAnUnreadableFile: start-up's check is
// quiet for a good file or none, and keeps a bad one aside.
func TestCheckSessionsFileWarnsOnlyAboutAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if msg, err := CheckSessionsFile(path); msg != "" || err != nil {
		t.Errorf("no file, nothing to say: %q %v", msg, err)
	}
	_ = os.WriteFile(path, []byte("[]"), 0o600)
	if msg, err := CheckSessionsFile(path); msg != "" || err != nil {
		t.Errorf("a good file, nothing to say: %q %v", msg, err)
	}
	_ = os.WriteFile(path, nil, 0o600)
	if msg, err := CheckSessionsFile(path); msg == "" || err != nil {
		t.Errorf("an empty file is reported: %q %v", msg, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("and kept aside: %v", err)
	}
}

func TestRunningTotalsAreChargedByHowFarTheyGrew(t *testing.T) {
	var s Session
	if got := s.CostSince(0.50); got != 0.50 {
		t.Errorf("first report: %v", got)
	}
	if got := s.CostSince(0.80); math.Abs(got-0.30) > 1e-9 {
		t.Errorf("second report should charge the growth, got %v", got)
	}
	// A total below the last one: the CLI is counting a new session.
	if got := s.CostSince(0.10); got != 0.10 {
		t.Errorf("a restarted total is all new, got %v", got)
	}
	in, cached, out := s.TokensSince(100, 40, 10)
	if in != 100 || cached != 40 || out != 10 {
		t.Errorf("first token report: %d %d %d", in, cached, out)
	}
	s.Replace("new-id", SessionRebuilt)
	if s.ReportedCostUSD != 0 || s.ReportedInputTokens != 0 || s.ReportedOutputTokens != 0 {
		t.Error("a replaced session starts its totals from zero")
	}
}
