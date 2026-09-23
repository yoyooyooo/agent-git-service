package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	applog "github.com/ngaut/agent-git-service/internal/logging"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ForgejoProjectionTriggerPullRequest  = "pull_request_projection"
	ForgejoProjectionTriggerActionRebase = "forgejo_action_rebase"

	ForgejoProjectionPhasePreflight           = "preflight"
	ForgejoProjectionPhaseRebasing            = "rebasing"
	ForgejoProjectionPhaseProjectionResume    = "projection_resume"
	ForgejoProjectionPhaseVerifyingPR         = "verifying_pr"
	ForgejoProjectionPhaseNeedsRebase         = "needs_rebase"
	ForgejoProjectionPhaseQueued              = "queued"
	ForgejoProjectionPhasePushingRef          = "pushing_ref"
	ForgejoProjectionPhaseVerifyingRef        = "verifying_ref"
	ForgejoProjectionPhaseEnsuringPR          = "ensuring_pr"
	ForgejoProjectionPhaseRecordingProjection = "recording_projection"
	ForgejoProjectionPhaseProjected           = "projected"
	ForgejoProjectionPhaseFailedRetryable     = "failed_retryable"
	ForgejoProjectionPhaseFailedTerminal      = "failed_terminal"

	forgejoProjectionFailureContextCanceled = "context_canceled"
	forgejoProjectionFailureContextDeadline = "context_deadline_exceeded"
	forgejoProjectionFailureTransport       = "transport_error"

	defaultForgejoProjectionWorkerTimeout     = 10 * time.Minute
	defaultForgejoProjectionWorkerMaxAttempts = 3
	defaultForgejoProjectionWorkerRetryDelay  = 250 * time.Millisecond

	projectionJobAttemptRunning         = "running"
	projectionJobAttemptRetryableFailed = "retryable_failed"
	projectionJobAttemptTerminalFailed  = "terminal_failed"
	projectionJobAttemptProjected       = "projected"
)

var errGenericProjectionClaimChanged = errors.New("generic projection job snapshot changed")

var forgejoProjectionTerminalPhases = []string{
	ForgejoProjectionPhaseProjected,
	ForgejoProjectionPhaseFailedTerminal,
}

type genericProjectionClaimContextKey struct{}

type genericProjectionClaim struct {
	JobID   uint
	Attempt int
}

func contextWithGenericProjectionClaim(ctx context.Context, job db.PullRequestProjectionJob) context.Context {
	return context.WithValue(ctx, genericProjectionClaimContextKey{}, genericProjectionClaim{JobID: job.ID, Attempt: job.Attempt})
}

func genericProjectionClaimFromContext(ctx context.Context) (genericProjectionClaim, bool) {
	claim, ok := ctx.Value(genericProjectionClaimContextKey{}).(genericProjectionClaim)
	return claim, ok && claim.JobID != 0 && claim.Attempt > 0
}

func nonTerminalGenericProjectionConflictWhere() clause.Where {
	return clause.Where{Exprs: []clause.Expression{clause.Expr{
		SQL:  "pull_request_projection_jobs.`trigger` = ? AND pull_request_projection_jobs.phase NOT IN ?",
		Vars: []any{ForgejoProjectionTriggerPullRequest, forgejoProjectionTerminalPhases},
	}}}
}

// EnqueuePullRequestCreatedIntegrations records durable async Forgejo PR projection work
// and keeps non-Forgejo integrations best-effort. REST/GraphQL PR creation uses this
// instead of running Forgejo git push/PR ensure in the request context.
func (s *Service) EnqueuePullRequestCreatedIntegrations(ctx context.Context, pr db.PullRequest) error {
	if s == nil {
		return nil
	}
	bgCtx := s.projectionBackgroundContext(ctx)
	var err error
	pr, err = s.projectionPullRequestFact(bgCtx, pr.ID)
	if err != nil {
		return err
	}
	if pr.AgentSessionID != nil {
		var err error
		bgCtx, err = s.ContextForDelegatedSessionID(bgCtx, *pr.AgentSessionID)
		if err != nil {
			// Preserve the marker so every downstream integration stays on the
			// delegated fail-closed path and records terminal projection denial.
			bgCtx = ContextWithDelegatedSession(bgCtx, db.DelegatedAgentSession{ID: strings.TrimSpace(*pr.AgentSessionID)})
		}
	}
	repoFullName, err := s.projectionRepoFullNameFromPR(bgCtx, pr)
	if err != nil {
		return err
	}
	headSHA := strings.TrimSpace(pr.HeadSHA)
	if headSHA == "" && s.Git != nil {
		headSHA, _ = s.Git.HeadSHA(bgCtx, repoFullName, pr.HeadRef)
	}

	var forgejoErr error
	if s.ForgejoIntegration != nil {
		if _, err := s.EnqueueForgejoPullRequestProjection(bgCtx, pr); err != nil {
			forgejoErr = fmt.Errorf("enqueue Forgejo PR projection: %w", err)
			slog.WarnContext(bgCtx, "enqueue Forgejo PR projection failed", "repo", repoFullName, "pr_number", pr.Number, "error", err)
		}
	}

	if (s.GitLabIntegration == nil && s.GitHubIntegration == nil) || s.Git == nil {
		return forgejoErr
	}
	repoPath, err := s.Git.GetRepoPath(bgCtx, repoFullName)
	if err != nil {
		if forgejoErr != nil {
			return errors.Join(forgejoErr, fmt.Errorf("lookup AGS repo path for shadow PR integration: %w", err))
		}
		return fmt.Errorf("lookup AGS repo path for shadow PR integration: %w", err)
	}
	var gitLabErr, gitHubErr error
	var gitLabResult gitlabintegration.ShadowMergeRequestResult
	var gitLabHandled, gitHubHandled bool
	if s.GitLabIntegration != nil {
		if pr.AgentSessionID != nil {
			if _, err := s.RevalidateDelegatedSession(bgCtx, pr.RepositoryID, "pr.create", "pr:create", map[string]string{"head_ref": pr.HeadRef, "base_ref": pr.BaseRef}); err != nil {
				change := ForgejoRefChange{Ref: "refs/heads/" + strings.TrimSpace(pr.HeadRef), After: headSHA}
				persistErr := s.DBForCtx(bgCtx).Transaction(func(tx *gorm.DB) error {
					txCtx := ContextWithDB(bgCtx, tx)
					if jobErr := s.recordTerminalProjectionAdmissionJob(txCtx, pr, ProjectionProviderGitLab, repoFullName, headSHA, err); jobErr != nil {
						return jobErr
					}
					return s.RecordGitLabProjectionFailure(txCtx, repoFullName, change, err)
				})
				gitLabAdmissionErr := fmt.Errorf("delegated GitLab projection admission: %w", err)
				return errors.Join(forgejoErr, gitLabAdmissionErr, persistErr)
			}
		}
		var err error
		gitLabResult, gitLabHandled, err = s.ensureGitLabShadowMergeRequest(bgCtx, repoFullName, repoPath, headSHA, pr)
		gitLabErr = err
		if gitLabErr != nil {
			slog.WarnContext(bgCtx, "ensure GitLab shadow MR failed", "repo", repoFullName, "pr_number", pr.Number, "error", gitLabErr)
		}
	}
	if s.GitHubIntegration != nil {
		if pr.AgentSessionID != nil {
			if _, err := s.RevalidateDelegatedSession(bgCtx, pr.RepositoryID, "pr.create", "pr:create", map[string]string{"head_ref": pr.HeadRef, "base_ref": pr.BaseRef}); err != nil {
				change := ForgejoRefChange{Ref: "refs/heads/" + strings.TrimSpace(pr.HeadRef), After: headSHA}
				persistErr := s.DBForCtx(bgCtx).Transaction(func(tx *gorm.DB) error {
					txCtx := ContextWithDB(bgCtx, tx)
					if jobErr := s.recordTerminalProjectionAdmissionJob(txCtx, pr, ProjectionProviderGitHub, repoFullName, headSHA, err); jobErr != nil {
						return jobErr
					}
					return s.RecordGitHubProjectionFailure(txCtx, repoFullName, change, err)
				})
				gitHubAdmissionErr := fmt.Errorf("delegated GitHub projection admission: %w", err)
				return errors.Join(forgejoErr, gitLabErr, gitHubAdmissionErr, persistErr)
			}
		}
		var err error
		_, gitHubHandled, err = s.ensureGitHubShadowPullRequest(bgCtx, repoFullName, repoPath, headSHA, pr)
		gitHubErr = err
		if gitHubErr != nil {
			slog.WarnContext(bgCtx, "ensure GitHub shadow PR failed", "repo", repoFullName, "pr_number", pr.Number, "error", gitHubErr)
		}
	}
	if gitLabHandled || gitHubHandled {
		s.projectPullRequestCreatedToMultica(bgCtx, repoFullName, headSHA, pr, forgejointegration.PullRequestResult{}, gitLabResult, gitLabHandled)
	}
	return errors.Join(forgejoErr, gitLabErr, gitHubErr)
}

func (s *Service) projectionPullRequestFact(ctx context.Context, pullRequestID uint) (db.PullRequest, error) {
	if pullRequestID == 0 {
		return db.PullRequest{}, fmt.Errorf("projection PR has no durable ID")
	}
	var pr db.PullRequest
	if err := s.DBForCtx(ctx).Select(
		"id", "number", "repository_id", "head_repository_id", "author_id", "agent_session_id",
		"title", "body", "head_ref", "head_sha", "base_ref", "base_sha", "state",
	).First(&pr, pullRequestID).Error; err != nil {
		return db.PullRequest{}, fmt.Errorf("load projection PR fact: %w", err)
	}
	return pr, nil
}

func (s *Service) recordTerminalProjectionAdmissionJob(ctx context.Context, pr db.PullRequest, provider, repoFullName, headSHA string, admissionErr error) error {
	now := time.Now().UTC()
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AgentSessionID: pr.AgentSessionID,
		Provider: provider, Trigger: ForgejoProjectionTriggerPullRequest, RepoFullName: repoFullName,
		AGSPRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: headSHA,
		Phase: ForgejoProjectionPhaseFailedTerminal, LastErrorType: ProjectionFailureProviderAdmissionDenied,
		LastError: DelegatedSessionDenialReason(admissionErr), RemoteRef: "refs/heads/" + strings.TrimSpace(pr.HeadRef),
		RemoteSHA: headSHA, FinishedAt: &now,
	}
	return s.DBForCtx(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "pull_request_id"}, {Name: "provider"}},
		DoUpdates: clause.Assignments(map[string]any{
			"repository_id": job.RepositoryID, "agent_session_id": job.AgentSessionID,
			"trigger": job.Trigger, "repo_full_name": repoFullName, "ags_pr_number": pr.Number,
			"head_ref": pr.HeadRef, "base_ref": pr.BaseRef, "head_sha": headSHA,
			"phase": job.Phase, "last_error_type": job.LastErrorType, "last_error": job.LastError,
			"remote_ref": job.RemoteRef, "remote_sha": headSHA, "next_run_at": nil,
			"finished_at": &now, "updated_at": now,
		}),
		Where: nonTerminalGenericProjectionConflictWhere(),
	}).Create(&job).Error
}

func (s *Service) projectionRepoFullNameFromPR(ctx context.Context, pr db.PullRequest) (string, error) {
	if pr.RepositoryID == 0 {
		return "", fmt.Errorf("projection PR has no repository")
	}
	var repository db.Repository
	if err := s.DBForCtx(ctx).Select("id", "full_name").First(&repository, pr.RepositoryID).Error; err != nil {
		return "", fmt.Errorf("lookup projection PR repository: %w", err)
	}
	return strings.TrimSpace(repository.FullName), nil
}

// EnqueueForgejoPullRequestProjection upserts the durable Forgejo projection job
// for an AGS PR and schedules the in-process worker unless disabled.
func (s *Service) EnqueueForgejoPullRequestProjection(ctx context.Context, pr db.PullRequest) (db.PullRequestProjectionJob, error) {
	if s == nil || pr.ID == 0 || s.ForgejoIntegration == nil {
		return db.PullRequestProjectionJob{}, nil
	}
	bgCtx := s.projectionBackgroundContext(ctx)
	var err error
	pr, err = s.projectionPullRequestFact(bgCtx, pr.ID)
	if err != nil {
		return db.PullRequestProjectionJob{}, err
	}
	repoFullName, err := s.projectionRepoFullNameFromPR(bgCtx, pr)
	if err != nil {
		return db.PullRequestProjectionJob{}, err
	}
	headSHA := strings.TrimSpace(pr.HeadSHA)
	if headSHA == "" && s.Git != nil {
		headSHA, _ = s.Git.HeadSHA(bgCtx, repoFullName, pr.HeadRef)
	}
	now := time.Now().UTC()
	job := db.PullRequestProjectionJob{
		PullRequestID:  pr.ID,
		RepositoryID:   pr.RepositoryID,
		AgentSessionID: pr.AgentSessionID,
		Provider:       ProjectionProviderForgejo,
		Trigger:        ForgejoProjectionTriggerPullRequest,
		RepoFullName:   repoFullName,
		AGSPRNumber:    pr.Number,
		HeadRef:        pr.HeadRef,
		BaseRef:        pr.BaseRef,
		HeadSHA:        headSHA,
		Phase:          ForgejoProjectionPhaseQueued,
		RemoteRef:      "refs/heads/" + strings.TrimSpace(pr.HeadRef),
		RemoteSHA:      headSHA,
		NextRunAt:      &now,
	}
	dbCtx := s.DBForCtx(bgCtx)
	if err := dbCtx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "pull_request_id"}, {Name: "provider"}},
		DoNothing: true,
	}).Create(&job).Error; err != nil {
		return db.PullRequestProjectionJob{}, err
	}
	if err := dbCtx.Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderForgejo).First(&job).Error; err != nil {
		return db.PullRequestProjectionJob{}, err
	}
	if job.Trigger != ForgejoProjectionTriggerPullRequest {
		return job, fmt.Errorf("generic projection enqueue cannot replace action generation %d: %w", job.ActionGeneration, ErrInvalidState)
	}
	// Generic Forgejo projection is a server-owned standing-executor effect.
	// Originating delegated sessions expire and must not gate enqueue.
	// Requeue when AGS head advanced, or when a legacy session-admission
	// terminal is healed. Same-head duplicate enqueue keeps other terminal
	// projected facts intact.
	headAdvanced := strings.TrimSpace(job.HeadSHA) != headSHA
	admissionHeal := genericForgejoSessionAdmissionHeal(job)
	if (headAdvanced && job.Phase != ForgejoProjectionPhaseFailedTerminal) || admissionHeal {
		now = time.Now().UTC()
		updates := map[string]any{
			"phase":           ForgejoProjectionPhaseQueued,
			"head_sha":        headSHA,
			"remote_sha":      headSHA,
			"remote_ref":      "refs/heads/" + strings.TrimSpace(pr.HeadRef),
			"head_ref":        pr.HeadRef,
			"base_ref":        pr.BaseRef,
			"ags_pr_number":   pr.Number,
			"next_run_at":     &now,
			"finished_at":     nil,
			"last_error":      "",
			"last_error_type": "",
		}
		requeue := dbCtx.Model(&db.PullRequestProjectionJob{}).
			Where("id = ? AND `trigger` = ?", job.ID, ForgejoProjectionTriggerPullRequest)
		if !admissionHeal {
			requeue = requeue.Where("phase <> ?", ForgejoProjectionPhaseFailedTerminal)
		} else {
			requeue = requeue.Where("phase = ? AND last_error_type = ?",
				ForgejoProjectionPhaseFailedTerminal, ProjectionFailureProviderAdmissionDenied)
		}
		if err := requeue.Updates(updates).Error; err != nil {
			return db.PullRequestProjectionJob{}, err
		}
		if err := dbCtx.Where("id = ?", job.ID).First(&job).Error; err != nil {
			return db.PullRequestProjectionJob{}, err
		}
	}
	s.scheduleForgejoProjectionJob(bgCtx, job.ID)
	return job, nil
}

// RetryForgejoPullRequestProjection resets a retryable Forgejo PR projection job
// to queued and schedules the worker. It is the safe repair path for partial
// Forgejo projection states; it never creates Forgejo PRs directly.
func (s *Service) RetryForgejoPullRequestProjection(ctx context.Context, repoFullName string, prNumber int) (ProjectionJobStatus, error) {
	if s == nil {
		return ProjectionJobStatus{}, fmt.Errorf("service is nil")
	}
	if s.ForgejoIntegration == nil {
		return ProjectionJobStatus{}, fmt.Errorf("forgejo projection retry unavailable: %w", ErrInvalidState)
	}
	repo, err := s.projectionRepository(ctx, repoFullName)
	if err != nil {
		return ProjectionJobStatus{}, err
	}
	if err := s.requireRepoPermission(ctx, repo.ID, RepoPermissionWrite); err != nil {
		return ProjectionJobStatus{}, err
	}
	var pr db.PullRequest
	if err := s.DBForCtx(ctx).
		Preload("Repository").
		Preload("HeadRepository").
		Preload("Author").
		Where("repository_id = ? AND number = ?", repo.ID, prNumber).
		First(&pr).Error; err != nil {
		return ProjectionJobStatus{}, err
	}
	var job db.PullRequestProjectionJob
	err = s.DBForCtx(ctx).Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderForgejo).First(&job).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			created, enqueueErr := s.EnqueueForgejoPullRequestProjection(ctx, pr)
			if enqueueErr != nil {
				return ProjectionJobStatus{}, enqueueErr
			}
			return projectionJobStatus(created), nil
		}
		return ProjectionJobStatus{}, err
	}
	switch job.Phase {
	case ForgejoProjectionPhaseProjected:
		return projectionJobStatus(job), nil
	case ForgejoProjectionPhaseFailedTerminal:
		return ProjectionJobStatus{}, fmt.Errorf("forgejo projection job is terminal failed; inspect drift before retry: %w", ErrInvalidState)
	}
	now := time.Now().UTC()
	if job.Trigger == ForgejoProjectionTriggerActionRebase {
		if strings.TrimSpace(job.DesiredAGSHeadSHA) == "" || job.ActionIntentID == nil || strings.TrimSpace(*job.ActionIntentID) == "" {
			return ProjectionJobStatus{}, fmt.Errorf("Forgejo rebase job has no saved desired head or exact intent binding: %w", ErrInvalidState)
		}
		ctx = contextWithForgejoActionJob(ctx, job)
		if err := s.updateForgejoActionRebaseJob(ctx, job.ID, ForgejoProjectionPhaseProjectionResume, map[string]any{
			"last_error_type": "", "last_error": "", "next_run_at": &now, "finished_at": nil,
		}); err != nil {
			return ProjectionJobStatus{}, err
		}
		if err := s.DBForCtx(ctx).First(&job, job.ID).Error; err != nil {
			return ProjectionJobStatus{}, err
		}
		s.scheduleForgejoProjectionJob(ctx, job.ID)
		return projectionJobStatus(job), nil
	}
	updates := map[string]any{
		"phase":           ForgejoProjectionPhaseQueued,
		"head_ref":        firstNonEmpty(job.HeadRef, pr.HeadRef),
		"base_ref":        firstNonEmpty(job.BaseRef, pr.BaseRef),
		"head_sha":        firstNonEmpty(job.HeadSHA, pr.HeadSHA),
		"remote_ref":      "refs/heads/" + strings.TrimSpace(firstNonEmpty(job.HeadRef, pr.HeadRef)),
		"remote_sha":      firstNonEmpty(job.HeadSHA, pr.HeadSHA),
		"last_error_type": "",
		"last_error":      "",
		"next_run_at":     &now,
		"finished_at":     nil,
		"updated_at":      now,
	}
	if err := s.retryGenericForgejoProjectionJob(ctx, job, updates); err != nil {
		return ProjectionJobStatus{}, err
	}
	if err := s.DBForCtx(ctx).Where("id = ?", job.ID).First(&job).Error; err != nil {
		return ProjectionJobStatus{}, err
	}
	s.scheduleForgejoProjectionJob(ctx, job.ID)
	return projectionJobStatus(job), nil
}

func (s *Service) retryGenericForgejoProjectionJob(ctx context.Context, job db.PullRequestProjectionJob, updates map[string]any) error {
	if updates == nil {
		updates = map[string]any{}
	}
	// Manual retry is a generation transition, not a phase rewrite. Advancing
	// attempt atomically invalidates every phase, failure, provider, and mapping
	// writer still carrying the previous worker claim.
	updates["attempt"] = gorm.Expr("attempt + 1")
	updates["phase"] = ForgejoProjectionPhaseQueued
	updates["started_at"] = nil
	result := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where("id = ? AND `trigger` = ? AND phase = ? AND updated_at = ? AND phase NOT IN ?",
			job.ID, job.Trigger, job.Phase, job.UpdatedAt, forgejoProjectionTerminalPhases).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("forgejo projection job changed or became terminal before retry: %w", ErrInvalidState)
	}
	return nil
}

// ResumePendingForgejoProjectionJobs schedules queued/retryable or interrupted
// Forgejo projection jobs from the DB. It is safe to call at startup or after a
// disabled worker is re-enabled.
func (s *Service) ResumePendingForgejoProjectionJobs(ctx context.Context) error {
	if s == nil || s.DisableForgejoProjectionWorker {
		return nil
	}
	if err := s.requeueGenericForgejoSessionAdmissionJobs(ctx); err != nil {
		return err
	}
	phases := []string{
		ForgejoProjectionPhaseQueued,
		ForgejoProjectionPhasePreflight,
		ForgejoProjectionPhaseRebasing,
		ForgejoProjectionPhaseProjectionResume,
		ForgejoProjectionPhasePushingRef,
		ForgejoProjectionPhaseVerifyingRef,
		ForgejoProjectionPhaseVerifyingPR,
		ForgejoProjectionPhaseEnsuringPR,
		ForgejoProjectionPhaseRecordingProjection,
		ForgejoProjectionPhaseFailedRetryable,
	}
	var jobs []db.PullRequestProjectionJob
	now := time.Now().UTC()
	if err := s.DBForCtx(ctx).
		Where("provider = ? AND phase IN ? AND (next_run_at IS NULL OR next_run_at <= ? OR phase IN ?)", ProjectionProviderForgejo, phases, now, []string{
			ForgejoProjectionPhasePreflight,
			ForgejoProjectionPhaseRebasing,
			ForgejoProjectionPhaseProjectionResume,
			ForgejoProjectionPhasePushingRef,
			ForgejoProjectionPhaseVerifyingRef,
			ForgejoProjectionPhaseVerifyingPR,
			ForgejoProjectionPhaseEnsuringPR,
			ForgejoProjectionPhaseRecordingProjection,
			ForgejoProjectionPhaseFailedRetryable,
		}).
		Order("updated_at ASC").
		Limit(100).
		Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		if job.Trigger == ForgejoProjectionTriggerActionRebase && (job.ActionIntentID == nil || strings.TrimSpace(*job.ActionIntentID) == "" || job.ActionGeneration == 0) {
			finished := time.Now().UTC()
			if err := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
				Where("id = ? AND `trigger` = ? AND (action_intent_id IS NULL OR action_intent_id = ?) AND phase NOT IN ?", job.ID, ForgejoProjectionTriggerActionRebase, "", []string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal}).
				Updates(map[string]any{
					"phase": ForgejoProjectionPhaseFailedTerminal, "last_error_type": ProjectionFailureProviderAdmissionDenied,
					"last_error": "historical rebase job has no exact action intent binding", "next_run_at": nil, "finished_at": &finished,
				}).Error; err != nil {
				return err
			}
			continue
		}
		s.scheduleForgejoProjectionJob(ctx, job.ID)
	}
	return nil
}

func (s *Service) projectionBackgroundContext(ctx context.Context) context.Context {
	if s == nil {
		return context.Background()
	}
	bgCtx := s.ServerCtx()
	if bgCtx == nil {
		bgCtx = context.Background()
	}
	if ctx != nil {
		bgCtx = applog.CloneContext(bgCtx, ctx)
		if tenantDB, ok := DBFromContext(ctx); ok {
			bgCtx = ContextWithDB(bgCtx, tenantDB)
		}
		if viewer, ok := UserFromContext(ctx); ok {
			bgCtx = ContextWithUser(bgCtx, viewer)
		}
		if sessionID, ok := DelegatedSessionIDFromContext(ctx); ok {
			bgCtx = ContextWithDelegatedSession(bgCtx, db.DelegatedAgentSession{ID: sessionID})
		}
	}
	return bgCtx
}

func (s *Service) pullRequestRepoFullName(ctx context.Context, pr db.PullRequest) (string, error) {
	repoFullName := strings.TrimSpace(pr.Repository.FullName)
	if repoFullName != "" {
		return repoFullName, nil
	}
	if pr.RepositoryID == 0 {
		return "", fmt.Errorf("lookup PR repo: missing repository id")
	}
	repo, err := s.GetRepoByID(ctx, fmt.Sprint(pr.RepositoryID))
	if err != nil {
		return "", fmt.Errorf("lookup PR repo: %w", err)
	}
	return repo.FullName, nil
}

func (s *Service) scheduleForgejoProjectionJob(ctx context.Context, jobID uint) {
	if s == nil || jobID == 0 || s.DisableForgejoProjectionWorker {
		return
	}
	key := fmt.Sprint(jobID)
	if _, loaded := s.forgejoProjectionJobLocks.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	bgCtx := s.projectionBackgroundContext(ctx)
	s.Wg.Add(1)
	go func() {
		defer s.Wg.Done()
		defer s.forgejoProjectionJobLocks.Delete(key)
		s.runForgejoProjectionJob(bgCtx, jobID)
	}()
}

func (s *Service) runForgejoProjectionJob(ctx context.Context, jobID uint) {
	maxAttempts := s.forgejoProjectionMaxAttempts()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return
		}
		job, err := s.loadForgejoProjectionJob(ctx, jobID)
		if err != nil {
			slog.WarnContext(ctx, "load Forgejo projection job failed", "job_id", jobID, "error", err)
			return
		}
		if job.Phase == ForgejoProjectionPhaseProjected || job.Phase == ForgejoProjectionPhaseFailedTerminal {
			return
		}
		if job.NextRunAt != nil && job.NextRunAt.After(time.Now().UTC()) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(*job.NextRunAt)):
			}
			// The durable row is the authority after a wait. A different worker,
			// manual retry, enqueue, or terminal denial may have changed it.
			attempt--
			continue
		}
		jobCtx := ctx
		if job.Trigger == ForgejoProjectionTriggerActionRebase {
			jobCtx = contextWithForgejoActionJob(jobCtx, job)
		} else {
			claimed, claimErr := s.claimGenericForgejoProjectionJob(jobCtx, job)
			if claimErr != nil {
				if errors.Is(claimErr, errGenericProjectionClaimChanged) {
					// A CAS miss means another writer changed this snapshot. Reload it
					// instead of writing a late phase or failure from stale state.
					attempt--
					continue
				}
				slog.WarnContext(jobCtx, "claim generic Forgejo projection job failed", "job_id", job.ID, "error", claimErr)
				return
			}
			job = claimed
			jobCtx = contextWithGenericProjectionClaim(jobCtx, job)
		}
		workerCtx, cancel := s.forgejoProjectionAttemptContext(jobCtx, job)
		failure := s.processForgejoProjectionJobAttempt(workerCtx, job)
		cancel()
		if failure == nil {
			return
		}
		if !failure.retryable {
			s.markForgejoProjectionJobFailed(jobCtx, job.ID, ForgejoProjectionPhaseFailedTerminal, nil, failure)
			return
		}
		if failure.preserveAttempt && failure.retryAt != nil {
			next := failure.retryAt.UTC()
			s.markForgejoProjectionJobFailed(jobCtx, job.ID, ForgejoProjectionPhaseFailedRetryable, &next, failure)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
			}
			attempt--
			continue
		}
		if attempt+1 >= maxAttempts {
			s.markForgejoProjectionJobFailed(jobCtx, job.ID, ForgejoProjectionPhaseFailedRetryable, nil, failure)
			return
		}
		next := time.Now().UTC().Add(s.forgejoProjectionRetryDelay() * time.Duration(attempt+1))
		s.markForgejoProjectionJobFailed(jobCtx, job.ID, ForgejoProjectionPhaseFailedRetryable, &next, failure)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
	}
}

func (s *Service) forgejoProjectionAttemptContext(ctx context.Context, job db.PullRequestProjectionJob) (context.Context, context.CancelFunc) {
	if job.Trigger != ForgejoProjectionTriggerActionRebase && job.NextRunAt != nil {
		// next_run_at is the persisted generic-generation lease expiry. The old
		// worker is canceled no later than the instant another instance may take
		// over; it must not receive a fresh timeout starting after claim latency.
		return context.WithDeadline(ctx, job.NextRunAt.UTC())
	}
	return context.WithTimeout(ctx, s.forgejoProjectionWorkerTimeout())
}

func (s *Service) claimGenericForgejoProjectionJob(ctx context.Context, job db.PullRequestProjectionJob) (db.PullRequestProjectionJob, error) {
	if job.Trigger == ForgejoProjectionTriggerActionRebase {
		return db.PullRequestProjectionJob{}, fmt.Errorf("action rebase jobs use the exact intent claim protocol")
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(s.forgejoProjectionWorkerTimeout())
	result := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where("id = ? AND `trigger` = ? AND phase = ? AND updated_at = ? AND phase NOT IN ?",
			job.ID, job.Trigger, job.Phase, job.UpdatedAt, forgejoProjectionTerminalPhases).
		Updates(map[string]any{
			"phase": ForgejoProjectionPhasePreflight, "attempt": job.Attempt + 1,
			"started_at": &now, "finished_at": nil, "next_run_at": &leaseUntil,
			"updated_at": now,
		})
	if result.Error != nil {
		return db.PullRequestProjectionJob{}, result.Error
	}
	if result.RowsAffected != 1 {
		return db.PullRequestProjectionJob{}, errGenericProjectionClaimChanged
	}
	return s.loadForgejoProjectionJob(ctx, job.ID)
}

func (s *Service) loadForgejoProjectionJob(ctx context.Context, jobID uint) (db.PullRequestProjectionJob, error) {
	var job db.PullRequestProjectionJob
	err := s.DBForCtx(ctx).
		Preload("PullRequest").
		Preload("PullRequest.Repository").
		Preload("PullRequest.HeadRepository").
		Preload("PullRequest.Author").
		Preload("Repository").
		First(&job, jobID).Error
	return job, err
}

func (s *Service) processForgejoProjectionJobAttempt(ctx context.Context, job db.PullRequestProjectionJob) *projectionJobFailure {
	if job.Trigger == ForgejoProjectionTriggerActionRebase {
		if job.AgentSessionID != nil {
			delegatedCtx, err := s.ContextForDelegatedSessionID(ctx, *job.AgentSessionID)
			if err != nil {
				return newProjectionJobFailure(job.RemoteRef, job.HeadSHA, err, false)
			}
			ctx = delegatedCtx
		}
		return s.processForgejoActionRebaseJobAttempt(ctx, job)
	}
	if s == nil || s.ForgejoIntegration == nil {
		return newProjectionJobFailure(job.RemoteRef, job.HeadSHA, fmt.Errorf("forgejo integration is not configured"), false)
	}
	pr := job.PullRequest
	if pr.ID == 0 {
		if err := s.DBForCtx(ctx).Preload("Repository").Preload("HeadRepository").Preload("Author").First(&pr, job.PullRequestID).Error; err != nil {
			return newProjectionJobFailure(job.RemoteRef, job.HeadSHA, fmt.Errorf("load pull request: %w", err), false)
		}
	}
	repoFullName := strings.TrimSpace(job.RepoFullName)
	if repoFullName == "" {
		repoFullName = strings.TrimSpace(job.Repository.FullName)
	}
	if repoFullName == "" {
		repoFullName = strings.TrimSpace(pr.Repository.FullName)
	}
	if repoFullName == "" {
		repo, err := s.GetRepoByID(ctx, fmt.Sprint(job.RepositoryID))
		if err != nil {
			return newProjectionJobFailure(job.RemoteRef, job.HeadSHA, fmt.Errorf("lookup PR repo: %w", err), false)
		}
		repoFullName = repo.FullName
		pr.Repository = repo
	}
	headSHA := strings.TrimSpace(job.HeadSHA)
	if headSHA == "" {
		headSHA = strings.TrimSpace(pr.HeadSHA)
	}
	if headSHA == "" && s.Git != nil {
		headSHA, _ = s.Git.HeadSHA(ctx, repoFullName, firstNonEmpty(job.HeadRef, pr.HeadRef))
	}
	if headSHA == "" {
		return newProjectionJobFailure(job.RemoteRef, job.HeadSHA, fmt.Errorf("missing AGS PR head SHA"), true)
	}
	if s.Git == nil {
		return newProjectionJobFailure(job.RemoteRef, headSHA, fmt.Errorf("git store is not configured"), true)
	}
	repoPath, err := s.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		return newProjectionJobFailure(job.RemoteRef, headSHA, fmt.Errorf("lookup AGS repo path for PR projection: %w", err), true)
	}
	attemptStarted := time.Now().UTC()
	payloadSummary, payloadSummaryOK := forgejoProjectionPayloadSummary(ctx, repoPath, firstNonEmpty(job.BaseRef, pr.BaseRef), headSHA)
	if !payloadSummaryOK {
		slog.DebugContext(ctx, "Forgejo projection payload summary unavailable", "repo", repoFullName, "pr_number", pr.Number)
	}
	newFailure := func(ref, expectedSHA string, err error, defaultRetryable bool) *projectionJobFailure {
		failure := newProjectionJobFailure(ref, expectedSHA, err, defaultRetryable)
		if failure != nil {
			failure.duration = time.Since(attemptStarted)
			failure.payload = payloadSummary
		}
		return failure
	}
	pr.HeadRef = firstNonEmpty(job.HeadRef, pr.HeadRef)
	pr.BaseRef = firstNonEmpty(job.BaseRef, pr.BaseRef)
	pr.HeadSHA = headSHA
	if pr.RepositoryID == 0 {
		pr.RepositoryID = job.RepositoryID
	}
	if pr.Repository.FullName == "" {
		pr.Repository = job.Repository
	}
	if pr.Repository.FullName == "" {
		pr.Repository.FullName = repoFullName
	}
	return s.withForgejoProjectionLock(forgejoProjectionLockKey(repoFullName, pr.ID, pr.HeadRef), func() *projectionJobFailure {
		if err := s.updateForgejoProjectionJobPhase(ctx, job.ID, ForgejoProjectionPhasePushingRef, map[string]any{
			"finished_at": nil,
			"remote_ref":  "refs/heads/" + strings.TrimSpace(pr.HeadRef),
			"remote_sha":  headSHA,
		}); err != nil {
			return newFailure(job.RemoteRef, headSHA, fmt.Errorf("persist Forgejo projection job phase: %w", err), true)
		}
		result, handled, err := s.ensureForgejoPullRequestExternalWithAuthority(ctx, repoFullName, repoPath, headSHA, pr, func(checkCtx context.Context) error {
			return s.revalidateGenericProjectionProviderWrite(checkCtx, job, pr)
		})
		if err != nil {
			return newFailure("refs/heads/"+strings.TrimSpace(pr.HeadRef), headSHA, err, true)
		}
		if !handled {
			return newFailure("refs/heads/"+strings.TrimSpace(pr.HeadRef), headSHA, fmt.Errorf("forgejo projection skipped for repo %s branch %s", repoFullName, pr.HeadRef), false)
		}
		// The provider result is outside the AGS transaction. Reload the exact
		// durable generation before allowing any AGS projection materialization.
		if err := s.revalidateGenericProjectionGeneration(ctx, job.ID, ForgejoProjectionPhasePushingRef); err != nil {
			return newFailure("refs/heads/"+strings.TrimSpace(pr.HeadRef), headSHA, err, true)
		}
		if err := s.updateForgejoProjectionJobPhase(ctx, job.ID, ForgejoProjectionPhaseRecordingProjection, map[string]any{
			"external_repo":   result.ExternalRepo,
			"external_number": result.Number,
			"external_url":    result.URL,
		}); err != nil {
			return newFailure("refs/heads/"+strings.TrimSpace(pr.HeadRef), headSHA, fmt.Errorf("persist Forgejo projection recording phase: %w", err), true)
		}
		if err := s.completeGenericForgejoPullRequestProjection(ctx, job.ID, repoFullName, headSHA, pr, result); err != nil {
			return newFailure("refs/heads/"+strings.TrimSpace(pr.HeadRef), headSHA, err, true)
		}
		slog.InfoContext(ctx, "Forgejo projection job projected",
			"repo", repoFullName,
			"pr_number", pr.Number,
			"phase", ForgejoProjectionPhaseProjected,
			"duration_ms", time.Since(attemptStarted).Milliseconds(),
			"payload_changed_files", payloadSummary.ChangedFiles,
			"payload_head_bytes", payloadSummary.HeadBlobBytes,
		)
		s.projectPullRequestCreatedToMultica(ctx, repoFullName, headSHA, pr, result, gitlabintegration.ShadowMergeRequestResult{}, false)
		return nil
	})
}

func (s *Service) withForgejoProjectionLock(key string, fn func() *projectionJobFailure) *projectionJobFailure {
	if s == nil || strings.TrimSpace(key) == "" {
		return fn()
	}
	value, _ := s.forgejoProjectionLocks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

func forgejoProjectionLockKey(repoFullName string, prID uint, headRef string) string {
	return strings.Join([]string{
		strings.TrimSpace(repoFullName),
		fmt.Sprint(prID),
		strings.TrimSpace(headRef),
	}, "\x00")
}

func (s *Service) revalidateGenericProjectionGeneration(ctx context.Context, jobID uint, allowedPhases ...string) error {
	claim, claimed := genericProjectionClaimFromContext(ctx)
	if !claimed || claim.JobID != jobID || len(allowedPhases) == 0 {
		return fmt.Errorf("missing generic projection claim: %w", errGenericProjectionClaimChanged)
	}
	var count int64
	err := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where("id = ? AND `trigger` = ? AND attempt = ? AND phase IN ? AND next_run_at > ?",
			jobID, ForgejoProjectionTriggerPullRequest, claim.Attempt, allowedPhases, time.Now().UTC()).
		Count(&count).Error
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("generic projection generation changed, expired, or left its provider phase: %w", errGenericProjectionClaimChanged)
	}
	return nil
}

func (s *Service) revalidateGenericProjectionProviderWrite(ctx context.Context, job db.PullRequestProjectionJob, pr db.PullRequest) error {
	return s.revalidateGenericProjectionGeneration(ctx, job.ID, ForgejoProjectionPhasePushingRef)
}

func genericForgejoSessionAdmissionHeal(job db.PullRequestProjectionJob) bool {
	return job.Trigger == ForgejoProjectionTriggerPullRequest &&
		job.Phase == ForgejoProjectionPhaseFailedTerminal &&
		strings.TrimSpace(job.LastErrorType) == ProjectionFailureProviderAdmissionDenied
}

func (s *Service) requeueGenericForgejoSessionAdmissionJobs(ctx context.Context) error {
	now := time.Now().UTC()
	// Quote trigger: TiDB/MySQL reserved word. Unquoted WHERE fails bootstrap.
	return s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where("provider = ? AND `trigger` = ? AND phase = ? AND last_error_type = ?",
			ProjectionProviderForgejo, ForgejoProjectionTriggerPullRequest,
			ForgejoProjectionPhaseFailedTerminal, ProjectionFailureProviderAdmissionDenied).
		Updates(map[string]any{
			"phase":           ForgejoProjectionPhaseQueued,
			"last_error_type": "",
			"last_error":      "",
			"finished_at":     nil,
			"next_run_at":     &now,
			"updated_at":      now,
		}).Error
}

func (s *Service) completeGenericForgejoPullRequestProjection(ctx context.Context, jobID uint, repoFullName, headSHA string, pr db.PullRequest, res forgejointegration.PullRequestResult) error {
	finished := time.Now().UTC()
	err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		txCtx := ContextWithDB(ctx, tx)
		if err := s.revalidateGenericProjectionGeneration(txCtx, jobID, ForgejoProjectionPhaseRecordingProjection); err != nil {
			return err
		}
		if err := s.UpsertPullRequestProjection(txCtx, db.PullRequestProjection{
			PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
			ExternalRepo: res.ExternalRepo, ExternalNumber: res.Number, ExternalURL: res.URL,
			SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: ProjectionStateOpen, LastSyncedSHA: headSHA,
		}); err != nil {
			return fmt.Errorf("record Forgejo projection: %w", err)
		}
		if s.testGenericProjectionBeforeFinalCAS != nil {
			s.testGenericProjectionBeforeFinalCAS(tx)
		}
		query, err := s.projectionJobMutationScope(txCtx, jobID)
		if err != nil {
			return err
		}
		result := query.Where("phase = ? AND next_run_at > ?", ForgejoProjectionPhaseRecordingProjection, time.Now().UTC()).Updates(map[string]any{
			"phase": ForgejoProjectionPhaseProjected, "external_repo": res.ExternalRepo,
			"external_number": res.Number, "external_url": res.URL, "remote_sha": headSHA,
			"next_run_at": nil, "finished_at": finished, "updated_at": finished,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("projection job generation changed before projection commit: %w", errGenericProjectionClaimChanged)
		}
		var current db.PullRequestProjectionJob
		if err := tx.First(&current, jobID).Error; err != nil {
			return err
		}
		return s.persistProjectionJobAttempt(txCtx, current, projectionJobAttemptProjected, "", "", &finished)
	})
	if err != nil {
		return fmt.Errorf("commit Forgejo projection mapping and job generation: %w", err)
	}
	slog.InfoContext(ctx, "forgejo pull request projection recorded", "repo", repoFullName, "pr_number", pr.Number, "external_number", res.Number)
	return nil
}

func (s *Service) projectionJobMutationScope(ctx context.Context, jobID uint) (*gorm.DB, error) {
	query := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{})
	if binding, bound := forgejoActionBindingFromContext(ctx); bound {
		if binding.JobID != jobID || binding.ActionGeneration == 0 {
			return nil, fmt.Errorf("projection job action binding mismatch")
		}
		return query.Where("id = ? AND `trigger` = ? AND action_generation = ? AND action_intent_id = ? AND phase NOT IN ?",
			jobID, ForgejoProjectionTriggerActionRebase, binding.ActionGeneration, binding.IntentID,
			[]string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal}), nil
	}
	// Generic pull-request projection jobs use the durable attempt as their
	// generation token. A worker that resumes after another instance took over
	// cannot write phases or failures from its stale snapshot.
	claim, claimed := genericProjectionClaimFromContext(ctx)
	if !claimed || claim.JobID != jobID {
		return nil, fmt.Errorf("missing generic projection claim")
	}
	return query.Where("id = ? AND `trigger` <> ? AND attempt = ? AND phase NOT IN ?",
		jobID, ForgejoProjectionTriggerActionRebase, claim.Attempt, forgejoProjectionTerminalPhases), nil
}

func (s *Service) updateForgejoProjectionJobPhase(ctx context.Context, jobID uint, phase string, updates map[string]any) error {
	if updates == nil {
		updates = map[string]any{}
	}
	updates["phase"] = phase
	updates["updated_at"] = time.Now().UTC()
	query, err := s.projectionJobMutationScope(ctx, jobID)
	if err != nil {
		return err
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("projection job generation changed or became terminal")
	}
	return nil
}

func (s *Service) markForgejoProjectionJobFailed(ctx context.Context, jobID uint, phase string, nextRunAt *time.Time, failure *projectionJobFailure) {
	if failure == nil {
		return
	}
	updates := map[string]any{
		"phase":           phase,
		"last_error_type": failure.failureType,
		"last_error":      failure.summary,
		"next_run_at":     nextRunAt,
		"updated_at":      time.Now().UTC(),
	}
	if strings.TrimSpace(failure.remoteSHA) != "" {
		updates["remote_sha"] = strings.TrimSpace(failure.remoteSHA)
	}
	_, actionBound := forgejoActionBindingFromContext(ctx)
	if phase == ForgejoProjectionPhaseFailedTerminal && failure.failureType == ProjectionFailureProviderAdmissionDenied && actionBound {
		if err := s.terminalDenyForgejoAction(ctx, "", jobID, failure.summary, "delegated authority changed before provider effect", failure.remoteSHA); err != nil {
			slog.WarnContext(ctx, "persist Forgejo projection provider denial failed", "job_id", jobID, "error", err, "cause", failure.err)
		}
		return
	}
	if phase == ForgejoProjectionPhaseFailedTerminal {
		now := time.Now().UTC()
		updates["finished_at"] = now
	}
	query, scopeErr := s.projectionJobMutationScope(ctx, jobID)
	if scopeErr != nil {
		slog.WarnContext(ctx, "persist Forgejo projection job failure rejected", "job_id", jobID, "phase", phase, "error", scopeErr, "cause", failure.err)
		return
	}
	result := query.Updates(updates)
	if result.Error != nil || result.RowsAffected != 1 {
		slog.WarnContext(ctx, "persist Forgejo projection job failure failed", "job_id", jobID, "phase", phase, "error", result.Error, "rows", result.RowsAffected, "cause", failure.err)
		return
	}
	var current db.PullRequestProjectionJob
	if loadErr := s.DBForCtx(ctx).First(&current, jobID).Error; loadErr != nil {
		slog.WarnContext(ctx, "reload Forgejo projection job after failure failed", "job_id", jobID, "error", loadErr, "cause", failure.err)
	} else {
		attemptStatus := projectionJobAttemptRetryableFailed
		if phase == ForgejoProjectionPhaseFailedTerminal {
			attemptStatus = projectionJobAttemptTerminalFailed
		}
		var finishedAt *time.Time
		if phase == ForgejoProjectionPhaseFailedTerminal {
			now := time.Now().UTC()
			finishedAt = &now
		}
		if persistErr := s.persistProjectionJobAttempt(ctx, current, attemptStatus, failure.failureType, failure.summary, finishedAt); persistErr != nil {
			slog.WarnContext(ctx, "persist Forgejo projection job attempt failure history failed", "job_id", jobID, "attempt", current.Attempt, "error", persistErr)
		}
	}
	slog.WarnContext(ctx, "Forgejo projection job failed",
		"job_id", jobID,
		"phase", phase,
		"error_type", failure.failureType,
		"error", failure.summary,
		"duration_ms", failure.duration.Milliseconds(),
		"payload_changed_files", failure.payload.ChangedFiles,
		"payload_head_bytes", failure.payload.HeadBlobBytes,
	)
}

func (s *Service) persistProjectionJobAttempt(ctx context.Context, job db.PullRequestProjectionJob, status, errorType, errSummary string, finishedAt *time.Time) error {
	if s == nil || job.ID == 0 || job.Attempt <= 0 {
		return nil
	}
	now := time.Now().UTC()
	row := db.PullRequestProjectionJobAttempt{
		JobID: job.ID, Attempt: job.Attempt, Phase: job.Phase, Status: status,
		ErrorType: strings.TrimSpace(errorType), ErrorSummary: strings.TrimSpace(errSummary),
		RemoteSHA: strings.TrimSpace(job.RemoteSHA), StartedAt: job.StartedAt, FinishedAt: finishedAt,
		CreatedAt: now, UpdatedAt: now,
	}
	return s.DBForCtx(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "job_id"}, {Name: "attempt"}},
		DoUpdates: clause.AssignmentColumns([]string{"phase", "status", "error_type", "error_summary", "remote_sha", "started_at", "finished_at", "updated_at"}),
	}).Create(&row).Error
}

func (s *Service) forgejoProjectionWorkerTimeout() time.Duration {
	if s != nil && s.ForgejoProjectionWorkerTimeout > 0 {
		return s.ForgejoProjectionWorkerTimeout
	}
	return defaultForgejoProjectionWorkerTimeout
}

func (s *Service) forgejoProjectionMaxAttempts() int {
	if s != nil && s.ForgejoProjectionWorkerMaxAttempts > 0 {
		return s.ForgejoProjectionWorkerMaxAttempts
	}
	return defaultForgejoProjectionWorkerMaxAttempts
}

func (s *Service) forgejoProjectionRetryDelay() time.Duration {
	if s != nil && s.ForgejoProjectionWorkerRetryDelay > 0 {
		return s.ForgejoProjectionWorkerRetryDelay
	}
	return defaultForgejoProjectionWorkerRetryDelay
}

type projectionPayloadInfo struct {
	ChangedFiles  int
	HeadBlobBytes int64
}

type projectionJobFailure struct {
	err             error
	failureType     string
	summary         string
	remoteSHA       string
	retryable       bool
	retryAt         *time.Time
	preserveAttempt bool
	duration        time.Duration
	payload         projectionPayloadInfo
}

func newProjectionJobFailure(ref, expectedSHA string, err error, defaultRetryable bool) *projectionJobFailure {
	if err == nil {
		return nil
	}
	failureType := forgejointegration.ProjectionFailureUnknown
	summary := strings.TrimSpace(err.Error())
	retryable := defaultRetryable
	remoteSHA := ""
	switch {
	case errors.Is(err, context.Canceled):
		failureType = forgejoProjectionFailureContextCanceled
		summary = context.Canceled.Error()
		retryable = true
	case errors.Is(err, context.DeadlineExceeded):
		failureType = forgejoProjectionFailureContextDeadline
		summary = context.DeadlineExceeded.Error()
		retryable = true
	case errors.Is(err, ErrDelegatedSessionUseTimeDenied):
		failureType = ProjectionFailureProviderAdmissionDenied
		summary = DelegatedSessionDenialReason(err)
		retryable = false
	default:
		classified := forgejointegration.ClassifyProjectionError(ref, expectedSHA, err)
		failureType = classified.Type
		if strings.TrimSpace(classified.ErrorSummary) != "" {
			summary = classified.ErrorSummary
		}
		remoteSHA = strings.TrimSpace(classified.ActualSHA)
		var networkErr net.Error
		if errors.As(err, &networkErr) {
			failureType = forgejoProjectionFailureTransport
			retryable = true
			break
		}
		switch classified.Type {
		case forgejointegration.ProjectionFailureNonFastForward,
			forgejointegration.ProjectionFailureSHADrift,
			forgejointegration.ProjectionFailureAuthFailed,
			forgejointegration.ProjectionFailureRepoMissing:
			retryable = false
		}
	}
	return &projectionJobFailure{err: err, failureType: failureType, summary: summary, remoteSHA: remoteSHA, retryable: retryable}
}

func forgejoProjectionPayloadSummary(ctx context.Context, repoPath, baseRef, headSHA string) (projectionPayloadInfo, bool) {
	repoPath = strings.TrimSpace(repoPath)
	baseRef = strings.TrimSpace(baseRef)
	headSHA = strings.TrimSpace(headSHA)
	if repoPath == "" || baseRef == "" || headSHA == "" {
		return projectionPayloadInfo{}, false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "diff", "--name-only", "-z", "--diff-filter=ACMRT", baseRef+".."+headSHA)
	out, err := cmd.Output()
	if err != nil {
		return projectionPayloadInfo{}, false
	}
	summary := projectionPayloadInfo{}
	for _, rawPath := range strings.Split(string(out), "\x00") {
		rawPath = strings.TrimSpace(rawPath)
		if rawPath == "" {
			continue
		}
		summary.ChangedFiles++
		if size, ok := gitBlobSizeAtRef(ctx, repoPath, headSHA, rawPath); ok {
			summary.HeadBlobBytes += size
		}
	}
	return summary, true
}

func gitBlobSizeAtRef(ctx context.Context, repoPath, ref, filePath string) (int64, bool) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "cat-file", "-s", ref+":"+filePath)
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || size < 0 {
		return 0, false
	}
	return size, true
}
