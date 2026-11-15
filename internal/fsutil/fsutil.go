// Package fsutil holds the two file habits ritam's small shared files need:
// a lock that holds across processes, and a write that survives a power cut.
//
// A sync.Mutex only orders the goroutines of one process, while the app
// home is shared by every ritam on the machine. A rename without fsync can
// leave an empty file behind after a power cut on most file systems: the
// rename reaches the disk before the data does.
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Lock takes an exclusive lock on the file at path (created if needed) and
// returns the function that releases it. Another process holding it is
// waited for, up to wait, so a crowd of instances queues rather than fails;
// past that it is an error rather than an endless hang. The OS drops the
// lock when its holder dies, so a crash never leaves it stuck.
func Lock(path string, wait time.Duration) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	deadline := time.Now().Add(wait)
	pause := time.Millisecond
	for {
		err := tryLock(f)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: still held by another process after %s: %w", path, wait, err)
		}
		time.Sleep(pause)
		if pause < 20*time.Millisecond {
			pause *= 2
		}
	}
}

// WriteAtomic replaces the file at path with data: a uniquely named
// temporary file beside it, flushed to disk, renamed over the old one, and
// the folder flushed too where the platform allows. A crash at any point
// leaves either the old file or the new one, never an empty one, and two
// writers never share a temporary file. The file is readable by its owner
// only.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	SyncDir(dir)
	return nil
}

// SetAside renames a file ritam cannot read to <path>.bad-<time>, so it
// is kept for a human to look at, and returns the new name.
func SetAside(path string) (string, error) {
	bad := path + ".bad-" + time.Now().Format("20060102-150405.000")
	if err := os.Rename(path, bad); err != nil {
		return "", err
	}
	return bad, nil
}
