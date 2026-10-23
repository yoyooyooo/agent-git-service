package router_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/router"
)

const forgejoWebTestBase = "http://forgejo.example.test:5555"

// forgejoWebMux wires the production router with one explicitly configured
// Forgejo web base URL and an AGS-to-Forgejo repo mapping whose names differ from
// the AGS names, so the tests also prove the mapping layer is honored.
func forgejoWebMux(t *testing.T) http.Handler {
	t.Helper()
	svc, gqlSrv, restDeps, gitHandler, oauthHandler := setupTestDeps(t)
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:    true,
		BaseURL:    "http://forgejo-api.internal:3000",
		WebBaseURL: forgejoWebTestBase,
		RepoMap: map[string]forgejointegration.RepoMapping{
			"alice/testrepo": {Owner: "mirror", Repo: "testrepo-ci"},
		},
	}, nil, nil)
	return router.RegisterRoutes(chi.NewRouter(), restDeps, gitHandler, gqlSrv, oauthHandler, "http://console.localhost")
}

func servedBy(t *testing.T, mux http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestForgejoWebRedirects(t *testing.T) {
	mux := forgejoWebMux(t)

	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "pull singular", path: "/alice/testrepo/pull/875", want: forgejoWebTestBase + "/mirror/testrepo-ci/pulls/875"},
		{name: "pulls plural", path: "/alice/testrepo/pulls/875", want: forgejoWebTestBase + "/mirror/testrepo-ci/pulls/875"},
		{name: "pulls list", path: "/alice/testrepo/pulls", want: forgejoWebTestBase + "/mirror/testrepo-ci/pulls"},
		{name: "repo root", path: "/alice/testrepo", want: forgejoWebTestBase + "/mirror/testrepo-ci"},
		{name: "repo root trailing slash", path: "/alice/testrepo/", want: forgejoWebTestBase + "/mirror/testrepo-ci"},
		{name: "commit", path: "/alice/testrepo/commit/0123456789abcdef0123456789abcdef01234567", want: forgejoWebTestBase + "/mirror/testrepo-ci/commit/0123456789abcdef0123456789abcdef01234567"},
		{name: "commits plural", path: "/alice/testrepo/commits/0123456", want: forgejoWebTestBase + "/mirror/testrepo-ci/commit/0123456"},
		{name: "tree maps to src", path: "/alice/testrepo/tree/main/internal", want: forgejoWebTestBase + "/mirror/testrepo-ci/src/main/internal"},
		{name: "blob maps to src", path: "/alice/testrepo/blob/main/README.md", want: forgejoWebTestBase + "/mirror/testrepo-ci/src/main/README.md"},
		{name: "issues list", path: "/alice/testrepo/issues", want: forgejoWebTestBase + "/mirror/testrepo-ci/issues"},
		{name: "issue detail", path: "/alice/testrepo/issues/42", want: forgejoWebTestBase + "/mirror/testrepo-ci/issues/42"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := servedBy(t, mux, method, tc.path)
				if w.Code != http.StatusFound {
					t.Fatalf("%s %s: expected 302, got %d: %s", method, tc.path, w.Code, w.Body.String())
				}
				if loc := w.Header().Get("Location"); loc != tc.want {
					t.Fatalf("%s %s: expected Location %q, got %q", method, tc.path, tc.want, loc)
				}
			}
		})
	}
}

func TestForgejoWebRedirectDropsAGSQueryString(t *testing.T) {
	mux := forgejoWebMux(t)

	w := servedBy(t, mux, http.MethodGet, "/alice/testrepo/pull/875?token=super-secret")
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != forgejoWebTestBase+"/mirror/testrepo-ci/pulls/875" {
		t.Fatalf("an AGS credential must not reach the provider URL, got %q", loc)
	}
}

func TestForgejoWebRedirectLeavesGitAndAPIBytesUnchanged(t *testing.T) {
	mux := forgejoWebMux(t)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/alice/testrepo/info/refs?service=git-upload-pack"},
		{http.MethodPost, "/alice/testrepo/git-upload-pack"},
		{http.MethodPost, "/alice/testrepo/git-receive-pack"},
		{http.MethodGet, "/alice/testrepo.git/info/refs?service=git-upload-pack"},
		{http.MethodGet, "/api/v3/repos/alice/testrepo/pulls/1"},
		{http.MethodGet, "/api/v3/repos/alice/testrepo/issues/1"},
		{http.MethodGet, "/api/v3/"},
		{http.MethodGet, "/api/v3/openapi.json"},
		{http.MethodPost, "/api/graphql"},
		{http.MethodGet, "/graphql"},
	} {
		w := servedBy(t, mux, tc.method, tc.path)
		if loc := w.Header().Get("Location"); strings.HasPrefix(loc, forgejoWebTestBase) {
			t.Fatalf("%s %s must not redirect to Forgejo, got %q", tc.method, tc.path, loc)
		}
	}
}

func TestForgejoWebRedirectRejectsNonGETMethods(t *testing.T) {
	mux := forgejoWebMux(t)

	paths := []string{
		"/alice/testrepo/pull/875",
		"/alice/testrepo/pulls",
		"/alice/testrepo/commit/0123456",
		"/alice/testrepo/tree/main",
		"/alice/testrepo",
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, path := range paths {
			w := servedBy(t, mux, method, path)
			if loc := w.Header().Get("Location"); strings.HasPrefix(loc, forgejoWebTestBase) {
				t.Fatalf("%s %s must not redirect to Forgejo, got %q", method, path, loc)
			}
		}
	}
}

func TestForgejoWebRedirectRejectsUnmappablePaths(t *testing.T) {
	mux := forgejoWebMux(t)

	for _, path := range []string{
		"/alice/testrepo/pull/not-a-number",
		"/alice/testrepo/pulls/0",
		"/alice/testrepo/commit/not-a-sha",
		"/alice/testrepo/tree/",
		"/alice/testrepo/tree",
		"/alice/testrepo/zzz-unmapped-page",
	} {
		w := servedBy(t, mux, http.MethodGet, path)
		if loc := w.Header().Get("Location"); strings.HasPrefix(loc, forgejoWebTestBase) {
			t.Fatalf("GET %s must not redirect to Forgejo, got %q", path, loc)
		}
	}
}

func TestForgejoWebRedirectAbsentConfigIsNoOp(t *testing.T) {
	// No Forgejo integration at all: the console redirect contract and the git
	// transport stay exactly what they were before this feature.
	_, mux := setupRouterTest(t)

	w := servedBy(t, mux, http.MethodGet, "/alice/testrepo")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "http://console.localhost/vault/alice/testrepo" {
		t.Fatalf("repo root must keep the console redirect, got %d %q", w.Code, w.Header().Get("Location"))
	}

	for _, path := range []string{
		"/alice/testrepo/pull/1",
		"/alice/testrepo/pulls",
		"/alice/testrepo/commit/0123456",
		"/alice/testrepo/tree/main",
		"/alice/testrepo/blob/main/README.md",
	} {
		w := servedBy(t, mux, http.MethodGet, path)
		if w.Code != http.StatusNotFound {
			t.Fatalf("GET %s without a Forgejo web base URL must stay 404, got %d %q", path, w.Code, w.Header().Get("Location"))
		}
	}
}

func TestForgejoWebRedirectIntegrationWithoutWebBaseURLIsNoOp(t *testing.T) {
	// The integration is enabled but no browser base URL is configured: no
	// redirect may be invented and BaseURL must not be used implicitly.
	svc, gqlSrv, restDeps, gitHandler, oauthHandler := setupTestDeps(t)
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true,
		BaseURL: "http://forgejo-api.internal:3000",
	}, nil, nil)
	mux := router.RegisterRoutes(chi.NewRouter(), restDeps, gitHandler, gqlSrv, oauthHandler, "http://console.localhost")

	if w := servedBy(t, mux, http.MethodGet, "/alice/testrepo/pull/1"); w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d %q", w.Code, w.Header().Get("Location"))
	}

	w := servedBy(t, mux, http.MethodGet, "/alice/testrepo")
	if w.Code != http.StatusFound || w.Header().Get("Location") != "http://console.localhost/vault/alice/testrepo" {
		t.Fatalf("repo root must keep the console redirect, got %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestForgejoWebRedirectKeepsDotGitRepoRootUnchanged(t *testing.T) {
	mux := forgejoWebMux(t)

	// A *.git repository segment is a git transport URL. With the browser base
	// URL configured it must still resolve to the pre-existing console
	// redirect, never to Forgejo.
	w := servedBy(t, mux, http.MethodGet, "/alice/testrepo.git")
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "http://console.localhost/vault/alice/testrepo" {
		t.Fatalf("*.git repo root must keep the console redirect, got %q", loc)
	}

	for _, path := range []string{"/alice/testrepo.git/pull/1", "/alice/testrepo.git/pulls", "/alice/testrepo.git/tree/main"} {
		w := servedBy(t, mux, http.MethodGet, path)
		if loc := w.Header().Get("Location"); strings.HasPrefix(loc, forgejoWebTestBase) {
			t.Fatalf("GET %s must not redirect to Forgejo, got %q", path, loc)
		}
	}
}
