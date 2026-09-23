//go:build !darwin && !linux

package snapshotstore

import (
	"errors"
	"os"
)

func lockRoot(string) (*os.File, error) {
	return nil, errors.New("snapshot storage currently requires Linux or macOS process locking")
}
func unlockRoot(f *os.File) error { return f.Close() }
