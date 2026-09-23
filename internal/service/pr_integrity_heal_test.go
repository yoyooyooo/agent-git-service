package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
)

func setupIntegrityHealFixture(t *testing.T, lastSynced, providerHead, sourceBranch string) (*Service, db.PullRequest, db.PullRequestProjection, string) {
	t.Helper()
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	store, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("git store: %v", err)
	}
	if err := store.Init(ctx, "example-owner/demo", "main", true); err != nil {
		t.Fatalf("init git: %v", err)
	}
	if err := store.CreateBranch(ctx, "example-owner/demo", "agent/wave", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := store.WriteFile(ctx, "example-owner/demo", "agent/wave", "wave.txt", "wave", []byte("wave\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tip, err := store.HeadSHA(ctx, "example-owner/demo", "agent/wave")
	if err != nil {
		t.Fatalf("tip: %v", err)
	}
	svc.Git = store
	svc.DisableForgejoProjectionWorker = true

	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	stale := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if lastSynced == "" {
		lastSynced = stale
	}
	if providerHead == "" {
		providerHead = tip
	}
	if sourceBranch == "" {
		sourceBranch = "agent/wave"
	}
	pr := db.PullRequest{
		Number: 7, RepositoryID: repo.ID, HeadRepositoryID: repo.ID, Title: "wave",
		State: db.StateOpen, HeadRef: "agent/wave", HeadSHA: stale, BaseRef: "main",
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create PR: %v", err)
	}
	projection := db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
		ExternalRepo: "forgejo/demo", ExternalNumber: 42, SourceBranch: sourceBranch, TargetBranch: "main",
		State: ProjectionStateOpen, LastSyncedSHA: lastSynced,
	}
	if err := svc.DB.Create(&projection).Error; err != nil {
		t.Fatalf("create projection: %v", err)
	}
	client := &fakePullRequestListClient{rows: []forgejointegration.PullRequestSnapshot{
		{Number: 42, State: "open", HeadRef: "agent/wave", HeadSHA: providerHead, BaseRef: "main"},
	}}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
	}, client, nil)
	return svc, pr, projection, tip
}

func activeIntegrityType(t *testing.T, svc *Service, number int) string {
	t.Helper()
	status, err := svc.GetProjectionStatus(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	ref := pullRequestProjectionIntegrityRef(number)
	for _, row := range status.Refs {
		if row.Ref == ref && row.Status == ProjectionStatusActive {
			return row.Type
		}
	}
	return ""
}

func TestScanForgejoPullRequestIntegrityHealsStaleHeadAndResolvesWhenProjectionConverged(t *testing.T) {
	svc, pr, _, tip := setupIntegrityHealFixture(t, "", "", "")
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ?", pr.ID).Update("last_synced_sha", tip).Error; err != nil {
		t.Fatalf("set last_synced: %v", err)
	}
	now := time.Now().UTC()
	if err := svc.recordPullRequestIntegrityFailure(context.Background(), "example-owner/demo", pullRequestProjectionIntegrityRef(pr.Number), pr, forgejointegration.ProjectionFailureSHADrift, pr.HeadSHA, tip, "stale", now); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("recorded=%d, want 0", recorded)
	}
	var got db.PullRequest
	if err := svc.DB.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload PR: %v", err)
	}
	if got.HeadSHA != tip {
		t.Fatalf("head_sha=%s want %s", got.HeadSHA, tip)
	}
	if got := activeIntegrityType(t, svc, pr.Number); got != "" {
		t.Fatalf("integrity still active: %s", got)
	}
}

func TestScanForgejoPullRequestIntegrityDoesNotResolveUnconvergedAcrossTwoScans(t *testing.T) {
	svc, pr, _, tip := setupIntegrityHealFixture(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", "")
	now := time.Now().UTC()
	if err := svc.recordPullRequestIntegrityFailure(context.Background(), "example-owner/demo", pullRequestProjectionIntegrityRef(pr.Number), pr, forgejointegration.ProjectionFailureSHADrift, pr.HeadSHA, tip, "stale", now); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	for round := 1; round <= 2; round++ {
		recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
		if err != nil {
			t.Fatalf("scan round %d: %v", round, err)
		}
		if recorded != 0 {
			t.Fatalf("round %d recorded=%d, want 0", round, recorded)
		}
		var got db.PullRequest
		if err := svc.DB.First(&got, pr.ID).Error; err != nil {
			t.Fatalf("reload PR round %d: %v", round, err)
		}
		if got.HeadSHA != tip {
			t.Fatalf("round %d head_sha=%s want %s", round, got.HeadSHA, tip)
		}
		if got := activeIntegrityType(t, svc, pr.Number); got != forgejointegration.ProjectionFailureSHADrift {
			t.Fatalf("round %d want leftover sha_drift, got %q", round, got)
		}
	}
}

func TestScanForgejoPullRequestIntegrityMatchedHeadWrongExternalRepoDoesNotResolve(t *testing.T) {
	svc, pr, _, tip := setupIntegrityHealFixture(t, "", "", "")
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("head_sha", tip).Error; err != nil {
		t.Fatalf("align PR head: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ?", pr.ID).Updates(map[string]any{
		"last_synced_sha": tip,
		"external_repo":   "other/demo",
	}).Error; err != nil {
		t.Fatalf("poison external_repo / last_synced: %v", err)
	}

	recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("recorded=%d, want 1", recorded)
	}
	var got db.PullRequest
	if err := svc.DB.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload PR: %v", err)
	}
	if got.HeadSHA != tip {
		t.Fatalf("head_sha changed: got %s want %s", got.HeadSHA, tip)
	}
	if got := activeIntegrityType(t, svc, pr.Number); got != forgejointegration.ProjectionFailurePullRequestAuthorityMissing {
		t.Fatalf("want mapping drift, got %q", got)
	}
}

func TestScanForgejoPullRequestIntegrityMatchedHeadBadCoordinatesDoesNotResolve(t *testing.T) {
	svc, pr, _, tip := setupIntegrityHealFixture(t, "", "", "agent/other")
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("head_sha", tip).Error; err != nil {
		t.Fatalf("align PR head: %v", err)
	}
	pr.HeadSHA = tip

	recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("recorded=%d, want 1", recorded)
	}
	var got db.PullRequest
	if err := svc.DB.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload PR: %v", err)
	}
	if got.HeadSHA != tip {
		t.Fatalf("head_sha changed: got %s want %s", got.HeadSHA, tip)
	}
	if got := activeIntegrityType(t, svc, pr.Number); got != forgejointegration.ProjectionFailurePullRequestAuthorityMissing {
		t.Fatalf("want mapping drift, got %q", got)
	}
}

func TestScanForgejoPullRequestIntegrityDoesNotResolveWhenProjectionUnconverged(t *testing.T) {
	svc, pr, _, tip := setupIntegrityHealFixture(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "", "")
	now := time.Now().UTC()
	if err := svc.recordPullRequestIntegrityFailure(context.Background(), "example-owner/demo", pullRequestProjectionIntegrityRef(pr.Number), pr, forgejointegration.ProjectionFailureSHADrift, pr.HeadSHA, tip, "stale", now); err != nil {
		t.Fatalf("seed drift: %v", err)
	}

	recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("recorded=%d, want 0 after heal-without-resolve", recorded)
	}
	var got db.PullRequest
	if err := svc.DB.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload PR: %v", err)
	}
	if got.HeadSHA != tip {
		t.Fatalf("head_sha=%s want %s", got.HeadSHA, tip)
	}
	if got := activeIntegrityType(t, svc, pr.Number); got != forgejointegration.ProjectionFailureSHADrift {
		t.Fatalf("want leftover sha_drift, got %q", got)
	}
}

func TestScanForgejoPullRequestIntegrityRecordsMappingDriftOnBadCoordinates(t *testing.T) {
	svc, pr, _, _ := setupIntegrityHealFixture(t, "", "", "agent/other")
	oldSHA := pr.HeadSHA

	recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("recorded=%d, want 1", recorded)
	}
	var got db.PullRequest
	if err := svc.DB.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload PR: %v", err)
	}
	if got.HeadSHA != oldSHA {
		t.Fatalf("write-heal on mapping drift: head_sha=%s", got.HeadSHA)
	}
	if got := activeIntegrityType(t, svc, pr.Number); got != forgejointegration.ProjectionFailurePullRequestAuthorityMissing {
		t.Fatalf("want mapping drift, got %q", got)
	}
}

func TestScanForgejoPullRequestIntegrityDoesNotResolveWhenEnqueueFails(t *testing.T) {
	svc, pr, _, tip := setupIntegrityHealFixture(t, "", "", "")
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ?", pr.ID).Update("last_synced_sha", tip).Error; err != nil {
		t.Fatalf("set last_synced: %v", err)
	}
	now := time.Now().UTC()
	if err := svc.recordPullRequestIntegrityFailure(context.Background(), "example-owner/demo", pullRequestProjectionIntegrityRef(pr.Number), pr, forgejointegration.ProjectionFailureSHADrift, pr.HeadSHA, tip, "stale", now); err != nil {
		t.Fatalf("seed drift: %v", err)
	}
	t.Cleanup(func() { testEnqueueForgejoPullRequestProjection = nil })
	testEnqueueForgejoPullRequestProjection = func(*Service, context.Context, db.PullRequest) (db.PullRequestProjectionJob, error) {
		return db.PullRequestProjectionJob{}, errors.New("enqueue boom")
	}

	recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("recorded=%d, want 1 sha_drift after failed heal", recorded)
	}
	if got := activeIntegrityType(t, svc, pr.Number); got != forgejointegration.ProjectionFailureSHADrift {
		t.Fatalf("want sha_drift still active, got %q", got)
	}
}

func TestScanForgejoPullRequestIntegrityReadsHeadRepositoryTip(t *testing.T) {
	svc, pr, _, _ := setupIntegrityHealFixture(t, "", "", "")
	ctx := context.Background()
	var base db.Repository
	if err := svc.DB.First(&base, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load base: %v", err)
	}
	fork := db.Repository{OwnerID: base.OwnerID, Name: "fork", FullName: "example-owner/fork", DefaultBranch: "main"}
	if err := svc.DB.Create(&fork).Error; err != nil {
		t.Fatalf("create fork repo: %v", err)
	}
	if err := svc.Git.Init(ctx, "example-owner/fork", "main", true); err != nil {
		t.Fatalf("init fork git: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "example-owner/fork", "agent/wave", "main"); err != nil {
		t.Fatalf("create fork branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, "example-owner/fork", "agent/wave", "fork.txt", "fork", []byte("fork\n")); err != nil {
		t.Fatalf("write fork: %v", err)
	}
	forkTip, err := svc.Git.HeadSHA(ctx, "example-owner/fork", "agent/wave")
	if err != nil {
		t.Fatalf("fork tip: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{
		"head_repository_id": fork.ID,
		"head_sha":           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}).Error; err != nil {
		t.Fatalf("point PR at fork: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ?", pr.ID).Update("last_synced_sha", forkTip).Error; err != nil {
		t.Fatalf("set last_synced: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
	}, &fakePullRequestListClient{rows: []forgejointegration.PullRequestSnapshot{
		{Number: 42, State: "open", HeadRef: "agent/wave", HeadSHA: forkTip, BaseRef: "main"},
	}}, nil)

	if _, err := svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var got db.PullRequest
	if err := svc.DB.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload PR: %v", err)
	}
	if got.HeadSHA != forkTip {
		t.Fatalf("head_sha=%s want fork tip %s (must not read base repo tip)", got.HeadSHA, forkTip)
	}
}
