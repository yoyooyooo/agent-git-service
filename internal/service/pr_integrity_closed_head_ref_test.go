package service

import (
	"context"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
)

func TestForgejoPullRequestCoordinateMismatchIgnoresClosedPullHeadRef(t *testing.T) {
	sha := strings.Repeat("ab", 20)
	pr := db.PullRequest{
		ID: 1, Number: 358, RepositoryID: 5, State: db.StateClosed,
		HeadRef: "cutover/access-grant-only-20260807", HeadSHA: sha, BaseRef: "main",
	}
	projection := db.PullRequestProjection{
		PullRequestID: 1, ExternalRepo: "operator/project-kit", ExternalNumber: 401,
		SourceBranch: pr.HeadRef, TargetBranch: "main", LastSyncedSHA: sha,
	}
	provider := forgejointegration.PullRequestSnapshot{
		Number: 401, State: "closed", HeadRef: "refs/pull/401/head", HeadSHA: sha, BaseRef: "main",
	}
	if got := forgejoPullRequestCoordinateMismatch(pr, projection, provider, "operator/project-kit"); got != "" {
		t.Fatalf("closed PR coordinate mismatch=%q", got)
	}
}

func TestForgejoPullRequestCoordinateMismatchStillRequiresOpenBranchHeadRef(t *testing.T) {
	sha := strings.Repeat("cd", 20)
	pr := db.PullRequest{
		ID: 1, Number: 7, RepositoryID: 5, State: db.StateOpen,
		HeadRef: "agent/wave", HeadSHA: sha, BaseRef: "main",
	}
	projection := db.PullRequestProjection{
		PullRequestID: 1, ExternalRepo: "forgejo/demo", ExternalNumber: 42,
		SourceBranch: pr.HeadRef, TargetBranch: "main", LastSyncedSHA: sha,
	}
	provider := forgejointegration.PullRequestSnapshot{
		Number: 42, State: "open", HeadRef: "refs/pull/42/head", HeadSHA: sha, BaseRef: "main",
	}
	got := forgejoPullRequestCoordinateMismatch(pr, projection, provider, "forgejo/demo")
	if got == "" {
		t.Fatal("open PR accepted Forgejo pull head ref in place of branch name")
	}
	if !strings.Contains(got, "refs/pull/42/head") {
		t.Fatalf("mismatch=%q", got)
	}
}

func TestScanForgejoPullRequestIntegrityResolvesClosedPRWhenSHAMatchesPullHeadRef(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	store, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("git store: %v", err)
	}
	if err := store.Init(ctx, "example-owner/demo", "main", true); err != nil {
		t.Fatalf("init git: %v", err)
	}
	if err := store.CreateBranch(ctx, "example-owner/demo", "agent/closed", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := store.WriteFile(ctx, "example-owner/demo", "agent/closed", "note.txt", "closed", []byte("closed\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tip, err := store.HeadSHA(ctx, "example-owner/demo", "agent/closed")
	if err != nil {
		t.Fatalf("tip: %v", err)
	}
	svc.Git = store
	svc.DisableForgejoProjectionWorker = true

	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	pr := db.PullRequest{
		Number: 358, RepositoryID: repo.ID, HeadRepositoryID: repo.ID, Title: "closed",
		State: db.StateClosed, HeadRef: "agent/closed", HeadSHA: tip, BaseRef: "main",
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create PR: %v", err)
	}
	if err := svc.DB.Create(&db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
		ExternalRepo: "forgejo/demo", ExternalNumber: 401, SourceBranch: pr.HeadRef, TargetBranch: "main",
		State: ProjectionStateClosed, LastSyncedSHA: tip,
	}).Error; err != nil {
		t.Fatalf("create projection: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
	}, &fakePullRequestListClient{rows: []forgejointegration.PullRequestSnapshot{
		{Number: 401, State: "closed", HeadRef: "refs/pull/401/head", HeadSHA: tip, BaseRef: "main"},
	}}, nil)

	recorded, err := svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("recorded=%d, want 0 for closed PR with matching SHA", recorded)
	}
	if got := activeIntegrityType(t, svc, pr.Number); got != "" {
		t.Fatalf("active integrity=%q, want resolved", got)
	}
}

func TestScanForgejoPullRequestIntegrityRecordsSHADriftOnClosedPRHeadMismatch(t *testing.T) {
	svc, pr, _, tip := setupIntegrityHealFixture(t, "", "", "")
	other := strings.Repeat("ee", 20)
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{
		"state": db.StateClosed, "head_sha": other,
	}).Error; err != nil {
		t.Fatalf("close PR: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ?", pr.ID).Updates(map[string]any{
		"state": ProjectionStateClosed, "last_synced_sha": other,
	}).Error; err != nil {
		t.Fatalf("close projection: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
	}, &fakePullRequestListClient{rows: []forgejointegration.PullRequestSnapshot{
		{Number: 42, State: "closed", HeadRef: "refs/pull/42/head", HeadSHA: tip, BaseRef: "main"},
	}}, nil)

	recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("recorded=%d, want 1 sha_drift", recorded)
	}
	if got := activeIntegrityType(t, svc, pr.Number); got != forgejointegration.ProjectionFailureSHADrift {
		t.Fatalf("want sha_drift, got %q", got)
	}
}
