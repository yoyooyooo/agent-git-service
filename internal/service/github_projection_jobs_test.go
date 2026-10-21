package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"gorm.io/gorm"
)

type retryGitHubClient struct {
	remote       githubintegration.PullRequest
	exists       bool
	observeErr   error
	closeErr     error
	ensureErr    error
	observations []int
	closes       int
	ensures      int
	inputs       []githubintegration.PullRequestInput
}

func (c *retryGitHubClient) ObservePullRequest(_ context.Context, _ string, number int, _, _ string) (githubintegration.PullRequest, bool, error) {
	c.observations = append(c.observations, number)
	return c.remote, c.exists, c.observeErr
}
func (c *retryGitHubClient) EnsurePullRequest(_ context.Context, _ string, in githubintegration.PullRequestInput) (githubintegration.PullRequest, error) {
	c.ensures++
	c.inputs = append(c.inputs, in)
	if c.ensureErr != nil {
		return githubintegration.PullRequest{}, c.ensureErr
	}
	c.exists = true
	c.remote = githubintegration.PullRequest{Number: 8, State: "open", HTMLURL: "https://github.example/fixture/repo/pull/8"}
	return c.remote, nil
}
func (c *retryGitHubClient) ClosePullRequest(context.Context, string, int) (githubintegration.PullRequest, error) {
	c.closes++
	if c.closeErr != nil {
		return githubintegration.PullRequest{}, c.closeErr
	}
	c.remote.State = "closed"
	return c.remote, nil
}
func (*retryGitHubClient) ClosePullRequestForBranch(context.Context, string, string, string, string) (githubintegration.PullRequest, bool, error) {
	panic("legacy close must not be used")
}

func retryGitHubService(t *testing.T) (*Service, db.PullRequest, *retryGitHubClient, *int) {
	t.Helper()
	svc := setupProjectionStateService(t)
	svc.DisableGitHubProjectionWorker = true
	svc.BaseURL = "https://ags.example"
	_, pr := seedOpenHeadPR(t, svc, "work/eligible", strings.Repeat("a", 40), 91)
	client := &retryGitHubClient{}
	pushes := new(int)
	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{Enabled: true, Token: "synthetic", MirrorBranchIncludes: []string{"work/*"}, MirrorBranchExcludes: []string{"work/excluded"}, Repos: map[string]githubintegration.RepoMapping{"example-owner/demo": {Owner: "fixture", Repo: "repo"}}}, func(context.Context, string, string, string, string) error { *pushes++; return nil }, client)
	return svc, pr, client, pushes
}
func githubJob(t *testing.T, svc *Service, pr db.PullRequest) db.PullRequestProjectionJob {
	t.Helper()
	var job db.PullRequestProjectionJob
	if err := svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderGitHub).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	return job
}
func dueGitHubJob(t *testing.T, svc *Service, pr db.PullRequest) db.PullRequestProjectionJob {
	t.Helper()
	job := githubJob(t, svc, pr)
	if err := svc.DB.Model(&job).Update("next_run_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	return githubJob(t, svc, pr)
}
func closeGitHubFact(t *testing.T, svc *Service, pr *db.PullRequest, merged bool) {
	t.Helper()
	now := time.Now().UTC()
	pr.State = db.StateClosed
	pr.ClosedAt = &now
	pr.Merged = merged
	updates := map[string]any{"state": pr.State, "merged": merged, "closed_at": now}
	if merged {
		pr.MergeCommitSHA = strings.Repeat("b", 40)
		updates["merge_commit_sha"] = pr.MergeCommitSHA
	}
	if err := svc.DB.Model(pr).Updates(updates).Error; err != nil {
		t.Fatal(err)
	}
}

func TestGitHubProjectionCloseRetriesEOFAndContinuesMultica(t *testing.T) {
	svc, pr, client, pushes := retryGitHubService(t)
	closeGitHubFact(t, svc, &pr, false)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "fixture/repo", 8, ProjectionStateOpen, pr.HeadSHA)
	seedProjection(t, svc, pr, ProjectionProviderForgejo, "fixture/authority", 42, ProjectionStateClosed, pr.HeadSHA)
	if err := svc.DB.AutoMigrate(&db.PullRequestMulticaLink{}, &db.OutboundDelivery{}); err != nil {
		t.Fatal(err)
	}
	svc.MulticaProjection = multicaprojection.New(multicaprojection.Config{Enabled: true, TargetInstance: "fixture", AppURL: "https://multica.example"})
	svc.OutboundEventTargets = map[string][]OutboundTarget{OutboundEventMulticaExternalPRTerminal: {{Name: OutboundTargetNameMulticaExternalPR, Type: OutboundTargetTypeMulticaExternalPR}}}
	if err := svc.DB.Create(&db.PullRequestMulticaLink{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Workspace: "fixture", WorkspaceID: "workspace", IssueID: "issue", IssueKey: "FIX-1", Confidence: db.MulticaLinkConfidenceAuthoritative, Source: db.MulticaLinkSourceTaskToken}).Error; err != nil {
		t.Fatal(err)
	}
	client.observeErr = errors.New("EOF")
	if err := svc.DispatchPullRequestClosedIntegrations(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	job := githubJob(t, svc, pr)
	if job.Phase != ForgejoProjectionPhaseFailedRetryable || job.Attempt != 1 || job.NextRunAt == nil {
		t.Fatalf("failure not durable: %+v", job)
	}
	var count int64
	svc.DB.Model(&db.OutboundDelivery{}).Count(&count)
	if count != 0 || loadProjection(t, svc, pr.ID, ProjectionProviderGitHub).State != ProjectionStateOpen {
		t.Fatal("failed close recorded success or notified Multica")
	}
	client.observeErr = nil
	client.exists = true
	client.remote = githubintegration.PullRequest{Number: 8, State: "open"}
	if err := svc.runGitHubProjectionAttempt(context.Background(), dueGitHubJob(t, svc, pr), ""); err != nil {
		t.Fatal(err)
	}
	row := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	if row.State != ProjectionStateClosed || client.closes != 1 || *pushes != 0 || client.ensures != 0 {
		t.Fatalf("row=%+v closes=%d pushes=%d ensures=%d", row, client.closes, *pushes, client.ensures)
	}
	if err := svc.DB.Model(&db.OutboundDelivery{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("Multica continuation count=%d err=%v", count, err)
	}
	for _, number := range client.observations {
		if number != 8 {
			t.Fatalf("recorded number lost: %v", client.observations)
		}
	}
}

func TestGitHubProjectionEnsureRetriesAndDropsClosedPR(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry ensure", true: "closed while pending"}[terminal], func(t *testing.T) {
			svc, pr, client, _ := retryGitHubService(t)
			client.ensureErr = errors.New("EOF")
			_, handled, err := svc.ensureGitHubShadowPullRequest(context.Background(), "example-owner/demo", "/fixture/repo.git", pr.HeadSHA, pr)
			if err == nil || handled || client.ensures != 1 {
				t.Fatalf("handled=%v err=%v ensures=%d", handled, err, client.ensures)
			}
			if terminal {
				closeGitHubFact(t, svc, &pr, false)
			}
			client.ensureErr = nil
			if err := svc.runGitHubProjectionAttempt(context.Background(), dueGitHubJob(t, svc, pr), "/fixture/repo.git"); err != nil {
				t.Fatal(err)
			}
			job := githubJob(t, svc, pr)
			if job.Phase != ForgejoProjectionPhaseProjected || job.Attempt != 2 {
				t.Fatalf("job=%+v", job)
			}
			_, exists, err := svc.gitHubProjection(context.Background(), pr.ID)
			if err != nil || exists == terminal || client.ensures != map[bool]int{false: 2, true: 1}[terminal] {
				t.Fatalf("exists=%v ensures=%d err=%v", exists, client.ensures, err)
			}
		})
	}
}

func TestGitHubProjectionCloseAlreadyTerminalOrMissingDoesNotWrite(t *testing.T) {
	for _, state := range []string{"closed", "merged", "missing"} {
		t.Run(state, func(t *testing.T) {
			svc, pr, client, pushes := retryGitHubService(t)
			closeGitHubFact(t, svc, &pr, true)
			seedProjection(t, svc, pr, ProjectionProviderGitHub, "fixture/repo", 8, ProjectionStateOpen, pr.HeadSHA)
			client.exists = state != "missing"
			client.remote = githubintegration.PullRequest{Number: 8, State: "closed", Merged: state == "merged"}
			if _, _, err := svc.closeGitHubProjectionDurably(context.Background(), pr); err != nil {
				t.Fatal(err)
			}
			row := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
			if row.State != ProjectionStateClosed || row.LastSyncedSHA != pr.MergeCommitSHA || client.closes != 0 || client.ensures != 0 || *pushes != 0 {
				t.Fatalf("row=%+v client=%+v", row, client)
			}
		})
	}
}

func TestGitHubProjectionAttemptsBackoffAndDuplicateEnqueue(t *testing.T) {
	svc, pr, client, _ := retryGitHubService(t)
	closeGitHubFact(t, svc, &pr, false)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "fixture/repo", 8, ProjectionStateOpen, pr.HeadSHA)
	client.observeErr = errors.New("EOF")
	for attempt := 1; attempt <= githubProjectionMaxAttempts; attempt++ {
		job, err := svc.enqueueGitHubProjection(context.Background(), pr, githubProjectionClose)
		if err != nil {
			t.Fatal(err)
		}
		if attempt > 1 {
			job = dueGitHubJob(t, svc, pr)
		}
		before := time.Now().UTC()
		if err := svc.runGitHubProjectionAttempt(context.Background(), job, ""); err == nil {
			t.Fatal("expected provider failure")
		}
		saved := githubJob(t, svc, pr)
		if saved.Attempt != attempt {
			t.Fatalf("attempt=%d want %d", saved.Attempt, attempt)
		}
		if attempt < githubProjectionMaxAttempts && (saved.NextRunAt == nil || saved.NextRunAt.Before(before.Add(githubProjectionRetryDelay(attempt)-time.Second))) {
			t.Fatalf("backoff not persisted: %+v", saved)
		}
	}
	exhausted := githubJob(t, svc, pr)
	if exhausted.Phase != ForgejoProjectionPhaseFailedTerminal || exhausted.NextRunAt != nil {
		t.Fatalf("not exhausted: %+v", exhausted)
	}
	_, _, _ = svc.closeGitHubProjectionDurably(context.Background(), pr)
	if len(client.observations) != githubProjectionMaxAttempts {
		t.Fatalf("duplicate dispatch reset max attempts: %d", len(client.observations))
	}
}

func TestGitHubProjectionClaimSupersededByClose(t *testing.T) {
	svc, pr, client, pushes := retryGitHubService(t)
	job, err := svc.enqueueGitHubProjection(context.Background(), pr, githubProjectionEnsure)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := svc.claimGitHubProjection(context.Background(), job)
	if err != nil || !ok {
		t.Fatalf("claim=%v %v", ok, err)
	}
	if _, ok, err := svc.claimGitHubProjection(context.Background(), job); err != nil || ok {
		t.Fatalf("duplicate claim=%v %v", ok, err)
	}
	closeGitHubFact(t, svc, &pr, false)
	if _, err := svc.enqueueGitHubProjection(context.Background(), pr, githubProjectionClose); err != nil {
		t.Fatal(err)
	}
	if err := svc.revalidateGitHubProjectionWrite(context.Background(), claimed); err == nil {
		t.Fatal("superseded ensure retained write authority")
	}
	if err := svc.finishGitHubProjection(context.Background(), claimed, nil); !errors.Is(err, errGenericProjectionClaimChanged) {
		t.Fatalf("stale completion accepted: %v", err)
	}
	if client.closes != 0 || client.ensures != 0 || *pushes != 0 {
		t.Fatal("stale worker wrote")
	}
}

func TestGitHubProjectionStartupResumesInterruptedClose(t *testing.T) {
	svc, pr, client, _ := retryGitHubService(t)
	closeGitHubFact(t, svc, &pr, false)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "fixture/repo", 8, ProjectionStateOpen, pr.HeadSHA)
	client.exists = true
	client.remote = githubintegration.PullRequest{Number: 8, State: "closed"}
	job, err := svc.enqueueGitHubProjection(context.Background(), pr, githubProjectionClose)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := svc.claimGitHubProjection(context.Background(), job); err != nil || !ok {
		t.Fatalf("claim=%v %v", ok, err)
	}
	dueGitHubJob(t, svc, pr)
	// Fresh service instance, same isolated durable database, no retained locks.
	restarted := &Service{DB: svc.DB, GitHubIntegration: svc.GitHubIntegration}
	if err := restarted.ResumePendingGitHubProjectionJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted.Wg.Wait()
	saved := githubJob(t, svc, pr)
	if saved.Phase != ForgejoProjectionPhaseProjected || saved.Attempt != 2 || client.closes != 0 {
		t.Fatalf("resume=%+v closes=%d", saved, client.closes)
	}
}

func TestGitHubProjectionMulticaPersistenceFailureRemainsRetryable(t *testing.T) {
	svc, pr, client, _ := retryGitHubService(t)
	closeGitHubFact(t, svc, &pr, false)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "fixture/repo", 8, ProjectionStateOpen, pr.HeadSHA)
	client.exists = true
	client.remote = githubintegration.PullRequest{Number: 8, State: "open"}
	svc.MulticaProjection = multicaprojection.New(multicaprojection.Config{Enabled: true, TargetInstance: "fixture", AppURL: "https://multica.example"})
	svc.OutboundEventTargets = map[string][]OutboundTarget{OutboundEventMulticaExternalPRTerminal: {{Name: OutboundTargetNameMulticaExternalPR, Type: OutboundTargetTypeMulticaExternalPR}}}
	seedProjection(t, svc, pr, ProjectionProviderForgejo, "fixture/authority", 42, ProjectionStateClosed, pr.HeadSHA)
	// Missing Multica table injects a durable continuation failure after GitHub
	// accepts close. Both mapping and job success must roll back together.
	if _, _, err := svc.closeGitHubProjectionDurably(context.Background(), pr); err == nil {
		t.Fatal("expected continuation persistence error")
	}
	job := githubJob(t, svc, pr)
	if job.Phase != ForgejoProjectionPhaseFailedRetryable || loadProjection(t, svc, pr.ID, ProjectionProviderGitHub).State != ProjectionStateOpen {
		t.Fatalf("partial success persisted: %+v", job)
	}
	svc.MulticaProjection = nil
	if err := svc.DB.AutoMigrate(&db.PullRequestMulticaLink{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.runGitHubProjectionAttempt(context.Background(), dueGitHubJob(t, svc, pr), ""); err != nil {
		t.Fatal(err)
	}
	if client.closes != 1 {
		t.Fatal("unknown close replayed instead of observing terminal state")
	}
}

func TestGitHubProjectionCloseWithoutMappingRetiresUnknownEnsure(t *testing.T) {
	svc, pr, client, pushes := retryGitHubService(t)
	// Ensure had created remotely before its result could be mapped locally.
	client.exists = true
	client.remote = githubintegration.PullRequest{Number: 8, State: "open"}
	closeGitHubFact(t, svc, &pr, false)
	if err := svc.DispatchPullRequestClosedIntegrations(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	row := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	if row.State != ProjectionStateClosed || client.closes != 1 || client.ensures != 0 || *pushes != 0 || client.observations[0] != 0 {
		t.Fatalf("row=%+v client=%+v", row, client)
	}
}

func TestGitHubProjectionClaimDoesNotAdoptSupersedingWorkerToken(t *testing.T) {
	svc, pr, _, _ := retryGitHubService(t)
	ctx := context.Background()
	job, err := svc.enqueueGitHubProjection(ctx, pr, githubProjectionEnsure)
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	// Supersede A's successful claim before it returns, in the same database
	// transaction. This deterministically exercises the former reload window.
	const callback = "test:supersede_github_claim"
	if err := svc.DB.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
		if fired || tx.Statement.Table != "pull_request_projection_jobs" {
			return
		}
		fired = true
		otherLease := time.Now().UTC().Add(2 * time.Minute)
		if err := tx.Session(&gorm.Session{NewDB: true}).Model(&db.PullRequestProjectionJob{}).Where("id = ?", job.ID).Updates(map[string]any{"trigger": githubProjectionClose, "attempt": 1, "phase": ForgejoProjectionPhasePreflight, "next_run_at": otherLease}).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.DB.Callback().Update().Remove(callback) })
	claimed, ok, err := svc.claimGitHubProjection(ctx, job)
	if err != nil || !ok || !fired {
		t.Fatalf("claim=%+v ok=%v fired=%v err=%v", claimed, ok, fired, err)
	}
	current := githubJob(t, svc, pr)
	if claimed.NextRunAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("lease exceeds database precision: %s", claimed.NextRunAt)
	}
	if claimed.Trigger != githubProjectionEnsure || claimed.Attempt != 1 || claimed.NextRunAt.Equal(*current.NextRunAt) {
		t.Fatalf("adopted successor token: claim=%+v current=%+v", claimed, current)
	}
	if err := svc.revalidateGitHubProjectionWrite(ctx, claimed); !errors.Is(err, errGenericProjectionClaimChanged) {
		t.Fatalf("stale write authority: %v", err)
	}
	if err := svc.finishGitHubProjection(ctx, claimed, nil); !errors.Is(err, errGenericProjectionClaimChanged) {
		t.Fatalf("stale completion: %v", err)
	}
}

func TestGitHubProjectionStaleExhaustionCannotOverwriteCompletion(t *testing.T) {
	svc, pr, _, _ := retryGitHubService(t)
	ctx := context.Background()
	job, err := svc.enqueueGitHubProjection(ctx, pr, githubProjectionEnsure)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&job).Update("attempt", githubProjectionMaxAttempts-1).Error; err != nil {
		t.Fatal(err)
	}
	job = githubJob(t, svc, pr)
	claim, ok, err := svc.claimGitHubProjection(ctx, job)
	if err != nil || !ok {
		t.Fatalf("claim=%v %v", ok, err)
	}
	// Shorten only this fixture's lease so the delayed scheduler can run after
	// successful completion with its previously loaded fifth-attempt snapshot.
	lease := time.Now().UTC().Add(100 * time.Millisecond)
	if err := svc.DB.Model(&job).Update("next_run_at", lease).Error; err != nil {
		t.Fatal(err)
	}
	claim = githubJob(t, svc, pr)
	if err := svc.finishGitHubProjection(ctx, claim, func(txCtx context.Context) error {
		return svc.UpsertPullRequestProjection(txCtx, db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderGitHub, ExternalRepo: "fixture/repo", ExternalNumber: 8, State: ProjectionStateOpen})
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(lease) + time.Millisecond)
	if _, ok, err := svc.claimGitHubProjection(ctx, claim); err != nil || ok {
		t.Fatalf("stale scheduler=%v %v", ok, err)
	}
	current := githubJob(t, svc, pr)
	if current.Phase != ForgejoProjectionPhaseProjected || loadProjection(t, svc, pr.ID, ProjectionProviderGitHub).ExternalNumber != 8 {
		t.Fatalf("completion overwritten: %+v", current)
	}
}

func TestGitHubProjectionRefreshesMetadataWithoutSHAChange(t *testing.T) {
	svc, pr, client, _ := retryGitHubService(t)
	ctx := context.Background()
	for _, field := range []string{"initial", "title", "body", "duplicate"} {
		switch field {
		case "title":
			pr.Title = "updated title"
		case "body":
			pr.Body = "updated body"
		}
		if err := svc.DB.Model(&pr).Updates(map[string]any{"title": pr.Title, "body": pr.Body}).Error; err != nil {
			t.Fatal(err)
		}
		res, handled, err := svc.ensureGitHubShadowPullRequest(ctx, "example-owner/demo", "/fixture.git", pr.HeadSHA, pr)
		if err != nil || !handled || res.Number != 8 {
			t.Fatalf("%s: res=%+v handled=%v err=%v", field, res, handled, err)
		}
	}
	if client.ensures != 3 || len(client.inputs) != 3 || client.inputs[1].Title != "updated title" || !strings.Contains(client.inputs[2].Body, "updated body") {
		t.Fatalf("metadata refresh lost: %+v", client)
	}
	job := githubJob(t, svc, pr)
	if job.HeadSHA != pr.HeadSHA || job.GitHubDesiredFactHash != gitHubProjectionDesiredFact(pr) || job.Phase != ForgejoProjectionPhaseProjected {
		t.Fatalf("receipt=%+v", job)
	}
}

func TestGitHubProjectionMetadataChangeFencesInflightEnsure(t *testing.T) {
	svc, pr, _, _ := retryGitHubService(t)
	ctx := context.Background()
	job, err := svc.enqueueGitHubProjection(ctx, pr, githubProjectionEnsure)
	if err != nil {
		t.Fatal(err)
	}
	claim, ok, err := svc.claimGitHubProjection(ctx, job)
	if err != nil || !ok {
		t.Fatalf("claim=%v %v", ok, err)
	}
	pr.Title = "changed during ensure"
	if err := svc.DB.Model(&pr).Update("title", pr.Title).Error; err != nil {
		t.Fatal(err)
	}
	refresh, err := svc.enqueueGitHubProjection(ctx, pr, githubProjectionEnsure)
	if err != nil || refresh.Phase != ForgejoProjectionPhaseQueued || refresh.Attempt != 0 {
		t.Fatalf("refresh=%+v err=%v", refresh, err)
	}
	if err := svc.revalidateGitHubProjectionWrite(ctx, claim); !errors.Is(err, errGenericProjectionClaimChanged) {
		t.Fatalf("old metadata write allowed: %v", err)
	}
	if err := svc.finishGitHubProjection(ctx, claim, func(context.Context) error { t.Fatal("stale fact materialized"); return nil }); !errors.Is(err, errGenericProjectionClaimChanged) {
		t.Fatalf("old receipt accepted: %v", err)
	}
}
