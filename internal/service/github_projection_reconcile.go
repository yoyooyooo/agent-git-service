package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
)

// ReconcileGitHubPullRequestStates inspects a bounded keyset batch, including
// rows locally marked closed (legacy merge bookkeeping was optimistic). The
// cursor advances even on provider errors or ineligible branches, so a noisy
// early PR cannot starve later PRs. Restart begins a new sweep; no authority is
// derived from the cursor or from GitHub data.
func (s *Service) ReconcileGitHubPullRequestStates(ctx context.Context, limit int) (int, error) {
	if s == nil || s.GitHubIntegration == nil {
		return 0, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	s.githubProjectionScanMu.Lock()
	defer s.githubProjectionScanMu.Unlock()
	if err := s.ResumePendingGitHubProjectionJobs(ctx); err != nil {
		return 0, err
	}
	var candidates []db.PullRequest
	if err := s.DBForCtx(ctx).Select("id").Where("id > ?", s.githubProjectionScanCursor).
		Where("state = ? OR id IN (SELECT pull_request_id FROM pull_request_projections WHERE provider = ?)", db.StateOpen, ProjectionProviderGitHub).
		Order("id ASC").Limit(limit).Find(&candidates).Error; err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		s.githubProjectionScanCursor = 0
		return 0, nil
	}
	var errs []error
	processed := 0
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return processed, errors.Join(append(errs, ctx.Err())...)
		}
		s.githubProjectionScanCursor = candidate.ID
		roundCtx, cancel := context.WithTimeout(ctx, githubProjectionAttemptTimeout)
		changed, err := s.reconcileGitHubPullRequestState(roundCtx, candidate.ID)
		cancel()
		if changed {
			processed++
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(candidates) < limit {
		s.githubProjectionScanCursor = 0
	}
	return processed, errors.Join(errs...)
}

func (s *Service) reconcileGitHubPullRequestState(ctx context.Context, id uint) (bool, error) {
	pr, err := s.projectionPullRequestFact(ctx, id)
	if err != nil {
		return false, err
	}
	repo, err := s.projectionRepoFullNameFromPR(ctx, pr)
	if err != nil {
		return false, err
	}
	projection, found, err := s.gitHubProjection(ctx, id)
	if err != nil {
		return false, err
	}
	closed := pr.State != db.StateOpen || pr.Merged
	if closed && !found {
		return false, nil
	}
	if !s.GitHubIntegration.ShadowPullRequestEnabled(repo, pr.HeadRef) {
		// Branch filters limit creation, not retirement of an existing shadow.
		if !closed || !s.GitHubIntegration.ShadowPullRequestsConfigured(repo) {
			return false, nil
		}
	}
	target, _ := s.GitHubIntegration.TargetRepoFor(repo)
	if found && projection.ExternalRepo != target {
		return false, fmt.Errorf("GitHub projection target changed: %w", ErrInvalidState)
	}
	observed, exists, err := s.GitHubIntegration.ObserveShadowPullRequest(ctx, repo, projection.ExternalNumber, pr.HeadRef, pr.BaseRef)
	if err != nil {
		return false, err
	}
	var closeJobs int64
	if !closed {
		if err := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
			Where("pull_request_id = ? AND provider = ? AND `trigger` = ?", id, ProjectionProviderGitHub, githubProjectionClose).Count(&closeJobs).Error; err != nil {
			return false, err
		}
	}
	// A reopen must supersede close work even if an existing terminal shadow
	// needs no provider write. Let the durable ensure path observe it below.
	if !closed && exists && closeJobs == 0 {
		target, _ := s.GitHubIntegration.TargetRepoFor(repo)
		state := ProjectionStateOpen
		if observed.State == "closed" || observed.Merged {
			state = ProjectionStateClosed
		}
		if found && projection.State == state {
			return false, nil
		}
		return true, s.UpsertPullRequestProjection(ctx, db.PullRequestProjection{PullRequestID: id, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderGitHub, ExternalRepo: target, ExternalNumber: observed.Number, ExternalURL: observed.HTMLURL, SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: state, LastSyncedSHA: pr.HeadSHA})
	}
	trigger := githubProjectionEnsure
	if closed {
		trigger = githubProjectionClose
	}
	job, err := s.enqueueGitHubProjection(ctx, pr, trigger)
	if err != nil {
		return false, err
	}
	// A successful job is a durable receipt, not a permanent exemption from
	// observation. Requeue only a proven new drift, never an exhausted failure.
	if job.Phase == ForgejoProjectionPhaseProjected {
		needsRepair := !closed || (exists && observed.State != "closed" && !observed.Merged) || projection.State != ProjectionStateClosed
		if !needsRepair {
			return false, nil
		}
		now := time.Now().UTC()
		result := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).Where("id = ? AND `trigger` = ? AND phase = ? AND attempt = ?", job.ID, trigger, job.Phase, job.Attempt).
			Updates(map[string]any{"phase": ForgejoProjectionPhaseQueued, "attempt": 0, "next_run_at": now, "finished_at": nil})
		if result.Error != nil {
			return false, result.Error
		}
		if result.RowsAffected != 1 {
			return false, nil
		}
		if err := s.DBForCtx(ctx).First(&job, job.ID).Error; err != nil {
			return false, err
		}
	}
	if job.Phase == ForgejoProjectionPhaseFailedTerminal {
		return false, nil
	}
	err = s.runGitHubProjectionAttempt(ctx, job, "")
	s.scheduleGitHubProjection(ctx, job.ID)
	return true, err
}
