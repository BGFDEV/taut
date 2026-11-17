//go:build unix

package local

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive advisory lock on f without waiting. The lock
// belongs to the open file, so the kernel drops it when the process exits,
// however it exits: a killed process never leaves the state directory
// locked.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
