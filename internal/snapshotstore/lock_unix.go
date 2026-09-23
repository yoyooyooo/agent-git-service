//go:build darwin || linux

package snapshotstore

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Kernel ownership is released on process death; no stale PID lock needs to
// be force-removed. Replacing/removing the lock file while open is forbidden.
func lockRoot(root string) (*os.File, error) {
	path := filepath.Join(root, ".owner.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("snapshot root is already owned by another process")
	}
	return f, nil
}

func unlockRoot(f *os.File) error {
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
