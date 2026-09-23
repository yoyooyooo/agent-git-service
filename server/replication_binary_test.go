package server

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

func TestPrimaryExecutableWithConfiguredPeerAndRealEdgeClone(t *testing.T) {
	f := newReplicationRuntimeFixture(t)
	binary := filepath.Join(t.TempDir(), "gh-server-test")
	build := exec.Command("go", "build", "-o", binary, "./cmd/gh-server")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	start := func() (*exec.Cmd, <-chan error, *bytes.Buffer) {
		cmd := exec.Command(binary)
		cmd.Dir = f.root
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.root, "TMPDIR=" + os.TempDir(), "DB_DSN=" + f.cfg.DBdsn, "GIT_REPO_DIR=" + f.cfg.GitRepoDir, "BASE_URL=" + f.cfg.BaseURL, "PORT=" + f.cfg.Port, "LISTEN_MODE=production", "ENVIRONMENT=production", "AGS_REPLICATION_CONFIG_FILE=" + f.path}
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		return cmd, done, &output
	}
	cmd, done, output := start()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	peer := f.peer(t)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := peer.PrepareRead(context.Background(), "edge-1", "Bearer runtime-original-test-only", f.request()); err == nil {
			break
		}
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("primary exited during startup: %v\n%s", err, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("primary peer listener did not become usable")
		}
		time.Sleep(30 * time.Millisecond)
	}
	mirror, err := edge.NewMirror(context.Background(), edge.MirrorConfig{Root: filepath.Join(f.root, "edge-cache"), MaxPackBytes: 16 << 20, Concurrency: 1, MaxPending: 4, SyncTimeout: 20 * time.Second}, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()
	reader, err := edge.NewReadRuntime(edge.ReadRuntimeConfig{EdgeID: "edge-1", Bindings: []edge.ReadBinding{{Repository: "peer-owner/project", Identity: f.identity}}, Concurrency: 2, RecentViews: 4, RecentWindow: time.Minute}, peer, mirror)
	if err != nil {
		t.Fatal(err)
	}
	ingress := httptest.NewUnstartedServer(nil)
	_, port, _ := net.SplitHostPort(ingress.Listener.Addr().String())
	canonical := "http://ags.binary.test:" + port
	edgeServer, err := edge.New(config.EdgeConfig{ID: "edge-1", PrimaryURL: f.cfg.BaseURL, CanonicalURL: canonical}, edge.WithReadRuntime(reader))
	if err != nil {
		t.Fatal(err)
	}
	ingress.Config.Handler = edgeServer
	ingress.Start()
	defer ingress.Close()
	clone := filepath.Join(t.TempDir(), "clone")
	remote := canonical + "/peer-owner/project.git"
	git := func(dir string, args ...string) string {
		t.Helper()
		prefix := []string{"-c", "http.proxy=", "-c", "http.curloptResolve=ags.binary.test:" + port + ":127.0.0.1", "-c", "http.extraHeader=Authorization: Bearer runtime-original-test-only", "-c", "protocol.version=2"}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "git", append(prefix, args...)...)
		c.Dir = dir
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Replication Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Replication Test", "GIT_COMMITTER_EMAIL=test@example.test"}
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("Git operation failed: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "clone", "--depth=1", remote, clone)
	if git(clone, "remote", "get-url", "origin") != remote {
		t.Fatal("canonical remote changed")
	}
	git(clone, "checkout", "-b", "binary-acceptance")
	git(clone, "commit", "--allow-empty", "-m", "real primary process acceptance")
	head := git(clone, "rev-parse", "HEAD")
	git(clone, "push", "origin", "HEAD:refs/heads/binary-acceptance")
	second := filepath.Join(t.TempDir(), "second")
	git("", "clone", "--branch", "binary-acceptance", remote, second)
	if git(second, "rev-parse", "HEAD") != head {
		t.Fatal("read after process-backed push was stale")
	}
	ingress.Close()
	if err := mirror.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("primary shutdown failed: %v\n%s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("primary shutdown did not drain")
	}
	store, err := snapshotstore.Open(filepath.Join(f.root, "retained"), 0)
	if err != nil {
		t.Fatal("process leaked retained lock", err)
	}
	store.Close()
	// Exercise the executable's bind-failure path, not just Server.Start.
	occupied, err := net.Listen("tcp", f.file.ListenAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	bad, badDone, badOutput := start()
	select {
	case err := <-badDone:
		if err == nil {
			t.Fatal("occupied peer listener did not fail the process")
		}
	case <-time.After(15 * time.Second):
		_ = bad.Process.Kill()
		<-badDone
		t.Fatalf("partial startup remained alive\n%s", badOutput.String())
	}
	public, err := net.Listen("tcp", strings.TrimPrefix(f.cfg.BaseURL, "http://"))
	if err != nil {
		t.Fatal("failed process retained public port", err)
	}
	public.Close()
	store, err = snapshotstore.Open(filepath.Join(f.root, "retained"), 0)
	if err != nil {
		t.Fatal("failed process retained snapshot lock", err)
	}
	store.Close()
}
