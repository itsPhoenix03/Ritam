//go:build !windows

package storage

import (
	"os"
	"syscall"
)

// tryLock takes an exclusive, non-blocking lock on f. The kernel drops it
// when the descriptor closes, including when the process dies.
func tryLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
