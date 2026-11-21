package githttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/githttp"
	"github.com/ngaut/agent-git-service/internal/middleware"
)

func TestCredentialProbeDelegatedAuthority(t *testing.T) {
	env := setupTestServer(t, "probe-session", "repo", "main", false)
	var repo db.Repository
	if err := env.DB.First(&repo, "full_name = ?", "probe-session/repo").Error; err != nil {
		t.Fatal(err)
	}
	other := db.Repository{Name: "other", FullName: "probe-session/other", OwnerID: repo.OwnerID, DefaultBranch: "main"}
	if err := env.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	read := seedAccessGrantGitSession(t, env, "probe-read", "git.read", "active")
	write := seedAccessGrantGitSession(t, env, "probe-write", "git.push", "active")
	rest := seedAccessGrantGitSession(t, env, "probe-rest", "repo.read", "active")
	expired := seedAccessGrantGitSession(t, env, "probe-expired", "git.read", "expired")
	revoked := seedAccessGrantGitSession(t, env, "probe-revoked", "git.read", "revoked")

	// The auth-only adapter must not touch either the service or HTTP Git store.
	env.Svc.Git = nil
	handler := middleware.OptionalTokenAuth(env.Svc)(http.HandlerFunc(githttp.New(nil, env.Svc).CredentialProbe))
	for _, tc := range []struct {
		name, repo, token string
		status            int
	}{
		{"git read", "repo", read.Token, http.StatusOK},
		{"other public repo", "other", read.Token, http.StatusForbidden},
		{"push only", "repo", write.Token, http.StatusForbidden},
		{"REST read only", "repo", rest.Token, http.StatusForbidden},
		{"expired", "repo", expired.Token, http.StatusUnauthorized},
		{"revoked", "repo", revoked.Token, http.StatusUnauthorized},
	} {
		for _, method := range []string{http.MethodHead, http.MethodGet} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				req := newRequestWithParams(method, "http://example.test/probe-session/"+tc.repo+".git", nil,
					map[string]string{"owner": "probe-session", "repo": tc.repo + ".git"})
				req.Header.Set("Authorization", "Bearer "+tc.token)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				if w.Code != tc.status || w.Header().Get("Location") != "" {
					t.Fatalf("got status=%d Location=%q, want status=%d without redirect", w.Code, w.Header().Get("Location"), tc.status)
				}
			})
		}
	}
}
