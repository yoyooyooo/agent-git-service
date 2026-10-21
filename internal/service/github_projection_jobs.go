package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errGitHubProjectionPending = errors.New("GitHub projection job is pending or exhausted")

const (
	githubProjectionEnsure         = "github_shadow_ensure"
	githubProjectionClose          = "github_shadow_close"
	githubProjectionMaxAttempts    = 5
	githubProjectionAttemptTimeout = time.Minute
)

func gitHubProjectionDesiredFact(pr db.PullRequest) string {
	fact, _ := json.Marshal([]string{pr.HeadSHA, pr.HeadRef, pr.BaseRef, pr.Title, string(pr.Body)})
	digest := sha256.Sum256(fact)
	return hex.EncodeToString(digest[:])
}

// One provider row carries the desired state. Close and authoritative AGS
// reopen transitions supersede the old trigger and invalidate its claim;
// duplicate dispatch never resets attempts or backoff.
func (s *Service) enqueueGitHubProjection(ctx context.Context, pr db.PullRequest, trigger string) (db.PullRequestProjectionJob, error) {
	repo, err := s.projectionRepoFullNameFromPR(ctx, pr)
	if err != nil {
		return db.PullRequestProjectionJob{}, err
	}
	now := time.Now().UTC()
	job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AgentSessionID: pr.AgentSessionID,
		Provider: ProjectionProviderGitHub, Trigger: trigger, RepoFullName: repo, AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA, GitHubDesiredFactHash: gitHubProjectionDesiredFact(pr), Phase: ForgejoProjectionPhaseQueued, NextRunAt: &now}
	database := s.DBForCtx(ctx)
	if err := database.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "pull_request_id"}, {Name: "provider"}}, DoNothing: true}).Create(&job).Error; err != nil {
		return job, err
	}
	job = db.PullRequestProjectionJob{}
	if err := database.Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderGitHub).First(&job).Error; err != nil {
		return job, err
	}
	if trigger == githubProjectionClose && job.Trigger != githubProjectionClose {
		result := database.Model(&db.PullRequestProjectionJob{}).Where("id = ? AND `trigger` = ? AND attempt = ?", job.ID, job.Trigger, job.Attempt).
			Updates(map[string]any{"trigger": trigger, "phase": ForgejoProjectionPhaseQueued, "attempt": 0, "next_run_at": now, "finished_at": nil, "last_error": "", "last_error_type": ""})
		if result.Error != nil {
			return job, result.Error
		}
		id := job.ID
		job = db.PullRequestProjectionJob{}
		if err := database.First(&job, id).Error; err != nil {
			return job, err
		}
	}
	if trigger == githubProjectionEnsure && job.Trigger == githubProjectionClose && pr.State == db.StateOpen && !pr.Merged {
		// Reopening AGS starts new ensure work even when close completed without
		// a mapping. Fence the full observed close snapshot, and require the
		// authoritative PR to still be open at this atomic transition.
		result := database.Model(&db.PullRequestProjectionJob{}).
			Where("id = ? AND `trigger` = ? AND phase = ? AND attempt = ? AND (next_run_at = ? OR (next_run_at IS NULL AND ? IS NULL))", job.ID, job.Trigger, job.Phase, job.Attempt, job.NextRunAt, job.NextRunAt).
			Where("pull_request_id IN (SELECT id FROM pull_requests WHERE id = ? AND state = ? AND merged = ?)", pr.ID, db.StateOpen, false).
			Updates(map[string]any{"trigger": trigger, "github_desired_fact_hash": gitHubProjectionDesiredFact(pr), "head_sha": pr.HeadSHA, "head_ref": pr.HeadRef, "base_ref": pr.BaseRef,
				"phase": ForgejoProjectionPhaseQueued, "attempt": 0, "next_run_at": now, "started_at": nil, "finished_at": nil, "last_error": "", "last_error_type": ""})
		if result.Error != nil {
			return job, result.Error
		}
		id := job.ID
		job = db.PullRequestProjectionJob{}
		if err := database.First(&job, id).Error; err != nil {
			return job, err
		}
	}
	if trigger == githubProjectionEnsure && job.Trigger == trigger && job.GitHubDesiredFactHash != gitHubProjectionDesiredFact(pr) {
		result := database.Model(&db.PullRequestProjectionJob{}).Where("id = ? AND `trigger` = ? AND phase = ? AND attempt = ? AND (next_run_at = ? OR (next_run_at IS NULL AND ? IS NULL))", job.ID, trigger, job.Phase, job.Attempt, job.NextRunAt, job.NextRunAt).
			Updates(map[string]any{"github_desired_fact_hash": gitHubProjectionDesiredFact(pr), "head_sha": pr.HeadSHA, "head_ref": pr.HeadRef, "base_ref": pr.BaseRef, "phase": ForgejoProjectionPhaseQueued, "attempt": 0, "next_run_at": now, "finished_at": nil})
		if result.Error != nil {
			return job, result.Error
		}
		id := job.ID
		job = db.PullRequestProjectionJob{}
		if err := database.First(&job, id).Error; err != nil {
			return job, err
		}
	}
	if job.Trigger != trigger {
		return job, errGitHubProjectionPending
	}
	return job, nil
}

func githubProjectionRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 5 {
		attempt = 5
	}
	return time.Second * 5 * time.Duration(1<<(attempt-1))
}

// Claim is both a cross-process lease and a generation fence. The trigger is
// part of every CAS because close may replace an in-flight ensure.
func (s *Service) claimGitHubProjection(ctx context.Context, job db.PullRequestProjectionJob) (db.PullRequestProjectionJob, bool, error) {
	now := time.Now().UTC()
	if job.Phase == ForgejoProjectionPhaseProjected || job.Phase == ForgejoProjectionPhaseFailedTerminal || (job.NextRunAt != nil && job.NextRunAt.After(now)) {
		return job, false, nil
	}
	if job.Attempt >= githubProjectionMaxAttempts {
		err := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).Where("id = ? AND `trigger` = ? AND phase = ? AND attempt = ? AND (next_run_at IS NULL OR next_run_at <= ?) AND (next_run_at = ? OR (next_run_at IS NULL AND ? IS NULL))", job.ID, job.Trigger, job.Phase, job.Attempt, now, job.NextRunAt, job.NextRunAt).
			Updates(map[string]any{"phase": ForgejoProjectionPhaseFailedTerminal, "next_run_at": nil, "finished_at": now}).Error
		return job, false, err
	}
	// Match the deployed MySQL/TiDB datetime(3) lease precision without
	// rounding the due-time comparison back before a just-enqueued job.
	lease := now.Add(githubProjectionAttemptTimeout).Truncate(time.Millisecond)
	result := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where("id = ? AND `trigger` = ? AND phase = ? AND attempt = ? AND (next_run_at IS NULL OR next_run_at <= ?) AND (next_run_at = ? OR (next_run_at IS NULL AND ? IS NULL))", job.ID, job.Trigger, job.Phase, job.Attempt, now, job.NextRunAt, job.NextRunAt).
		Updates(map[string]any{"attempt": job.Attempt + 1, "phase": ForgejoProjectionPhasePreflight, "next_run_at": lease, "started_at": now, "finished_at": nil})
	if result.Error != nil || result.RowsAffected != 1 {
		return job, false, result.Error
	}
	// Return only our own CAS values, never a newer worker's fencing token.
	job.Attempt++
	job.Phase = ForgejoProjectionPhasePreflight
	job.NextRunAt = &lease
	job.StartedAt = &now
	job.FinishedAt = nil
	return job, true, nil
}

func (s *Service) gitHubProjectionClaimScope(ctx context.Context, job db.PullRequestProjectionJob) *gorm.DB {
	return s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where("id = ? AND provider = ? AND `trigger` = ? AND attempt = ? AND phase = ? AND next_run_at = ? AND (github_desired_fact_hash = ? OR (github_desired_fact_hash IS NULL AND ? = ''))", job.ID, ProjectionProviderGitHub, job.Trigger, job.Attempt, ForgejoProjectionPhasePreflight, job.NextRunAt, job.GitHubDesiredFactHash, job.GitHubDesiredFactHash)
}

func (s *Service) revalidateGitHubProjectionWrite(ctx context.Context, job db.PullRequestProjectionJob) error {
	var count int64
	if err := s.gitHubProjectionClaimScope(ctx, job).Where("next_run_at > ?", time.Now().UTC()).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return errGenericProjectionClaimChanged
	}
	pr, err := s.projectionPullRequestFact(ctx, job.PullRequestID)
	if err != nil {
		return err
	}
	if job.GitHubDesiredFactHash != "" && gitHubProjectionDesiredFact(pr) != job.GitHubDesiredFactHash {
		return errGenericProjectionClaimChanged
	}
	if pr.State != db.StateOpen || pr.Merged {
		return fmt.Errorf("AGS PR no longer open: %w", ErrInvalidState)
	}
	if _, delegated := DelegatedSessionIDFromContext(ctx); pr.AgentSessionID != nil || delegated {
		_, err = s.RevalidateDelegatedSession(ctx, pr.RepositoryID, "pr.create", "pr:create", map[string]string{"head_ref": pr.HeadRef, "base_ref": pr.BaseRef})
	}
	return err
}

// attemptGitHubProjection persists completion only while its lease is current.
// Provider I/O is never inside a database retry transaction.
func (s *Service) attemptGitHubProjection(ctx context.Context, job db.PullRequestProjectionJob, repoPath string) error {
	pr, err := s.projectionPullRequestFact(ctx, job.PullRequestID)
	if err != nil {
		return err
	}
	if job.Trigger == githubProjectionEnsure && (pr.State != db.StateOpen || pr.Merged) {
		return s.finishGitHubProjection(ctx, job, nil)
	}
	if job.Trigger == githubProjectionClose && pr.State == db.StateOpen && !pr.Merged {
		return fmt.Errorf("refuse close for open AGS PR: %w", ErrInvalidState)
	}
	if pr.AgentSessionID != nil && job.Trigger == githubProjectionEnsure {
		ctx, err = s.ContextForDelegatedSessionID(ctx, *pr.AgentSessionID)
		if err != nil {
			return err
		}
	}
	projection, _, err := s.gitHubProjection(ctx, pr.ID)
	if err != nil {
		return err
	}
	target, configured := s.GitHubIntegration.TargetRepoFor(job.RepoFullName)
	if !configured || !s.GitHubIntegration.ShadowPullRequestsConfigured(job.RepoFullName) {
		return fmt.Errorf("GitHub shadow projection unavailable")
	}
	if projection.ExternalRepo != "" && projection.ExternalRepo != target {
		return fmt.Errorf("GitHub projection target changed: %w", ErrInvalidState)
	}
	if job.Trigger == githubProjectionClose {
		result, _, err := s.GitHubIntegration.CloseShadowPullRequest(ctx, githubintegration.CloseShadowPullRequestRequest{
			RepoFullName: job.RepoFullName, HeadBranch: pr.HeadRef, BaseBranch: pr.BaseRef, ExternalNumber: projection.ExternalNumber, PRNumber: pr.Number, MergedSHA: firstNonEmpty(pr.MergeCommitSHA, pr.HeadSHA), BeforeWrite: func(checkCtx context.Context) error {
				var count int64
				if err := s.gitHubProjectionClaimScope(checkCtx, job).Where("next_run_at > ?", time.Now().UTC()).Count(&count).Error; err != nil {
					return err
				}
				if count != 1 {
					return errGenericProjectionClaimChanged
				}
				current, err := s.projectionPullRequestFact(checkCtx, pr.ID)
				if err != nil {
					return err
				}
				if current.State == db.StateOpen && !current.Merged {
					return ErrInvalidState
				}
				return nil
			}})
		if err != nil {
			return err
		}
		return s.finishGitHubProjection(ctx, job, func(txCtx context.Context) error {
			if err := s.recordGitHubShadowProjectionClosed(txCtx, pr, result, firstNonEmpty(pr.MergeCommitSHA, pr.HeadSHA)); err != nil {
				return err
			}
			var open int64
			if err := s.DBForCtx(txCtx).Model(&db.PullRequestProjection{}).Where("pull_request_id = ? AND state NOT IN ?", pr.ID, []string{ProjectionStateClosed, ProjectionStateMerged}).Count(&open).Error; err != nil {
				return err
			}
			if open == 0 && !pr.Merged {
				return s.enqueueClosedMulticaExternalPRTerminalDeliveryTx(txCtx, s.DBForCtx(txCtx), pr, gitHubProjectionClosedAt(pr))
			}
			return nil
		})
	}
	if !s.GitHubIntegration.ShadowPullRequestEnabled(job.RepoFullName, pr.HeadRef) {
		return s.finishGitHubProjection(ctx, job, nil)
	}
	observed, exists, observeErr := s.GitHubIntegration.ObserveShadowPullRequest(ctx, job.RepoFullName, projection.ExternalNumber, pr.HeadRef, pr.BaseRef)
	if observeErr != nil {
		return observeErr
	}
	if exists && (observed.State == "closed" || observed.Merged) {
		target, _ := s.GitHubIntegration.TargetRepoFor(job.RepoFullName)
		return s.finishGitHubProjection(ctx, job, func(txCtx context.Context) error {
			return s.UpsertPullRequestProjection(txCtx, db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderGitHub, ExternalRepo: target, ExternalNumber: observed.Number, ExternalURL: observed.HTMLURL, SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: ProjectionStateClosed, LastSyncedSHA: pr.HeadSHA})
		})
	}
	if repoPath == "" {
		if s.Git == nil {
			return fmt.Errorf("GitHub projection git store unavailable")
		}
		repoPath, err = s.Git.GetRepoPath(ctx, job.RepoFullName)
		if err != nil {
			return err
		}
	}
	result, handled, err := s.GitHubIntegration.EnsureShadowPullRequest(ctx, githubintegration.ShadowPullRequestRequest{
		RepoFullName: job.RepoFullName, RepoPath: repoPath, HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
		Title: pr.Title, Body: string(pr.Body), AGSNumber: pr.Number, ExternalNumber: projection.ExternalNumber,
		BeforeWrite: func(checkCtx context.Context) error { return s.revalidateGitHubProjectionWrite(checkCtx, job) }})
	if err != nil {
		return err
	}
	if !handled {
		return fmt.Errorf("GitHub shadow ensure not handled")
	}
	if err := s.finishGitHubProjection(ctx, job, func(txCtx context.Context) error {
		return s.UpsertPullRequestProjection(txCtx, db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderGitHub,
			ExternalRepo: result.TargetRepo, ExternalNumber: result.Number, ExternalURL: result.WebURL, SourceBranch: pr.HeadRef, TargetBranch: result.TargetBranch, State: firstNonEmpty(result.State, ProjectionStateOpen), LastSyncedSHA: pr.HeadSHA})
	}); err != nil {
		return err
	}

	return nil
}

func (s *Service) finishGitHubProjection(ctx context.Context, job db.PullRequestProjectionJob, record func(context.Context) error) error {
	return s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialize local materialization with terminal AGS transitions. A provider
		// response arriving after close must never commit a stale open mapping.
		var current db.PullRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, job.PullRequestID).Error; err != nil {
			return err
		}
		if job.Trigger == githubProjectionEnsure && (current.State != db.StateOpen || current.Merged) {
			record = nil
		}
		if job.Trigger == githubProjectionClose && current.State == db.StateOpen && !current.Merged {
			return ErrInvalidState
		}
		if job.Trigger == githubProjectionEnsure && record != nil && job.GitHubDesiredFactHash != "" && gitHubProjectionDesiredFact(current) != job.GitHubDesiredFactHash {
			return errGenericProjectionClaimChanged
		}
		txCtx := ContextWithDB(ctx, tx)
		result := s.gitHubProjectionClaimScope(txCtx, job).Where("next_run_at > ?", time.Now().UTC()).Updates(map[string]any{"phase": ForgejoProjectionPhaseProjected, "next_run_at": nil, "finished_at": time.Now().UTC(), "last_error": "", "last_error_type": ""})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errGenericProjectionClaimChanged
		}
		if record != nil {
			return record(txCtx)
		}
		return nil
	})
}

func (s *Service) gitHubProjection(ctx context.Context, prID uint) (db.PullRequestProjection, bool, error) {
	var row db.PullRequestProjection
	err := s.DBForCtx(ctx).Where("pull_request_id = ? AND provider = ?", prID, ProjectionProviderGitHub).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, false, nil
	}
	return row, err == nil, err
}

func (s *Service) runGitHubProjectionAttempt(ctx context.Context, job db.PullRequestProjectionJob, repoPath string) error {
	claimed, ok, err := s.claimGitHubProjection(ctx, job)
	if err != nil {
		return err
	}
	if !ok {
		if job.Phase == ForgejoProjectionPhaseProjected {
			return nil
		}
		return errGitHubProjectionPending
	}
	attemptCtx, cancel := context.WithDeadline(ctx, *claimed.NextRunAt)
	err = s.attemptGitHubProjection(attemptCtx, claimed, repoPath)
	cancel()
	if err == nil {
		return nil
	}
	phase := ForgejoProjectionPhaseFailedRetryable
	next := time.Now().UTC().Add(githubProjectionRetryDelay(claimed.Attempt))
	updates := map[string]any{"phase": phase, "next_run_at": next, "last_error_type": "projection_failed", "last_error": forgejointegration.SanitizeProjectionErrorSummary(err.Error())}
	if claimed.Attempt >= githubProjectionMaxAttempts || errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		updates["phase"] = ForgejoProjectionPhaseFailedTerminal
		updates["next_run_at"] = nil
		updates["finished_at"] = time.Now().UTC()
	}
	if saveErr := s.gitHubProjectionClaimScope(ctx, claimed).Updates(updates).Error; saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return err
}

func (s *Service) scheduleGitHubProjection(ctx context.Context, jobID uint) {
	if jobID == 0 || s.DisableGitHubProjectionWorker || s.GitHubIntegration == nil {
		return
	}
	if _, loaded := s.githubProjectionJobLocks.LoadOrStore(jobID, struct{}{}); loaded {
		return
	}
	ctx = s.projectionBackgroundContext(ctx)
	s.Wg.Add(1)
	go func() {
		defer s.Wg.Done()
		defer s.githubProjectionJobLocks.Delete(jobID)
		for ctx.Err() == nil {
			var job db.PullRequestProjectionJob
			if err := s.DBForCtx(ctx).First(&job, jobID).Error; err != nil {
				return
			}
			if job.Phase == ForgejoProjectionPhaseProjected || job.Phase == ForgejoProjectionPhaseFailedTerminal {
				if ctx.Err() == nil {
					_ = s.ResumePendingGitHubProjectionJobs(ctx)
				}
				return
			}
			if job.NextRunAt != nil && job.NextRunAt.After(time.Now().UTC()) {
				timer := time.NewTimer(time.Until(*job.NextRunAt))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			if err := s.runGitHubProjectionAttempt(ctx, job, ""); err != nil {
				slog.WarnContext(ctx, "GitHub shadow projection attempt failed", "job_id", jobID, "error", forgejointegration.SanitizeProjectionErrorSummary(err.Error()))
			}
		}
	}()
}

// ResumePendingGitHubProjectionJobs includes future retries and interrupted
// leases. Bounded scans and watch rounds continue draining beyond this batch.
func (s *Service) ResumePendingGitHubProjectionJobs(ctx context.Context) error {
	if s == nil || s.GitHubIntegration == nil || s.DisableGitHubProjectionWorker {
		return nil
	}
	var jobs []db.PullRequestProjectionJob
	if err := s.DBForCtx(ctx).Where("provider = ? AND `trigger` IN ? AND phase NOT IN ?", ProjectionProviderGitHub, []string{githubProjectionEnsure, githubProjectionClose}, forgejoProjectionTerminalPhases).
		Order("next_run_at ASC, id ASC").Limit(100).Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		s.scheduleGitHubProjection(ctx, job.ID)
	}
	return nil
}

func gitHubProjectionClosedAt(pr db.PullRequest) time.Time {
	if pr.ClosedAt != nil {
		return pr.ClosedAt.UTC()
	}
	return pr.UpdatedAt.UTC()
}
