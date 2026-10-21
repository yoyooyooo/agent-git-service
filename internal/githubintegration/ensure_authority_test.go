package githubintegration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureShadowPullRequestRechecksAuthorityAfterFinalRead(t *testing.T) {
	for _, effect := range []string{"create", "metadata"} {
		t.Run(effect, func(t *testing.T) {
			revoked, reads, writes, pushes := false, 0, 0, 0
			denied := errors.New("synthetic authority revoked")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					t.Errorf("denied provider write: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				reads++
				if reads == 2 {
					revoked = true
				}
				if effect == "create" {
					fmt.Fprint(w, `[]`)
					return
				}
				if reads == 1 {
					fmt.Fprint(w, `[{"number":8,"state":"open","title":"old"}]`)
				} else {
					fmt.Fprint(w, `{"number":8,"state":"open","title":"old"}`)
				}
			}))
			defer server.Close()
			integration := NewWithClient(Config{Enabled: true, Token: "synthetic", Repos: map[string]RepoMapping{"fixture/ags": {Owner: "fixture", Repo: "repo"}}}, func(context.Context, string, string, string, string) error { pushes++; return nil }, NewHTTPClient(server.URL, "synthetic"))
			_, handled, err := integration.EnsureShadowPullRequest(context.Background(), ShadowPullRequestRequest{RepoFullName: "fixture/ags", RepoPath: "/fixture.git", HeadBranch: "work", HeadSHA: "aaa", BaseBranch: "main", Title: "new", BeforeWrite: func(context.Context) error {
				if revoked {
					return denied
				}
				return nil
			}})
			if !errors.Is(err, denied) || handled || reads != 2 || writes != 0 || pushes != 1 {
				t.Fatalf("handled=%v err=%v reads=%d writes=%d pushes=%d", handled, err, reads, writes, pushes)
			}
		})
	}
}
