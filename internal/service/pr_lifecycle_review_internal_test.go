package service

import (
	"context"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/stretchr/testify/require"
)

// RunLifecycleReviewWorker exercises the real worker without timer polling.
func RunLifecycleReviewWorker(t *testing.T, s *Service, id uint) {
	t.Helper()
	s.runForgejoProjectionJob(context.Background(), id)
	var job db.PullRequestProjectionJob
	require.NoError(t, s.DB.First(&job, id).Error)
	require.Equal(t, ForgejoProjectionPhaseProjected, job.Phase, job.LastError)
}

func TestLifecycleReviewDelayedAsyncCompletion(t *testing.T) {
	for _, change := range []string{"close", "generation", "binding", "merged"} {
		t.Run(change, func(t *testing.T) {
			svc := setupProjectionStateService(t)
			var repo db.Repository
			require.NoError(t, svc.DB.First(&repo).Error)
			pr := db.PullRequest{Number: 801, RepositoryID: repo.ID, State: db.StateOpen, HeadRef: "feature", BaseRef: "main", HeadSHA: "abc1234"}
			require.NoError(t, svc.DB.Create(&pr).Error)
			binding := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, ExternalRepo: "forgejo/repo", ExternalNumber: 42, SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: ProjectionStateOpen}
			require.NoError(t, svc.DB.Create(&binding).Error)
			job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhaseQueued)
			claim, err := svc.claimGenericForgejoProjectionJob(context.Background(), job)
			require.NoError(t, err)
			ctx := contextWithGenericProjectionClaim(context.Background(), claim)
			require.NoError(t, svc.updateForgejoProjectionJobPhase(ctx, job.ID, ForgejoProjectionPhaseRecordingProjection, nil))
			ready, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(ready)
				<-release
				done <- svc.completeGenericForgejoPullRequestProjection(ctx, job.ID, repo.FullName, pr.HeadSHA, pr, forgejointegration.PullRequestResult{Number: 42, ExternalRepo: "forgejo/repo"})
			}()
			<-ready
			switch change {
			case "close":
				require.NoError(t, svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("state", db.StateClosed).Error)
			case "generation":
				require.NoError(t, svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("lifecycle_generation", 1).Error)
			case "binding":
				require.NoError(t, svc.DB.Model(&binding).Update("external_number", 43).Error)
			case "merged":
				require.NoError(t, svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{"merged": true, "state": db.StateClosed}).Error)
			}
			close(release)
			require.Error(t, <-done, "delayed worker must not materialize an obsolete open projection")
			var got db.PullRequestProjectionJob
			require.NoError(t, svc.DB.First(&got, job.ID).Error)
			require.Equal(t, ForgejoProjectionPhaseRecordingProjection, got.Phase)
			require.NoError(t, svc.DB.First(&binding, binding.ID).Error)
			if change == "binding" {
				require.Equal(t, 43, binding.ExternalNumber)
			}
		})
	}
}

func TestLifecycleReviewPendingStartupDiscovery(t *testing.T) {
	for _, kind := range []string{"projected", "retired-closed", "unrelated-terminal"} {
		t.Run(kind, func(t *testing.T) {
			svc := setupProjectionStateService(t)
			svc.DisableForgejoProjectionWorker = true
			var repo db.Repository
			require.NoError(t, svc.DB.First(&repo).Error)
			pr := db.PullRequest{Number: 802, RepositoryID: repo.ID, State: db.StateOpen, HeadRef: "feature", BaseRef: "main", HeadSHA: "abc1234", ForgejoReopenPending: true, LifecycleGeneration: 1}
			require.NoError(t, svc.DB.Create(&pr).Error)
			phase := ForgejoProjectionPhaseProjected
			if kind != "projected" {
				phase = ForgejoProjectionPhaseFailedTerminal
			}
			job := seedGenericProjectionJob(t, svc, pr, phase)
			if kind != "projected" {
				message := "projection job retired because the authoritative AGS pull request is closed"
				if kind == "unrelated-terminal" {
					message = "provider identity drift requires explicit repair"
				}
				require.NoError(t, svc.DB.Model(&job).Updates(map[string]any{"last_error_type": forgejointegration.ProjectionFailurePullRequestStateDrift, "last_error": message}).Error)
			}
			svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true}, nil, nil)
			_, err := svc.EnqueueForgejoPullRequestProjection(context.Background(), pr)
			require.NoError(t, err)
			require.NoError(t, svc.DB.First(&job, job.ID).Error)
			if kind == "unrelated-terminal" {
				require.Equal(t, ForgejoProjectionPhaseFailedTerminal, job.Phase)
				return
			}
			require.Equal(t, ForgejoProjectionPhaseQueued, job.Phase)
			require.NotNil(t, job.NextRunAt)
			require.WithinDuration(t, time.Now(), *job.NextRunAt, time.Minute)
		})
	}
}
