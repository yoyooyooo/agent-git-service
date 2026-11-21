package router_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githttp"
	"github.com/ngaut/agent-git-service/internal/middleware"
	"github.com/ngaut/agent-git-service/internal/router"
	"github.com/ngaut/agent-git-service/internal/service"
)

type credentialProbeAuthenticator struct {
	identity middleware.EmbeddedIdentity
}

func (a credentialProbeAuthenticator) Authenticate(r *http.Request) (middleware.EmbeddedIdentity, bool, error) {
	switch r.Header.Get("X-Test-Identity") {
	case "":
		return middleware.EmbeddedIdentity{}, false, nil
	case "owner":
		return a.identity, true, nil
	default:
		return middleware.EmbeddedIdentity{}, false, errors.New("invalid test identity")
	}
}

func credentialProbeMux(t *testing.T, consoleURL string, forgejoWeb bool) http.Handler {
	t.Helper()
	svc, gql, deps, _, oauth := setupTestDeps(t)
	users := []db.User{
		{Login: "probe-owner", Type: db.TypeUser, Status: db.UserStatusActive},
		{Login: "probe-reader", Type: db.TypeUser, Status: db.UserStatusActive},
		{Login: "probe-outsider", Type: db.TypeUser, Status: db.UserStatusActive},
	}
	for i := range users {
		if err := svc.DB.Create(&users[i]).Error; err != nil {
			t.Fatal(err)
		}
		if err := svc.DB.Create(&db.Token{UserID: users[i].ID, Value: users[i].Login + "-token"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	identity := middleware.EmbeddedIdentity{Provider: "test-gateway", Subject: "probe-subject", Login: "probe-embedded"}
	embeddedUser, err := svc.ResolveEmbeddedIdentity(context.Background(), service.EmbeddedIdentity(identity))
	if err != nil {
		t.Fatal(err)
	}
	repos := []db.Repository{
		{Name: "public", FullName: "probe-owner/public", OwnerID: users[0].ID, DefaultBranch: "main"},
		{Name: "private", FullName: "probe-owner/private", OwnerID: users[0].ID, DefaultBranch: "main", Private: true},
		{Name: "private", FullName: "probe-embedded/private", OwnerID: embeddedUser.ID, DefaultBranch: "main", Private: true},
	}
	for i := range repos {
		if err := svc.DB.Create(&repos[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.DB.Create(&db.Collaborator{RepositoryID: repos[1].ID, UserID: users[1].ID, Permission: "read"}).Error; err != nil {
		t.Fatal(err)
	}
	if forgejoWeb {
		svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
			Enabled:    true,
			WebBaseURL: forgejoWebTestBase,
			RepoMap: map[string]forgejointegration.RepoMapping{
				"probe-owner/public":     {Owner: "mirror", Repo: "public-ci"},
				"probe-owner/private":    {Owner: "mirror", Repo: "private-ci"},
				"probe-embedded/private": {Owner: "mirror", Repo: "embedded-ci"},
			},
		}, nil, nil)
	}
	// These requests must authorize without preparing storage, refreshing
	// receive hooks, or executing git-http-backend. Neither layer has a store.
	svc.Git = nil
	return router.RegisterRoutes(chi.NewRouter(), deps, githttp.New(nil, svc), gql, oauth, consoleURL,
		middleware.EmbeddedAuthConfig{Authenticator: credentialProbeAuthenticator{identity: identity}})
}

func credentialProbeAuthorization(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("git:"+token))
}

func TestGitCredentialProbeRoutes(t *testing.T) {
	for _, consoleURL := range []string{"http://console.example.test:5173", ""} {
		t.Run("console="+consoleURL, func(t *testing.T) {
			mux := credentialProbeMux(t, consoleURL, false)
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			for _, access := range []struct {
				name, repo, authorization string
				status                    int
			}{
				{"anonymous public", "public", "", http.StatusOK},
				{"anonymous private", "private", "", http.StatusUnauthorized},
				{"owner private", "private", credentialProbeAuthorization("probe-owner-token"), http.StatusOK},
			} {
				for _, suffix := range []string{"", "/", ".git", ".git/"} {
					for _, method := range []string{http.MethodHead, http.MethodGet} {
						if method == http.MethodGet && !strings.HasPrefix(suffix, ".git") {
							continue
						}
						t.Run(access.name+"/"+method+"/"+suffix, func(t *testing.T) {
							req, err := http.NewRequest(method, server.URL+"/probe-owner/"+access.repo+suffix, nil)
							if err != nil {
								t.Fatal(err)
							}
							req.Header.Set("Authorization", access.authorization)
							resp, err := client.Do(req)
							if err != nil {
								t.Fatal(err)
							}
							defer resp.Body.Close()
							body, err := io.ReadAll(resp.Body)
							if err != nil {
								t.Fatal(err)
							}
							if resp.StatusCode != access.status || resp.Header.Get("Location") != "" {
								t.Fatalf("got status=%d Location=%q, want status=%d without redirect", resp.StatusCode, resp.Header.Get("Location"), access.status)
							}
							if method == http.MethodHead && len(body) != 0 {
								t.Fatalf("HEAD returned %d body bytes", len(body))
							}
							if access.status == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") != `Basic realm="GitHub"` {
								t.Fatalf("missing Git Basic challenge: %q", resp.Header.Get("WWW-Authenticate"))
							}
						})
					}
				}
			}
			for _, suffix := range []string{"", "/"} {
				req := httptest.NewRequest(http.MethodGet, "/probe-owner/public"+suffix+"?view=history", nil)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if consoleURL == "" {
					if w.Code != http.StatusNotFound || w.Header().Get("Location") != "" {
						t.Fatalf("unconfigured browser route: %d %q", w.Code, w.Header().Get("Location"))
					}
				} else if w.Code != http.StatusFound || w.Header().Get("Location") != consoleURL+"/vault/probe-owner/public?view=history" {
					t.Fatalf("browser redirect changed: %d %q", w.Code, w.Header().Get("Location"))
				}
			}
		})
	}
}

func TestGitCredentialProbePermissions(t *testing.T) {
	mux := credentialProbeMux(t, "http://console.example.test:5173", false)
	for _, tc := range []struct {
		name, path, authorization, embedded string
		status                              int
	}{
		{"read collaborator", "/probe-owner/private.git", credentialProbeAuthorization("probe-reader-token"), "", http.StatusOK},
		{"unrelated user", "/probe-owner/private.git", credentialProbeAuthorization("probe-outsider-token"), "", http.StatusNotFound},
		{"invalid token on private", "/probe-owner/private.git", credentialProbeAuthorization("invalid-probe-token"), "", http.StatusUnauthorized},
		{"invalid token on public", "/probe-owner/public.git", credentialProbeAuthorization("invalid-probe-token"), "", http.StatusUnauthorized},
		{"malformed credentials", "/probe-owner/public.git", "Basic invalid-base64", "", http.StatusUnauthorized},
		{"missing repo", "/probe-owner/missing.git", "", "", http.StatusNotFound},
		{"embedded owner", "/probe-embedded/private.git", "", "owner", http.StatusOK},
		{"missing embedded identity", "/probe-embedded/private.git", "", "", http.StatusUnauthorized},
		{"rejected embedded identity", "/probe-owner/public.git", "", "invalid", http.StatusUnauthorized},
	} {
		for _, method := range []string{http.MethodHead, http.MethodGet} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				req := httptest.NewRequest(method, tc.path, nil)
				req.Header.Set("Authorization", tc.authorization)
				req.Header.Set("X-Test-Identity", tc.embedded)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != tc.status || w.Header().Get("Location") != "" {
					t.Fatalf("got status=%d Location=%q, want status=%d without redirect", w.Code, w.Header().Get("Location"), tc.status)
				}
			})
		}
	}
	// A successful read probe must not grant write access to the real Git routes.
	for _, tc := range []struct{ method, path, authorization string }{
		{http.MethodGet, "/probe-owner/private.git/info/refs?service=git-receive-pack", credentialProbeAuthorization("probe-reader-token")},
		{http.MethodPost, "/probe-owner/private.git/git-receive-pack", credentialProbeAuthorization("probe-reader-token")},
		{http.MethodPost, "/probe-owner/public.git/git-receive-pack", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", tc.authorization)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		want := http.StatusNotFound
		if tc.authorization == "" {
			want = http.StatusUnauthorized
		}
		if w.Code != want {
			t.Fatalf("write authorization changed for %s %s: got %d, want %d", tc.method, tc.path, w.Code, want)
		}
	}
}

func TestGitCredentialProbeWithForgejoRedirects(t *testing.T) {
	for _, consoleURL := range []string{"http://console.example.test:5173", ""} {
		t.Run("console="+consoleURL, func(t *testing.T) {
			mux := credentialProbeMux(t, consoleURL, true)
			for _, tc := range []struct {
				name, repo, authorization, embedded string
				status                              int
			}{
				{"anonymous public", "probe-owner/public", "", "", http.StatusOK},
				{"anonymous private", "probe-owner/private", "", "", http.StatusUnauthorized},
				{"owner private", "probe-owner/private", credentialProbeAuthorization("probe-owner-token"), "", http.StatusOK},
				{"unrelated user", "probe-owner/private", credentialProbeAuthorization("probe-outsider-token"), "", http.StatusNotFound},
				{"invalid token", "probe-owner/public", credentialProbeAuthorization("invalid-probe-token"), "", http.StatusUnauthorized},
				{"embedded owner", "probe-embedded/private", "", "owner", http.StatusOK},
			} {
				for _, suffix := range []string{"", "/", ".git", ".git/"} {
					for _, userAgent := range []string{"", "git-credential-manager/2.6.1", "Mozilla/5.0", "git/2.48.1"} {
						t.Run(tc.name+"/"+suffix+"/ua="+userAgent, func(t *testing.T) {
							req := httptest.NewRequest(http.MethodHead, "/"+tc.repo+suffix, nil)
							req.Header.Set("User-Agent", userAgent)
							req.Header.Set("Authorization", tc.authorization)
							req.Header.Set("X-Test-Identity", tc.embedded)
							w := httptest.NewRecorder()
							mux.ServeHTTP(w, req)
							if w.Code != tc.status || w.Header().Get("Location") != "" {
								t.Fatalf("got status=%d Location=%q, want status=%d without redirect", w.Code, w.Header().Get("Location"), tc.status)
							}
							if tc.name == "anonymous private" && w.Header().Get("WWW-Authenticate") != `Basic realm="GitHub"` {
								t.Fatalf("missing Git Basic challenge: %q", w.Header().Get("WWW-Authenticate"))
							}
						})
					}
				}
			}
			for _, suffix := range []string{"", "/"} {
				w := servedBy(t, mux, http.MethodGet, "/probe-owner/public"+suffix+"?view=history")
				if w.Code != http.StatusFound || w.Header().Get("Location") != forgejoWebTestBase+"/mirror/public-ci" {
					t.Fatalf("browser Forgejo redirect changed: %d %q", w.Code, w.Header().Get("Location"))
				}
			}
		})
	}
}

func TestGitCredentialProbePreservesAPINotFound(t *testing.T) {
	for _, config := range []struct {
		name, consoleURL string
		forgejoWeb       bool
	}{
		{name: "no frontend"},
		{name: "console", consoleURL: "http://console.example.test:5173"},
		{name: "Forgejo", forgejoWeb: true},
		{name: "both", consoleURL: "http://console.example.test:5173", forgejoWeb: true},
	} {
		t.Run(config.name, func(t *testing.T) {
			mux := credentialProbeMux(t, config.consoleURL, config.forgejoWeb)
			for _, path := range []string{"/api/unknown", "/api/unknown/", "/api/unknown.git", "/api/unknown.git/"} {
				for _, method := range []string{http.MethodGet, http.MethodHead} {
					for _, authorization := range []string{"", "Basic invalid-base64"} {
						req := httptest.NewRequest(method, path, nil)
						req.Header.Set("Authorization", authorization)
						w := httptest.NewRecorder()
						mux.ServeHTTP(w, req)
						if w.Code != http.StatusNotFound || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Location") != "" || w.Header().Get("WWW-Authenticate") != "" {
							t.Errorf("%s %s: unknown API response changed: status=%d headers=%v", method, path, w.Code, w.Header())
						}
						if method == http.MethodGet && w.Body.String() != `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}` {
							t.Errorf("%s %s: unknown API JSON changed: %q", method, path, w.Body.String())
						}
					}
				}
			}
		})
	}
}
