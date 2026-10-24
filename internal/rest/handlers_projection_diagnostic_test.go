package rest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gittransport"
	"github.com/ngaut/agent-git-service/internal/rest"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestProjectionDiagnosticLegacyHTTPStatusCompatibility(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "projection.db")+"?_foreign_keys=on"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := database.AutoMigrate(&db.User{}, &db.Repository{}, &db.Label{}, &db.PullRequest{}, &db.PullRequestProjection{}, &db.PullRequestProjectionJob{}, &db.PullRequestProjectionJobAttempt{}, &db.ProjectionEvent{}, &db.ProjectionRefState{}, &db.PullRequestActionIntent{}); err != nil {
		t.Fatal(err)
	}
	owner := db.User{Login: "fixture-owner", Type: db.TypeUser, SiteAdmin: true}
	if err := database.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{OwnerID: owner.ID, Name: "repo", FullName: "fixture-owner/repo", DefaultBranch: "main"}
	if err := database.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	svc := &service.Service{DB: database}
	ctx := service.ContextWithUser(context.Background(), owner)
	pe := forgejointegration.ClassifyProjectionError("refs/heads/topic", strings.Repeat("a", 40), gittransport.SafeError(errors.New("403 forbidden sentinel-token Authorization: sentinel-header /private/sentinel-path sentinel-pr-body")))
	if err := svc.RecordForgejoProjectionFailure(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: pe.Ref, After: pe.ExpectedSHA}}, &pe); err != nil {
		t.Fatal(err)
	}
	mux := chi.NewRouter()
	deps := &rest.Deps{Svc: svc}
	mux.Get("/api/v3/repos/{owner}/{repo}/projection/status", deps.GetProjectionStatus)
	req := httptest.NewRequest(http.MethodGet, "/api/v3/repos/fixture-owner/repo/projection/status", nil).WithContext(ctx)
	out := httptest.NewRecorder()
	mux.ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("legacy status failed: %d %s", out.Code, out.Body)
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(out.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"refs", "events", "jobs"} {
		if _, ok := status[key]; !ok {
			t.Fatalf("legacy key missing: %s", key)
		}
	}
	for _, bad := range []string{"sentinel-token", "sentinel-header", "sentinel-path", "sentinel-pr-body"} {
		if strings.Contains(out.Body.String(), bad) {
			t.Fatal("HTTP status leaked provider response")
		}
	}
	if !strings.Contains(out.Body.String(), forgejointegration.ProjectionFailurePermissionDenied) {
		t.Fatal("old stable type lost")
	}
}
