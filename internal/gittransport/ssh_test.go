package gittransport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedSSHStrictTrustAndPrivatePathValidation(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "identity with 'quote'")
	hosts := filepath.Join(root, "known_hosts")
	os.WriteFile(key, []byte("synthetic private material"), 0600)
	os.WriteFile(hosts, []byte("pinned host key"), 0600)
	cfg := SSHConfig{IdentityFile: key, KnownHostsFile: hosts}
	command, err := cfg.command()
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"StrictHostKeyChecking=yes", "IdentityAgent=none", "IdentitiesOnly=yes", "GlobalKnownHostsFile=/dev/null", "UpdateHostKeys=no", "-F /dev/null"} {
		if !strings.Contains(command, option) {
			t.Fatal("missing trust isolation", option)
		}
	}
	if strings.Contains(command, "synthetic private material") || strings.Contains(command, "StrictHostKeyChecking=no") {
		t.Fatal("unsafe SSH command")
	}
	os.Chmod(key, 0644)
	if _, err := cfg.command(); err == nil {
		t.Fatal("public private key accepted")
	}
	os.Chmod(key, 0600)
	cfg.KnownHostsFile = "relative"
	if _, err := cfg.command(); err == nil {
		t.Fatal("relative trust file accepted")
	}
	if _, err := RunSSH(context.Background(), "https://forgejo.example.test/repo.git", SSHConfig{}, "ls-remote", "https://forgejo.example.test/repo.git"); err == nil {
		t.Fatal("HTTP accepted by SSH transport")
	}
}
