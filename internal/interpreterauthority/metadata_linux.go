//go:build linux

package interpreterauthority

import (
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func sameSnapshot(a, b os.FileInfo) bool {
	x, y := a.Sys().(*syscall.Stat_t), b.Sys().(*syscall.Stat_t)
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime() == b.ModTime() &&
		x.Uid == y.Uid && x.Gid == y.Gid && x.Nlink == y.Nlink && x.Ctim == y.Ctim
}

func noACL(name string) error {
	for _, key := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		_, err := unix.Lgetxattr(name, key, nil)
		if err != unix.ENODATA && err != unix.ENOTSUP {
			return refusal("authority_acl_unsupported")
		}
	}
	return nil
}
