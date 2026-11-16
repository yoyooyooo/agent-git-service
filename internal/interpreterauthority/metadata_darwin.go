//go:build darwin

package interpreterauthority

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func sameSnapshot(a, b os.FileInfo) bool {
	x, y := a.Sys().(*syscall.Stat_t), b.Sys().(*syscall.Stat_t)
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime() == b.ModTime() &&
		x.Uid == y.Uid && x.Gid == y.Gid && x.Nlink == y.Nlink && x.Ctimespec == y.Ctimespec
}

// /bin/ls is an explicit sealed-system metadata reader in the Darwin TCB.
// No PATH lookup, caller environment, shell or interpreting candidate is used.
// Version one admits no ACL; it does not infer ACL absence from mode bits.
func noACL(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/ls", "-lde", name)
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C"}
	raw, err := command.Output()
	if err != nil || len(raw) > 65536 {
		return refusal("authority_metadata_unavailable")
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 1 {
		return refusal("authority_acl_unsupported")
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 || len(fields[0]) < 10 || strings.Contains(fields[0], "+") {
		return refusal("authority_acl_unsupported")
	}
	return nil
}
