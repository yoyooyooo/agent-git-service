package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

func TestActionCleanupUsesBoundedFreshContextAndRollsBackBothRowsOnFailure(t *testing.T) {
	svc, ctx, pr, projection, _ := interruptionFixture(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	id := intent.ID
	job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo, Trigger: ForgejoProjectionTriggerActionRebase, ActionGeneration: 1, ActionIntentID: &id, AGSPRNumber: pr.Number, ExternalRepo: projection.ExternalRepo, ExternalNumber: 42, Phase: ForgejoProjectionPhasePushingRef, DesiredAGSHeadSHA: pr.HeadSHA, ExpectedForgejoOldHeadSHA: pr.HeadSHA}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	ctx = contextWithForgejoActionJob(ctx, job)
	observed := false
	if err := svc.DB.Callback().Update().Before("gorm:update").Register("interruption_database_failure", func(tx *gorm.DB) {
		if tx.Statement.Table != "pull_request_projection_jobs" {
			return
		}
		observed = true
		deadline, ok := tx.Statement.Context.Deadline()
		if !ok || tx.Statement.Context.Err() != nil || time.Until(deadline) > forgejoActionCleanupTimeout {
			t.Error("cleanup escaped its fresh bounded context")
		}
		_ = tx.AddError(errors.New("fixture local DB write failed"))
	}); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	changed, err := svc.recordForgejoActionInterruption(canceled)
	if err == nil || changed || !observed {
		t.Fatal("failed local receipt claimed success", changed, err)
	}
	var saved db.PullRequestActionIntent
	if err := svc.DB.First(&saved, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.State != intent.State {
		t.Fatal("partial local state committed")
	}
}

func TestActionProjectionReadFailureIsNotMissingMapping(t *testing.T) {
	svc, ctx, pr, _, _ := interruptionFixture(t)
	if err := svc.DB.Callback().Query().Before("gorm:query").Register("fixture_projection_read_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "pull_request_projections" {
			_ = tx.AddError(errors.New("fixture database temporarily unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, found, err := svc.readForgejoActionProjection(ctx, pr.ID)
	if found || !IsForgejoActionObservationUnavailable(err) {
		t.Fatal("read failure silently became missing mapping", err)
	}
	if IsForgejoActionObservationUnavailable(forgejoActionReadError("identity", gorm.ErrRecordNotFound)) {
		t.Fatal("proven missing identity became transient")
	}
}
