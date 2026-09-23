package gittransport

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func nativeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = processEnvironment()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture Git %s failed: %v: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitFixture(t *testing.T) (root, source, backend string) {
	t.Helper()
	root = t.TempDir()
	source = filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	nativeGit(t, source, "init", "-b", "main")
	nativeGit(t, source, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-m", "fixture")
	nativeGit(t, root, "init", "--bare", "-b", "main", "remote.git")
	nativeGit(t, filepath.Join(root, "remote.git"), "config", "http.receivepack", "true")
	backend = filepath.Join(nativeGit(t, root, "--exec-path"), "git-http-backend")
	return
}

func TestRealGitPushReadAndFetchUseTransientAuthentication(t *testing.T) {
	root, source, backend := gitFixture(t)
	secret := "fixture-provider-token-with-special-characters:@ /%"
	encoded := base64.StdEncoding.EncodeToString([]byte("oauth2:" + secret))
	var accepted, rejected atomic.Int32
	handler := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=fixture"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+encoded {
			rejected.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(401)
			return
		}
		accepted.Add(1)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	remote, _ := url.Parse(server.URL + "/remote.git")
	remote.User = url.UserPassword("oauth2", secret)
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("AGS_UNRELATED_TOKEN", "unrelated-secret-must-not-reach-child")
	if _, err := Run(context.Background(), remote.String(), secret, "-C", source, "push", remote.String(), "refs/heads/main:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	want := nativeGit(t, source, "rev-parse", "HEAD")
	out, err := Run(context.Background(), remote.String(), "", "-C", source, "ls-remote", remote.String(), "refs/heads/main")
	if err != nil || !strings.Contains(string(out), want) {
		t.Fatalf("remote ref not preserved: %v", err)
	}
	bare := filepath.Join(root, "destination.git")
	nativeGit(t, root, "init", "--bare", bare)
	if _, err := Run(context.Background(), remote.String(), "", "-C", bare, "fetch", "--no-tags", remote.String(), want); err != nil {
		t.Fatal(err)
	}
	if got := nativeGit(t, bare, "rev-parse", "FETCH_HEAD"); got != want {
		t.Fatal("fetch did not retain the exact requested commit")
	}
	if rejected.Load() != 0 || accepted.Load() < 3 {
		t.Fatalf("unexpected authentication negotiation: accepted=%d rejected=%d", accepted.Load(), rejected.Load())
	}
	for _, repo := range []string{filepath.Join(source, ".git"), bare} {
		config, err := os.ReadFile(filepath.Join(repo, "config"))
		if err != nil || strings.Contains(string(config), secret) || strings.Contains(string(config), encoded) || strings.Contains(string(config), "http-auth") {
			t.Fatal("transient credentials entered repository configuration")
		}
	}
	entries, err := os.ReadDir(os.Getenv("TMPDIR"))
	if err != nil || len(entries) != 0 {
		t.Fatal("authentication files survived successful command completion")
	}
}

func TestRedirectNeverReceivesCredential(t *testing.T) {
	_, source, _ := gitFixture(t)
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetRequests.Add(1); w.WriteHeader(401) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/other.git/info/refs", 302)
	}))
	defer origin.Close()
	remote := origin.URL + "/remote.git"
	_, err := Run(context.Background(), remote, "redirect-fixture", "-C", source, "ls-remote", remote)
	if err == nil || targetRequests.Load() != 0 {
		t.Fatalf("redirect was followed: error=%v target_requests=%d", err, targetRequests.Load())
	}
}

func TestProcessBoundaryHasNoCredentialOrInheritedGitOverrides(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-observation fixture")
	}
	root := t.TempDir()
	argvFile, envFile := filepath.Join(root, "argv"), filepath.Join(root, "environment")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quote(argvFile) + "\nenv > " + quote(envFile) + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(root, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("GIT_TRACE", "unsafe-trace-target")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.extraHeader")
	t.Setenv("GIT_CONFIG_VALUE_0", "unrelated-header-secret")
	t.Setenv("PRIMARY_ANOTHER_PROVIDER_TOKEN", "unrelated-service-secret")
	secret := "process-fixture-secret"
	remote := "https://oauth2:" + secret + "@provider.example.test/team/repository.git"
	if _, err := Run(context.Background(), remote, secret, "-C", root, "push", "--force-with-lease=refs/heads/work:"+strings.Repeat("a", 40), remote, "refs/heads/work:refs/heads/work"); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(argvFile)
	environment, _ := os.ReadFile(envFile)
	for _, forbidden := range []string{secret, base64.StdEncoding.EncodeToString([]byte("oauth2:" + secret)), "unrelated-header-secret", "unrelated-service-secret", "unsafe-trace-target"} {
		if strings.Contains(string(argv), forbidden) || strings.Contains(string(environment), forbidden) {
			t.Fatal("credential or tracing configuration reached the process boundary")
		}
	}
	if !strings.Contains(string(argv), "https://provider.example.test/team/repository.git") || !strings.Contains(string(argv), "--force-with-lease=refs/heads/work:") {
		t.Fatal("clean remote or exact lease was lost")
	}
	for _, arg := range strings.Split(string(argv), "\n") {
		if strings.HasPrefix(arg, "include.path=") {
			if _, err := os.Stat(strings.TrimPrefix(arg, "include.path=")); !os.IsNotExist(err) {
				t.Fatal("authentication file survived command completion")
			}
		}
	}
}

func TestCancellationAndErrorCannotExposeOrRetainCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-observation fixture")
	}
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			root, staging := t.TempDir(), t.TempDir()
			secret := "failure-fixture-secret"
			script := "#!/bin/sh\nprintf 'remote: " + secret + "\\n' >&2\nexit 1\n"
			if cancel {
				script = "#!/bin/sh\nexec sleep 60\n"
			}
			if err := os.WriteFile(filepath.Join(root, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TMPDIR", staging)
			ctx, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer stop()
			remote := "https://provider.example.test/team/repository.git"
			_, err := Run(ctx, remote, secret, "-C", root, "push", remote, "main:main")
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsafe error: %v", err)
			}
			if cancel && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("cancellation cause was lost")
			}
			entries, readErr := os.ReadDir(staging)
			if readErr != nil || len(entries) != 0 {
				t.Fatal("private files survived failed command")
			}
		})
	}
}

func TestCredentialValidationAndNonHTTPCompatibility(t *testing.T) {
	for _, raw := range []string{"https://user:pass@provider.example.test/repo.git?token=x", "https://user:pass@provider.example.test/repo.git#fragment", "ssh://user:pass@provider.example.test/repo.git", "file://user:pass@/repo.git", "ext::unsafe", "--upload-pack=unsafe", "https://user:%0apass@provider.example.test/repo.git"} {
		if _, _, _, err := credentialParts(raw, ""); err == nil {
			t.Fatalf("unsafe remote admitted: %q", raw)
		}
	}
	for _, raw := range []string{"git@provider.example.test:team/repository.git", "ssh://git@provider.example.test/team/repository.git", "file:///tmp/fixture.git", "/tmp/local fixture.git"} {
		clean, _, password, err := credentialParts(raw, "unused-http-token")
		if err != nil || clean != raw || password != "" {
			t.Fatalf("non-HTTP compatibility changed: %q %v", raw, err)
		}
	}
	if _, _, _, err := credentialParts("https://user:one@provider.example.test/repo.git", "different"); err == nil {
		t.Fatal("mismatched credential sources admitted")
	}
	clean, _, password, err := credentialParts("https://[::1]:6666/repo.git", "ipv6-fixture")
	if err != nil || clean != "https://[::1]:6666/repo.git" || password != "ipv6-fixture" {
		t.Fatalf("IPv6 support changed: %v", err)
	}
}
