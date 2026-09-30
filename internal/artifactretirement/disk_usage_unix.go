//go:build linux || darwin

package artifactretirement

import (
	"os"
	"path/filepath"
	"syscall"
)

// DiskKiB matches the filesystem-block semantics used by du closely enough for
// before/after receipts; it is intentionally different from Git object raw size.
func DiskKiB(path string) (int64, error) {
	var bytes int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			bytes += stat.Blocks * 512
		}
		return nil
	})
	return (bytes + 1023) / 1024, err
}
