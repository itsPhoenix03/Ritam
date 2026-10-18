// Package backup keeps copies of each project's database in the app home.
//
// A project's database lives in the project, so deleting the project folder
// (or just its hidden folder) would lose its history. The Manager writes
// compressed snapshots to <app home>/backups/<project>/: when a task
// finishes, when the app exits, every so often while work is going on, and
// before the database is upgraded. A snapshot is skipped when nothing has
// changed since the last one, so an idle project costs nothing, and only the
// newest few are kept.
//
// Snapshots are taken with SQLite's VACUUM INTO, never by copying the file:
// a copy of a live database misses whatever is still in its write-ahead log.
package backup

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"ritam/internal/logging"
	"ritam/internal/storage"
)

// suffix ends every snapshot's file name.
const suffix = ".db.gz"

// stamp is the time format that starts a snapshot's name, so names sort in
// time order.
const stamp = "20060102-150405.000"

// Manager takes and prunes one project's snapshots.
type Manager struct {
	db   *storage.SQLite
	dir  string
	keep int
	log  *logging.Logger

	mu   sync.Mutex
	last int64 // the database's change count at the last snapshot
}

// New makes a manager writing to dir and keeping the newest keep snapshots
// (at least one).
func New(db *storage.SQLite, dir string, keep int, log *logging.Logger) *Manager {
	if keep < 1 {
		keep = 1
	}
	if log == nil {
		log = logging.NewDiscard()
	}
	return &Manager{db: db, dir: dir, keep: keep, log: log}
}

// Dir is where the snapshots go.
func (m *Manager) Dir() string { return m.dir }

// Snapshot takes a snapshot unless nothing has changed since the last one.
// It returns the snapshot's path, or "" when it was skipped.
func (m *Manager) Snapshot(ctx context.Context, reason string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.db.Changes(ctx)
	if err != nil {
		return "", err
	}
	if n == m.last {
		return "", nil
	}
	path, err := Take(ctx, m.db, m.dir, reason)
	if err != nil {
		return "", err
	}
	m.last = n
	if err := Prune(m.dir, m.keep); err != nil {
		m.log.Warn("prune backups", "dir", m.dir, "error", err)
	}
	m.log.Info("backup taken", "reason", reason, "file", path)
	return path, nil
}

// Run takes a snapshot every interval until ctx ends. Unchanged databases
// are skipped, so this costs nothing while the project is idle.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := m.Snapshot(ctx, "periodic"); err != nil && ctx.Err() == nil {
				m.log.Warn("periodic backup", "error", err)
			}
		}
	}
}

// Take writes one compressed snapshot of db into dir, whatever has changed.
func Take(ctx context.Context, db *storage.SQLite, dir, reason string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	name := time.Now().Format(stamp) + "-" + cleanReason(reason)
	raw := filepath.Join(dir, "."+name+".db.tmp")
	_ = os.Remove(raw)
	if err := db.Snapshot(ctx, raw); err != nil {
		return "", err
	}
	defer os.Remove(raw)
	out := filepath.Join(dir, name+suffix)
	if err := gzipFile(raw, out); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	return out, nil
}

// Snapshot describes one snapshot file.
type Snapshot struct {
	Path   string
	Taken  time.Time
	Reason string
	Size   int64
}

// List returns the snapshots in dir, newest first.
func List(dir string) ([]Snapshot, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, suffix) || len(name) < len(stamp) {
			continue
		}
		taken, err := time.ParseInLocation(stamp, name[:len(stamp)], time.Local)
		if err != nil {
			continue
		}
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		reason := strings.TrimSuffix(strings.TrimPrefix(name[len(stamp):], "-"), suffix)
		out = append(out, Snapshot{Path: filepath.Join(dir, name), Taken: taken, Reason: reason, Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path > out[j].Path })
	return out, nil
}

// Prune deletes all but the newest keep snapshots in dir.
func Prune(dir string, keep int) error {
	list, err := List(dir)
	if err != nil {
		return err
	}
	var errs []error
	for i := keep; i < len(list); i++ {
		if err := os.Remove(list[i].Path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Restore replaces the database at dbPath with a snapshot. The database must
// not be open. What it replaces is not lost: the caller takes a snapshot of
// it first (see the CLI's restore command).
func Restore(snapshot, dbPath string) error {
	tmp := dbPath + ".restoring"
	if err := gunzipFile(snapshot, tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("restore: %w", err)
	}
	for _, f := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(tmp)
			return fmt.Errorf("restore: %w", err)
		}
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}

// Archive writes a zip at out holding a snapshot of db (as its base name)
// and copies of the given files that exist, for keeping a project's tasks
// before they are cleared.
func Archive(ctx context.Context, db *storage.SQLite, files []string, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return err
	}
	raw := out + ".db.tmp"
	_ = os.Remove(raw)
	if err := db.Snapshot(ctx, raw); err != nil {
		return err
	}
	defer os.Remove(raw)

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	add := func(src, name string) error {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, in)
		return err
	}
	err = add(raw, filepath.Base(db.Path()))
	for _, src := range files {
		if err != nil {
			break
		}
		if _, statErr := os.Stat(src); statErr == nil {
			err = add(src, filepath.Base(src))
		}
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(out)
	}
	return err
}

func cleanReason(r string) string {
	r = strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			return c
		case c >= 'A' && c <= 'Z':
			return c + ('a' - 'A')
		}
		return '-'
	}, r)
	if r == "" {
		return "manual"
	}
	return r
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func gunzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer zr.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, zr)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// RestoreArchive puts an archive made by Archive back into a project: the
// database to dbPath and the other files into dbPath's folder. Like Restore,
// the database must not be open.
func RestoreArchive(archive, dbPath string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	defer zr.Close()
	dir := filepath.Dir(dbPath)
	for _, f := range zr.File {
		name := filepath.Base(f.Name) // never a path out of the folder
		dst := filepath.Join(dir, name)
		isDB := name == filepath.Base(dbPath)
		if isDB {
			dst = dbPath + ".restoring"
		}
		if err := extract(f, dst); err != nil {
			return fmt.Errorf("restore: %w", err)
		}
		if isDB {
			for _, old := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
				if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("restore: %w", err)
				}
			}
			if err := os.Rename(dst, dbPath); err != nil {
				return fmt.Errorf("restore: %w", err)
			}
		}
	}
	return nil
}

func extract(f *zip.File, dst string) error {
	in, err := f.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
