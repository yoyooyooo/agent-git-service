package rest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	"github.com/ngaut/agent-git-service/internal/rest"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGitLabMirrorFactsHTTPContract(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "facts.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := database.AutoMigrate(&db.User{}, &db.Repository{}, &db.Collaborator{}, &db.TeamMember{}, &db.TeamRepository{}, &db.OrganizationMember{}); err != nil {
		t.Fatal(err)
	}
	owner := db.User{Login: "fixture-owner", Type: db.TypeUser}
	if err := database.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{OwnerID: owner.ID, Name: "repo", FullName: "fixture-owner/repo", Private: true}
	if err := database.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{DB: database, GitLabIntegration: gitlabintegration.New(gitlabintegration.Config{
		Enabled: true, BaseURL: "https://gitlab.example.test", MergeAuthority: "forgejo", Token: "sentinel-token", TokenFile: "/private/sentinel-file",
		MirrorBranchExcludes: []string{"private/"},
	}, func(context.Context, string, string, string, string) error {
		t.Fatal("HTTP facts pushed to provider")
		return nil
	})}
	mux := chi.NewRouter()
	deps := &rest.Deps{Svc: svc}
	mux.Get("/api/ext/v1/repos/{owner}/{repo}/projection/gitlab/facts", deps.GetGitLabMirrorFacts)
	read := func(ctx context.Context, ref string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/ext/v1/repos/fixture-owner/repo/projection/gitlab/facts?ref="+url.QueryEscape(ref), nil).WithContext(ctx)
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, r)
		return out
	}
	if out := read(service.ContextWithAnonRequest(t.Context()), "refs/heads/main"); out.Code != http.StatusNotFound {
		t.Fatalf("private facts exposed: %d", out.Code)
	}
	ctx := service.ContextWithUser(t.Context(), owner)
	for _, tc := range []struct {
		ref    string
		target bool
	}{{"refs/heads/main", true}, {"refs/heads/private/a/b", false}, {"refs/tags/v1", false}} {
		out := read(ctx, tc.ref)
		if out.Code != http.StatusOK || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("facts response: %d %s", out.Code, out.Body)
		}
		var got service.GitLabMirrorFacts
		if err := json.Unmarshal(out.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.GitLabRefReadback != "not_claimed" || got.Ref != tc.ref || (got.Target != nil) != tc.target {
			t.Fatalf("wrong ref facts: %+v", got)
		}
		if strings.Contains(out.Body.String(), "sentinel-") || strings.Contains(out.Body.String(), "sha") {
			t.Fatal("response leaked a secret or invented an observed SHA")
		}
	}
	if out := read(ctx, "sentinel-invalid-ref"); out.Code != http.StatusUnprocessableEntity || strings.Contains(out.Body.String(), "sentinel-invalid-ref") {
		t.Fatalf("invalid ref response: %d %s", out.Code, out.Body)
	}
	svc.GitLabIntegration = nil
	if out := read(ctx, "refs/heads/main"); out.Code != 200 || !strings.Contains(out.Body.String(), `"configuration":null`) || !strings.Contains(out.Body.String(), `"target":null`) {
		t.Fatalf("absent integration response: %d %s", out.Code, out.Body)
	}
}
