package router_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ngaut/agent-git-service/internal/buildinfo"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githttp"
	"github.com/ngaut/agent-git-service/internal/graphql"
	"github.com/ngaut/agent-git-service/internal/oauth"
	"github.com/ngaut/agent-git-service/internal/rest"
	"github.com/ngaut/agent-git-service/internal/router"
	"github.com/ngaut/agent-git-service/internal/service"
)

// Root discovery has no storage or provider dependencies. Wire the complete
// production router so method handling and API-host rewriting remain covered.
func rootMux(integration *forgejointegration.Integration) http.Handler {
	svc := &service.Service{ForgejoIntegration: integration}
	r := chi.NewRouter()
	mux := router.RegisterRoutes(r, &rest.Deps{Svc: svc}, &githttp.Handler{}, graphql.NewServer(svc), &oauth.Handler{Svc: svc}, "")
	// Server composition registers these after RegisterRoutes.
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("readiness unchanged\n"))
	})
	r.Get("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("metrics unchanged\n"))
	})
	return mux
}

func TestServiceRootGET(t *testing.T) {
	configs := []struct {
		name string
		cfg  *forgejointegration.Integration
		base string
	}{
		{name: "absent"},
		{name: "no web URL", cfg: forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo-api.internal:3000"}, nil, nil)},
		{name: "disabled", cfg: forgejointegration.New(forgejointegration.Config{WebBaseURL: forgejoWebTestBase}, nil, nil)},
		{name: "configured", cfg: forgejointegration.New(forgejointegration.Config{Enabled: true, WebBaseURL: forgejoWebTestBase + "/"}, nil, nil), base: forgejoWebTestBase},
		{name: "path prefix", cfg: forgejointegration.New(forgejointegration.Config{Enabled: true, WebBaseURL: forgejoWebTestBase + "/forgejo/"}, nil, nil), base: forgejoWebTestBase + "/forgejo"},
	}
	requests := []struct {
		name    string
		headers http.Header
		browser bool
		git     bool
	}{
		{name: "browser", headers: http.Header{"User-Agent": {"Mozilla/5.0"}, "Accept": {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"}}, browser: true},
		{name: "HTML without UA", headers: http.Header{"Accept": {"text/html"}}, browser: true},
		{name: "XHTML", headers: http.Header{"Accept": {"application/xhtml+xml;q=0.8"}}, browser: true},
		{name: "multiple accept lines", headers: http.Header{"Accept": {"application/json", "text/html;q=0.5"}}, browser: true},
		{name: "no headers"},
		{name: "curl", headers: http.Header{"User-Agent": {"curl/8.0"}, "Accept": {"*/*"}}},
		{name: "API", headers: http.Header{"User-Agent": {"gh/2.0"}, "Accept": {"application/vnd.github+json"}}},
		{name: "browser JSON", headers: http.Header{"User-Agent": {"Mozilla/5.0"}, "Accept": {"application/json"}}},
		{name: "browser without accept", headers: http.Header{"User-Agent": {"Mozilla/5.0"}}},
		{name: "HTML refused", headers: http.Header{"Accept": {"text/html;q=0,*/*;q=1"}}},
		{name: "invalid quality", headers: http.Header{"Accept": {"text/html;q=invalid"}}},
		{name: "out of range quality", headers: http.Header{"Accept": {"text/html;q=2"}}},
		{name: "Git", headers: http.Header{"User-Agent": {"git/2.48.1"}, "Accept": {"text/html"}}, git: true},
		{name: "JGit", headers: http.Header{"User-Agent": {"custom (JGit)"}, "Accept": {"text/html"}}, git: true},
		{name: "libgit2", headers: http.Header{"User-Agent": {"libgit2/1.0"}, "Accept": {"text/html"}}, git: true},
		{name: "go-git", headers: http.Header{"User-Agent": {"go-git/5"}, "Accept": {"text/html"}}, git: true},
		{name: "git-lfs", headers: http.Header{"User-Agent": {"git-lfs/3.0"}, "Accept": {"text/html"}}, git: true},
		{name: "Git accept", headers: http.Header{"User-Agent": {"Mozilla/5.0"}, "Accept": {"text/html", "application/x-git-upload-pack-advertisement"}}, git: true},
		{name: "LFS accept", headers: http.Header{"Accept": {"text/html,application/vnd.git-lfs+json"}}, git: true},
		{name: "Git content type", headers: http.Header{"Accept": {"text/html"}, "Content-Type": {"application/x-git-receive-pack-request"}}, git: true},
	}
	for _, config := range configs {
		t.Run(config.name, func(t *testing.T) {
			mux := rootMux(config.cfg)
			for _, tc := range requests {
				t.Run(tc.name, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodGet, "/?token=synthetic-secret", nil)
					req.Header = tc.headers.Clone()
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					if tc.git {
						if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || w.Body.String() != "404 page not found\n" {
							t.Fatalf("Git root response changed: %d %v %q", w.Code, w.Header(), w.Body.String())
						}
						return
					}
					if tc.browser && config.base != "" {
						if w.Code != http.StatusFound || w.Header().Get("Location") != config.base+"/" {
							t.Fatalf("want Forgejo homepage, got %d %v %q", w.Code, w.Header(), w.Body.String())
						}
					} else {
						want := fmt.Sprintf("agent-git-service (AGS)\nVersion: %s\nGit and pull request service.\nHealth check: /readyz\n", buildinfo.Version)
						if w.Code != http.StatusOK || w.Header().Get("Location") != "" || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Body.String() != want {
							t.Fatalf("want service information, got %d %v %q", w.Code, w.Header(), w.Body.String())
						}
					}
					if strings.Contains(w.Body.String(), "synthetic-secret") || strings.Contains(w.Header().Get("Location"), "synthetic-secret") {
						t.Fatal("root response exposed the AGS query string")
					}
					if w.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("root response must not be cached across request types")
					}
				})
			}
		})
	}
}

func TestServiceRootPreservesOtherRequests(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("configured=%t", enabled), func(t *testing.T) {
			mux := rootMux(forgejointegration.New(forgejointegration.Config{Enabled: enabled, WebBaseURL: forgejoWebTestBase}, nil, nil))
			for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace} {
				for _, accept := range []string{"text/html", "application/json", "*/*"} {
					req := httptest.NewRequest(method, "/", nil)
					req.Header.Set("Accept", accept)
					req.Header.Set("User-Agent", "Mozilla/5.0")
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" || w.Header().Get("Allow") != "" || w.Header().Get("Cache-Control") != "" {
						t.Errorf("%s / (%s) changed: %d %v", method, accept, w.Code, w.Header())
					}
				}
			}
			for _, tc := range []struct {
				path string
				host string
				code int
				body string
			}{
				{path: "/readyz", code: http.StatusServiceUnavailable, body: "readiness unchanged\n"},
				{path: "/metrics", code: http.StatusOK, body: "metrics unchanged\n"},
				{path: "/api/unknown", code: http.StatusNotFound, body: `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`},
				{path: "/", host: "api.github.localhost", code: http.StatusOK},
				{path: "/api/v3/", code: http.StatusOK},
				{path: "/unknown", code: http.StatusNotFound, body: "404 page not found\n"},
			} {
				req := httptest.NewRequest(http.MethodGet, tc.path, nil)
				req.Header.Set("Accept", "text/html")
				if tc.host != "" {
					req.Host = tc.host
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != tc.code || w.Header().Get("Location") != "" || (tc.body != "" && w.Body.String() != tc.body) {
					t.Errorf("GET %s host=%s changed: %d %v %q", tc.path, tc.host, w.Code, w.Header(), w.Body.String())
				}
				if tc.body == "" && !strings.Contains(w.Body.String(), `"current_user_url"`) {
					t.Errorf("GET %s host=%s lost API discovery: %q", tc.path, tc.host, w.Body.String())
				}
			}
		})
	}
}
