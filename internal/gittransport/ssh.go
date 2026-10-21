package gittransport

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// SSHConfig selects one AGS-managed identity with explicit pinned host trust.
// Only paths cross the process boundary; private bytes never enter argv/env.
type SSHConfig struct{ IdentityFile, KnownHostsFile string }

func (c SSHConfig) command() (string, error) {
	for _, path := range []string{c.IdentityFile, c.KnownHostsFile} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
			return "", errors.New("invalid managed SSH file path")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return "", errors.New("managed SSH file unavailable")
		}
		if path == c.IdentityFile && info.Mode().Perm() != 0600 {
			return "", errors.New("managed SSH identity must have mode 0600")
		}
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	return "ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none -o StrictHostKeyChecking=yes -o GlobalKnownHostsFile=/dev/null -o UpdateHostKeys=no -o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o ConnectTimeout=10 -i " + quote(c.IdentityFile) + " -o " + quote("UserKnownHostsFile="+c.KnownHostsFile), nil
}

func RunSSH(ctx context.Context, remoteURL string, cfg SSHConfig, args ...string) ([]byte, error) {
	u, err := url.Parse(remoteURL)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" || u.User == nil {
		return nil, errors.New("managed SSH requires SSH remote")
	}
	if _, ok := u.User.Password(); ok {
		return nil, errors.New("managed SSH remote cannot contain credentials")
	}
	return run(ctx, remoteURL, "", &cfg, args...)
}
