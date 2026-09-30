//go:build !linux && !darwin

package artifactretirement

import (
	"os"
	"path/filepath"
)

// DiskKiB falls back to logical file bytes on unsupported development hosts.
// Production artifact retirement is currently exercised on Linux/Darwin.
func DiskKiB(path string) (int64, error) {
	var bytes int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			bytes += info.Size()
		}
		return nil
	})
	return (bytes + 1023) / 1024, err
}
