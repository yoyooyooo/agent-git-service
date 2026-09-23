package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"gorm.io/gorm"
)

func seedGenericProjectionJob(t *testing.T, svc *Service, pr db.PullRequest, phase string) db.PullRequestProjectionJob {
	t.Helper()
	now := time.Now().UTC()
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID,
		Provider: ProjectionProviderForgejo, Trigger: ForgejoProjectionTriggerPullRequest,
		RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA,
		Phase: phase, NextRunAt: &now,
	}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatalf("create generic projection job: %v", err)
	}
	return job
}

func TestGenericProjectionLateWritersCannotReviveTerminalOrProjectedJob(t *testing.T) {
	for _, terminalPhase := range forgejoProjectionTerminalPhases {
		t.Run(terminalPhase, func(t *testing.T) {
			svc, pr, _ := setupIntentTestService(t)
			job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhaseQueued)
			stale := job
			if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", job.ID).Updates(map[string]any{
				"phase": terminalPhase, "last_error_type": "terminal_fact", "next_run_at": nil,
			}).Error; err != nil {
				t.Fatal(err)
			}

			if _, err := svc.claimGenericForgejoProjectionJob(context.Background(), stale); err == nil {
				t.Fatal("late worker claimed terminal generic projection job")
			}
			if err := svc.retryGenericForgejoProjectionJob(context.Background(), stale, map[string]any{
				"phase": ForgejoProjectionPhaseQueued, "updated_at": time.Now().UTC(),
			}); err == nil {
				t.Fatal("manual retry CAS revived terminal generic projection job")
			}
			svc.markForgejoProjectionJobFailed(context.Background(), job.ID, ForgejoProjectionPhaseFailedRetryable, nil,
				newProjectionJobFailure(job.RemoteRef, job.HeadSHA, errors.New("late failure"), true))

			var got db.PullRequestProjectionJob
			if err := svc.DB.First(&got, job.ID).Error; err != nil {
				t.Fatal(err)
			}
			if got.Phase != terminalPhase || got.LastErrorType != "terminal_fact" {
				t.Fatalf("late writer changed terminal job: phase=%s type=%s", got.Phase, got.LastErrorType)
			}
		})
	}
}

func TestGenericProjectionTakeoverRejectsLateWorkerGeneration(t *testing.T) {
	svc, pr, _ := setupIntentTestService(t)
	job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhaseQueued)
	first, err := svc.claimGenericForgejoProjectionJob(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx := contextWithGenericProjectionClaim(context.Background(), first)

	past := time.Now().UTC().Add(-time.Second)
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", job.ID).Update("next_run_at", &past).Error; err != nil {
		t.Fatal(err)
	}
	var takeoverSnapshot db.PullRequestProjectionJob
	if err := svc.DB.First(&takeoverSnapshot, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	second, err := svc.claimGenericForgejoProjectionJob(context.Background(), takeoverSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	secondCtx := contextWithGenericProjectionClaim(context.Background(), second)
	if second.Attempt != first.Attempt+1 {
		t.Fatalf("takeover attempt=%d, want %d", second.Attempt, first.Attempt+1)
	}

	if err := svc.updateForgejoProjectionJobPhase(firstCtx, job.ID, ForgejoProjectionPhaseRecordingProjection, nil); err == nil {
		t.Fatal("late worker wrote after another generation took over")
	}
	if err := svc.updateForgejoProjectionJobPhase(secondCtx, job.ID, ForgejoProjectionPhaseRecordingProjection, nil); err != nil {
		t.Fatalf("current worker generation could not write: %v", err)
	}
}

func TestGenericProjectionManualRetryAdvancesGenerationAndRejectsActiveWorker(t *testing.T) {
	svc, pr, _ := setupIntentTestService(t)
	job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhaseQueued)
	active, err := svc.claimGenericForgejoProjectionJob(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	activeCtx := contextWithGenericProjectionClaim(context.Background(), active)

	var retrySnapshot db.PullRequestProjectionJob
	if err := svc.DB.First(&retrySnapshot, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	workerReady := make(chan struct{})
	retryCommitted := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(workerReady)
		<-retryCommitted
		if err := svc.updateForgejoProjectionJobPhase(activeCtx, job.ID, ForgejoProjectionPhaseRecordingProjection, nil); err == nil {
			t.Error("old active worker wrote a phase after manual retry")
		}
		svc.markForgejoProjectionJobFailed(activeCtx, job.ID, ForgejoProjectionPhaseFailedRetryable, nil,
			newProjectionJobFailure(job.RemoteRef, job.HeadSHA, errors.New("old worker failure"), true))
	}()
	<-workerReady
	now := time.Now().UTC()
	if err := svc.retryGenericForgejoProjectionJob(context.Background(), retrySnapshot, map[string]any{
		"next_run_at": &now, "updated_at": now, "last_error_type": "", "last_error": "", "finished_at": nil,
	}); err != nil {
		close(retryCommitted)
		wg.Wait()
		t.Fatal(err)
	}
	close(retryCommitted)
	wg.Wait()

	var got db.PullRequestProjectionJob
	if err := svc.DB.First(&got, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Attempt != active.Attempt+1 || got.Phase != ForgejoProjectionPhaseQueued || got.LastError != "" {
		t.Fatalf("manual retry generation was overwritten: %#v", got)
	}
	if err := svc.revalidateGenericProjectionProviderWrite(activeCtx, active, pr); err == nil {
		t.Fatal("old active worker retained provider authority after manual retry")
	}
}

func TestGenericProjectionTakeoverStopsOldProviderAndMappingWrites(t *testing.T) {
	svc := setupProjectionStateService(t)
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{Number: 776, RepositoryID: repo.ID, State: db.StateOpen, HeadRef: "agent/takeover", HeadSHA: "abc1234", BaseRef: "main"}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhaseQueued)
	first, err := svc.claimGenericForgejoProjectionJob(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	firstCtx := contextWithGenericProjectionClaim(context.Background(), first)
	if err := svc.updateForgejoProjectionJobPhase(firstCtx, job.ID, ForgejoProjectionPhasePushingRef, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.revalidateGenericProjectionProviderWrite(firstCtx, first, pr); err != nil {
		t.Fatalf("current provider check: %v", err)
	}

	past := time.Now().UTC().Add(-time.Second)
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", job.ID).Update("next_run_at", &past).Error; err != nil {
		t.Fatal(err)
	}
	var takeoverSnapshot db.PullRequestProjectionJob
	if err := svc.DB.First(&takeoverSnapshot, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	second, err := svc.claimGenericForgejoProjectionJob(context.Background(), takeoverSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	providerCalls := 0
	if err := svc.revalidateGenericProjectionProviderWrite(firstCtx, first, pr); err == nil {
		providerCalls++
	}
	if providerCalls != 0 {
		t.Fatalf("old provider calls=%d after takeover", providerCalls)
	}
	result := forgejointegration.PullRequestResult{ExternalRepo: "other/repo", Number: 99, URL: "https://forgejo.local/other/repo/pulls/99"}
	if err := svc.completeGenericForgejoPullRequestProjection(firstCtx, job.ID, "example-owner/demo", pr.HeadSHA, pr, result); err == nil {
		t.Fatal("old generation recorded mapping after takeover")
	}
	var mappings int64
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderForgejo).Count(&mappings).Error; err != nil {
		t.Fatal(err)
	}
	if mappings != 0 || second.Attempt != first.Attempt+1 {
		t.Fatalf("takeover mapping/generation drift: mappings=%d second=%#v", mappings, second)
	}
}

func TestGenericProjectionFinalCASRollsBackMapping(t *testing.T) {
	svc := setupProjectionStateService(t)
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{Number: 777, RepositoryID: repo.ID, State: db.StateOpen, HeadRef: "agent/final-cas", HeadSHA: "abc1234", BaseRef: "main"}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhaseQueued)
	claimed, err := svc.claimGenericForgejoProjectionJob(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	ctx := contextWithGenericProjectionClaim(context.Background(), claimed)
	if err := svc.updateForgejoProjectionJobPhase(ctx, job.ID, ForgejoProjectionPhaseRecordingProjection, nil); err != nil {
		t.Fatal(err)
	}
	svc.testGenericProjectionBeforeFinalCAS = func(tx *gorm.DB) {
		if err := tx.Model(&db.PullRequestProjectionJob{}).Where("id = ?", job.ID).Update("phase", ForgejoProjectionPhaseQueued).Error; err != nil {
			t.Errorf("inject final CAS drift: %v", err)
		}
	}
	result := forgejointegration.PullRequestResult{ExternalRepo: "forgejo/repo", Number: 777, URL: "https://forgejo.local/forgejo/repo/pulls/777"}
	if err := svc.completeGenericForgejoPullRequestProjection(ctx, job.ID, repo.FullName, pr.HeadSHA, pr, result); err == nil {
		t.Fatal("final generation CAS unexpectedly succeeded")
	}
	var mappings int64
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderForgejo).Count(&mappings).Error; err != nil {
		t.Fatal(err)
	}
	var got db.PullRequestProjectionJob
	if err := svc.DB.First(&got, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if mappings != 0 || got.Phase != ForgejoProjectionPhaseRecordingProjection {
		t.Fatalf("failed final CAS did not roll back transaction: mappings=%d phase=%s", mappings, got.Phase)
	}
}

func TestGenericProjectionClaimDeadlineEqualsDurableLease(t *testing.T) {
	svc, pr, _ := setupIntentTestService(t)
	svc.ForgejoProjectionWorkerTimeout = 2 * time.Second
	job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhaseQueued)
	claimed, err := svc.claimGenericForgejoProjectionJob(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := svc.forgejoProjectionAttemptContext(context.Background(), claimed)
	defer cancel()
	deadline, ok := workerCtx.Deadline()
	if !ok || claimed.NextRunAt == nil || !deadline.Equal(*claimed.NextRunAt) {
		t.Fatalf("worker deadline=%v ok=%v lease=%v", deadline, ok, claimed.NextRunAt)
	}
}

func TestGenericProjectionEnqueueCannotReplaceActionGeneration(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	svc.DisableForgejoProjectionWorker = true
	intent := seedDispatchedIntent(t, svc, pr, projection)
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseRebasing,
	}
	_ = bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := svc.EnqueueForgejoPullRequestProjection(context.Background(), pr); err == nil {
		t.Fatal("generic enqueue accepted an exact action generation row")
	}
	var got db.PullRequestProjectionJob
	if err := svc.DB.First(&got, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Trigger != ForgejoProjectionTriggerActionRebase || got.ActionIntentID == nil || *got.ActionIntentID != intent.ID || got.Phase != ForgejoProjectionPhaseRebasing {
		t.Fatalf("generic enqueue overwrote action generation: %#v", got)
	}
}

func TestDuplicateGenericProjectionEnqueueDoesNotRewindActiveGeneration(t *testing.T) {
	svc, pr, _ := setupIntentTestService(t)
	svc.DisableForgejoProjectionWorker = true
	job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhasePushingRef)
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", job.ID).Updates(map[string]any{
		"attempt": 3, "last_error_type": "active_generation",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EnqueueForgejoPullRequestProjection(context.Background(), pr); err != nil {
		t.Fatal(err)
	}
	var got db.PullRequestProjectionJob
	if err := svc.DB.First(&got, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Phase != ForgejoProjectionPhasePushingRef || got.Attempt != 3 || got.LastErrorType != "active_generation" {
		t.Fatalf("duplicate enqueue rewound active generation: %#v", got)
	}
}

func TestDuplicateGenericProjectionEnqueuePreservesTerminalFact(t *testing.T) {
	for _, terminalPhase := range forgejoProjectionTerminalPhases {
		t.Run(terminalPhase, func(t *testing.T) {
			svc, pr, _ := setupIntentTestService(t)
			svc.DisableForgejoProjectionWorker = true
			job := seedGenericProjectionJob(t, svc, pr, terminalPhase)
			if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", job.ID).
				Updates(map[string]any{"last_error_type": "terminal_fact", "next_run_at": nil}).Error; err != nil {
				t.Fatal(err)
			}

			got, err := svc.EnqueueForgejoPullRequestProjection(context.Background(), pr)
			if err != nil {
				t.Fatalf("duplicate enqueue: %v", err)
			}
			if got.ID != job.ID || got.Phase != terminalPhase || got.LastErrorType != "terminal_fact" {
				t.Fatalf("duplicate enqueue overwrote terminal job: %#v", got)
			}
		})
	}
}

func TestGenericProjectionSuccessRetainsAttemptHistory(t *testing.T) {
	svc, pr, _ := setupIntentTestService(t)
	svc.DisableForgejoProjectionWorker = true
	ctx := context.Background()
	job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhaseQueued)
	claimed, err := svc.claimGenericForgejoProjectionJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	claimedCtx := contextWithGenericProjectionClaim(ctx, claimed)
	svc.markForgejoProjectionJobFailed(claimedCtx, claimed.ID, ForgejoProjectionPhaseFailedRetryable, nil,
		newProjectionJobFailure(claimed.RemoteRef, claimed.HeadSHA, errors.New("attempt 1 timeout"), true))

	var afterFail db.PullRequestProjectionJob
	if err := svc.DB.First(&afterFail, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if afterFail.Attempt != 1 || afterFail.LastError == "" {
		t.Fatalf("attempt 1 failure not retained on job: %#v", afterFail)
	}

	claimed2, err := svc.claimGenericForgejoProjectionJob(ctx, afterFail)
	if err != nil {
		t.Fatal(err)
	}
	if claimed2.Attempt != 2 {
		t.Fatalf("attempt=%d, want 2", claimed2.Attempt)
	}
	claimed2Ctx := contextWithGenericProjectionClaim(ctx, claimed2)
	if err := svc.updateForgejoProjectionJobPhase(claimed2Ctx, claimed2.ID, ForgejoProjectionPhaseRecordingProjection, map[string]any{
		"external_repo": "forgejo/repo", "external_number": 42, "external_url": "http://forgejo.local/pulls/42",
	}); err != nil {
		t.Fatalf("recording phase: %v", err)
	}
	if err := svc.completeGenericForgejoPullRequestProjection(claimed2Ctx, claimed2.ID, "example-owner/demo", pr.HeadSHA, pr, forgejointegration.PullRequestResult{
		Number: 42, URL: "http://forgejo.local/pulls/42", ExternalRepo: "forgejo/repo", HeadSHA: pr.HeadSHA,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var got db.PullRequestProjectionJob
	if err := svc.DB.Preload("Attempts").First(&got, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Phase != ForgejoProjectionPhaseProjected {
		t.Fatalf("phase=%s, want projected", got.Phase)
	}
	if got.LastError == "" || !strings.Contains(got.LastError, "attempt 1 timeout") {
		t.Fatalf("success erased attempt 1 last_error: %#v", got)
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("attempts=%#v, want 2", got.Attempts)
	}
	first, second := got.Attempts[0], got.Attempts[1]
	if first.Attempt > second.Attempt {
		first, second = second, first
	}
	if first.Attempt != 1 || first.ErrorSummary == "" || !strings.Contains(first.ErrorSummary, "attempt 1 timeout") {
		t.Fatalf("attempt 1 history missing error: %#v", first)
	}
	if second.Attempt != 2 || second.Status != projectionJobAttemptProjected {
		t.Fatalf("attempt 2 history=%#v", second)
	}

	status, ok, err := svc.PullRequestProjectionJob(ctx, pr.ID)
	if err != nil || !ok {
		t.Fatalf("presentation: ok=%v err=%v", ok, err)
	}
	if status.LastError == "" || len(status.Attempts) != 2 || status.Attempts[0].Error == "" {
		t.Fatalf("presentation dropped attempt history: %#v", status)
	}
}
