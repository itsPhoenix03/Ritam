//go:build windows

package fsutil

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes an exclusive, non-blocking lock on f, the same way the
// database lock does. Windows drops it when the handle closes, including
// when the process dies.
func tryLock(f *os.File) error {
	ol := windows.Overlapped{OffsetHigh: 1}
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}

// SyncDir does nothing on Windows: a folder cannot be opened for flushing
// there, and NTFS journals the rename itself.
func SyncDir(string) {}
