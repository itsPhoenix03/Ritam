package main

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"ritam/internal/backup"
	"ritam/internal/domain"
	"ritam/internal/storage"
)

// olderSchema is the schema version an older ritam left: before deliveries
// (3) and turns (4).
const olderSchema = 2

// downgrade makes the database at path look as an older ritam left it.
func downgrade(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, q := range []string{"DROP TABLE turns", "DROP TABLE deliveries", "PRAGMA user_version = 2"} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func schemaVersion(t *testing.T, path string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestEveryDataCommandBacksUpAnOlderDatabaseBeforeUpgradingIt: `backups
// now`, `restore` and `archive` opened the database without the hook the
// TUI's start passes, so on a database an older ritam left they applied the
// schema migrations with no backup taken. Each now leaves a before-upgrade
// snapshot of the database as it was, and the database upgraded.
func TestEveryDataCommandBacksUpAnOlderDatabaseBeforeUpgradingIt(t *testing.T) {
	for _, args := range [][]string{{"backups", "now"}, {"archive"}, {"restore", "latest"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			inProject(t)
			cfg, pl, err := prepare("")
			if err != nil {
				t.Fatal(err)
			}
			dir := pl.home.BackupDir(pl.proj)
			db, err := storage.OpenSQLite(cfg.Storage.Path)
			if err != nil {
				t.Fatal(err)
			}
			_ = db.Storage().Tasks.Create(context.Background(), &domain.Task{ID: "TASK-001", Title: "kept"})
			if args[0] == "restore" {
				// Something to restore from; it is the latest backup.
				if _, err := backup.Take(context.Background(), db, dir, "manual"); err != nil {
					t.Fatal(err)
				}
			}
			_ = db.Close()
			downgrade(t, cfg.Storage.Path)

			var out bytes.Buffer
			if _, err := runCommand(args, &out); err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out.String())
			}
			list, _ := backup.List(dir)
			var before *backup.Snapshot
			for i := range list {
				if strings.HasPrefix(list[i].Reason, "before-upgrade") {
					before = &list[i]
				}
			}
			if before == nil {
				t.Fatalf("%v must leave a before-upgrade snapshot: %+v", args, list)
			}
			// The snapshot is the database as the older ritam left it.
			restored := filepath.Join(t.TempDir(), "old.db")
			if err := backup.Restore(before.Path, restored); err != nil {
				t.Fatal(err)
			}
			if v := schemaVersion(t, restored); v != olderSchema {
				t.Errorf("the before-upgrade snapshot is at schema %d, want the old %d", v, olderSchema)
			}
			if args[0] == "backups" {
				if v := schemaVersion(t, cfg.Storage.Path); v <= olderSchema {
					t.Errorf("the database must be upgraded after its backup, still at %d", v)
				}
			}
		})
	}
}
