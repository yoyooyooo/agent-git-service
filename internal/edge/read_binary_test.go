package edge_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edge"
)

// Build/start the actual entrypoint with explicit file-backed node config,
// clone using only the canonical remote and curloptResolve, then terminate
// cleanly and prove the snapshot root can be owned by a new process/runtime.
func TestEdgeExecutableConfiguredReadAndGracefulShutdown(t *testing.T) {
	f := newControlFixture(t)
	configPath := writeReadConfig(t, f)
	binary := filepath.Join(t.TempDir(), "ags-edge")
	build := exec.Command("go", "build", "-o", binary, "./cmd/ags-edge")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Edge: %v %s", err, out)
	}
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserve.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	if err := reserve.Close(); err != nil {
		t.Fatal(err)
	}
	canonical := "http://ags.binary.test:" + port
	cmd := exec.Command(binary)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "AGS_EDGE_ID=edge-fixture-test", "AGS_EDGE_LISTEN_ADDR=" + address, "AGS_EDGE_PRIMARY_URL=http://127.0.0.1:1", "AGS_EDGE_CANONICAL_URL=" + canonical, "AGS_EDGE_READ_CONFIG_FILE=" + configPath}
	var logs bytes.Buffer
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var runErr error
	go func() { runErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("Edge did not drain")
		}
		if runErr != nil {
			t.Errorf("Edge exit: %v %s", runErr, logs.String())
		}
	})
	client := &http.Client{Timeout: time.Second}
	live := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		select {
		case <-done:
			t.Fatalf("Edge exited before liveness: %v %s", runErr, logs.String())
		default:
		}
		resp, err := client.Get("http://" + address + "/livez")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				live = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !live {
		t.Fatal("Edge never became live")
	}
	dir := filepath.Join(t.TempDir(), "clone")
	remote := canonical + "/" + f.repo.FullName + ".git"
	replicaGit(t, "", "", "-c", "http.curloptResolve=ags.binary.test:"+port+":127.0.0.1", "-c", "http.extraHeader=Authorization: Bearer "+f.token, "-c", "protocol.version=2", "clone", remote, dir)
	if got := replicaGit(t, dir, "", "remote", "get-url", "origin"); got != remote {
		t.Fatalf("binary changed remote: %s", got)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Edge shutdown timeout")
	}
	if runErr != nil {
		t.Fatalf("Edge shutdown failed: %v %s", runErr, logs.String())
	}
	reopened, err := edge.OpenReadResources(context.Background(), "edge-fixture-test", configPath)
	if err != nil {
		t.Fatalf("Edge left snapshot owner/leases behind: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
