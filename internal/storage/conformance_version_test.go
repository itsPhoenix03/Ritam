package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

func TestConformance_ATaskUpdatedFromAStaleCopyIsRefused(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		task := &domain.Task{ID: "T", Title: "first", AuthorityAgent: "opus"}
		if err := s.Tasks.Create(c, task); err != nil {
			t.Fatal(err)
		}
		if task.Version != 1 {
			t.Fatalf("Create must set the version it stored, got %d", task.Version)
		}
		a, _ := s.Tasks.Get(c, "T")
		b, _ := s.Tasks.Get(c, "T")
		if a.Version != 1 || b.Version != 1 {
			t.Fatalf("Get must return the stored version, got %d and %d", a.Version, b.Version)
		}

		a.AuthorityAgent = "codex"
		if err := s.Tasks.Update(c, a); err != nil {
			t.Fatal(err)
		}
		if a.Version != 2 {
			t.Fatalf("Update must set the new version on the copy written, got %d", a.Version)
		}

		// b was read before a's change: writing it whole would hand the
		// authority back to opus.
		b.MessageCount = 7
		if err := s.Tasks.Update(c, b); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("an update from a stale copy must be ErrConflict, got %v", err)
		}
		got, _ := s.Tasks.Get(c, "T")
		if got.AuthorityAgent != "codex" || got.MessageCount != 0 || got.Version != 2 {
			t.Fatalf("the refused update changed the task: %+v", got)
		}

		// Read again, the same change goes through.
		b, _ = s.Tasks.Get(c, "T")
		b.MessageCount = 7
		if err := s.Tasks.Update(c, b); err != nil {
			t.Fatal(err)
		}
		// The copy just written can be written again: its version moved on
		// with the stored one.
		b.Title = "second"
		if err := s.Tasks.Update(c, b); err != nil {
			t.Fatalf("the copy that was just written must stay writable: %v", err)
		}
		list, _ := s.Tasks.List(c)
		if len(list) != 1 || list[0].Version != 4 || list[0].MessageCount != 7 || list[0].AuthorityAgent != "codex" {
			t.Fatalf("List: %+v", list[0])
		}
		if err := s.Tasks.Update(c, &domain.Task{ID: "MISSING", Version: 1}); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("a missing task is ErrNotFound, not a conflict: %v", err)
		}
	})
}

func TestConformance_AStaleUpdateIsRefusedInsideATransactionToo(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		_ = s.Tasks.Create(c, &domain.Task{ID: "T"})
		stale, _ := s.Tasks.Get(c, "T")
		fresh, _ := s.Tasks.Get(c, "T")
		_ = s.Tasks.Update(c, fresh)
		err := s.WithTx(c, func(tx *ports.Storage) error { return tx.Tasks.Update(c, stale) })
		if !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("got %v, want ErrConflict", err)
		}
	})
}

// TestSQLiteUpgradesVersionOneTasksAfterTheBackupHook builds a database as
// the first schema left it and opens it with this build.
func TestSQLiteUpgradesVersionOneTasksAfterTheBackupHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO tasks(id, state, created_at, updated_at, data) VALUES (?, ?, ?, ?, ?)`,
		"TASK-001", "COMPLETED", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z",
		`{"ID":"TASK-001","Title":"old","State":"COMPLETED"}`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	var hook []int
	db, err := OpenSQLiteWith(path, Options{BeforeUpgrade: func(s *SQLite, from, to int) error {
		// The backup is taken here, so nothing may have changed yet.
		var cols int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'version'`).Scan(&cols); err != nil {
			t.Fatal(err)
		}
		if cols != 0 {
			t.Error("the schema changed before the backup hook ran")
		}
		hook = append(hook, from, to)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if len(hook) != 2 || hook[0] != 1 || hook[1] != len(migrations) {
		t.Fatalf("the backup hook must run once for 1 -> %d, ran with %v", len(migrations), hook)
	}
	s := db.Storage()
	got, err := s.Tasks.Get(ctx(), "TASK-001")
	if err != nil || got.Title != "old" || got.Version != 1 {
		t.Fatalf("an existing task must start at version 1: %v %+v", err, got)
	}
	got.Title = "new"
	if err := s.Tasks.Update(ctx(), got); err != nil || got.Version != 2 {
		t.Fatalf("update after the upgrade: %v, version %d", err, got.Version)
	}
}

func TestSQLiteKeepsTaskVersionsAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.db")
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	s := db.Storage()
	task := &domain.Task{ID: "T"}
	_ = s.Tasks.Create(ctx(), task)
	_ = s.Tasks.Update(ctx(), task)
	_ = db.Close()

	db, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got, _ := db.Storage().Tasks.Get(ctx(), "T"); got.Version != 2 {
		t.Fatalf("version after reopen = %d, want 2", got.Version)
	}
}
