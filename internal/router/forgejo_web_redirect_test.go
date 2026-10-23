package router_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/router"
	"github.com/ngaut/agent-git-service/internal/service"
)

const forgejoWebTestBase = "http://forgejo.example.test:5555"

// forgejoWebMux wires the production router with one explicitly configured
// Forgejo web base URL and an AGS-to-Forgejo repo mapping whose names differ from
// the AGS names, so the tests also prove the mapping layer is honored.
func forgejoWebMux(t *testing.T) http.Handler {
	t.Helper()
	svc, gqlSrv, restDeps, gitHandler, oauthHandler := setupTestDeps(t)
	seedWebProjection(t, svc, 875, 876, "mirror/testrepo-ci")
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
		{name: "pull singular", path: "/alice/testrepo/pull/875", want: forgejoWebTestBase + "/mirror/testrepo-ci/pulls/876"},
		{name: "pulls plural", path: "/alice/testrepo/pulls/875", want: forgejoWebTestBase + "/mirror/testrepo-ci/pulls/876"},
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
	if loc := w.Header().Get("Location"); loc != forgejoWebTestBase+"/mirror/testrepo-ci/pulls/876" {
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

func seedWebProjection(t *testing.T, svc *service.Service, number, external int, externalRepo string) db.PullRequestProjection {
	t.Helper()
	var admin db.User
	if err := svc.DB.Where("login = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{Name: "testrepo", FullName: "alice/testrepo", OwnerID: admin.ID}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{Number: number, RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: admin.ID, Title: "Browser redirect fixture"}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	projection := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: "forgejo", ExternalRepo: externalRepo, ExternalNumber: external, State: "open", ExternalURL: "https://untrusted.example.test/pulls/999"}
	if err := svc.DB.Create(&projection).Error; err != nil {
		t.Fatal(err)
	}
	return projection
}

func TestForgejoWebProjectionIdentityAndFallback(t *testing.T) {
	svc, gql, deps, git, oauth := setupTestDeps(t)
	projection := seedWebProjection(t, svc, 177, 178, "recorded/actual-repo")
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, WebBaseURL: forgejoWebTestBase, RepoMap: map[string]forgejointegration.RepoMapping{"alice/testrepo": {Owner: "mirror", Repo: "testrepo-ci"}}}, nil, nil)
	mux := router.RegisterRoutes(chi.NewRouter(), deps, git, gql, oauth, "http://console.localhost")
	list := forgejoWebTestBase + "/mirror/testrepo-ci/pulls"
	for _, method := range []string{"GET", "HEAD"} {
		for _, prefix := range []string{"pull", "pulls"} {
			w := servedBy(t, mux, method, "/alice/testrepo/"+prefix+"/177")
			if w.Code != 302 || w.Header().Get("Location") != forgejoWebTestBase+"/recorded/actual-repo/pulls/178" {
				t.Fatalf("mapping: %d %q", w.Code, w.Header().Get("Location"))
			}
		}
	}
	var nativePR db.PullRequest
	if err := svc.DB.First(&nativePR, projection.PullRequestID).Error; err != nil {
		t.Fatal(err)
	}
	nativePR.ID = 0
	nativePR.Number = 179
	if err := svc.DB.Omit("Repository", "HeadRepository", "Author").Create(&nativePR).Error; err != nil {
		t.Fatal(err)
	}
	for _, number := range []int{178, 179, 999} {
		w := servedBy(t, mux, "GET", fmt.Sprintf("/alice/testrepo/pull/%d", number))
		if w.Header().Get("Location") != list {
			t.Fatalf("missing mapping must use list: %q", w.Header().Get("Location"))
		}
	}
	for _, state := range []string{"closed", "merged"} {
		if err := svc.DB.Model(&projection).Update("state", state).Error; err != nil {
			t.Fatal(err)
		}
		w := servedBy(t, mux, "GET", "/alice/testrepo/pull/177")
		if w.Header().Get("Location") != forgejoWebTestBase+"/recorded/actual-repo/pulls/178" {
			t.Fatalf("terminal mapping lost: %q", w.Header().Get("Location"))
		}
	}
	other := db.Repository{Name: "other", FullName: "alice/other"}
	var admin db.User
	if err := svc.DB.Where("login = ?", "admin").First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	other.OwnerID = admin.ID
	if err := svc.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	for _, updates := range []map[string]any{
		{"state": "pending"}, {"state": "open", "provider": "github"}, {"provider": "forgejo", "external_number": 0},
		{"external_number": 178, "external_repo": "../escape"},
		{"external_repo": "recorded/actual-repo", "repository_id": other.ID},
	} {
		if err := svc.DB.Model(&projection).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
		w := servedBy(t, mux, "GET", "/alice/testrepo/pull/177")
		if w.Header().Get("Location") != list {
			t.Fatalf("invalid mapping %v: %q", updates, w.Header().Get("Location"))
		}
	}
}

func TestForgejoWebExcludedRequestsMatchUnconfiguredRouter(t *testing.T) {
	svc, gql, deps, git, oauth := setupTestDeps(t)
	base := router.RegisterRoutes(chi.NewRouter(), deps, git, gql, oauth, "http://console.localhost")
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, WebBaseURL: forgejoWebTestBase}, nil, nil)
	enabled := router.RegisterRoutes(chi.NewRouter(), deps, git, gql, oauth, "http://console.localhost")
	compare := func(method, path, host string, headers http.Header) {
		t.Helper()
		call := func(mux http.Handler) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, path, nil)
			req.Header = headers.Clone()
			if host != "" {
				req.Host = host
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			return w
		}
		before, after := call(base), call(enabled)
		if before.Code != after.Code || before.Header().Get("Location") != after.Header().Get("Location") || !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
			t.Errorf("%s %s %s %v: base %d %q %q; enabled %d %q %q", method, path, host, headers, before.Code, before.Header().Get("Location"), before.Body.String(), after.Code, after.Header().Get("Location"), after.Body.String())
		}
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE"} {
		for _, path := range []string{"/alice/testrepo/pull/177", "/alice/testrepo/pulls", "/alice/testrepo/tree/main", "/alice/testrepo/blob/main/README.md", "/alice/testrepo/commit/0123456", "/alice/testrepo/issues", "/alice/testrepo/issues/1", "/alice/testrepo"} {
			compare(method, path, "", http.Header{})
		}
	}
	for _, method := range []string{"GET", "HEAD"} {
		for _, path := range []string{"/api/graphql", "/api/v3", "/api/v3/", "/api/v3/pulls", "/api/v3/pull/1", "/api/demo/pulls", "/api/ext/v1/pulls", "/api/graphql/tree/main", "/api/%76%33/pulls"} {
			compare(method, path, "", http.Header{})
		}
		compare(method, "/alice/testrepo/pulls", "api.github.localhost", http.Header{})
		for _, headers := range []http.Header{
			{"User-Agent": []string{"git/2.48.1"}}, {"User-Agent": []string{"JGit/6.0"}}, {"User-Agent": []string{"custom (JGit)"}},
			{"User-Agent": []string{"libgit2/1.0"}}, {"User-Agent": []string{"go-git/5"}},
			{"User-Agent": []string{"git-lfs/3.0"}},
			{"Accept": []string{"application/vnd.git-lfs+json"}},
			{"Accept": []string{"text/html", "application/x-git-upload-pack-advertisement"}},
			{"Content-Type": []string{"application/x-git-receive-pack-request; charset=utf-8"}},
		} {
			for _, path := range []string{"/alice/testrepo", "/alice/testrepo/pull/177", "/alice/testrepo/tree/main", "/alice/testrepo/issues/1"} {
				compare(method, path, "", headers)
			}
		}
	}
}

func TestForgejoWebFixedPathSegmentsAndEscaping(t *testing.T) {
	mux := forgejoWebMux(t)
	for _, word := range []string{"tree", "blob", "pull", "commit"} {
		for _, repo := range []string{word + "/demo", "alice/" + word} {
			for _, action := range []string{"tree", "blob"} {
				path := "/" + repo + "/" + action + "/main/a%20b/%E4%B8%AD%E6%96%87.txt"
				want := forgejoWebTestBase + "/" + repo + "/src/main/a%20b/%E4%B8%AD%E6%96%87.txt"
				w := servedBy(t, mux, "GET", path)
				if w.Code != 302 || w.Header().Get("Location") != want {
					t.Errorf("%s: %d %q want %q", path, w.Code, w.Header().Get("Location"), want)
				}
			}
		}
	}
	for _, path := range []string{"/alice/api/pulls", "/graphql/demo/pulls"} {
		if w := servedBy(t, mux, "GET", path); w.Code != 302 {
			t.Errorf("ordinary repo %s: %d", path, w.Code)
		}
	}
}

func TestForgejoWebRejectsPathInjection(t *testing.T) {
	mux := forgejoWebMux(t)
	for _, path := range []string{
		"/alice/testrepo/tree/../../user/login", "/alice/testrepo/tree/%2e%2e/%2e%2e/user/login",
		"/alice/testrepo/tree/main/%252e%252e", "/alice/testrepo/tree/main/%2F%2Fevil.example",
		"/alice/testrepo/tree/main/%5cevil.example", "/alice/testrepo/tree/main/%0d%0aLocation:evil",
		"/alice/testrepo/tree/main/%3Fnext=evil", "/alice/testrepo/tree/main/%23fragment", "/alice/testrepo/tree/main//evil",
		"/alice/%2e%2e/pulls", "/%2e%2e/testrepo/pulls", "/%2f%2fevil.example/demo/pulls",
		"/alice/testrepo/tree/main/%250a", "/alice/testrepo/tree/main/%00",
	} {
		w := servedBy(t, mux, "GET", path)
		loc := w.Header().Get("Location")
		if strings.HasPrefix(loc, forgejoWebTestBase) {
			t.Errorf("unsafe path %s redirects: %q", path, loc)
		}
		if loc != "" {
			u, err := url.Parse(loc)
			if err != nil || u.Host != "console.localhost" {
				t.Errorf("unexpected target: %q", loc)
			}
		}
	}
}

func TestForgejoWebDeviceVerificationPreservesAuthentication(t *testing.T) {
	svc, gql, deps, git, oauth := setupTestDeps(t)
	base := router.RegisterRoutes(chi.NewRouter(), deps, git, gql, oauth, "http://console.localhost")
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, WebBaseURL: forgejoWebTestBase}, nil, nil)
	enabled := router.RegisterRoutes(chi.NewRouter(), deps, git, gql, oauth, "http://console.localhost")
	for _, authenticated := range []bool{false, true} {
		t.Run(fmt.Sprintf("authenticated=%t", authenticated), func(t *testing.T) {
			call := func(mux http.Handler) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/login/device", nil)
				if authenticated {
					req.Header.Set("Authorization", "token test-token")
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				return w
			}
			before, after := call(base), call(enabled)
			want := http.StatusUnauthorized
			if authenticated {
				want = http.StatusOK
				if !strings.Contains(after.Body.String(), "Device Code Verification") {
					t.Error("authenticated request lost the device verification form")
				}
			}
			for _, w := range []*httptest.ResponseRecorder{before, after} {
				if w.Code != want || w.Header().Get("Location") != "" {
					t.Errorf("want %d without redirect, got %d %q", want, w.Code, w.Header().Get("Location"))
				}
			}
			if !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) || before.Header().Get("Content-Type") != after.Header().Get("Content-Type") {
				t.Error("device verification response changed when redirect was enabled")
			}
		})
	}
}

func TestForgejoWebRegisteredRoutesTakePrecedence(t *testing.T) {
	svc, gql, deps, git, oauth := setupTestDeps(t)
	baseRouter, enabledRouter := chi.NewRouter(), chi.NewRouter()
	base := router.RegisterRoutes(baseRouter, deps, git, gql, oauth, "http://console.localhost")
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, WebBaseURL: forgejoWebTestBase}, nil, nil)
	enabled := router.RegisterRoutes(enabledRouter, deps, git, gql, oauth, "http://console.localhost")
	// Server composition adds /readyz and /metrics after RegisterRoutes. Future
	// feature routes, including POST-only routes, must retain their handlers too.
	for _, raw := range []*chi.Mux{baseRouter, enabledRouter} {
		for _, path := range []string{"/readyz", "/metrics", "/healthz", "/device/verify", "/admin/status"} {
			raw.Get(path, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("AGS endpoint")) })
		}
		raw.Route("/future", func(r chi.Router) {
			r.Post("/action", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
		})
	}
	// Discover every fixed namespace from the real router and add a browser-shaped
	// endpoint under it. New namespaces automatically join the regression matrix.
	namespaces := make(map[string]bool)
	if err := chi.Walk(baseRouter, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		first := strings.Split(strings.TrimPrefix(route, "/"), "/")[0]
		if first != "" && !strings.ContainsAny(first, "{*}") {
			namespaces[first] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for first := range namespaces {
		for _, raw := range []*chi.Mux{baseRouter, enabledRouter} {
			raw.Post("/"+first+"/redirect-probe", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
		}
	}
	for first := range namespaces {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			for _, path := range []string{"/" + first + "/redirect-probe", "/" + fmt.Sprintf("%%%02x", first[0]) + first[1:] + "/redirect-probe"} {
				t.Run(method+path, func(t *testing.T) {
					before, after := servedBy(t, base, method, path), servedBy(t, enabled, method, path)
					if before.Code != after.Code || before.Header().Get("Location") != after.Header().Get("Location") || !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
						t.Errorf("registered path changed: base %d %q %q; enabled %d %q %q", before.Code, before.Header().Get("Location"), before.Body.String(), after.Code, after.Header().Get("Location"), after.Body.String())
					}
				})
			}
		}
	}
	for _, path := range []string{"/avatars/alice", "/auth/connected/login", "/readyz", "/metrics", "/healthz", "/device/verify", "/admin/status", "/future/action"} {
		t.Run(path, func(t *testing.T) {
			before, after := servedBy(t, base, http.MethodGet, path), servedBy(t, enabled, http.MethodGet, path)
			if before.Code != after.Code || before.Header().Get("Location") != after.Header().Get("Location") || !bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
				t.Errorf("explicit route changed: base %d %q; enabled %d %q", before.Code, before.Header().Get("Location"), after.Code, after.Header().Get("Location"))
			}
		})
	}
	// A registered fixed second segment must not reserve an ordinary owner.
	if w := servedBy(t, enabled, http.MethodGet, "/alice/login/pulls"); w.Code != http.StatusFound || w.Header().Get("Location") != forgejoWebTestBase+"/alice/login/pulls" {
		t.Fatalf("ordinary repo lost redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
}
