//go:build !windows

package fsutil

import (
	"os"
	"syscall"
)

// tryLock takes an exclusive, non-blocking lock on f. The kernel drops it
// when the descriptor closes, including when the process dies.
func tryLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// SyncDir flushes a folder, so a file just renamed into it is still there
// after a power cut. It is best effort: some file systems refuse it, and
// the data itself was already flushed.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
