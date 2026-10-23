package service_test

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/stretchr/testify/require"
)

func TestLateClosedMetadataAdvanceAfterEvent(t *testing.T) {
	svc, pr, client := lateCloseFixture(t)
	ctx := context.Background()
	configureMulticaTerminalDelivery(svc)
	addAuthoritativeMulticaLink(t, svc, pr)
	h, b := signedLateClose("2001-01-01T00:00:01Z")
	_, err := svc.HandleForgejoWebhook(ctx, h, b)
	require.NoError(t, err)
	reopenLateClose(t, svc, pr, "rest")
	// The event exists before the metadata update. The barrier forces that
	// update to commit before the callback can issue its exact GET.
	client.set(db.StateClosed, "2001-01-01T00:00:03Z")
	h, b = signedLateClose("2001-01-01T00:00:03Z")
	ready, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { close(ready); <-release; _, err := svc.HandleForgejoWebhook(ctx, h, b); done <- err }()
	<-ready
	client.set(db.StateClosed, "2001-01-01T00:00:04Z")
	close(release)
	require.NoError(t, <-done)
	current, err := svc.GetPRByID(ctx, pr.ID)
	require.NoError(t, err)
	require.Equal(t, db.StateClosed, current.State, "metadata after the only legitimate close must not swallow it")
	require.NotNil(t, current.ClosedAt)
	var count int64
	require.NoError(t, svc.DB.Model(&db.OutboundDelivery{}).Count(&count).Error)
	require.EqualValues(t, 2, count)
	// Retrying the accepted event remains idempotent even though live metadata
	// is newer; the accepted watermark is the lifecycle event's timestamp.
	_, err = svc.HandleForgejoWebhook(ctx, h, b)
	require.NoError(t, err)
	require.NoError(t, svc.DB.Model(&db.OutboundDelivery{}).Count(&count).Error)
	require.EqualValues(t, 2, count)
}

func TestLateClosedPendingReopenRetry(t *testing.T) {
	for _, transport := range []string{"rest", "graphql", "projection-retry", "startup"} {
		t.Run(transport, func(t *testing.T) {
			svc, pr, client := lateCloseFixture(t)
			ctx := context.Background()
			h, b := signedLateClose("2001-01-01T00:00:01Z")
			_, err := svc.HandleForgejoWebhook(ctx, h, b)
			require.NoError(t, err)
			// Seed a completed same-head job: this must not hide pending lifecycle work.
			_, err = svc.EnqueueForgejoPullRequestProjection(ctx, pr)
			require.NoError(t, err)
			// The native PR is closed, so enqueue is intentionally ineligible here.
			job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo, Trigger: service.ForgejoProjectionTriggerPullRequest, Phase: service.ForgejoProjectionPhaseProjected, HeadSHA: pr.HeadSHA, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, RepoFullName: pr.Repository.FullName, AGSPRNumber: pr.Number}
			require.NoError(t, svc.DB.Create(&job).Error)
			client.mu.Lock()
			client.failAfterWrite = true
			client.mu.Unlock()
			reopenLateClose(t, svc, pr, "rest")
			pending, err := svc.GetPRByID(ctx, pr.ID)
			require.NoError(t, err)
			require.True(t, pending.ForgejoReopenPending)
			client.mu.Lock()
			require.Equal(t, db.StateOpen, client.state, "provider write succeeded before GET failed")
			client.readError = nil
			client.failAfterWrite = false
			client.mu.Unlock()
			if transport == "startup" {
				svc.DisableForgejoProjectionWorker = false
				require.NoError(t, svc.ResumePendingForgejoProjectionJobs(ctx))
				svc.Wg.Wait()
			} else if transport == "projection-retry" {
				_, err = svc.RetryForgejoPullRequestProjection(ctx, pr.Repository.FullName, pr.Number)
				require.NoError(t, err)
				// Run the actual generic worker; no sleeping/polling for its completion.
				service.RunLifecycleReviewWorker(t, svc, job.ID)
			} else {
				reopenLateClose(t, svc, pr, transport)
			}
			assertLateCloseOpen(t, svc, pr)
			client.set(db.StateClosed, "2001-01-01T00:00:03Z")
			h, b = signedLateClose("2001-01-01T00:00:03Z")
			_, err = svc.HandleForgejoWebhook(ctx, h, b)
			require.NoError(t, err)
			current, err := svc.GetPRByID(ctx, pr.ID)
			require.NoError(t, err)
			require.Equal(t, db.StateClosed, current.State)
		})
	}
}
