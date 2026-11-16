//go:build linux

package routebstage0

import (
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/ngaut/agent-git-service/internal/interpreterauthority"
)

// The fixed Python entry calls getpwuid to select its managed root. Require
// the registered account to succeed in the frozen files backend before any
// unmodeled NSS module or nscd cache can affect that authorization input.
func accountResourcePolicy(passwd, nss []byte, host HostProfile) error {
	found := 0
	for _, line := range strings.Split(string(nss), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		key, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "passwd" {
			continue
		}
		found++
		services := strings.Fields(rest)
		if len(services) == 0 || services[0] != "files" || strings.ContainsAny(rest, "[]\\\x00") {
			return refused("stage0_nss_policy_unsupported")
		}
	}
	if found != 1 {
		return refused("stage0_nss_policy_unsupported")
	}
	found = 0
	for _, line := range strings.Split(string(passwd), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 7 {
			return refused("stage0_passwd_format_unsupported")
		}
		if parts[0] == host.User || parts[2] == strconv.FormatUint(uint64(host.UID), 10) {
			if parts[0] != host.User || parts[2] != strconv.FormatUint(uint64(host.UID), 10) || parts[3] != strconv.FormatUint(uint64(host.GID), 10) || parts[5] != host.Home {
				return refused("stage0_account_changed")
			}
			found++
		}
	}
	if found != 1 {
		return refused("stage0_account_changed")
	}
	return nil
}

func (l *loaded) verifyAccountResources() error {
	known := l.catalog.Files()
	content := map[string][]byte{}
	for _, path := range []string{"/etc/passwd", "/etc/nsswitch.conf"} {
		file, ok := known[path]
		if !ok {
			return refused("stage0_account_resource_unregistered")
		}
		raw, err := interpreterauthority.ReadProtected(path, 1<<20)
		if err != nil {
			return err
		}
		if hash(raw) != file.SHA256 {
			return refused("stage0_account_resource_changed")
		}
		content[path] = raw
	}
	if err := accountResourcePolicy(content["/etc/passwd"], content["/etc/nsswitch.conf"], l.host); err != nil {
		return err
	}
	if err := interpreterauthority.ProtectedDirectory("/run", false); err != nil {
		return err
	}
	if err := interpreterauthority.ProtectedDirectory("/var", false); err != nil {
		return err
	}
	info, err := os.Lstat("/var/run")
	if err != nil || info.Mode()&os.ModeSymlink == 0 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		return refused("stage0_nscd_path_unknown")
	}
	link, err := os.Readlink("/var/run")
	if err != nil || link != "/run" {
		return refused("stage0_nscd_path_unknown")
	}
	if _, err = os.Lstat("/run/nscd"); !os.IsNotExist(err) {
		return refused("stage0_nscd_cache_unsupported")
	}
	return nil
}
