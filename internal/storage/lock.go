package storage

import (
	"fmt"
	"os"
	"strconv"
)

// ErrLocked is returned when another ritam already has the database open.
type ErrLocked struct{ Path, Holder string }

func (e *ErrLocked) Error() string {
	holder := ""
	if e.Holder != "" {
		holder = " (pid " + e.Holder + ")"
	}
	return fmt.Sprintf("%s is in use by another ritam%s; close it first, or point storage.path elsewhere", e.Path, holder)
}

// lockDatabase makes this process the database's only user. Two ritam
// instances on one database would each think the other's running tasks were
// interrupted, and drive the same agent sessions at once.
func lockDatabase(dbPath string) (release func(), err error) {
	path := dbPath + ".lock"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open lock %s: %w", path, err)
	}
	if err := tryLock(f); err != nil {
		holder, _ := os.ReadFile(path)
		_ = f.Close()
		return nil, &ErrLocked{Path: dbPath, Holder: string(holder)}
	}
	// The pid is for the error message only; the OS lock is what counts.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return func() { _ = f.Close() }, nil
}
