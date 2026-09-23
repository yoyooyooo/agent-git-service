package edge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/config"
)

func TestAliasIngressKeepsCanonicalUpstreamAndReadGates(t *testing.T) {
	type observed struct{ host, auth, path string }
	requests := make(chan observed, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- observed{r.Host, r.Header.Get("Authorization"), r.URL.Path}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer primary.Close()
	cfg := config.EdgeConfig{ID: "edge-1", PrimaryURL: primary.URL, CanonicalURL: "http://primary.example.test:6666", CanonicalAliases: []string{"http://primary-alias.example.test:6666"}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		host   string
		status int
	}{
		{"primary.example.test:6666", 503}, {"primary-alias.example.test:6666", 503},
		{"PRIMARY-ALIAS.EXAMPLE.TEST:6666", 503},
		{"primary-alias.example.test.evil:6666", 421}, {"primary-alias.example.test:6667", 421}, {"other.example.test:6666", 421},
	} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/owner/project.git/info/refs?service=git-upload-pack", nil)
		r.Header.Set("X-Forwarded-Host", "primary.example.test:6666")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("host %s: got %d want %d", tc.host, w.Code, tc.status)
		}
	}
	if len(requests) != 0 {
		t.Fatal("alias bypassed local read gate via primary")
	}
	r := httptest.NewRequest("POST", "http://primary-alias.example.test:6666/owner/project.git/git-receive-pack", strings.NewReader("git-request"))
	r.Header.Set("Authorization", "Bearer original-test")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("alias write forwarding failed: %d", w.Code)
	}
	got := <-requests
	if got.host != "primary.example.test:6666" || got.auth != "Bearer original-test" || got.path != "/owner/project.git/git-receive-pack" {
		t.Fatal("alias changed primary identity, user or request path")
	}
	withoutAlias := cfg
	withoutAlias.CanonicalAliases = nil
	closed, err := New(withoutAlias)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	closed.ServeHTTP(w, httptest.NewRequest("GET", "http://primary-alias.example.test:6666/owner/project.git/info/refs?service=git-upload-pack", nil))
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatal("aliases became implicit")
	}
}
