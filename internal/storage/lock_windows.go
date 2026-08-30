//go:build windows

package storage

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes an exclusive, non-blocking lock on f. Windows drops it when
// the handle closes, including when the process dies. The locked byte lies
// far past the end of the file, so the pid written at its start stays
// readable to a second instance reporting who holds it.
func tryLock(f *os.File) error {
	ol := windows.Overlapped{OffsetHigh: 1}
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}
