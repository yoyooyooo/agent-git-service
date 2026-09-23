//go:build linux || darwin

package snapshotstore

import (
	"errors"
	"syscall"
)

func availableBytes(path string) (uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, err
	}
	if fs.Bsize <= 0 || uint64(fs.Bavail) > ^uint64(0)/uint64(fs.Bsize) {
		return 0, errors.New("invalid filesystem capacity")
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), nil
}
