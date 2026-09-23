package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestEnqueueForgejoPullRequestProjectionIgnoresExpiredOriginatingSession(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	pr, headSHA := seedStandingExecutorProjectionPR(t, svc, "expired-sess", "agent/expired-session")
	attachExpiredOriginatingSession(t, svc, &pr)

	client := &projectionSyncForgejoClient{headSHA: headSHA}
	svc.ForgejoIntegration = standingExecutorForgejo(t, "expired-sess/repo", client, nil)
	svc.DisableForgejoProjectionWorker = true

	job, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr)
	if err != nil {
		t.Fatalf("enqueue generic Forgejo projection: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseQueued {
		t.Fatalf("phase=%s last_error=%s, want queued without session admission", job.Phase, job.LastError)
	}
	if job.LastErrorType == service.ProjectionFailureProviderAdmissionDenied || job.LastError == service.DelegatedDenialSessionExpired {
		t.Fatalf("generic projection used originating session: type=%s error=%s", job.LastErrorType, job.LastError)
	}
}

func TestForgejoProjectionWorkerProjectsWhenOriginatingSessionExpired(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	pr, headSHA := seedStandingExecutorProjectionPR(t, svc, "expired-worker", "agent/expired-worker")
	attachExpiredOriginatingSession(t, svc, &pr)

	client := &projectionSyncForgejoClient{headSHA: headSHA}
	pushes := 0
	svc.ForgejoIntegration = standingExecutorForgejo(t, "expired-worker/repo", client, func(context.Context, forgejointegration.PushRequest) error {
		pushes++
		return nil
	})

	if _, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	svc.Wg.Wait()
	if pushes != 1 || len(client.requests) != 1 {
		t.Fatalf("standing executor did not project, pushes=%d requests=%d", pushes, len(client.requests))
	}
	var job db.PullRequestProjectionJob
	if err := svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&job).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseProjected {
		t.Fatalf("phase=%s error=%s, want projected via standing executor", job.Phase, job.LastError)
	}
}

func TestEnqueueForgejoPullRequestProjectionRequeuesSessionAdmissionTerminal(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	pr, headSHA := seedStandingExecutorProjectionPR(t, svc, "heal-term", "agent/heal-term")
	attachExpiredOriginatingSession(t, svc, &pr)
	svc.ForgejoIntegration = standingExecutorForgejo(t, "heal-term/repo", &projectionSyncForgejoClient{headSHA: headSHA}, nil)
	svc.DisableForgejoProjectionWorker = true

	now := time.Now().UTC()
	seed := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AgentSessionID: pr.AgentSessionID,
		Provider: service.ProjectionProviderForgejo, Trigger: service.ForgejoProjectionTriggerPullRequest,
		RepoFullName: "heal-term/repo", AGSPRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
		HeadSHA: pr.HeadSHA, Phase: service.ForgejoProjectionPhaseFailedTerminal,
		LastErrorType: service.ProjectionFailureProviderAdmissionDenied, LastError: service.DelegatedDenialSessionExpired,
		FinishedAt: &now,
	}
	if err := svc.DB.Create(&seed).Error; err != nil {
		t.Fatalf("seed terminal admission job: %v", err)
	}

	job, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr)
	if err != nil {
		t.Fatalf("enqueue heal: %v", err)
	}
	if job.ID != seed.ID || job.Phase != service.ForgejoProjectionPhaseQueued {
		t.Fatalf("admission terminal not requeued: id=%d phase=%s error=%s", job.ID, job.Phase, job.LastError)
	}
}

func TestResumePendingForgejoProjectionJobsHealsSessionAdmissionTerminal(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	pr, headSHA := seedStandingExecutorProjectionPR(t, svc, "resume-heal", "agent/resume-heal")
	attachExpiredOriginatingSession(t, svc, &pr)
	client := &projectionSyncForgejoClient{headSHA: headSHA}
	pushes := 0
	svc.ForgejoIntegration = standingExecutorForgejo(t, "resume-heal/repo", client, func(context.Context, forgejointegration.PushRequest) error {
		pushes++
		return nil
	})
	svc.DisableForgejoProjectionWorker = true

	now := time.Now().UTC()
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AgentSessionID: pr.AgentSessionID,
		Provider: service.ProjectionProviderForgejo, Trigger: service.ForgejoProjectionTriggerPullRequest,
		RepoFullName: "resume-heal/repo", AGSPRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
		HeadSHA: pr.HeadSHA, Phase: service.ForgejoProjectionPhaseFailedTerminal,
		LastErrorType: service.ProjectionFailureProviderAdmissionDenied, LastError: service.DelegatedDenialSessionExpired,
		FinishedAt: &now,
	}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatalf("seed resume job: %v", err)
	}

	svc.DisableForgejoProjectionWorker = false
	if err := svc.ResumePendingForgejoProjectionJobs(context.Background()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	svc.Wg.Wait()
	if pushes != 1 {
		t.Fatalf("resume did not heal session admission terminal, pushes=%d", pushes)
	}
	var got db.PullRequestProjectionJob
	if err := svc.DB.First(&got, job.ID).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if got.Phase != service.ForgejoProjectionPhaseProjected {
		t.Fatalf("phase=%s error=%s, want projected after resume heal", got.Phase, got.LastError)
	}
}

func TestSyncPRHeadAfterPushRequeuesWhenOriginatingSessionExpired(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	pr, _ := seedStandingExecutorProjectionPR(t, svc, "head-sync", "agent/head-sync")
	attachExpiredOriginatingSession(t, svc, &pr)
	svc.ForgejoIntegration = standingExecutorForgejo(t, "head-sync/repo", &projectionSyncForgejoClient{headSHA: pr.HeadSHA}, nil)
	svc.DisableForgejoProjectionWorker = true
	if _, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr); err != nil {
		t.Fatalf("seed projected job enqueue: %v", err)
	}

	newSHA, err := svc.Git.WriteFile(ctx, "head-sync/repo", pr.HeadRef, "next.txt", "advance head", []byte("next\n"))
	if err != nil {
		t.Fatalf("advance head: %v", err)
	}
	updated, err := svc.SyncPRHeadAfterPush(ctx, pr.ID, "head-sync/repo")
	if err != nil {
		t.Fatalf("sync PR head after push: %v", err)
	}
	if updated.HeadSHA != newSHA {
		t.Fatalf("head_sha=%s want %s", updated.HeadSHA, newSHA)
	}
	var job db.PullRequestProjectionJob
	if err := svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&job).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseQueued || job.HeadSHA != newSHA {
		t.Fatalf("head sync did not requeue standing projection: phase=%s sha=%s error=%s", job.Phase, job.HeadSHA, job.LastError)
	}
}

func seedStandingExecutorProjectionPR(t *testing.T, svc *service.Service, owner, branch string) (db.PullRequest, string) {
	t.Helper()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: owner, Name: owner, Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: owner, Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	repo := owner + "/repo"
	if err := svc.Git.CreateBranch(ctx, repo, branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, repo, branch, "note.txt", "fixture", []byte("fixture\n"))
	if err != nil {
		t.Fatalf("write file: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: repo, Title: "standing executor", HeadRef: branch, BaseRef: "main", AuthorLogin: owner})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	return pr, headSHA
}

func attachExpiredOriginatingSession(t *testing.T, svc *service.Service, pr *db.PullRequest) {
	t.Helper()
	sessionID := "expired-" + strings.ReplaceAll(pr.HeadRef, "/", "-")
	hash := (fmtHex(sessionID) + strings.Repeat("ab", 32))[:64]
	session := db.DelegatedAgentSession{
		ID: sessionID, CredentialHash: hash, CredentialPrefix: "exp",
		PrincipalUserID: pr.AuthorID, PrincipalLogin: "expired-principal",
		Issuer: "multica", AssertionVersion: 1, AssertionPurpose: "ags_session_exchange",
		AssertionJTI: sessionID + "-jti", AssertionAudience: "urn:ags:workload-session-exchange:v1",
		TargetInstance: "primary-b", RepositoryID: pr.RepositoryID, OperationName: "pr.create",
		GrantedCapabilities: []string{"pr:create"},
		PolicyVersion:       "2026-09-08.1",
		PolicySnapshotHash:  strings.Repeat("ef", 32),
		CreatedAt:           time.Now().UTC().Add(-2 * time.Hour),
		ExpiresAt:           time.Now().UTC().Add(-time.Minute),
	}
	if err := svc.DB.Create(&session).Error; err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("agent_session_id", session.ID).Error; err != nil {
		t.Fatalf("attach session: %v", err)
	}
	pr.AgentSessionID = &session.ID
}

func standingExecutorForgejo(t *testing.T, repoFullName string, client forgejointegration.Client, push func(context.Context, forgejointegration.PushRequest) error) *forgejointegration.Integration {
	t.Helper()
	if push == nil {
		push = func(context.Context, forgejointegration.PushRequest) error { return nil }
	}
	return forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "standing-executor-token",
		AutoPullRequest: true, MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			repoFullName: {Owner: "forgejo", Repo: "repo"},
		},
	}, client, push)
}

func fmtHex(s string) string {
	out := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		out = append(out, "0123456789abcdef"[s[i]>>4], "0123456789abcdef"[s[i]&0x0f])
	}
	return string(out)
}
