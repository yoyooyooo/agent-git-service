package edge_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/githttp"
	"github.com/ngaut/agent-git-service/internal/middleware"
)

type readIngress struct {
	server        *httptest.Server
	remote        string
	resolve       string
	upstreamReads atomic.Int64
}

func newReadIngress(t *testing.T, f *controlFixture, reader edge.SnapshotReader, window time.Duration, wrap func(http.Handler) http.Handler, gateway ...bool) *readIngress {
	t.Helper()
	ingress := &readIngress{}
	primaryGit := githttp.New(f.svc.Git, f.svc)
	router := chi.NewRouter()
	router.Use(middleware.OptionalTokenAuth(f.svc))
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.RawQuery, "git-upload-pack") || strings.HasSuffix(r.URL.Path, "git-upload-pack") {
				ingress.upstreamReads.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	router.Get("/{owner}/{repo}.git/info/refs", primaryGit.InfoRefs)
	router.Post("/{owner}/{repo}.git/git-upload-pack", primaryGit.UploadPack)
	router.Post("/{owner}/{repo}.git/git-receive-pack", primaryGit.ReceivePack)
	primary := httptest.NewServer(router)
	t.Cleanup(primary.Close)
	rt, err := edge.NewReadRuntime(edge.ReadRuntimeConfig{EdgeID: "edge-fixture-test", Bindings: []edge.ReadBinding{{Repository: f.repo.FullName, Identity: f.identity}}, Concurrency: 8, RecentViews: 8, RecentWindow: window}, f.peer, reader)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	canonical := "http://ags.edge.test:" + port
	policy := "reject"
	if len(gateway) > 0 && gateway[0] {
		policy = "primary"
	}
	svc, err := edge.New(config.EdgeConfig{ID: "edge-fixture-test", PrimaryURL: primary.URL, CanonicalURL: canonical, RequestTimeout: 30 * time.Second, UnboundReads: policy}, edge.WithReadRuntime(rt))
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler = svc
	if wrap != nil {
		handler = wrap(handler)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(func() { server.Close(); f.svc.Wg.Wait() })
	ingress.server, ingress.remote, ingress.resolve = server, canonical+"/"+f.repo.FullName+".git", "ags.edge.test:"+port+":127.0.0.1"
	return ingress
}

func (s *readIngress) git(t *testing.T, f *controlFixture, dir, protocol string, args ...string) string {
	t.Helper()
	base := []string{"-c", "http.curloptResolve=" + s.resolve, "-c", "http.extraHeader=Authorization: Bearer " + f.token, "-c", "protocol.version=" + protocol}
	return replicaGit(t, dir, "", append(base, args...)...)
}

func TestEdgeActualReadPathNativeCloneFetchPushWithUnchangedRemote(t *testing.T) {
	f := newControlFixture(t)
	mirror := newReplicaMirror(t, f.peer)
	ingress := newReadIngress(t, f, mirror, 5*time.Minute, nil)
	for _, protocol := range []string{"0", "2"} {
		t.Run("v"+protocol, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "clone")
			ingress.git(t, f, "", protocol, "clone", "--depth=1", ingress.remote, dir)
			if remote := replicaGit(t, dir, "", "remote", "get-url", "origin"); remote != ingress.remote {
				t.Fatalf("remote rewritten: %s", remote)
			}
			newSHA, err := f.svc.Git.WriteFile(f.ctx, f.repo.FullName, "main", "next-"+protocol+".txt", "primary advance", []byte("advance\n"))
			if err != nil {
				t.Fatal(err)
			}
			ingress.git(t, f, dir, protocol, "fetch", "--unshallow", "origin")
			if got := replicaGit(t, dir, "", "rev-parse", "refs/remotes/origin/main"); got != newSHA {
				t.Fatalf("stale read: %s != %s", got, newSHA)
			}
			replicaGit(t, dir, "", "checkout", "-b", "edge-"+protocol, "origin/main")
			replicaGit(t, dir, "", "commit", "--allow-empty", "-m", "edge push")
			ingress.git(t, f, dir, protocol, "push", "origin", "HEAD:refs/heads/edge-"+protocol)
			want := replicaGit(t, dir, "", "rev-parse", "HEAD")
			got, err := f.svc.Git.HeadSHA(f.ctx, f.repo.FullName, "edge-"+protocol)
			if err != nil || got != want {
				t.Fatalf("primary push mismatch: %s %v", got, err)
			}
			other := filepath.Join(t.TempDir(), "second")
			ingress.git(t, f, "", protocol, "clone", "--branch", "edge-"+protocol, ingress.remote, other)
			if got := replicaGit(t, other, "", "rev-parse", "HEAD"); got != want {
				t.Fatalf("read after push stale: %s", got)
			}
		})
	}
	if ingress.upstreamReads.Load() != 0 {
		t.Fatal("Git download was silently proxied to primary")
	}
	response := httptest.NewRecorder()
	req := httptest.NewRequest("GET", ingress.remote+"/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Git-Protocol", "version=2")
	ingress.server.Config.Handler.ServeHTTP(response, req)
	body := response.Body.String()
	if response.Code != 200 || !strings.Contains(body, "version 2") || strings.Contains(body, "object-info") || strings.Contains(body, "packfile-uris") || strings.Contains(body, "filter") {
		t.Fatalf("wrong v2 capabilities: %d %s", response.Code, body)
	}
}

func TestEdgeActualReadPathSurvivesInterleavedForceUpdate(t *testing.T) {
	for _, version := range []string{"0", "2"} {
		t.Run("v"+version, func(t *testing.T) {
			f := newControlFixture(t)
			old, err := f.svc.Git.HeadSHA(f.ctx, f.repo.FullName, "main")
			if err != nil {
				t.Fatal(err)
			}
			source, err := f.svc.Git.GetRepoPath(f.ctx, f.repo.FullName)
			if err != nil {
				t.Fatal(err)
			}
			tree := replicaGit(t, source, "", "rev-parse", old+"^{tree}")
			// An unrelated root ensures the new view does not contain old HEAD.
			newHead := replicaGit(t, source, "replacement root\n", "commit-tree", tree)
			var changed atomic.Bool
			wrap := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "POST" {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							http.Error(w, "body", 500)
							return
						}
						r.Body = io.NopCloser(bytes.NewReader(body))
						if bytes.Contains(body, []byte("want ")) && changed.CompareAndSwap(false, true) {
							if err := f.svc.Git.UpdateRefCAS(f.ctx, f.repo.FullName, "refs/heads/main", newHead, old); err != nil {
								t.Error(err)
								http.Error(w, "mutation", 500)
								return
							}
							// A second client's discovery installs B as the newest hint
							// before the first client fetches A. No IP/session pinning.
							u := *r.URL
							u.Path = strings.TrimSuffix(u.Path, "git-upload-pack") + "info/refs"
							u.RawQuery = "service=git-upload-pack"
							discovery := httptest.NewRequest("GET", u.String(), nil)
							discovery.Host = r.Host
							discovery.Header.Set("Authorization", "Bearer "+f.token)
							other := httptest.NewRecorder()
							next.ServeHTTP(other, discovery)
							if other.Code != 200 || !strings.Contains(other.Body.String(), newHead) {
								t.Errorf("interleaved discovery failed: %d", other.Code)
							}
						}
					}
					next.ServeHTTP(w, r)
				})
			}
			ingress := newReadIngress(t, f, newReplicaMirror(t, f.peer), 5*time.Minute, wrap)
			dir := filepath.Join(t.TempDir(), "clone")
			ingress.git(t, f, "", version, "clone", ingress.remote, dir)
			if !changed.Load() || replicaGit(t, dir, "", "rev-parse", "HEAD") != old {
				t.Fatal("substituted B for requested A")
			}
			ingress.git(t, f, dir, version, "fetch", "origin")
			if got := replicaGit(t, dir, "", "rev-parse", "origin/main"); got != newHead {
				t.Fatalf("next discovery not fresh: %s", got)
			}
			if ingress.upstreamReads.Load() != 0 {
				t.Fatal("read fallback reached primary")
			}
		})
	}
}

type revokeReadSource struct {
	source edge.SnapshotSource
	revoke func()
	once   atomic.Bool
}

func (s *revokeReadSource) OpenSnapshot(ctx context.Context, descriptor edgeprotocol.RepositorySnapshot) (edgeprotocol.Manifest, io.ReadCloser, error) {
	manifest, body, err := s.source.OpenSnapshot(ctx, descriptor)
	if err == nil && s.once.CompareAndSwap(false, true) {
		s.revoke()
	}
	return manifest, body, err
}

func TestEdgeActualReadPathRevalidatesAfterSynchronization(t *testing.T) {
	f := newControlFixture(t)
	source := &revokeReadSource{source: f.peer, revoke: func() {
		if err := f.svc.DB.Where("value = ?", f.token).Delete(&db.Token{}).Error; err != nil {
			t.Error(err)
		}
	}}
	ingress := newReadIngress(t, f, newReplicaMirror(t, source), time.Minute, nil)
	r := httptest.NewRequest("GET", ingress.remote+"/info/refs?service=git-upload-pack", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	rr := httptest.NewRecorder()
	ingress.server.Config.Handler.ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized || strings.Contains(rr.Body.String(), "PACK") || strings.Contains(rr.Body.String(), f.token) || rr.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("served after revoke: %d %s", rr.Code, rr.Body.String())
	}
	if ingress.upstreamReads.Load() != 0 {
		t.Fatal("denial fell back to primary")
	}
}

func TestEdgeActualReadPathMissingCredentialChallengesAndNoCache(t *testing.T) {
	f := newControlFixture(t)
	ingress := newReadIngress(t, f, newReplicaMirror(t, f.peer), time.Minute, nil)
	for _, auth := range []string{"", "Bearer invalid"} {
		r := httptest.NewRequest("GET", ingress.remote+"/info/refs?service=git-upload-pack", nil)
		r.Header.Set("Authorization", auth)
		rr := httptest.NewRecorder()
		ingress.server.Config.Handler.ServeHTTP(rr, r)
		if rr.Code != 401 || rr.Header().Get("WWW-Authenticate") == "" || rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("invalid challenge: %d %v", rr.Code, rr.Header())
		}
	}
	// Actual Git credential-helper challenge on the canonical remote. Passing
	// curloptResolve changes only the dial target, not credential selection.
	dir := filepath.Join(t.TempDir(), "clone")
	u, _ := url.Parse(ingress.remote)
	cmd := exec.Command("git", "-c", "http.proxy=", "-c", "http.curloptResolve="+ingress.resolve, "-c", fmt.Sprintf("credential.helper=!f() { echo username=test; echo password=%s; }; f", f.token), "clone", ingress.remote, dir)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("credential helper on %s failed: %v %s", u.Host, err, out)
	}
}
