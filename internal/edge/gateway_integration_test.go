package edge_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestHostGatewayKeepsUnregisteredRepositoriesUsableWithoutGrantingReplication(t *testing.T) {
	f := newControlFixture(t)
	other, err := f.svc.CreateRepo(f.ctx, service.CreateRepoInput{OwnerLogin: f.owner.Login, Name: "unregistered", Private: true, AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	mirror := newReplicaMirror(t, f.peer)
	ingress := newReadIngress(t, f, mirror, 5*time.Minute, nil, true)
	remote := strings.Replace(ingress.remote, f.repo.FullName, other.FullName, 1)
	for _, version := range []string{"0", "2"} {
		dir := filepath.Join(t.TempDir(), "unbound")
		ingress.git(t, f, "", version, "clone", remote, dir)
		if got := replicaGit(t, dir, "", "remote", "get-url", "origin"); got != remote {
			t.Fatal("remote rewritten")
		}
		replicaGit(t, dir, "", "commit", "--allow-empty", "-m", "unbound gateway push")
		ingress.git(t, f, dir, version, "push", "origin", "HEAD:refs/heads/gateway-"+version)
		ingress.git(t, f, dir, version, "fetch", "origin")
	}
	if ingress.upstreamReads.Load() == 0 {
		t.Fatal("unbound reads not routed to primary")
	}
	if mirror.Stats().FullTransfers != 0 {
		t.Fatal("unregistered repo copied into cache")
	}
	var row struct{ GitStorageID *string }
	if err := f.svc.DB.Table("repositories").Where("id = ?", other.ID).Select("git_storage_id").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.GitStorageID != nil {
		t.Fatal("read silently registered repo")
	}
	for _, url := range []string{ingress.remote, remote} {
		r := httptest.NewRequest("GET", url+"/info/refs?service=git-upload-pack", nil)
		r.Header.Set("Authorization", "Bearer invalid-gateway-user")
		w := httptest.NewRecorder()
		ingress.server.Config.Handler.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal("bad user admitted")
		}
		if w.Header().Get("X-AGS-Edge-Request-ID") == "" {
			t.Fatal("missing diagnostic id")
		}
	}
}

func TestGatewayPreservesLFSAndOtherNativeSurfacesWithoutLeakingDiagnostics(t *testing.T) {
	var calls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-AGS-Edge-Route", "spoof")
		w.Header().Set("X-AGS-Edge-Request-ID", "spoof")
		w.WriteHeader(418)
		_, _ = io.Copy(w, r.Body)
	}))
	defer primary.Close()
	srv, err := edge.New(config.EdgeConfig{ID: "gateway", PrimaryURL: primary.URL, CanonicalURL: "http://primary.example.test:6666", UnboundReads: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/owner/repo.git/info/lfs/objects/batch", "/owner/repo.git/objects/ab/cdef", "/api/v3/repos/owner/repo"} {
		r := httptest.NewRequest("POST", "http://primary.example.test:6666"+p, strings.NewReader("exact body"))
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != 418 || w.Body.String() != "exact body" || w.Header().Get("X-AGS-Edge-Route") != "primary_other" || w.Header().Get("X-AGS-Edge-Request-ID") == "spoof" {
			t.Fatalf("changed primary result %d %s", w.Code, w.Body.String())
		}
	}
	before := calls.Load()
	for _, p := range []string{"/_ags/edge/v1/warm", "/_ags/edge/v1/status", "/internal/metrics", "/owner/repo.git/info/refs?service=git-upload-pack&service=git-receive-pack"} {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest("GET", "http://primary.example.test:6666"+p, nil))
		if w.Code < 400 {
			t.Fatal("internal/ambiguous path accepted")
		}
	}
	if calls.Load() != before {
		t.Fatal("internal request forwarded")
	}
}

func TestGitResolveListFallsBackOnlyBeforeHTTPRequest(t *testing.T) {
	f := newControlFixture(t)
	mirror := newReplicaMirror(t, f.peer)
	ingress := newReadIngress(t, f, mirror, 5*time.Minute, nil)
	_, port, _ := net.SplitHostPort(ingress.server.Listener.Addr().String())
	resolve := "ags.edge.test:" + port + ":[::1],127.0.0.1"
	args := []string{"-c", "http.proxy=", "-c", "http.curloptResolve=" + resolve, "-c", "http.extraHeader=Authorization: Bearer " + f.token, "ls-remote", ingress.remote, "refs/heads/main"}
	out := replicaGit(t, "", "", args...)
	if !strings.Contains(out, "refs/heads/main") {
		t.Fatal("connection fallback failed")
	}
	// Same HOST/PORT, first address accepts the connection and rejects HTTP.
	// libcurl must not silently retry that application-level denial elsewhere.
	ln, err := net.Listen("tcp6", net.JoinHostPort("::1", port))
	if err != nil {
		t.Fatal(err)
	}
	var rejected atomic.Int32
	deny := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rejected.Add(1); http.Error(w, "denied", 403) })}
	go deny.Serve(ln)
	defer deny.Close()
	cmd := exec.Command("git", args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	output, err := cmd.CombinedOutput()
	if err == nil || rejected.Load() != 1 || !strings.Contains(string(output), "403") {
		t.Fatalf("denial fell through: %v %s", err, output)
	}
}

func TestWarmNodePollPreparesRealRepositoryButDoesNotAuthorizeAUser(t *testing.T) {
	f := newControlFixture(t)
	mirror := newReplicaMirror(t, f.peer)
	p, err := edge.StartPrewarmer(context.Background(), f.peer, mirror, []edge.ReadBinding{{Repository: f.repo.FullName, Identity: f.identity}}, 10*time.Second, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && p.Snapshot()[0].State != "synchronized" {
		time.Sleep(20 * time.Millisecond)
	}
	if p.Snapshot()[0].State != "synchronized" {
		t.Fatalf("prewarm failed: %+v", p.Snapshot())
	}
	before := mirror.Stats().FullTransfers
	ingress := newReadIngress(t, f, mirror, 5*time.Minute, nil, true)
	if out := ingress.git(t, f, "", "2", "ls-remote", ingress.remote, "refs/heads/main"); !strings.Contains(out, "refs/heads/main") {
		t.Fatal(out)
	}
	if mirror.Stats().FullTransfers != before {
		t.Fatal("prewarmed read downloaded again")
	}
	wrong := f.identity
	wrong.StoreID = "ungranted"
	if _, err := f.peer.WarmSnapshot(context.Background(), wrong); err == nil {
		t.Fatal("warm expanded node grant")
	}
	if _, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", "", f.request()); err == nil {
		t.Fatal("warm bypassed user auth")
	}
	// On the next periodic pass a server-side write is copied without a user
	// request. Polling is explicitly bounded, not claimed to be real-time.
	sha, err := f.svc.Git.WriteFile(f.ctx, f.repo.FullName, "main", "background.txt", "prewarm mutation", []byte("changed"))
	if err != nil {
		t.Fatal(err)
	}
	first := p.Snapshot()[0].Snapshot
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && p.Snapshot()[0].Snapshot == first {
		time.Sleep(40 * time.Millisecond)
	}
	if p.Snapshot()[0].Snapshot == first {
		t.Fatalf("next generation not prewarmed: %+v", p.Snapshot())
	}
	if got := ingress.git(t, f, "", "2", "ls-remote", ingress.remote, "refs/heads/main"); !strings.Contains(got, sha) {
		t.Fatal(fmt.Sprintf("stale warmed read %s", got))
	}
}
