package edge_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

type failingReadSource struct{}

func (failingReadSource) OpenSnapshot(context.Context, edgeprotocol.RepositorySnapshot) (edgeprotocol.Manifest, io.ReadCloser, error) {
	return edgeprotocol.Manifest{}, nil, errors.New("test-only-sensitive failure detail")
}

func TestEdgeActualReadPathNeverFallsBackWhenSyncFails(t *testing.T) {
	f := newControlFixture(t)
	ingress := newReadIngress(t, f, newReplicaMirror(t, failingReadSource{}), time.Minute, nil)
	r := httptest.NewRequest("GET", ingress.remote+"/info/refs?service=git-upload-pack", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	ingress.server.Config.Handler.ServeHTTP(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), "test-only-sensitive") || strings.Contains(w.Body.String(), "refs/heads/") || ingress.upstreamReads.Load() != 0 {
		t.Fatalf("false read success/leak/fallback: %d %s", w.Code, w.Body.String())
	}
}

func TestEdgeActualReadPathUnknownWantCannotReadAnotherView(t *testing.T) {
	f := newControlFixture(t)
	ingress := newReadIngress(t, f, newReplicaMirror(t, f.peer), time.Minute, nil)
	pkt := func(line string) string { return fmt.Sprintf("%04x%s\n", len(line)+5, line) }
	for _, version := range []string{"0", "2"} {
		body := pkt("want "+strings.Repeat("f", 40)) + "0000" + pkt("done")
		if version == "2" {
			body = pkt("command=fetch") + "0001" + pkt("want "+strings.Repeat("f", 40)) + pkt("done") + "0000"
		}
		r := httptest.NewRequest("POST", ingress.remote+"/git-upload-pack", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.token)
		r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		r.Header.Set("Git-Protocol", "version="+version)
		w := httptest.NewRecorder()
		ingress.server.Config.Handler.ServeHTTP(w, r)
		if w.Code != 503 || strings.Contains(w.Body.String(), "PACK") || ingress.upstreamReads.Load() != 0 {
			t.Fatalf("unknown want served: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestEdgeActualReadPathExpiredHistoryFailsRatherThanSubstituting(t *testing.T) {
	f := newControlFixture(t)
	ingress := newReadIngress(t, f, newReplicaMirror(t, f.peer), time.Second, nil)
	old, err := f.svc.Git.HeadSHA(f.ctx, f.repo.FullName, "main")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", ingress.remote+"/info/refs?service=git-upload-pack", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	ingress.server.Config.Handler.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), old) {
		t.Fatalf("initial advertisement: %d", w.Code)
	}
	path, err := f.svc.Git.GetRepoPath(f.ctx, f.repo.FullName)
	if err != nil {
		t.Fatal(err)
	}
	tree := replicaGit(t, path, "", "rev-parse", old+"^{tree}")
	replacement := replicaGit(t, path, "replace\n", "commit-tree", tree)
	if err := f.svc.Git.UpdateRefCAS(f.ctx, f.repo.FullName, "refs/heads/main", replacement, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	pkt := func(line string) string { return fmt.Sprintf("%04x%s\n", len(line)+5, line) }
	fetch := httptest.NewRequest("POST", ingress.remote+"/git-upload-pack", strings.NewReader(pkt("want "+old)+"0000"+pkt("done")))
	fetch.Header.Set("Authorization", "Bearer "+f.token)
	fetch.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	result := httptest.NewRecorder()
	ingress.server.Config.Handler.ServeHTTP(result, fetch)
	if result.Code != 503 || strings.Contains(result.Body.String(), "PACK") || strings.Contains(result.Body.String(), replacement) {
		t.Fatalf("expired view silently substituted: %d %s", result.Code, result.Body.String())
	}
}

func TestEdgeActualReadPathWarmCacheCannotBypassRepositoryDeletion(t *testing.T) {
	f := newControlFixture(t)
	ingress := newReadIngress(t, f, newReplicaMirror(t, f.peer), time.Minute, nil)
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", ingress.remote+"/info/refs?service=git-upload-pack", nil)
		r.Header.Set("Authorization", "Bearer "+f.token)
		w := httptest.NewRecorder()
		ingress.server.Config.Handler.ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != http.StatusOK {
		t.Fatalf("warm read: %d", w.Code)
	}
	if err := f.svc.DeleteRepo(f.ctx, f.repo.FullName); err != nil {
		t.Fatal(err)
	}
	if w := request(); w.Code != 403 || strings.Contains(w.Body.String(), "refs/heads/main") {
		t.Fatalf("cached deleted repo served: %d %s", w.Code, w.Body.String())
	}
}
