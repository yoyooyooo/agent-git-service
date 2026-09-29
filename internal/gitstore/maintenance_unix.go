//go:build linux || darwin

package gitstore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func configureMaintenanceCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Let native Git remove its own temporary/lock files before escalation.
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
}

// WaitDelay bounds a process that ignores TERM; finish cancellation by
// killing its process group before the repository admission lease is released.
func finishMaintenanceCommand(cmd *exec.Cmd, cancelled bool) {
	if cancelled && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func maintenanceSpaceAvailable(dir string, stats StorageStats) error {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return errors.New("maintenance free space unavailable")
	}
	// Packing needs a new pack before removing the old one. This is a bounded
	// observation, not a reservation against unrelated processes.
	kib := stats.PackKiB + stats.LooseKiB
	if kib < 0 || kib > (1<<60)/1024 {
		return errors.New("maintenance storage size exceeds budget")
	}
	required := uint64(kib)*1024 + 64*1024*1024
	if required < 512*1024*1024 {
		required = 512 * 1024 * 1024
	}
	if uint64(fs.Bavail)*uint64(fs.Bsize) < required {
		return errors.New("maintenance free space insufficient")
	}
	return nil
}

func lockRepositoryMaintenance(dir string) (func(), error) {
	// The configured parent may be an OS alias (macOS /var -> /private/var).
	// Never accept a symlink at the repository or object-store boundary itself.
	for _, p := range []string{dir, filepath.Join(dir, "objects"), filepath.Join(dir, "objects", "pack")} {
		st, err := os.Lstat(p)
		if os.IsNotExist(err) && p == filepath.Join(dir, "objects", "pack") {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("maintenance refuses symlinked repository storage")
		}
	}
	path := filepath.Join(dir, "ags-maintenance.lock")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("maintenance lock is not a regular file")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrMaintenanceBusy
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
}
