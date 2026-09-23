package edge

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
)

func testEdge(t *testing.T, upstream http.Handler) (*Server, *httptest.Server) {
	t.Helper()
	primary := httptest.NewServer(upstream)
	t.Cleanup(primary.Close)
	srv, err := New(config.EdgeConfig{ID: "edge-1", PrimaryURL: primary.URL, CanonicalURL: "http://ags.test:6666", RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return srv, primary
}

func edgeRequest(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, "http://ags.test:6666"+target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer original-user-token")
	return r
}

func TestEdgeRoutesAndReadinessFailClosed(t *testing.T) {
	var calls atomic.Int32
	srv, _ := testEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	for _, tc := range []struct {
		name, method, target string
		status               int
		forwarded            bool
	}{
		{"live", "GET", "/livez", 200, false},
		{"not ready", "GET", "/readyz", 503, false},
		{"advertise read", "GET", "/alice/demo.git/info/refs?service=git-upload-pack", 503, false},
		{"fetch is POST", "POST", "/alice/demo.git/git-upload-pack", 503, false},
		{"wiki read", "POST", "/alice/demo.wiki.git/git-upload-pack", 503, false},
		{"push advertise", "GET", "/alice/demo.git/info/refs?service=git-receive-pack", 202, true},
		{"push RPC", "POST", "/alice/demo.git/git-receive-pack", 202, true},
		{"API", "POST", "/api/v3/repos/alice/demo/pulls", 202, true},
		{"discovery trailing slash", "GET", "/api/v3/", 202, true},
		{"replication hidden", "POST", "/internal/edge/read/prepare", 404, false},
		{"internal root hidden", "GET", "/internal", 404, false},
		{"reserved hidden", "GET", "/_ags/exports/one", 404, false},
		{"unknown git", "GET", "/alice/demo.git/objects/info/packs", 400, false},
		{"missing service", "GET", "/alice/demo.git/info/refs", 400, false},
		{"unknown service", "GET", "/alice/demo.git/info/refs?service=other", 400, false},
		{"duplicate service", "GET", "/alice/demo.git/info/refs?service=git-upload-pack&service=git-receive-pack", 400, false},
		{"RPC query confusion", "POST", "/alice/demo.git/git-upload-pack?service=git-receive-pack", 400, false},
		{"invalid query", "GET", "/alice/demo.git/info/refs?service=git-upload-pack;service=git-receive-pack", 400, false},
		{"bad query escape", "GET", "/api/v3/?q=%ZZ", 400, false},
		{"dot segments", "GET", "/alice/../demo.git/info/refs?service=git-upload-pack", 400, false},
		{"encoded separator", "GET", "/alice%2fdemo.git/info/refs?service=git-upload-pack", 400, false},
		{"double slash", "POST", "/alice//demo.git/git-upload-pack", 400, false},
		{"wrong method", "GET", "/alice/demo.git/git-upload-pack", 400, false},
		{"trace", "TRACE", "/api/v3/user", 405, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, edgeRequest(tc.method, tc.target, "0000"))
			if rr.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rr.Code, tc.status, rr.Body.String())
			}
			wantCalls := int32(0)
			if tc.forwarded {
				wantCalls = 1
			}
			if calls.Load()-before != wantCalls {
				t.Fatalf("unexpected primary request count: %d", calls.Load()-before)
			}
			if tc.status == 503 && (!strings.Contains(rr.Body.String(), "read_path_not_wired") || rr.Header().Get("Cache-Control") != "no-store") {
				t.Fatalf("not-ready response=%s", rr.Body.String())
			}
		})
	}
}

func TestEdgePreservesOriginalWriteAndRemovesSpoofedAssertions(t *testing.T) {
	type observed struct{ method, host, uri, body, auth, internal, forwarded, edge string }
	seen := make(chan observed, 1)
	srv, _ := testEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- observed{r.Method, r.Host, r.RequestURI, string(body), r.Header.Get("Authorization"), r.Header.Get("X-AGS-Internal-User"), r.Header.Get("X-Forwarded-For"), r.Header.Get(hopHeader)}
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.Header().Set("X-Primary-Response", "preserved")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "0018ng refs/heads/main denied\n0000")
	}))
	r := edgeRequest("POST", "/alice/demo.git/git-receive-pack", "exact git body")
	r.Header.Set("X-AGS-Internal-User", "admin")
	r.Header.Set("X-AGS-Edge-Authorization", "spoof")
	r.Header.Set("X-Forwarded-For", "spoofed-address")
	r.Header.Set("Forwarded", "for=spoofed-address")
	r.Header.Set("Idempotency-Key", "caller-owned")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, r)
	got := <-seen
	if got.method != "POST" || got.host != "ags.test:6666" || got.uri != "/alice/demo.git/git-receive-pack" || got.body != "exact git body" || got.auth != "Bearer original-user-token" || got.internal != "" || strings.Contains(got.forwarded, "spoofed") || got.edge != "edge-1" {
		t.Fatalf("forwarded request changed: %+v", got)
	}
	if rr.Code != 200 || rr.Header().Get("X-Primary-Response") != "preserved" || !strings.Contains(rr.Body.String(), "denied") {
		t.Fatalf("Git response rewritten: %d %s", rr.Code, rr.Body.String())
	}
}

func TestEdgeHostLoopAndProxyEnvironment(t *testing.T) {
	var primaryCalls, proxyCalls atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyCalls.Add(1); w.WriteHeader(500) }))
	defer trap.Close()
	t.Setenv("HTTP_PROXY", trap.URL)
	t.Setenv("HTTPS_PROXY", trap.URL)
	srv, _ := testEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { primaryCalls.Add(1); w.WriteHeader(204) }))
	for _, tc := range []struct {
		host, hop string
		status    int
	}{
		{"attacker.test", "", 421},
		{"ags.test:6666", "another-edge", 508},
		{"ags.test:6666", "", 204},
	} {
		r := edgeRequest("POST", "/api/v3/repos", "{}")
		r.Host = tc.host
		r.Header.Set(hopHeader, tc.hop)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, r)
		if rr.Code != tc.status {
			t.Fatalf("status=%d want=%d", rr.Code, tc.status)
		}
	}
	if primaryCalls.Load() != 1 || proxyCalls.Load() != 0 {
		t.Fatalf("primary=%d environment proxy=%d", primaryCalls.Load(), proxyCalls.Load())
	}
}

func TestEdgeNeverRetriesOrFollowsRedirects(t *testing.T) {
	t.Run("uncertain POST", func(t *testing.T) {
		var calls atomic.Int32
		srv, _ := testEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}))
		r := edgeRequest("POST", "/api/v3/repos/alice/demo/pulls/1/merge", "one effect")
		r.Header.Set("Idempotency-Key", "stable-key")
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("one effect")), nil }
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, r)
		if calls.Load() != 1 || rr.Code != 502 || strings.Contains(rr.Body.String(), "original-user-token") {
			t.Fatalf("calls=%d status=%d body=%s", calls.Load(), rr.Code, rr.Body.String())
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var trapCalls atomic.Int32
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { trapCalls.Add(1) }))
		defer trap.Close()
		srv, _ := testEdge(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Location", trap.URL); w.WriteHeader(307) }))
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, edgeRequest("POST", "/api/v3/repos", "{}"))
		if rr.Code != 307 || rr.Header().Get("Location") != trap.URL || trapCalls.Load() != 0 {
			t.Fatal("proxy followed or rewrote redirect")
		}
	})
}

func TestEdgeRunWithoutPrimaryDBAndStopsOnCancel(t *testing.T) {
	t.Setenv("DB_DSN", "must-not-be-opened")
	srv, err := New(config.EdgeConfig{ID: "edge-1", PrimaryURL: "http://127.0.0.1:1", CanonicalURL: "http://ags.test"})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, listener) }()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + listener.Addr().String() + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "edge") {
		t.Fatalf("health=%d %s", resp.StatusCode, body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Edge did not stop")
	}
}
