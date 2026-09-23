package forgejointegration

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteURLDoesNotAttachHTTPTokenToOtherTransports(t *testing.T) {
	for _, base := range []string{"file:///tmp/provider-fixture", "ssh://git@provider.example.test"} {
		t.Run(strings.Split(base, ":")[0], func(t *testing.T) {
			remote, err := (Config{BaseURL: base, Token: "fixture-http-token"}).authenticatedRemoteURL(TargetRepo{Owner: "team", Repo: "project"})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(remote)
			if err != nil || strings.Contains(remote, "fixture-http-token") {
				t.Fatal("HTTP credential entered non-HTTP URL")
			}
			if parsed.Scheme == "file" && parsed.User != nil {
				t.Fatal("file URL acquired userinfo")
			}
			if parsed.Scheme == "ssh" && (parsed.User == nil || parsed.User.Username() != "git") {
				t.Fatal("operator SSH user changed")
			}
		})
	}
}

func TestLocalProviderURLWorksWithNativeRefInspection(t *testing.T) {
	root := t.TempDir()
	owner := filepath.Join(root, "team")
	if err := os.Mkdir(owner, 0o700); err != nil {
		t.Fatal(err)
	}
	remotePath := filepath.Join(owner, "project.git")
	command := exec.Command("git", "init", "--bare", remotePath)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	base := (&url.URL{Scheme: "file", Path: root}).String()
	remote, err := (Config{BaseURL: base, Token: "unused-http-token"}).authenticatedRemoteURL(TargetRepo{Owner: "team", Repo: "project"})
	if err != nil {
		t.Fatal(err)
	}
	sha, err := GitRemoteRefSHA(context.Background(), root, remote, "refs/heads/main")
	if err != nil || sha != "" {
		t.Fatalf("empty local repository read failed: %v", err)
	}
}
