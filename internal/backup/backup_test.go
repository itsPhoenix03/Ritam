package backup

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"projectorch/internal/domain"
	"projectorch/internal/storage"
)

func openDB(t *testing.T, path string) *storage.SQLite {
	t.Helper()
	db, err := storage.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func addTask(t *testing.T, db *storage.SQLite, title string) domain.TaskID {
	t.Helper()
	repo := db.Storage().Tasks
	n, err := repo.NextSequence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tk := &domain.Task{ID: domain.TaskID(fmt.Sprintf("TASK-%03d", n)), Title: title, CreatedAt: time.Now()}
	if err := repo.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	return tk.ID
}

func TestSnapshotsAreTakenOnlyWhenSomethingChanged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openDB(t, filepath.Join(dir, "p.db"))
	defer db.Close()
	m := New(db, filepath.Join(dir, "backups"), 2, nil)

	if p, err := m.Snapshot(ctx, "exit"); err != nil || p != "" {
		t.Fatalf("nothing changed, so no snapshot: %q %v", p, err)
	}
	addTask(t, db, "one")
	first, err := m.Snapshot(ctx, "task done")
	if err != nil || first == "" {
		t.Fatalf("a change should be backed up: %q %v", first, err)
	}
	if p, _ := m.Snapshot(ctx, "periodic"); p != "" {
		t.Error("an unchanged database is not backed up twice")
	}

	// Only the newest two are kept.
	for i := 0; i < 3; i++ {
		time.Sleep(2 * time.Millisecond)
		addTask(t, db, "more")
		if _, err := m.Snapshot(ctx, "periodic"); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := List(m.Dir())
	if len(list) != 2 || list[0].Reason != "periodic" || !list[0].Taken.After(list[1].Taken) {
		t.Fatalf("want the newest 2 snapshots, newest first: %+v", list)
	}
}

func TestASnapshotRestoresTheDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "p.db")
	db := openDB(t, path)
	id := addTask(t, db, "keep me")
	snap, err := Take(ctx, db, filepath.Join(dir, "backups"), "manual")
	if err != nil {
		t.Fatal(err)
	}
	addTask(t, db, "after the snapshot")
	_ = db.Close()

	if err := Restore(snap, path); err != nil {
		t.Fatal(err)
	}
	db = openDB(t, path)
	defer db.Close()
	tasks, _ := db.Storage().Tasks.List(ctx)
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Errorf("the restored database should hold just the snapshot's task: %+v", tasks)
	}
}

func TestAnArchiveRestoresTheProject(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "p.db")
	db := openDB(t, path)
	addTask(t, db, "archived")
	out := filepath.Join(dir, "archives", "a.zip")
	if err := Archive(ctx, db, []string{filepath.Join(dir, "missing.json")}, out); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	other := filepath.Join(t.TempDir(), "p.db")
	if err := RestoreArchive(out, other); err != nil {
		t.Fatal(err)
	}
	db2 := openDB(t, other)
	defer db2.Close()
	if tasks, _ := db2.Storage().Tasks.List(ctx); len(tasks) != 1 || tasks[0].Title != "archived" {
		t.Errorf("the archive should bring back the tasks: %+v", tasks)
	}
}
