package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupProjectionStateService(t *testing.T) *Service {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(t.TempDir()+"/projection.db"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Repository{}, &db.Label{}, &db.PullRequest{}, &db.PullRequestProjection{}, &db.PullRequestProjectionJob{}, &db.PullRequestProjectionJobAttempt{}, &db.ProjectionEvent{}, &db.ProjectionRefState{}, &db.PullRequestActionIntent{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	owner := db.User{Login: "example-owner", Type: "User"}
	if err := gdb.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	repo := db.Repository{OwnerID: owner.ID, Owner: owner, Name: "demo", FullName: "example-owner/demo", DefaultBranch: "main"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return &Service{DB: gdb}
}

func TestRecordProjectionFailurePersistsEventAndActiveState(t *testing.T) {
	svc := setupProjectionStateService(t)
	projectionErr := &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureNonFastForward,
		Repo:         "example-owner/demo",
		TargetRepo:   "example-owner/demo",
		Ref:          "refs/heads/main",
		Branch:       "main",
		ExpectedSHA:  "abc123",
		ActualSHA:    "def456",
		ErrorSummary: "git push rejected: non-fast-forward",
	}

	if err := svc.RecordForgejoProjectionFailure(context.Background(), "example-owner/demo", []ForgejoRefChange{{Ref: "refs/heads/main", After: "abc123"}}, projectionErr); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	status, err := svc.GetProjectionStatus(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Refs) != 1 {
		t.Fatalf("refs=%#v", status.Refs)
	}
	ref := status.Refs[0]
	if ref.Status != "active" || ref.Type != forgejointegration.ProjectionFailureNonFastForward || ref.AGSSHA != "abc123" || ref.ForgejoSHA != "def456" {
		t.Fatalf("unexpected ref state: %#v", ref)
	}
	if len(status.Events) != 1 || status.Events[0].ErrorSummary == "" {
		t.Fatalf("events=%#v", status.Events)
	}
}

func TestResolveProjectionRefStateMarksStateResolved(t *testing.T) {
	svc := setupProjectionStateService(t)
	projectionErr := &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureSHADrift,
		Repo:         "example-owner/demo",
		TargetRepo:   "example-owner/demo",
		Ref:          "refs/heads/agent/demo",
		Branch:       "agent/demo",
		ExpectedSHA:  "abc123",
		ActualSHA:    "def456",
		ErrorSummary: "head mismatch",
	}
	if err := svc.RecordForgejoProjectionFailure(context.Background(), "example-owner/demo", nil, projectionErr); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	resolvedAt := time.Now().UTC()
	if err := svc.ResolveProjectionRefState(context.Background(), "example-owner/demo", ProjectionProviderForgejo, "refs/heads/agent/demo", "abc123", "abc123", resolvedAt); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := svc.ResolveProjectionRefState(context.Background(), "example-owner/demo", ProjectionProviderForgejo, "refs/heads/agent/demo", "abc123", "abc123", resolvedAt.Add(time.Hour)); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	status, err := svc.GetProjectionStatus(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Refs) != 1 || status.Refs[0].Status != "resolved" || status.Refs[0].ResolvedAt == nil {
		t.Fatalf("unexpected resolved state: %#v", status.Refs)
	}
	if !status.Refs[0].ResolvedAt.Equal(resolvedAt) {
		t.Fatalf("resolved_at refreshed after already resolved: got %s want %s", status.Refs[0].ResolvedAt, resolvedAt)
	}
}

func TestGetProjectionStatusIncludesForgejoRepositoryReadback(t *testing.T) {
	svc := setupProjectionStateService(t)
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	now := time.Now().UTC()
	head := strings.Repeat("a", 40)
	if err := svc.DB.Create(&db.ProjectionRefState{
		Provider:     ProjectionProviderForgejo,
		RepositoryID: repo.ID,
		RepoFullName: repo.FullName,
		TargetRepo:   repo.FullName,
		Ref:          "refs/heads/main",
		Branch:       "main",
		Type:         "resolved",
		Status:       ProjectionStatusResolved,
		Authority:    ProjectionAuthorityAGS,
		AGSSHA:       head,
		ExternalSHA:  head,
		FirstSeenAt:  now,
		LastSeenAt:   now,
		ResolvedAt:   &now,
		Generation:   1,
	}).Error; err != nil {
		t.Fatalf("create resolved ref state: %v", err)
	}

	requests := 0
	forgejo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected Forgejo request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token secret-token" {
			t.Fatalf("authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/example-owner/demo":
			_, _ = w.Write([]byte(`{"id":42,"full_name":"example-owner/demo","html_url":"http://forgejo.local/example-owner/demo","clone_url":"http://forgejo.local/example-owner/demo.git","default_branch":"main"}`))
		case "/api/v1/repos/example-owner/demo/branches/main":
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`))
		default:
			t.Fatalf("unexpected Forgejo request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer forgejo.Close()
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:      true,
		BaseURL:      forgejo.URL,
		Token:        "secret-token",
		DefaultOwner: "example-owner",
	}, nil, nil)

	status, err := svc.GetProjectionStatus(context.Background(), repo.FullName)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Refs) != 1 {
		t.Fatalf("refs=%#v", status.Refs)
	}
	raw, err := json.Marshal(status.Refs[0])
	if err != nil {
		t.Fatalf("marshal ref status: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode ref status: %v", err)
	}
	if payload["target_id"] != float64(42) ||
		payload["html_url"] != "http://forgejo.local/example-owner/demo" ||
		payload["clone_url"] != "http://forgejo.local/example-owner/demo.git" {
		t.Fatalf("repository readback missing from projection status: %s", raw)
	}
	if requests != 2 {
		t.Fatalf("repository readback requests=%d, want 2", requests)
	}
}

func TestReconcileDefaultBranchProjectionStatusUsesLiveHeads(t *testing.T) {
	oldSHA := strings.Repeat("a", 40)
	liveSHA := strings.Repeat("b", 40)
	status := ProjectionRefStatus{
		Provider: "forgejo", TargetRepo: "example-owner/demo", Ref: "refs/heads/main", Branch: "main",
		Type: "resolved", Status: ProjectionStatusResolved, Authority: ProjectionAuthorityAGS,
		AGSSHA: oldSHA, ForgejoSHA: oldSHA, ExternalSHA: oldSHA, ErrorSummary: "stale",
	}
	snapshot := forgejointegration.RepositorySnapshot{
		ID: 42, FullName: "example-owner/demo", HTMLURL: "http://forgejo.local/example-owner/demo",
		CloneURL: "http://forgejo.local/example-owner/demo.git", DefaultBranch: "main", DefaultBranchSHA: liveSHA,
	}

	got := reconcileDefaultBranchProjectionStatus(status, "main", liveSHA, snapshot)
	if got.Status != ProjectionStatusResolved || got.Type != "resolved" || got.AGSSHA != liveSHA || got.ForgejoSHA != liveSHA || got.ExternalSHA != liveSHA || got.ErrorSummary != "" {
		t.Fatalf("exact live heads not reconciled: %#v", got)
	}

	drift := reconcileDefaultBranchProjectionStatus(status, "main", oldSHA, snapshot)
	if drift.Status != ProjectionStatusActive || drift.Type != forgejointegration.ProjectionFailureSHADrift || drift.AGSSHA != oldSHA || drift.ForgejoSHA != liveSHA || drift.ExternalSHA != liveSHA {
		t.Fatalf("live drift not exposed: %#v", drift)
	}
}

func TestResolveProjectionRefStateFallsBackToOrphanedRepoName(t *testing.T) {
	svc := setupProjectionStateService(t)
	projectionErr := &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureSHADrift,
		Repo:         "example-owner/demo",
		TargetRepo:   "example-owner/demo",
		Ref:          "refs/heads/agent/orphaned",
		Branch:       "agent/orphaned",
		ExpectedSHA:  "abc123",
		ActualSHA:    "",
		ErrorSummary: "repository removed before projection state cleanup",
	}
	if err := svc.RecordForgejoProjectionFailure(context.Background(), "example-owner/demo", nil, projectionErr); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if err := svc.DB.Where("full_name = ?", "example-owner/demo").Delete(&db.Repository{}).Error; err != nil {
		t.Fatalf("delete repository fixture: %v", err)
	}

	resolvedAt := time.Now().UTC()
	if err := svc.ResolveProjectionRefState(context.Background(), "example-owner/demo", ProjectionProviderForgejo, "refs/heads/agent/orphaned", "", "", resolvedAt); err != nil {
		t.Fatalf("resolve orphaned state: %v", err)
	}
	var state db.ProjectionRefState
	if err := svc.DB.Where("repo_full_name = ? AND ref = ?", "example-owner/demo", "refs/heads/agent/orphaned").First(&state).Error; err != nil {
		t.Fatalf("load orphaned state: %v", err)
	}
	if state.Status != ProjectionStatusResolved || state.ResolvedAt == nil || !state.ResolvedAt.Equal(resolvedAt) {
		t.Fatalf("orphaned state not resolved: %#v", state)
	}
}

func TestDispatchForgejoIntegrationResolvesPreviousFailureOnDeleteSuccess(t *testing.T) {
	svc := setupProjectionStateService(t)
	projectionErr := &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureSHADrift,
		Repo:         "example-owner/demo",
		TargetRepo:   "example-owner/demo",
		Ref:          "refs/heads/agent/deleted",
		Branch:       "agent/deleted",
		ActualSHA:    "def456",
		ErrorSummary: "Forgejo has extra ref absent from AGS authoritative heads",
	}
	if err := svc.RecordForgejoProjectionFailure(context.Background(), "example-owner/demo", nil, projectionErr); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		MirrorBranchIncludes: []string{"agent/*"},
	}, &fakeProjectionClient{}, func(ctx context.Context, req forgejointegration.PushRequest) error { return nil })

	if err := svc.DispatchForgejoIntegration(context.Background(), "example-owner/demo", "/repos/example-owner/demo.git", []ForgejoRefChange{{Ref: "refs/heads/agent/deleted", After: "0000000000000000000000000000000000000000", Deleted: true}}); err != nil {
		t.Fatalf("dispatch delete success: %v", err)
	}
	status, err := svc.GetProjectionStatus(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Refs) != 1 || status.Refs[0].Status != ProjectionStatusResolved || status.Refs[0].AGSSHA != "" || status.Refs[0].ForgejoSHA != "" {
		t.Fatalf("delete success did not resolve to absent/absent: %#v", status.Refs)
	}
}

func TestDispatchForgejoIntegrationResolvesPreviousFailureOnSuccess(t *testing.T) {
	svc := setupProjectionStateService(t)
	projectionErr := &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureNonFastForward,
		Repo:         "example-owner/demo",
		TargetRepo:   "example-owner/demo",
		Ref:          "refs/heads/main",
		Branch:       "main",
		ExpectedSHA:  "abc123",
		ErrorSummary: "rejected",
	}
	if err := svc.RecordForgejoProjectionFailure(context.Background(), "example-owner/demo", nil, projectionErr); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		MirrorBranchIncludes: []string{"main"},
	}, &fakeProjectionClient{}, func(ctx context.Context, req forgejointegration.PushRequest) error { return nil })

	if err := svc.DispatchForgejoIntegration(context.Background(), "example-owner/demo", "/repos/example-owner/demo.git", []ForgejoRefChange{{Ref: "refs/heads/main", After: "abc123"}}); err != nil {
		t.Fatalf("dispatch success: %v", err)
	}
	status, err := svc.GetProjectionStatus(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Refs) != 1 || status.Refs[0].Status != ProjectionStatusResolved {
		t.Fatalf("state not resolved: %#v", status.Refs)
	}
}

func TestDispatchForgejoIntegrationRecordsProjectionFailure(t *testing.T) {
	svc := setupProjectionStateService(t)
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		MirrorBranchIncludes: []string{"main"},
	}, &fakeProjectionClient{}, func(ctx context.Context, req forgejointegration.PushRequest) error {
		return errors.New("git push: exit status 1: ! [rejected] main -> main (non-fast-forward)")
	})

	err := svc.DispatchForgejoIntegration(context.Background(), "example-owner/demo", "/repos/example-owner/demo.git", []ForgejoRefChange{{Ref: "refs/heads/main", After: "abc123"}})
	if err == nil {
		t.Fatal("expected dispatch error")
	}
	status, statusErr := svc.GetProjectionStatus(context.Background(), "example-owner/demo")
	if statusErr != nil {
		t.Fatalf("status: %v", statusErr)
	}
	if len(status.Refs) != 1 || status.Refs[0].Type != forgejointegration.ProjectionFailureNonFastForward {
		t.Fatalf("projection failure not recorded: %#v", status.Refs)
	}
}

type fakeProjectionClient struct{}

func (fakeProjectionClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}
func (fakeProjectionClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (fakeProjectionClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}

func TestGetProjectionStatusIncludesForgejoProjectionJobs(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	pr := db.PullRequest{
		Number:       7,
		RepositoryID: repo.ID,
		Title:        "large projection",
		State:        db.StateOpen,
		HeadRef:      "agent/large-binary",
		HeadSHA:      "abc123",
		BaseRef:      "main",
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create pr: %v", err)
	}
	next := time.Now().UTC().Add(time.Minute)
	if err := svc.DB.Create(&db.PullRequestProjectionJob{
		PullRequestID:             pr.ID,
		RepositoryID:              repo.ID,
		Provider:                  ProjectionProviderForgejo,
		Trigger:                   ForgejoProjectionTriggerActionRebase,
		ActionGeneration:          3,
		CorrelationID:             "delivery-42",
		RepoFullName:              repo.FullName,
		AGSPRNumber:               pr.Number,
		HeadRef:                   pr.HeadRef,
		BaseRef:                   pr.BaseRef,
		HeadSHA:                   pr.HeadSHA,
		PreflightAGSHeadSHA:       "old123",
		PreflightBaseSHA:          "base123",
		ExpectedForgejoOldHeadSHA: "old123",
		DesiredAGSHeadSHA:         pr.HeadSHA,
		ObservedForgejoHeadSHA:    "old123",
		Phase:                     ForgejoProjectionPhaseFailedRetryable,
		Attempt:                   2,
		LastErrorType:             forgejointegration.ProjectionFailureUnknown,
		LastError:                 "git push: signal: killed",
		ExternalRepo:              "forgejo/demo",
		ExternalNumber:            42,
		ExternalURL:               "http://forgejo.local/example-owner/demo/pulls/42",
		RemoteRef:                 "refs/heads/agent/large-binary",
		RemoteSHA:                 "abc123",
		NextRunAt:                 &next,
	}).Error; err != nil {
		t.Fatalf("create projection job: %v", err)
	}

	status, err := svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Jobs) != 1 {
		t.Fatalf("jobs=%#v", status.Jobs)
	}
	job := status.Jobs[0]
	if job.Phase != ForgejoProjectionPhaseFailedRetryable || job.Status != "retryable_failed" || job.Attempt != 2 {
		t.Fatalf("unexpected job phase/status/attempt: %#v", job)
	}
	if job.RemoteSHA != "abc123" || job.LastSyncedAGSHeadSHA != "abc123" || job.ExternalNumber != 42 || job.ExternalURL == "" {
		t.Fatalf("job lacks projection facts: %#v", job)
	}
	if job.Trigger != ForgejoProjectionTriggerActionRebase || job.ActionGeneration != 3 || job.CorrelationID != "delivery-42" || job.PreflightAGSHeadSHA != "old123" || job.PreflightBaseSHA != "base123" || job.ExpectedForgejoOldSHA != "old123" || job.DesiredAGSHeadSHA != "abc123" || job.ObservedForgejoSHA != "old123" {
		t.Fatalf("job lacks rebase recovery facts: %#v", job)
	}
	if job.NextRepairAction != "wait_for_next_retry_or_call_projection_retry" {
		t.Fatalf("next repair action=%q", job.NextRepairAction)
	}
}

func TestForgejoActionRebaseJobClaimUsesDatabaseCAS(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	intentID := "claim-intent"
	job := db.PullRequestProjectionJob{
		PullRequestID: 99, RepositoryID: 1, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: 7,
		ActionIntentID: &intentID, ActionGeneration: 1, Phase: ForgejoProjectionPhaseProjectionResume,
	}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatalf("create action job: %v", err)
	}
	var first, second db.PullRequestProjectionJob
	if err := svc.DB.First(&first, job.ID).Error; err != nil {
		t.Fatalf("load first claim snapshot: %v", err)
	}
	if err := svc.DB.First(&second, job.ID).Error; err != nil {
		t.Fatalf("load second claim snapshot: %v", err)
	}
	firstClaimed, err := svc.claimForgejoActionRebaseJob(ctx, &first)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	secondClaimed, err := svc.claimForgejoActionRebaseJob(ctx, &second)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !firstClaimed || secondClaimed {
		t.Fatalf("DB CAS claims first=%v second=%v", firstClaimed, secondClaimed)
	}
}

func TestRetryForgejoProjectionRequeuesFailedRetryableJob(t *testing.T) {
	svc := setupProjectionStateService(t)
	svc.DisableForgejoProjectionWorker = true
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token"}, &fakeProjectionClient{}, nil)
	ctx := context.Background()
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	pr := db.PullRequest{
		Number:       8,
		RepositoryID: repo.ID,
		Repository:   repo,
		Title:        "retry projection",
		State:        db.StateOpen,
		HeadRef:      "agent/retry",
		HeadSHA:      "def456",
		BaseRef:      "main",
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create pr: %v", err)
	}
	oldNext := time.Now().UTC().Add(-time.Hour)
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID,
		RepositoryID:  repo.ID,
		Provider:      ProjectionProviderForgejo,
		RepoFullName:  repo.FullName,
		AGSPRNumber:   pr.Number,
		HeadRef:       pr.HeadRef,
		BaseRef:       pr.BaseRef,
		HeadSHA:       pr.HeadSHA,
		Phase:         ForgejoProjectionPhaseFailedRetryable,
		Attempt:       3,
		LastErrorType: forgejoProjectionFailureContextDeadline,
		LastError:     "context deadline exceeded",
		RemoteRef:     "refs/heads/agent/retry",
		RemoteSHA:     pr.HeadSHA,
		NextRunAt:     &oldNext,
	}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatalf("create failed job: %v", err)
	}

	result, err := svc.RetryForgejoPullRequestProjection(ctx, "example-owner/demo", pr.Number)
	if err != nil {
		t.Fatalf("retry projection: %v", err)
	}
	if result.Phase != ForgejoProjectionPhaseQueued || result.Status != "queued" || result.LastErrorType != "" || result.LastError != "" {
		t.Fatalf("unexpected retry result: %#v", result)
	}
	var reloaded db.PullRequestProjectionJob
	if err := svc.DB.First(&reloaded, job.ID).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if reloaded.Phase != ForgejoProjectionPhaseQueued || reloaded.Attempt != job.Attempt+1 || reloaded.NextRunAt == nil || reloaded.LastErrorType != "" || reloaded.LastError != "" {
		t.Fatalf("job not requeued as a new clean generation: %#v", reloaded)
	}
}

func TestRecordProjectionFailureClassifiesGenericErrors(t *testing.T) {
	svc := setupProjectionStateService(t)
	err := errors.New("git push: exit status 1: ! [rejected] main -> main (non-fast-forward)")
	if recErr := svc.RecordForgejoProjectionFailure(context.Background(), "example-owner/demo", []ForgejoRefChange{{Ref: "refs/heads/main", After: "abc123"}}, err); recErr != nil {
		t.Fatalf("record generic failure: %v", recErr)
	}
	status, err := svc.GetProjectionStatus(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Refs[0].Type != forgejointegration.ProjectionFailureNonFastForward {
		t.Fatalf("type=%q", status.Refs[0].Type)
	}
}

func TestRecordProjectionFailurePreservesSpecificActiveTypeFromGenericScan(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	if err := svc.RecordForgejoProjectionFailure(ctx, "example-owner/demo", []ForgejoRefChange{{Ref: "refs/heads/main", After: "abc123"}}, &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureProtectedBranch,
		Repo:         "example-owner/demo",
		Ref:          "refs/heads/main",
		Branch:       "main",
		ExpectedSHA:  "abc123",
		ErrorSummary: "Forgejo: Not allowed to push to protected branch main",
	}); err != nil {
		t.Fatalf("record protected branch failure: %v", err)
	}
	if err := svc.recordProjectionFailure(ctx, projectionFailureRecord{
		Provider:     ProjectionProviderForgejo,
		RepoFullName: "example-owner/demo",
		Ref:          "refs/heads/main",
		Branch:       "main",
		Type:         forgejointegration.ProjectionFailureSHADrift,
		Authority:    ProjectionAuthorityAGS,
		AGSSHA:       "def456",
		ExternalSHA:  "abc123",
		ErrorSummary: "Forgejo ref SHA differs from AGS authoritative ref",
	}); err != nil {
		t.Fatalf("record scan drift: %v", err)
	}
	status, err := svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Refs) != 1 {
		t.Fatalf("refs=%#v", status.Refs)
	}
	ref := status.Refs[0]
	if ref.Type != forgejointegration.ProjectionFailureProtectedBranch {
		t.Fatalf("type=%q, want protected branch; ref=%#v", ref.Type, ref)
	}
	if ref.AGSSHA != "def456" || ref.ForgejoSHA != "abc123" {
		t.Fatalf("sha fields were not refreshed: %#v", ref)
	}
	if ref.ErrorSummary != "Forgejo: Not allowed to push to protected branch main" {
		t.Fatalf("summary overwritten: %q", ref.ErrorSummary)
	}
}

type fakePullRequestListClient struct {
	rows       []forgejointegration.PullRequestSnapshot
	exactRows  map[int]forgejointegration.PullRequestSnapshot
	exactCalls []int
	states     []string
}

func (f *fakePullRequestListClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}
func (f *fakePullRequestListClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (f *fakePullRequestListClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (f *fakePullRequestListClient) ListPullRequests(ctx context.Context, owner, repo, state string) ([]forgejointegration.PullRequestSnapshot, error) {
	f.states = append(f.states, state)
	return append([]forgejointegration.PullRequestSnapshot(nil), f.rows...), nil
}
func (f *fakePullRequestListClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (forgejointegration.PullRequestSnapshot, bool, error) {
	f.exactCalls = append(f.exactCalls, number)
	if row, ok := f.exactRows[number]; ok {
		return row, true, nil
	}
	for _, row := range f.rows {
		if row.Number == number {
			return row, true, nil
		}
	}
	return forgejointegration.PullRequestSnapshot{}, false, nil
}

type fakePullRequestListOnlyClient struct {
	rows []forgejointegration.PullRequestSnapshot
}

func (f *fakePullRequestListOnlyClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}
func (f *fakePullRequestListOnlyClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (f *fakePullRequestListOnlyClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (f *fakePullRequestListOnlyClient) ListPullRequests(ctx context.Context, owner, repo, state string) ([]forgejointegration.PullRequestSnapshot, error) {
	return append([]forgejointegration.PullRequestSnapshot(nil), f.rows...), nil
}

func TestScanForgejoPullRequestIntegrityFailsClosedWithoutExactConfirmation(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	pr := db.PullRequest{Number: 1, RepositoryID: repo.ID, Title: "candidate", State: db.StateOpen, HeadRef: "agent/candidate", HeadSHA: "sha1", BaseRef: "main"}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create PR: %v", err)
	}
	projection := db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
		ExternalRepo: "forgejo/demo", ExternalNumber: 42, SourceBranch: pr.HeadRef,
		TargetBranch: "main", State: ProjectionStateOpen, LastSyncedSHA: pr.HeadSHA,
	}
	if err := svc.DB.Create(&projection).Error; err != nil {
		t.Fatalf("create projection: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
	}, &fakePullRequestListOnlyClient{}, nil)

	recorded, err := svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
	if err == nil || !strings.Contains(err.Error(), "exact") {
		t.Fatalf("scan error=%v, want exact-confirmation capability failure", err)
	}
	if recorded != 0 {
		t.Fatalf("recorded=%d, want no drift from an unconfirmed candidate", recorded)
	}
	var active int64
	if err := svc.DB.Model(&db.ProjectionRefState{}).Where("status = ?", ProjectionStatusActive).Count(&active).Error; err != nil {
		t.Fatalf("count active projection states: %v", err)
	}
	if active != 0 {
		t.Fatalf("active projection states=%d, want 0", active)
	}
}

func TestScanForgejoPullRequestIntegrityRechecksStaleBulkCandidates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		agsPR    db.PullRequest
		bulkRows []forgejointegration.PullRequestSnapshot
		exactRow forgejointegration.PullRequestSnapshot
	}{
		{
			name:     "new projection omitted from paginated snapshot",
			agsPR:    db.PullRequest{Number: 1, Title: "new", State: db.StateOpen, HeadRef: "agent/new", HeadSHA: "sha1", BaseRef: "main"},
			exactRow: forgejointegration.PullRequestSnapshot{Number: 42, State: "open", HeadRef: "agent/new", HeadSHA: "sha1", BaseRef: "main"},
		},
		{
			name:     "merge snapshot still reports open",
			agsPR:    db.PullRequest{Number: 1, Title: "merged", State: db.StateClosed, Merged: true, HeadRef: "agent/merged", HeadSHA: "sha1", BaseRef: "main", MergeCommitSHA: "sha1"},
			bulkRows: []forgejointegration.PullRequestSnapshot{{Number: 42, State: "open", HeadRef: "agent/merged", HeadSHA: "sha1", BaseRef: "main"}},
			exactRow: forgejointegration.PullRequestSnapshot{Number: 42, State: "closed", Merged: true, MergeCommitSHA: "sha1", HeadRef: "agent/merged", HeadSHA: "sha1", BaseRef: "main"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := setupProjectionStateService(t)
			ctx := context.Background()
			var repo db.Repository
			if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
				t.Fatalf("load repo: %v", err)
			}
			tc.agsPR.RepositoryID = repo.ID
			if err := svc.DB.Create(&tc.agsPR).Error; err != nil {
				t.Fatalf("create PR: %v", err)
			}
			projection := db.PullRequestProjection{
				PullRequestID: tc.agsPR.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
				ExternalRepo: "forgejo/demo", ExternalNumber: 42, SourceBranch: tc.agsPR.HeadRef,
				TargetBranch: "main", State: ProjectionStateOpen, LastSyncedSHA: tc.agsPR.HeadSHA,
			}
			if err := svc.DB.Create(&projection).Error; err != nil {
				t.Fatalf("create projection: %v", err)
			}
			client := &fakePullRequestListClient{
				rows:      tc.bulkRows,
				exactRows: map[int]forgejointegration.PullRequestSnapshot{42: tc.exactRow},
			}
			svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
				Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", AutoPullRequest: true,
				MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
				RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
			}, client, nil)

			recorded, err := svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
			if err != nil {
				t.Fatalf("scan PR integrity: %v", err)
			}
			if recorded != 0 {
				t.Fatalf("recorded=%d, want stale bulk candidate suppressed", recorded)
			}
			if len(client.exactCalls) != 1 || client.exactCalls[0] != 42 {
				t.Fatalf("exact calls=%v, want [42]", client.exactCalls)
			}
			var active int64
			if err := svc.DB.Model(&db.ProjectionRefState{}).Where("status = ?", ProjectionStatusActive).Count(&active).Error; err != nil {
				t.Fatalf("count active projection states: %v", err)
			}
			if active != 0 {
				t.Fatalf("active projection states=%d, want 0", active)
			}
		})
	}
}

func TestScanForgejoActivePullRequestIntegrityBoundsProviderReads(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	prs := []db.PullRequest{
		{Number: 1, RepositoryID: repo.ID, Title: "open", State: db.StateOpen, HeadRef: "agent/open", HeadSHA: "sha1", BaseRef: "main"},
		{Number: 2, RepositoryID: repo.ID, Title: "historical", State: db.StateClosed, HeadRef: "agent/closed", HeadSHA: "sha2", BaseRef: "main"},
	}
	if err := svc.DB.Create(&prs).Error; err != nil {
		t.Fatalf("create PRs: %v", err)
	}
	projections := []db.PullRequestProjection{
		{PullRequestID: prs[0].ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, ExternalRepo: "forgejo/demo", ExternalNumber: 42, SourceBranch: prs[0].HeadRef, TargetBranch: "main", State: ProjectionStateOpen, LastSyncedSHA: prs[0].HeadSHA},
		{PullRequestID: prs[1].ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, ExternalRepo: "forgejo/demo", ExternalNumber: 43, SourceBranch: prs[1].HeadRef, TargetBranch: "main", State: ProjectionStateClosed, LastSyncedSHA: prs[1].HeadSHA},
	}
	if err := svc.DB.Create(&projections).Error; err != nil {
		t.Fatalf("create projections: %v", err)
	}
	client := &fakePullRequestListClient{exactRows: map[int]forgejointegration.PullRequestSnapshot{
		42: {Number: 42, State: "closed", Merged: true, MergeCommitSHA: "merge1", HeadRef: prs[0].HeadRef, HeadSHA: prs[0].HeadSHA, BaseRef: "main"},
		43: {Number: 43, State: "closed", HeadRef: prs[1].HeadRef, HeadSHA: prs[1].HeadSHA, BaseRef: "main"},
	}}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
	}, client, nil)

	recorded, err := svc.scanForgejoActivePullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan active PR integrity: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("recorded=%d, want open PR state drift only", recorded)
	}
	if len(client.states) != 1 || client.states[0] != "open" {
		t.Fatalf("provider list states=%v, want [open]", client.states)
	}
	if len(client.exactCalls) != 1 || client.exactCalls[0] != 42 {
		t.Fatalf("exact calls=%v, want only active projection [42]", client.exactCalls)
	}
}

func TestScanForgejoPullRequestIntegrityFindsMissingAndStateDrift(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	prs := []db.PullRequest{
		{Number: 1, RepositoryID: repo.ID, Title: "missing", State: db.StateOpen, HeadRef: "agent/missing", HeadSHA: "sha1", BaseRef: "main"},
		{Number: 2, RepositoryID: repo.ID, Title: "merged remotely", State: db.StateOpen, HeadRef: "agent/merged", HeadSHA: "sha2", BaseRef: "main"},
		{Number: 3, RepositoryID: repo.ID, Title: "healthy", State: db.StateOpen, HeadRef: "agent/healthy", HeadSHA: "sha3", BaseRef: "main"},
		{Number: 4, RepositoryID: repo.ID, Title: "closed before projection", State: db.StateClosed, HeadRef: "agent/closed", HeadSHA: "sha4", BaseRef: "main"},
	}
	for i := range prs {
		if err := svc.DB.Create(&prs[i]).Error; err != nil {
			t.Fatalf("create PR %d: %v", i+1, err)
		}
	}
	for _, row := range []db.PullRequestProjection{
		{PullRequestID: prs[1].ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, ExternalRepo: "forgejo/demo", ExternalNumber: 42, SourceBranch: "agent/merged", TargetBranch: "main", State: ProjectionStateOpen, LastSyncedSHA: "sha2"},
		{PullRequestID: prs[2].ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, ExternalRepo: "forgejo/demo", ExternalNumber: 43, SourceBranch: "agent/healthy", TargetBranch: "main", State: ProjectionStateOpen, LastSyncedSHA: "sha3"},
	} {
		if err := svc.DB.Create(&row).Error; err != nil {
			t.Fatalf("create projection: %v", err)
		}
	}
	client := &fakePullRequestListClient{rows: []forgejointegration.PullRequestSnapshot{
		{Number: 42, State: "closed", Merged: true, MergeCommitSHA: "merge2", HeadRef: "agent/merged", HeadSHA: "sha2", BaseRef: "main"},
		{Number: 43, State: "open", HeadRef: "agent/healthy", HeadSHA: "sha3", BaseRef: "main"},
	}}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
	}, client, nil)

	recorded, err := svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan PR integrity: %v", err)
	}
	if len(client.states) != 1 || client.states[0] != "all" {
		t.Fatalf("provider list states=%v, want [all] for historical audit", client.states)
	}
	if recorded != 2 {
		t.Fatalf("recorded=%d, want 2", recorded)
	}
	status, err := svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	got := map[string]string{}
	for _, ref := range status.Refs {
		if ref.Status == ProjectionStatusActive {
			got[ref.Ref] = ref.Type
		}
	}
	if got[pullRequestProjectionIntegrityRef(1)] != forgejointegration.ProjectionFailurePullRequestProjectionMissing {
		t.Fatalf("missing projection not recorded: %#v", got)
	}
	if got[pullRequestProjectionIntegrityRef(2)] != forgejointegration.ProjectionFailurePullRequestStateDrift {
		t.Fatalf("state drift not recorded: %#v", got)
	}
	if _, exists := got[pullRequestProjectionIntegrityRef(3)]; exists {
		t.Fatalf("healthy PR recorded as drift: %#v", got)
	}
}

func TestScanMergedPullRequestIntegrityRecordsMissingBaseAndContinues(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	store, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("new git store: %v", err)
	}
	if err := store.Init(ctx, "example-owner/demo", "main", true); err != nil {
		t.Fatalf("init git repo: %v", err)
	}
	svc.Git = store
	repoPath, err := store.GetRepoPath(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	baseSHA, err := store.HeadSHA(ctx, "example-owner/demo", "main")
	if err != nil {
		t.Fatalf("main SHA: %v", err)
	}
	tree := strings.TrimSpace(runProjectionGit(t, repoPath, "rev-parse", baseSHA+"^{tree}"))
	lostSHA := strings.TrimSpace(runProjectionGit(t, repoPath, "commit-tree", tree, "-m", "second lost merged content"))

	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	mergedAt := time.Now().UTC()
	prs := []db.PullRequest{
		{Number: 1, RepositoryID: repo.ID, Title: "missing base", State: db.StateClosed, HeadRef: "agent/missing", BaseRef: "deleted-base", Merged: true, MergeCommitSHA: baseSHA, MergedAt: &mergedAt, ClosedAt: &mergedAt},
		{Number: 2, RepositoryID: repo.ID, Title: "lost merge", State: db.StateClosed, HeadRef: "agent/lost", BaseRef: "main", Merged: true, MergeCommitSHA: lostSHA, MergedAt: &mergedAt, ClosedAt: &mergedAt},
	}
	if err := svc.DB.Create(&prs).Error; err != nil {
		t.Fatalf("create PRs: %v", err)
	}

	recorded, err := svc.scanMergedPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan with missing base: %v", err)
	}
	if recorded != 2 {
		t.Fatalf("recorded=%d, want missing-base and lost-merge failures", recorded)
	}
	status, err := svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("projection status: %v", err)
	}
	active := map[string]string{}
	for _, row := range status.Refs {
		if row.Status == ProjectionStatusActive {
			active[row.Ref] = row.Type
		}
	}
	for _, number := range []int{1, 2} {
		if active[mergedPullRequestIntegrityRef(number)] != forgejointegration.ProjectionFailureMergedContentMissing {
			t.Fatalf("PR #%d merge failure missing: %#v", number, active)
		}
	}
}

func TestScanForgejoPullRequestIntegrityRecordsMissingBaseAndContinues(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	store, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("new git store: %v", err)
	}
	if err := store.Init(ctx, "example-owner/demo", "main", true); err != nil {
		t.Fatalf("init git repo: %v", err)
	}
	svc.Git = store
	repoPath, err := store.GetRepoPath(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	baseSHA, err := store.HeadSHA(ctx, "example-owner/demo", "main")
	if err != nil {
		t.Fatalf("main SHA: %v", err)
	}
	tree := strings.TrimSpace(runProjectionGit(t, repoPath, "rev-parse", baseSHA+"^{tree}"))
	lostSHA := strings.TrimSpace(runProjectionGit(t, repoPath, "commit-tree", tree, "-m", "forgejo second lost merge"))
	client := &fakePullRequestListClient{rows: []forgejointegration.PullRequestSnapshot{
		{Number: 8, State: "closed", Merged: true, MergeCommitSHA: baseSHA, HeadRef: "agent/missing", HeadSHA: baseSHA, BaseRef: "deleted-base"},
		{Number: 9, State: "closed", Merged: true, MergeCommitSHA: lostSHA, HeadRef: "agent/lost", HeadSHA: lostSHA, BaseRef: "main"},
	}}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "token",
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "example-owner", Repo: "demo", BaseBranch: "main"}},
	}, client, nil)

	recorded, err := svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan Forgejo PRs with missing base: %v", err)
	}
	if recorded != 4 {
		t.Fatalf("recorded=%d, want two authority and two content failures", recorded)
	}
	status, err := svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("projection status: %v", err)
	}
	active := map[string]string{}
	for _, row := range status.Refs {
		if row.Status == ProjectionStatusActive {
			active[row.Ref] = row.Type
		}
	}
	for _, number := range []int{8, 9} {
		if active[forgejoPullRequestAuthorityIntegrityRef(number)] != forgejointegration.ProjectionFailurePullRequestAuthorityMissing {
			t.Fatalf("Forgejo PR #%d authority failure missing: %#v", number, active)
		}
		if active[forgejoMergedPullRequestIntegrityRef(number)] != forgejointegration.ProjectionFailureMergedContentMissing {
			t.Fatalf("Forgejo PR #%d content failure missing: %#v", number, active)
		}
	}
}

func TestScanMergedPullRequestIntegrityRecordsAndResolvesLostMerge(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	store, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("new git store: %v", err)
	}
	if err := store.Init(ctx, "example-owner/demo", "main", true); err != nil {
		t.Fatalf("init git repo: %v", err)
	}
	svc.Git = store
	repoPath, err := store.GetRepoPath(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	baseSHA, err := store.HeadSHA(ctx, "example-owner/demo", "main")
	if err != nil {
		t.Fatalf("main SHA: %v", err)
	}
	tree := strings.TrimSpace(runProjectionGit(t, repoPath, "rev-parse", baseSHA+"^{tree}"))
	lostSHA := strings.TrimSpace(runProjectionGit(t, repoPath, "commit-tree", tree, "-m", "lost merged content"))

	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	mergedAt := time.Now().UTC()
	pr := db.PullRequest{
		Number: 1, RepositoryID: repo.ID, Title: "lost merge", State: db.StateClosed,
		HeadRef: "agent/lost", BaseRef: "main", Merged: true,
		MergeCommitSHA: lostSHA, MergedAt: &mergedAt, ClosedAt: &mergedAt,
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create PR: %v", err)
	}

	recorded, err := svc.scanMergedPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan lost merge: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("recorded=%d, want 1", recorded)
	}
	status, err := svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("projection status: %v", err)
	}
	if len(status.Refs) != 1 || status.Refs[0].Type != forgejointegration.ProjectionFailureMergedContentMissing || status.Refs[0].Status != ProjectionStatusActive {
		t.Fatalf("lost merge not recorded: %#v", status.Refs)
	}

	label := db.Label{RepositoryID: repo.ID, Name: MergeIntegritySupersededLabel, Color: "6b7280"}
	if err := svc.DB.Create(&label).Error; err != nil {
		t.Fatalf("create superseded label: %v", err)
	}
	if err := svc.DB.Model(&pr).Association("Labels").Append(&label); err != nil {
		t.Fatalf("attach superseded label: %v", err)
	}
	if err := svc.DB.Model(&pr).Update("body", db.LargeText("Merge-Integrity-Superseded-By: AGS PR #9 (0123456789012345678901234567890123456789)")).Error; err != nil {
		t.Fatalf("add superseded evidence: %v", err)
	}
	recorded, err = svc.scanMergedPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan superseded merge: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("audited superseded merge recorded=%d, want 0", recorded)
	}
	status, err = svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("projection status after superseded evidence: %v", err)
	}
	if len(status.Refs) != 1 || status.Refs[0].Status != ProjectionStatusResolved {
		t.Fatalf("superseded merge state not resolved: %#v", status.Refs)
	}

	if err := svc.DB.Model(&pr).Association("Labels").Clear(); err != nil {
		t.Fatalf("clear superseded label: %v", err)
	}
	if err := svc.DB.Model(&pr).Update("merge_commit_sha", baseSHA).Error; err != nil {
		t.Fatalf("repair PR merge SHA: %v", err)
	}
	recorded, err = svc.scanMergedPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("rescan repaired merge: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("recorded after repair=%d, want 0", recorded)
	}
	status, err = svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("projection status after repair: %v", err)
	}
	if len(status.Refs) != 1 || status.Refs[0].Status != ProjectionStatusResolved {
		t.Fatalf("repaired merge state not resolved: %#v", status.Refs)
	}
}

func TestScanForgejoPullRequestIntegrityFindsAndSuppressesAuditedForgejoOnlyMerge(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	store, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("new git store: %v", err)
	}
	if err := store.Init(ctx, "example-owner/demo", "main", true); err != nil {
		t.Fatalf("init git repo: %v", err)
	}
	svc.Git = store
	repoPath, err := store.GetRepoPath(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	baseSHA, err := store.HeadSHA(ctx, "example-owner/demo", "main")
	if err != nil {
		t.Fatalf("main SHA: %v", err)
	}
	tree := strings.TrimSpace(runProjectionGit(t, repoPath, "rev-parse", baseSHA+"^{tree}"))
	lostSHA := strings.TrimSpace(runProjectionGit(t, repoPath, "commit-tree", tree, "-m", "forgejo-only lost merge"))
	client := &fakePullRequestListClient{rows: []forgejointegration.PullRequestSnapshot{{
		Number: 8, State: "closed", Merged: true, MergeCommitSHA: lostSHA,
		HeadRef: "forkops", HeadSHA: lostSHA, BaseRef: "main",
	}}}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "token",
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "example-owner", Repo: "demo", BaseBranch: "main"}},
	}, client, nil)

	recorded, err := svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan Forgejo-only merge: %v", err)
	}
	if recorded != 2 {
		t.Fatalf("recorded=%d, want authority and content failures", recorded)
	}
	status, err := svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("projection status: %v", err)
	}
	active := map[string]string{}
	for _, row := range status.Refs {
		if row.Status == ProjectionStatusActive {
			active[row.Ref] = row.Type
		}
	}
	if active[forgejoPullRequestAuthorityIntegrityRef(8)] != forgejointegration.ProjectionFailurePullRequestAuthorityMissing {
		t.Fatalf("missing authority failure: %#v", active)
	}
	if active[forgejoMergedPullRequestIntegrityRef(8)] != forgejointegration.ProjectionFailureMergedContentMissing {
		t.Fatalf("missing content failure: %#v", active)
	}

	client.rows[0].Body = "[AGS-INTEGRITY-SUPERSEDED]\nSuperseded by RepoFlow."
	client.rows[0].Labels = []string{MergeIntegritySupersededLabel}
	recorded, err = svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan audited supersession: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("audited supersession recorded=%d, want 0", recorded)
	}
	status, err = svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("projection status after supersession: %v", err)
	}
	for _, row := range status.Refs {
		if row.Status != ProjectionStatusResolved {
			t.Fatalf("superseded Forgejo-only state still active: %#v", status.Refs)
		}
	}
}

func TestScanForgejoPullRequestIntegritySuppressesAuditedMappedStateDrift(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	store, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("new git store: %v", err)
	}
	if err := store.Init(ctx, "example-owner/demo", "main", true); err != nil {
		t.Fatalf("init git repo: %v", err)
	}
	svc.Git = store
	baseSHA, err := store.HeadSHA(ctx, "example-owner/demo", "main")
	if err != nil {
		t.Fatalf("main SHA: %v", err)
	}

	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	closedAt := time.Now().UTC()
	pr := db.PullRequest{
		Number: 1, RepositoryID: repo.ID, Title: "superseded projection", State: db.StateClosed,
		HeadRef: "agent/superseded", HeadSHA: baseSHA, BaseRef: "main", ClosedAt: &closedAt,
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create PR: %v", err)
	}
	if err := svc.DB.Create(&db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
		ExternalRepo: "example-owner/demo", ExternalNumber: 8, SourceBranch: pr.HeadRef,
		TargetBranch: pr.BaseRef, State: ProjectionStateOpen, LastSyncedSHA: baseSHA,
	}).Error; err != nil {
		t.Fatalf("create projection: %v", err)
	}
	client := &fakePullRequestListClient{rows: []forgejointegration.PullRequestSnapshot{{
		Number: 8, State: "closed", Merged: true, MergeCommitSHA: baseSHA,
		HeadRef: pr.HeadRef, HeadSHA: baseSHA, BaseRef: "main",
	}}}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "token",
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "example-owner", Repo: "demo", BaseBranch: "main"}},
	}, client, nil)

	recorded, err := svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan state drift: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("recorded=%d, want mapped state drift", recorded)
	}

	client.rows[0].Body = "[AGS-INTEGRITY-SUPERSEDED]\nMerge-Integrity-Superseded-By: AGS main"
	client.rows[0].Labels = []string{MergeIntegritySupersededLabel}
	recorded, err = svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("scan audited mapped supersession: %v", err)
	}
	if recorded != 0 {
		t.Fatalf("audited mapped supersession recorded=%d, want 0", recorded)
	}
	status, err := svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("projection status: %v", err)
	}
	for _, row := range status.Refs {
		if row.Ref == pullRequestProjectionIntegrityRef(pr.Number) && row.Status != ProjectionStatusResolved {
			t.Fatalf("superseded mapped state still active: %#v", row)
		}
	}
}

func runProjectionGit(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	gitArgs := []string{"-C", repoPath, "-c", "user.name=projection-integrity-test", "-c", "user.email=projection-integrity-test@localhost"}
	cmd := exec.Command("git", append(gitArgs, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestMergedForgejoSourceBranchCleanupPendingRequiresMergedProjection(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	mergedAt := time.Now().UTC()
	pr := db.PullRequest{
		Number:         1,
		RepositoryID:   repo.ID,
		Title:          "merged source branch",
		State:          db.StateClosed,
		HeadRef:        "agent/done",
		BaseRef:        "main",
		Merged:         true,
		MergeCommitSHA: "abc123",
		MergedAt:       &mergedAt,
		ClosedAt:       &mergedAt,
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create pr: %v", err)
	}
	if err := svc.DB.Create(&db.PullRequestProjection{
		PullRequestID:  pr.ID,
		RepositoryID:   repo.ID,
		Provider:       ProjectionProviderForgejo,
		ExternalRepo:   "forgejo/demo",
		ExternalNumber: 42,
		SourceBranch:   "agent/done",
		TargetBranch:   "main",
		State:          ProjectionStateClosed,
	}).Error; err != nil {
		t.Fatalf("create projection: %v", err)
	}

	pending, err := svc.isMergedForgejoSourceBranchCleanupPending(ctx, repo.ID, "agent/done")
	if err != nil {
		t.Fatalf("cleanup pending: %v", err)
	}
	if !pending {
		t.Fatal("expected merged Forgejo PR source branch to be cleanup-pending")
	}
}

func TestMergedForgejoSourceBranchCleanupPendingRejectsOpenReuse(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	mergedAt := time.Now().UTC()
	merged := db.PullRequest{
		Number:         1,
		RepositoryID:   repo.ID,
		Title:          "merged source branch",
		State:          db.StateClosed,
		HeadRef:        "agent/reused",
		BaseRef:        "main",
		Merged:         true,
		MergeCommitSHA: "abc123",
		MergedAt:       &mergedAt,
		ClosedAt:       &mergedAt,
	}
	open := db.PullRequest{
		Number:       2,
		RepositoryID: repo.ID,
		Title:        "open reuse",
		State:        db.StateOpen,
		HeadRef:      "agent/reused",
		BaseRef:      "main",
	}
	if err := svc.DB.Create(&merged).Error; err != nil {
		t.Fatalf("create merged pr: %v", err)
	}
	if err := svc.DB.Create(&open).Error; err != nil {
		t.Fatalf("create open pr: %v", err)
	}
	if err := svc.DB.Create(&db.PullRequestProjection{
		PullRequestID:  merged.ID,
		RepositoryID:   repo.ID,
		Provider:       ProjectionProviderForgejo,
		ExternalRepo:   "forgejo/demo",
		ExternalNumber: 42,
		SourceBranch:   "agent/reused",
		TargetBranch:   "main",
		State:          ProjectionStateClosed,
	}).Error; err != nil {
		t.Fatalf("create projection: %v", err)
	}

	pending, err := svc.isMergedForgejoSourceBranchCleanupPending(ctx, repo.ID, "agent/reused")
	if err != nil {
		t.Fatalf("cleanup pending: %v", err)
	}
	if pending {
		t.Fatal("open PR branch reuse must stay a real projection drift")
	}
}

func TestResolveAbsentForgejoProjectionStatesMarksGoneRefsResolved(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	if err := svc.RecordForgejoProjectionFailure(ctx, "example-owner/demo", nil, &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureSHADrift,
		Repo:         "example-owner/demo",
		Ref:          "refs/heads/agent/deleted",
		Branch:       "agent/deleted",
		ExpectedSHA:  "abc123",
		ErrorSummary: "Forgejo ref missing for AGS authoritative ref",
	}); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	if err := svc.resolveAbsentForgejoProjectionStates(ctx, "example-owner/demo", time.Now().UTC(), map[string]struct{}{}); err != nil {
		t.Fatalf("resolve absent states: %v", err)
	}
	status, err := svc.GetProjectionStatus(ctx, "example-owner/demo")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Refs) != 1 || status.Refs[0].Status != ProjectionStatusResolved || status.Refs[0].AGSSHA != "" || status.Refs[0].ForgejoSHA != "" {
		t.Fatalf("absent/absent state not resolved: %#v", status.Refs)
	}
}
