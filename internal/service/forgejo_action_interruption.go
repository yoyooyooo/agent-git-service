package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrForgejoActionObservationUnavailable means that an action could not inspect
// its exact live facts. It is NOT evidence of changed authority, and never
// authorizes a provider write. Keep the underlying cancellation/read cause.
var ErrForgejoActionObservationUnavailable = errors.New("Forgejo action observation unavailable")

func forgejoActionObservationError(operation string, err error) error {
	return fmt.Errorf("%s: %w: %w", operation, ErrForgejoActionObservationUnavailable, err)
}

func forgejoActionReadError(operation string, err error) error {
	// A proven missing database identity still fails closed as factual drift.
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return forgejoActionObservationError(operation, err)
}

// No string matching of private provider responses. A concrete authorization
// denial wins over concurrent cancellation; cancellation cannot restore it.
func IsForgejoActionObservationUnavailable(err error) bool {
	if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		return false
	}
	return errors.Is(err, ErrForgejoActionObservationUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Service) readForgejoActionProjection(ctx context.Context, prID uint) (db.PullRequestProjection, bool, error) {
	var projection db.PullRequestProjection
	err := s.DBForCtx(ctx).Where("pull_request_id = ? AND provider = ?", prID, ProjectionProviderForgejo).First(&projection).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return projection, false, nil
	}
	if err != nil {
		return projection, false, forgejoActionObservationError("read action projection", err)
	}
	return projection, true, nil
}

const forgejoActionCleanupTimeout = 5 * time.Second

// recordForgejoActionInterruption is LOCAL bookkeeping after the execution has
// returned. It deliberately does not call Git, providers, notifications, retry
// scheduling or authorization, and never extends an intent's expiry. Recovery
// remains owned by the existing explicit retry/startup worker with fresh checks.
// Lock order matches completion/denial: job, then intent. Only this exact bound
// generation may move, and a terminal decision must never be resurrected.
func (s *Service) recordForgejoActionInterruption(ctx context.Context) (bool, error) {
	binding, ok := forgejoActionBindingFromContext(ctx)
	if !ok || s == nil {
		return false, nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgejoActionCleanupTimeout)
	defer cancel()
	changed := false
	err := s.DBForCtx(cleanup).WithContext(cleanup).Transaction(func(tx *gorm.DB) error {
		var job *db.PullRequestProjectionJob
		if binding.JobID != 0 {
			var row db.PullRequestProjectionJob
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, binding.JobID).Error; err != nil {
				return err
			}
			if !sameActionIntentJobBinding(row, binding) || row.Trigger != ForgejoProjectionTriggerActionRebase {
				return nil
			}
			if stringInSlice(row.Phase, []string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal, ForgejoProjectionPhaseNeedsRebase}) {
				return nil
			}
			job = &row
		}
		var intent db.PullRequestActionIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&intent, "id = ?", binding.IntentID).Error; err != nil {
			return err
		}
		if intent.Action != "pr.rebase" || !stringInSlice(intent.State, forgejoActionActiveStates()) {
			return nil
		}
		if job == nil {
			var boundJobs int64
			if err := tx.Model(&db.PullRequestProjectionJob{}).Where("action_intent_id = ?", binding.IntentID).Count(&boundJobs).Error; err != nil {
				return err
			}
			// A caller without a job generation cannot overwrite an executor
			// that was admitted elsewhere in the meantime.
			if boundJobs != 0 {
				return nil
			}
		}
		if job != nil && (job.PullRequestID != intent.PullRequestID || job.RepositoryID != intent.RepositoryID || job.AGSPRNumber != intent.AGSPRNumber || job.ExternalRepo != intent.ForgejoRepo || job.ExternalNumber != intent.ForgejoPRNumber || !sameOptionalString(job.AgentSessionID, intent.AgentSessionID)) {
			return fmt.Errorf("interrupted rebase job and intent disagree")
		}
		// Before an executor job exists, preserve the intent but do not invent one.
		// After native mutation, only a previously durable desired SHA is retained.
		now := time.Now().UTC()
		summary := "Action observation interrupted; inspect or retry the same generation with current authorization. No provider write was authorized by cleanup."
		if err := tx.Model(&db.PullRequestActionIntent{}).Where("id = ? AND state = ?", intent.ID, intent.State).Updates(map[string]any{"state": ForgejoActionIntentRecovery, "failure_code": "action_observation_unavailable", "failure_summary": summary, "finished_at": nil, "updated_at": now}).Error; err != nil {
			return err
		}
		if job != nil {
			if err := tx.Model(&db.PullRequestProjectionJob{}).Where("id = ? AND action_generation = ? AND action_intent_id = ? AND phase = ?", job.ID, job.ActionGeneration, binding.IntentID, job.Phase).Updates(map[string]any{"phase": ForgejoProjectionPhaseFailedRetryable, "last_error_type": "action_observation_unavailable", "last_error": summary, "next_run_at": nil, "finished_at": &now, "updated_at": now}).Error; err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	return changed && err == nil, err
}
