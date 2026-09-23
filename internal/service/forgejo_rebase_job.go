package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *Service) beginForgejoActionRebaseJob(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, intent db.PullRequestActionIntent, preflight forgejoActionRebasePreflight) (db.PullRequestProjectionJob, error) {
	if s == nil || pr.ID == 0 || strings.TrimSpace(intent.ID) == "" {
		return db.PullRequestProjectionJob{}, fmt.Errorf("cannot begin Forgejo rebase job without service, PR and exact action intent")
	}
	if intent.Action != "pr.rebase" || intent.PullRequestID != pr.ID || intent.RepositoryID != pr.RepositoryID ||
		intent.ForgejoRepo != event.RepoFullName || intent.ForgejoPRNumber != event.PRNumber {
		return db.PullRequestProjectionJob{}, fmt.Errorf("Forgejo rebase job action intent facts do not match")
	}
	var existing db.PullRequestProjectionJob
	err := s.DBForCtx(ctx).Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderForgejo).First(&existing).Error
	generation := uint(1)
	if err == nil {
		generation = existing.ActionGeneration + 1
		if generation == 0 {
			generation = 1
		}
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return db.PullRequestProjectionJob{}, err
	}
	now := time.Now().UTC()
	agentSessionID := intent.AgentSessionID
	if id, ok := DelegatedSessionIDFromContext(ctx); ok {
		if id == "" || agentSessionID == nil || id != *agentSessionID {
			return db.PullRequestProjectionJob{}, delegatedUseTimeDenied(DelegatedDenialSessionMissing)
		}
	} else if agentSessionID != nil {
		return db.PullRequestProjectionJob{}, delegatedUseTimeDenied(DelegatedDenialSessionMissing)
	}
	actionIntentID := intent.ID
	correlationID := strings.TrimSpace(event.CorrelationID)
	if correlationID == "" {
		correlationID = fmt.Sprintf("forgejo-rebase:%s#%d:g%d", event.RepoFullName, event.PRNumber, generation)
	}
	job := db.PullRequestProjectionJob{
		PullRequestID:             pr.ID,
		RepositoryID:              pr.RepositoryID,
		AgentSessionID:            agentSessionID,
		ActionIntentID:            &actionIntentID,
		Provider:                  ProjectionProviderForgejo,
		Trigger:                   ForgejoProjectionTriggerActionRebase,
		ActionGeneration:          generation,
		CorrelationID:             correlationID,
		RepoFullName:              preflight.RepoFullName,
		AGSPRNumber:               pr.Number,
		HeadRef:                   pr.HeadRef,
		BaseRef:                   pr.BaseRef,
		HeadSHA:                   preflight.HeadSHA,
		PreflightAGSHeadSHA:       preflight.HeadSHA,
		PreflightBaseSHA:          preflight.BaseSHA,
		ExpectedForgejoOldHeadSHA: preflight.HeadSHA,
		ObservedForgejoHeadSHA:    preflight.ForgejoHeadSHA,
		Phase:                     ForgejoProjectionPhasePreflight,
		Attempt:                   0,
		ExternalRepo:              preflight.Projection.ExternalRepo,
		ExternalNumber:            preflight.Projection.ExternalNumber,
		ExternalURL:               preflight.Projection.ExternalURL,
		RemoteRef:                 preflight.Ref,
		RemoteSHA:                 preflight.ForgejoHeadSHA,
		StartedAt:                 &now,
	}
	updates := map[string]any{
		"repository_id": job.RepositoryID, "agent_session_id": job.AgentSessionID, "action_intent_id": actionIntentID, "trigger": job.Trigger, "action_generation": generation,
		"correlation_id": correlationID, "repo_full_name": job.RepoFullName, "ags_pr_number": job.AGSPRNumber,
		"head_ref": job.HeadRef, "base_ref": job.BaseRef, "head_sha": job.HeadSHA,
		"preflight_ags_head_sha": job.PreflightAGSHeadSHA, "preflight_base_sha": job.PreflightBaseSHA,
		"expected_forgejo_old_head_sha": job.ExpectedForgejoOldHeadSHA, "desired_ags_head_sha": "",
		"observed_forgejo_head_sha": job.ObservedForgejoHeadSHA, "phase": job.Phase, "attempt": 0,
		"last_error_type": "", "last_error": "", "external_repo": job.ExternalRepo,
		"external_number": job.ExternalNumber, "external_url": job.ExternalURL, "remote_ref": job.RemoteRef,
		"remote_sha": job.RemoteSHA, "next_run_at": nil, "started_at": &now, "finished_at": nil,
		"success_comment_dispatch_id": "", "success_comment_claim_token": "", "success_comment_claimed_at": nil,
		"success_commented_at": nil, "updated_at": now,
	}
	if err := s.DBForCtx(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "pull_request_id"}, {Name: "provider"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&job).Error; err != nil {
		return db.PullRequestProjectionJob{}, err
	}
	if err := s.DBForCtx(ctx).Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderForgejo).First(&job).Error; err != nil {
		return db.PullRequestProjectionJob{}, err
	}
	return job, nil
}

func (s *Service) processForgejoActionRebaseJobAttempt(ctx context.Context, job db.PullRequestProjectionJob) *projectionJobFailure {
	if job.ActionIntentID == nil || strings.TrimSpace(*job.ActionIntentID) == "" || job.ActionGeneration == 0 {
		return newProjectionJobFailure(job.RemoteRef, job.DesiredAGSHeadSHA, fmt.Errorf("Forgejo rebase job has no exact action intent binding"), false)
	}
	ctx = contextWithForgejoActionJob(ctx, job)
	if s == nil || s.ForgejoIntegration == nil || s.Git == nil {
		return newProjectionJobFailure(job.RemoteRef, job.DesiredAGSHeadSHA, fmt.Errorf("Forgejo rebase recovery dependencies are not configured"), false)
	}
	claimed, err := s.claimForgejoActionRebaseJob(ctx, &job)
	if err != nil {
		return newProjectionJobFailure(job.RemoteRef, job.DesiredAGSHeadSHA, fmt.Errorf("claim Forgejo rebase generation: %w", err), true)
	}
	if !claimed {
		return nil
	}
	if job.LastErrorType == "success_comment_claim_busy" && strings.TrimSpace(job.DesiredAGSHeadSHA) != "" {
		pr := job.PullRequest
		if pr.ID == 0 {
			if err := s.DBForCtx(ctx).Preload("Repository").First(&pr, job.PullRequestID).Error; err != nil {
				return newProjectionJobFailure(job.RemoteRef, job.DesiredAGSHeadSHA, err, true)
			}
		}
		event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: job.ExternalRepo, PRNumber: job.ExternalNumber}
		if err := s.publishForgejoActionRebaseSuccess(ctx, event, pr, job.ID, job.DesiredAGSHeadSHA); err != nil {
			var claimBusy *forgejoSuccessCommentClaimBusyError
			if errors.As(err, &claimBusy) {
				retryAt := claimBusy.retryAt
				return &projectionJobFailure{err: err, failureType: "success_comment_claim_busy", summary: err.Error(), remoteSHA: job.DesiredAGSHeadSHA, retryable: true, retryAt: &retryAt, preserveAttempt: true}
			}
			return newProjectionJobFailure(job.RemoteRef, job.DesiredAGSHeadSHA, err, true)
		}
		if err := s.completeForgejoActionRebaseJob(ctx, job.ID, job.DesiredAGSHeadSHA); err != nil {
			return newProjectionJobFailure(job.RemoteRef, job.DesiredAGSHeadSHA, err, true)
		}
		return nil
	}
	if strings.TrimSpace(job.DesiredAGSHeadSHA) == "" {
		var err error
		job, err = s.recoverForgejoActionRebaseDesiredHead(ctx, job)
		if err != nil {
			return newProjectionJobFailure(job.RemoteRef, job.PreflightAGSHeadSHA, err, false)
		}
	}
	event := forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: job.ExternalRepo, PRNumber: job.ExternalNumber, PRURL: job.ExternalURL,
		HeadBranch: job.HeadRef, HeadSHA: job.ExpectedForgejoOldHeadSHA, BaseBranch: job.BaseRef,
		LabelName: forgejointegration.AGSActionRebaseLabel, SenderLogin: "ags-forgejo-bot", CorrelationID: job.CorrelationID,
	}
	result := ForgejoWebhookResult{Handled: true, RepoFullName: job.RepoFullName, PRNumber: job.ExternalNumber, BaseBranch: job.BaseRef, AGSPRNumber: job.AGSPRNumber, WorkflowAction: "rebase", WorkflowLabel: forgejointegration.AGSActionRebaseLabel, WorkflowStatus: "queued"}
	_, err = s.resumeForgejoActionRebaseJob(ctx, event, job.PullRequest, job, result)
	if err != nil {
		var claimBusy *forgejoSuccessCommentClaimBusyError
		if errors.As(err, &claimBusy) {
			retryAt := claimBusy.retryAt
			return &projectionJobFailure{
				err: err, failureType: "success_comment_claim_busy", summary: err.Error(), remoteSHA: job.DesiredAGSHeadSHA,
				retryable: true, retryAt: &retryAt, preserveAttempt: true,
			}
		}
		return newProjectionJobFailure(job.RemoteRef, job.DesiredAGSHeadSHA, err, true)
	}
	var latest db.PullRequestProjectionJob
	if err := s.DBForCtx(ctx).First(&latest, job.ID).Error; err != nil {
		return newProjectionJobFailure(job.RemoteRef, job.DesiredAGSHeadSHA, err, true)
	}
	switch latest.Phase {
	case ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseNeedsRebase, ForgejoProjectionPhaseFailedTerminal:
		return nil
	case ForgejoProjectionPhaseFailedRetryable:
		return &projectionJobFailure{err: errors.New(latest.LastError), failureType: latest.LastErrorType, summary: latest.LastError, remoteSHA: latest.ObservedForgejoHeadSHA, retryable: true}
	default:
		return newProjectionJobFailure(job.RemoteRef, job.DesiredAGSHeadSHA, fmt.Errorf("rebase recovery stopped in phase %s", latest.Phase), true)
	}
}

func (s *Service) claimForgejoActionRebaseJob(ctx context.Context, job *db.PullRequestProjectionJob) (bool, error) {
	if job == nil || job.ID == 0 {
		return false, nil
	}
	claimedAt := time.Now().UTC()
	if job.ActionIntentID == nil || strings.TrimSpace(*job.ActionIntentID) == "" || job.ActionGeneration == 0 {
		return false, fmt.Errorf("Forgejo rebase job has no exact action intent binding")
	}
	res := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where(clause.Eq{Column: "trigger", Value: ForgejoProjectionTriggerActionRebase}).
		Where("id = ? AND action_generation = ? AND action_intent_id = ? AND updated_at = ? AND phase NOT IN ?",
			job.ID, job.ActionGeneration, *job.ActionIntentID, job.UpdatedAt,
			[]string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal}).
		Update("updated_at", claimedAt)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 1 {
		job.UpdatedAt = claimedAt
		return true, nil
	}
	return false, nil
}

func (s *Service) recoverForgejoActionRebaseDesiredHead(ctx context.Context, job db.PullRequestProjectionJob) (db.PullRequestProjectionJob, error) {
	pr := job.PullRequest
	if pr.ID == 0 {
		if err := s.DBForCtx(ctx).Preload("Repository").Preload("HeadRepository").First(&pr, job.PullRequestID).Error; err != nil {
			return job, err
		}
	}
	branchHead, err := s.Git.HeadSHA(ctx, job.RepoFullName, job.HeadRef)
	if err != nil {
		return job, err
	}
	preflightHead := strings.TrimSpace(job.PreflightAGSHeadSHA)
	if exactGitSHA(branchHead, preflightHead) && exactGitSHA(pr.HeadSHA, preflightHead) {
		if err := s.revalidateCurrentDelegatedProviderWrite(ctx, job.ExternalRepo, job.ExternalNumber); err != nil {
			return job, s.terminalizeBoundActionProviderDenial(ctx, err, "delegated authority changed before interrupted rebase recovery", job.DesiredAGSHeadSHA)
		}
		if err := s.updateForgejoActionRebaseJob(ctx, job.ID, ForgejoProjectionPhaseRebasing, nil); err != nil {
			return job, err
		}
		if err := s.revalidateCurrentDelegatedProviderWrite(ctx, job.ExternalRepo, job.ExternalNumber); err != nil {
			return job, s.terminalizeBoundActionProviderDenial(ctx, err, "delegated authority changed at interrupted rebase effect seam", job.DesiredAGSHeadSHA)
		}
		updated, desired, err := s.rebaseAGSPrBranchFromForgejoLabel(ctx, pr)
		if err != nil {
			s.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, preflightHead, err, false)
			return job, err
		}
		pr = updated
		branchHead = desired
	} else {
		baseHead, err := s.Git.HeadSHA(ctx, job.RepoFullName, job.BaseRef)
		if err != nil {
			return job, err
		}
		if !exactGitSHA(baseHead, job.PreflightBaseSHA) {
			_ = s.updateForgejoActionRebaseJob(ctx, job.ID, ForgejoProjectionPhaseNeedsRebase, map[string]any{"last_error_type": "base_moved", "last_error": "base moved while rebase action was interrupted"})
			return job, fmt.Errorf("base moved while rebase action was interrupted")
		}
		repoPath, err := s.Git.GetRepoPath(ctx, job.RepoFullName)
		if err != nil {
			return job, err
		}
		ancestor, err := gitCommitIsAncestor(ctx, repoPath, baseHead, branchHead)
		if err != nil || !ancestor {
			return job, fmt.Errorf("interrupted rebase produced an unverified AGS head")
		}
		if !exactGitSHA(pr.HeadSHA, branchHead) {
			if !exactGitSHA(pr.HeadSHA, preflightHead) {
				return job, fmt.Errorf("AGS PR changed outside interrupted rebase generation")
			}
			if err := s.UpdatePRFields(ctx, pr.ID, map[string]any{"head_sha": branchHead}); err != nil {
				return job, err
			}
		}
	}
	if err := s.updateForgejoActionRebaseJob(ctx, job.ID, ForgejoProjectionPhaseProjectionResume, map[string]any{
		"head_sha": branchHead, "desired_ags_head_sha": branchHead, "observed_forgejo_head_sha": job.ExpectedForgejoOldHeadSHA,
		"remote_sha": job.ExpectedForgejoOldHeadSHA, "attempt": job.Attempt + 1, "next_run_at": nil,
	}); err != nil {
		return job, err
	}
	if err := s.DBForCtx(ctx).Preload("PullRequest").Preload("PullRequest.Repository").First(&job, job.ID).Error; err != nil {
		return job, err
	}
	return job, nil
}

func (s *Service) findResumableForgejoActionRebaseJob(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, intent db.PullRequestActionIntent) (db.PullRequestProjectionJob, bool, error) {
	var job db.PullRequestProjectionJob
	err := s.DBForCtx(ctx).Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderForgejo).
		Where(clause.Eq{Column: "trigger", Value: ForgejoProjectionTriggerActionRebase}).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return job, false, nil
	}
	if err != nil {
		return job, false, err
	}
	if job.ExternalRepo != event.RepoFullName || job.ExternalNumber != event.PRNumber ||
		job.ActionIntentID == nil || strings.TrimSpace(*job.ActionIntentID) != intent.ID ||
		job.PullRequestID != intent.PullRequestID || job.RepositoryID != intent.RepositoryID ||
		!sameOptionalString(job.AgentSessionID, intent.AgentSessionID) {
		return job, false, nil
	}
	if (job.Phase == ForgejoProjectionPhaseProjected || job.Phase == ForgejoProjectionPhaseFailedTerminal) &&
		strings.TrimSpace(job.DesiredAGSHeadSHA) != "" && !exactGitSHA(job.DesiredAGSHeadSHA, pr.HeadSHA) {
		// A completed or terminal generation owns its saved desired head. A later
		// signed action for a different PR head must pass fresh live preflight and,
		// only if all authorities already converge, begin a new generation.
		return job, false, nil
	}
	switch job.Phase {
	case ForgejoProjectionPhasePreflight, ForgejoProjectionPhaseRebasing, ForgejoProjectionPhaseProjectionResume, ForgejoProjectionPhasePushingRef, ForgejoProjectionPhaseVerifyingRef,
		ForgejoProjectionPhaseVerifyingPR, ForgejoProjectionPhaseRecordingProjection, ForgejoProjectionPhaseFailedRetryable,
		ForgejoProjectionPhaseFailedTerminal, ForgejoProjectionPhaseProjected:
		return job, true, nil
	default:
		return job, false, nil
	}
}

func (s *Service) resumeForgejoActionRebaseJob(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, job db.PullRequestProjectionJob, result ForgejoWebhookResult) (ForgejoWebhookResult, error) {
	ctx = contextWithForgejoActionJob(ctx, job)
	if err := s.revalidateCurrentDelegatedProviderWrite(ctx, event.RepoFullName, event.PRNumber); err != nil {
		return result, s.terminalizeBoundActionProviderDenial(ctx, err, "delegated authority changed before rebase job resume", job.DesiredAGSHeadSHA)
	}
	if strings.TrimSpace(job.DesiredAGSHeadSHA) == "" {
		var err error
		job, err = s.recoverForgejoActionRebaseDesiredHead(ctx, job)
		if err != nil {
			return result, err
		}
		pr = job.PullRequest
	}
	desired := strings.TrimSpace(job.DesiredAGSHeadSHA)
	result.SyncedSHA = desired
	projection, ok := s.forgejoProjectionForPullRequest(ctx, pr.ID)
	if !ok || projection.Provider != ProjectionProviderForgejo || projection.ExternalRepo != event.RepoFullName || projection.ExternalNumber != event.PRNumber {
		return result, fmt.Errorf("resume Forgejo rebase action: durable projection mapping changed")
	}
	repoPath, err := s.Git.GetRepoPath(ctx, job.RepoFullName)
	if err != nil {
		return result, err
	}
	agsHead, err := s.Git.HeadSHA(ctx, job.RepoFullName, job.HeadRef)
	if err != nil {
		return result, err
	}
	freshPR, err := s.GetPR(ctx, job.RepoFullName, pr.Number)
	if err != nil {
		return result, err
	}
	if !exactGitSHA(agsHead, desired) || !exactGitSHA(freshPR.HeadSHA, desired) {
		failure := forgejoActionProjectionError(freshPR, event, desired, firstNonEmpty(agsHead, freshPR.HeadSHA), forgejointegration.ProjectionFailureSHADrift, "saved rebase generation no longer matches AGS branch and PR")
		s.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, desired, failure, false)
		result.WorkflowStatus = "projection_failed"
		return result, nil
	}
	preflight := forgejoActionRebasePreflight{
		Projection: projection, RepoFullName: job.RepoFullName, RepoPath: repoPath, Ref: job.RemoteRef,
		HeadSHA: job.ExpectedForgejoOldHeadSHA, BaseSHA: job.PreflightBaseSHA,
		ForgejoHeadSHA: job.ExpectedForgejoOldHeadSHA, ForgejoPRHeadSHA: job.ExpectedForgejoOldHeadSHA,
		ForgejoBaseSHA: job.PreflightBaseSHA,
	}
	if moved, detail, checkErr := s.forgejoActionBaseChanged(ctx, preflight, desired); checkErr != nil {
		s.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, desired, checkErr, true)
		result.WorkflowStatus = "projection_failed"
		return result, nil
	} else if moved {
		result.WorkflowStatus = "needs_rebase"
		_ = s.updateForgejoActionRebaseJob(ctx, job.ID, ForgejoProjectionPhaseNeedsRebase, map[string]any{"last_error_type": "base_moved", "last_error": detail})
		if notifyErr := s.markForgejoActionNeedsRebase(ctx, event, freshPR, desired, detail); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, "", job.ID, notifyErr, "delegated authority changed while publishing needs-rebase status", desired)
		}
		return result, nil
	}
	observed, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, job.RepoFullName, repoPath, job.HeadRef)
	if err != nil || !checked {
		if err == nil {
			err = fmt.Errorf("live Forgejo ref inspection unavailable")
		}
		s.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, desired, err, true)
		result.WorkflowStatus = "projection_failed"
		return result, nil
	}
	if !exactGitSHA(observed, job.ExpectedForgejoOldHeadSHA) && !exactGitSHA(observed, desired) {
		err := forgejoActionProjectionError(freshPR, event, job.ExpectedForgejoOldHeadSHA, observed, forgejointegration.ProjectionFailureSHADrift, "Forgejo head changed outside the saved rebase generation")
		provider := PullRequestProviderResult{Provider: ProjectionProviderForgejo, Required: true, Attempted: true, DesiredSHA: desired, ObservedSHA: observed, Err: err}
		failure, recordErr := s.recordForgejoActionProjectionFailure(ctx, event, freshPR, desired, provider, "projection_resume")
		if recordErr != nil {
			return result, recordErr
		}
		s.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, desired, err, false)
		if notifyErr := s.markForgejoActionProjectionFailed(ctx, event, freshPR, failure, true); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, "", job.ID, notifyErr, "delegated authority changed while publishing projection failure", desired)
		}
		result.WorkflowStatus = "projection_failed"
		return result, nil
	}
	if exactGitSHA(observed, job.ExpectedForgejoOldHeadSHA) {
		_ = s.updateForgejoActionRebaseJob(ctx, job.ID, ForgejoProjectionPhasePushingRef, map[string]any{"attempt": job.Attempt + 1, "observed_forgejo_head_sha": observed, "remote_sha": observed, "next_run_at": nil})
		dispatch, err := s.DispatchPullRequestIntegrationsWithPolicy(ctx, freshPR, PullRequestIntegrationPolicy{
			RequireForgejo: true,
			ForgejoRewrite: &ForgejoPullRequestRewritePolicy{ExternalRepo: projection.ExternalRepo, ExternalNumber: projection.ExternalNumber, ExpectedOldHeadSHA: job.ExpectedForgejoOldHeadSHA},
		})
		if err != nil {
			provider := dispatch.Forgejo
			if provider.Err == nil {
				provider.Err = err
			}
			failure, recordErr := s.recordForgejoActionProjectionFailure(ctx, event, freshPR, desired, provider, "projection_resume")
			if recordErr != nil {
				return result, recordErr
			}
			s.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, desired, provider.Err, true)
			if notifyErr := s.markForgejoActionProjectionFailed(ctx, event, freshPR, failure, true); notifyErr != nil {
				return result, s.handleForgejoStatusNotificationError(ctx, "", job.ID, notifyErr, "delegated authority changed while publishing projection failure", desired)
			}
			result.WorkflowStatus = "projection_failed"
			return result, nil
		}
	} else {
		_ = s.updateForgejoActionRebaseJob(ctx, job.ID, ForgejoProjectionPhaseVerifyingPR, map[string]any{"observed_forgejo_head_sha": observed, "remote_sha": observed})
		forgejoPR, found, err := s.ForgejoIntegration.PullRequestSnapshot(ctx, job.RepoFullName, projection.ExternalRepo, projection.ExternalNumber)
		if err != nil || !found || forgejoPR.HeadRef != job.HeadRef || forgejoPR.BaseRef != job.BaseRef || !exactGitSHA(forgejoPR.HeadSHA, desired) {
			if err == nil {
				err = fmt.Errorf("Forgejo PR has not converged to saved desired head")
			}
			s.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, desired, err, true)
			result.WorkflowStatus = "projection_failed"
			return result, nil
		}
		_ = s.updateForgejoActionRebaseJob(ctx, job.ID, ForgejoProjectionPhaseRecordingProjection, nil)
		if err := s.recordForgejoPullRequestProjection(ctx, job.RepoFullName, desired, freshPR, forgejointegration.PullRequestResult{
			Number: projection.ExternalNumber, URL: firstNonEmpty(forgejoPR.URL, projection.ExternalURL), ExternalRepo: projection.ExternalRepo, HeadSHA: desired,
		}); err != nil {
			s.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, desired, err, true)
			result.WorkflowStatus = "projection_failed"
			return result, nil
		}
	}
	if err := s.verifyForgejoActionConvergence(ctx, event, freshPR, preflight, desired); err != nil {
		s.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, desired, err, true)
		result.WorkflowStatus = "projection_failed"
		return result, nil
	}
	if err := s.ResolveProjectionRefState(ctx, job.RepoFullName, ProjectionProviderForgejo, job.RemoteRef, desired, desired, time.Now().UTC()); err != nil && !IsProjectionAlertingError(err) {
		return result, err
	}
	if err := s.clearForgejoWorkflowStatusLabels(ctx, event.RepoFullName, event.PRNumber); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			_ = s.terminalDenyForgejoAction(ctx, "", job.ID, DelegatedSessionDenialReason(err), "delegated authority changed while clearing provider status labels", desired)
		}
		return result, err
	}
	if err := s.publishForgejoActionRebaseSuccess(ctx, event, freshPR, job.ID, desired); err != nil {
		return result, err
	}
	if err := s.completeForgejoActionRebaseJob(ctx, job.ID, desired); err != nil {
		return result, err
	}
	result.WorkflowStatus = "rebased"
	return result, nil
}

func (s *Service) rebaseExistingAGSHeadForgejoAction(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, intent db.PullRequestActionIntent, preflight forgejoActionRebasePreflight, result ForgejoWebhookResult) (ForgejoWebhookResult, error) {
	ctx = contextWithForgejoActionIntent(ctx, intent.ID)
	if err := s.revalidateCurrentDelegatedProviderWrite(ctx, event.RepoFullName, event.PRNumber); err != nil {
		return result, err
	}
	actionJob, err := s.beginForgejoActionRebaseJob(ctx, event, pr, intent, preflight)
	if err != nil {
		return result, fmt.Errorf("begin durable legacy Forgejo rebase recovery: %w", err)
	}
	ctx = contextWithForgejoActionJob(ctx, actionJob)
	if err := s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseRebasing, map[string]any{
		"head_sha": pr.HeadSHA, "preflight_ags_head_sha": pr.HeadSHA,
	}); err != nil {
		return result, fmt.Errorf("persist legacy Forgejo rebase recovery: %w", err)
	}
	if err := s.clearForgejoWorkflowStatusLabels(ctx, event.RepoFullName, event.PRNumber); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			_ = s.terminalDenyForgejoAction(ctx, "", actionJob.ID, DelegatedSessionDenialReason(err), "delegated authority changed while clearing provider status labels", "")
		}
		return result, err
	}
	if err := s.addForgejoPullRequestLabels(ctx, event.RepoFullName, event.PRNumber, []string{forgejointegration.AGSStatusRebasingLabel}); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			_ = s.terminalDenyForgejoAction(ctx, "", actionJob.ID, DelegatedSessionDenialReason(err), "delegated authority changed while publishing provider rebase status", "")
			return result, err
		}
		slog.WarnContext(ctx, "add Forgejo PR rebasing status label failed during legacy recovery", "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "error", err)
	}
	if err := s.revalidateCurrentDelegatedProviderWrite(ctx, event.RepoFullName, event.PRNumber); err != nil {
		return result, s.terminalizeBoundActionProviderDenial(ctx, err, "delegated authority changed before legacy rebase effect", actionJob.DesiredAGSHeadSHA)
	}
	updatedPR, desired, err := s.rebaseAGSPrBranchFromForgejoLabel(ctx, pr)
	if err != nil {
		result.WorkflowStatus = "failed"
		if isLikelyRebaseConflict(err) {
			result.WorkflowStatus = "conflict"
		}
		s.markForgejoActionRebaseJobFailed(ctx, actionJob.ID, preflight.Ref, pr.HeadSHA, err, false)
		if notifyErr := s.markForgejoActionFailed(ctx, event, err); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, "", actionJob.ID, notifyErr, "delegated authority changed while publishing rebase failure", desired)
		}
		return result, nil
	}
	if err := s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseProjectionResume, map[string]any{
		"head_sha": desired, "desired_ags_head_sha": desired,
		"observed_forgejo_head_sha": preflight.ForgejoHeadSHA, "remote_sha": preflight.ForgejoHeadSHA,
		"attempt": actionJob.Attempt + 1, "next_run_at": nil,
	}); err != nil {
		return result, fmt.Errorf("persist rebased legacy Forgejo projection recovery: %w", err)
	}
	if err := s.DBForCtx(ctx).Preload("PullRequest").Preload("PullRequest.Repository").First(&actionJob, actionJob.ID).Error; err != nil {
		return result, fmt.Errorf("reload rebased legacy Forgejo projection recovery: %w", err)
	}
	return s.resumeForgejoActionRebaseJob(ctx, event, updatedPR, actionJob, result)
}

func (s *Service) resumeExistingAGSHeadForgejoAction(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, intent db.PullRequestActionIntent, preflight forgejoActionRebasePreflight, result ForgejoWebhookResult) (ForgejoWebhookResult, error) {
	ctx = contextWithForgejoActionIntent(ctx, intent.ID)
	desired := strings.TrimSpace(pr.HeadSHA)
	actionJob, err := s.beginForgejoActionRebaseJob(ctx, event, pr, intent, preflight)
	if err != nil {
		return result, fmt.Errorf("begin durable legacy Forgejo projection recovery: %w", err)
	}
	ctx = contextWithForgejoActionJob(ctx, actionJob)
	if err := s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseProjectionResume, map[string]any{
		"head_sha": desired, "desired_ags_head_sha": desired,
		"observed_forgejo_head_sha": preflight.ForgejoHeadSHA, "remote_sha": preflight.ForgejoHeadSHA,
		"attempt": actionJob.Attempt + 1, "next_run_at": nil,
	}); err != nil {
		return result, fmt.Errorf("persist legacy Forgejo projection recovery: %w", err)
	}
	if err := s.DBForCtx(ctx).Preload("PullRequest").Preload("PullRequest.Repository").First(&actionJob, actionJob.ID).Error; err != nil {
		return result, fmt.Errorf("reload legacy Forgejo projection recovery: %w", err)
	}
	return s.resumeForgejoActionRebaseJob(ctx, event, pr, actionJob, result)
}

func (s *Service) reconcileAlreadyConvergedForgejoAction(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, intent db.PullRequestActionIntent, preflight forgejoActionRebasePreflight, result ForgejoWebhookResult) (ForgejoWebhookResult, error) {
	ctx = contextWithForgejoActionIntent(ctx, intent.ID)
	if err := s.revalidateCurrentDelegatedProviderWrite(ctx, event.RepoFullName, event.PRNumber); err != nil {
		return result, err
	}
	desired := strings.TrimSpace(pr.HeadSHA)
	result.SyncedSHA = desired
	actionJob, err := s.beginForgejoActionRebaseJob(ctx, event, pr, intent, preflight)
	if err != nil {
		return result, fmt.Errorf("begin durable Forgejo convergence reconciliation: %w", err)
	}
	ctx = contextWithForgejoActionJob(ctx, actionJob)
	if err := s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseRecordingProjection, map[string]any{
		"head_sha": desired, "desired_ags_head_sha": desired,
		"observed_forgejo_head_sha": desired, "remote_sha": desired,
		"attempt": actionJob.Attempt + 1, "next_run_at": nil,
	}); err != nil {
		return result, fmt.Errorf("persist Forgejo convergence reconciliation: %w", err)
	}
	if err := s.recordForgejoPullRequestProjection(ctx, preflight.RepoFullName, desired, pr, forgejointegration.PullRequestResult{
		Number: preflight.Projection.ExternalNumber, URL: firstNonEmpty(preflight.ForgejoPRURL, preflight.Projection.ExternalURL),
		ExternalRepo: preflight.Projection.ExternalRepo, HeadSHA: desired,
	}); err != nil {
		s.markForgejoActionRebaseJobFailed(ctx, actionJob.ID, preflight.Ref, desired, err, true)
		return result, fmt.Errorf("record already-converged Forgejo projection: %w", err)
	}
	if err := s.verifyForgejoActionConvergence(ctx, event, pr, preflight, desired); err != nil {
		provider := PullRequestProviderResult{Provider: ProjectionProviderForgejo, Required: true, Attempted: true, DesiredSHA: desired, ObservedSHA: preflight.ForgejoHeadSHA, Err: err}
		failure, recordErr := s.recordForgejoActionProjectionFailure(ctx, event, pr, desired, provider, "convergence_reconcile")
		if recordErr != nil {
			return result, fmt.Errorf("persist Forgejo convergence reconciliation failure: %w", recordErr)
		}
		s.markForgejoActionRebaseJobFailed(ctx, actionJob.ID, preflight.Ref, desired, err, true)
		if notifyErr := s.markForgejoActionProjectionFailed(ctx, event, pr, failure, true); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, "", actionJob.ID, notifyErr, "delegated authority changed while publishing projection failure", desired)
		}
		result.WorkflowStatus = "projection_failed"
		return result, nil
	}
	if moved, detail, checkErr := s.forgejoActionBaseChanged(ctx, preflight, desired); checkErr != nil {
		result.WorkflowStatus = "projection_failed"
		provider := PullRequestProviderResult{Provider: ProjectionProviderForgejo, Required: true, Attempted: true, DesiredSHA: desired, ObservedSHA: preflight.ForgejoHeadSHA, Err: checkErr}
		failure, recordErr := s.recordForgejoActionProjectionFailure(ctx, event, pr, desired, provider, "convergence_base_verify")
		if recordErr != nil {
			return result, fmt.Errorf("persist Forgejo convergence base verification failure: %w", recordErr)
		}
		s.markForgejoActionRebaseJobFailed(ctx, actionJob.ID, preflight.Ref, desired, checkErr, true)
		if notifyErr := s.markForgejoActionProjectionFailed(ctx, event, pr, failure, true); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, "", actionJob.ID, notifyErr, "delegated authority changed while publishing projection failure", desired)
		}
		return result, nil
	} else if moved {
		result.WorkflowStatus = "needs_rebase"
		_ = s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseNeedsRebase, map[string]any{"last_error_type": "base_moved", "last_error": detail})
		if notifyErr := s.markForgejoActionNeedsRebase(ctx, event, pr, desired, detail); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, "", actionJob.ID, notifyErr, "delegated authority changed while publishing needs-rebase status", desired)
		}
		return result, nil
	}
	if err := s.ResolveProjectionRefState(ctx, preflight.RepoFullName, ProjectionProviderForgejo, preflight.Ref, desired, desired, time.Now().UTC()); err != nil {
		if IsProjectionAlertingError(err) {
			slog.WarnContext(ctx, "Forgejo convergence reconciliation resolved notification degraded", "repo", preflight.RepoFullName, "forgejo_repo", event.RepoFullName, "ags_pr", pr.Number, "forgejo_pr", event.PRNumber, "error", err)
		} else {
			return result, fmt.Errorf("resolve reconciled Forgejo projection drift: %w", err)
		}
	}
	if err := s.clearForgejoWorkflowStatusLabels(ctx, event.RepoFullName, event.PRNumber); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			_ = s.terminalDenyForgejoAction(ctx, "", actionJob.ID, DelegatedSessionDenialReason(err), "delegated authority changed while clearing provider status labels", desired)
		}
		return result, err
	}
	if err := s.publishForgejoActionRebaseSuccess(ctx, event, pr, actionJob.ID, desired); err != nil {
		return result, err
	}
	if err := s.completeForgejoActionRebaseJob(ctx, actionJob.ID, desired); err != nil {
		return result, fmt.Errorf("complete Forgejo convergence reconciliation: %w", err)
	}
	result.WorkflowStatus = "rebased"
	slog.InfoContext(ctx, "reconciled already-converged Forgejo action", "repo", preflight.RepoFullName, "ags_pr", pr.Number, "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "sha", desired)
	return result, nil
}

const defaultForgejoSuccessCommentClaimTTL = 5 * time.Minute

func (s *Service) forgejoSuccessCommentClaimTTL() time.Duration {
	if s != nil && s.ForgejoSuccessCommentClaimTTL > 0 {
		return s.ForgejoSuccessCommentClaimTTL
	}
	return defaultForgejoSuccessCommentClaimTTL
}

type forgejoSuccessCommentClaimBusyError struct {
	retryAt time.Time
}

func (e *forgejoSuccessCommentClaimBusyError) Error() string {
	return fmt.Sprintf("Forgejo success-comment dispatch is claimed until %s", e.retryAt.UTC().Format(time.RFC3339Nano))
}

func (s *Service) publishForgejoActionRebaseSuccess(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, jobID uint, sha string) error {
	sha = strings.ToLower(strings.TrimSpace(sha))
	binding, bound := forgejoActionBindingFromContext(ctx)
	if jobID == 0 || !isFullHexRevision(sha) || !bound || binding.JobID != jobID || binding.ActionGeneration == 0 {
		return fmt.Errorf("invalid Forgejo success-comment coordinate")
	}
	dispatchID := fmt.Sprintf("ags-rebase-success:%d:%s", jobID, sha)
	marker := "<!-- " + dispatchID + " -->"
	comment := fmt.Sprintf("AGS rebase completed. AGS PR #%d and Forgejo PR #%d were independently verified at `%s`.\n\n%s", pr.Number, event.PRNumber, sha, marker)

	// Persist the deterministic provider coordinate before any write. This is
	// not a success fact: success_commented_at remains nil until provider
	// readback confirms the exact marker.
	if err := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where("id = ? AND action_generation = ? AND action_intent_id = ? AND phase NOT IN ? AND success_comment_dispatch_id = ?",
			jobID, binding.ActionGeneration, binding.IntentID, []string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal}, "").
		Update("success_comment_dispatch_id", dispatchID).Error; err != nil {
		return fmt.Errorf("persist Forgejo success-comment dispatch ID: %w", err)
	}
	var job db.PullRequestProjectionJob
	if err := s.DBForCtx(ctx).Where("id = ? AND action_generation = ? AND action_intent_id = ?", jobID, binding.ActionGeneration, binding.IntentID).First(&job).Error; err != nil {
		return fmt.Errorf("load Forgejo success-comment job: %w", err)
	}
	if job.SuccessCommentedAt != nil {
		if job.SuccessCommentDispatchID != dispatchID {
			return fmt.Errorf("Forgejo success-comment dispatch identity changed")
		}
		return nil
	}
	if job.SuccessCommentDispatchID != dispatchID {
		return fmt.Errorf("Forgejo success-comment dispatch identity changed")
	}

	confirmed, err := s.ForgejoIntegration.HasPullRequestComment(ctx, event.RepoFullName, event.PRNumber, marker)
	if err != nil {
		return fmt.Errorf("read back Forgejo success comment: %w", err)
	}
	if confirmed {
		return s.confirmForgejoActionRebaseSuccessComment(ctx, jobID, dispatchID, "")
	}

	now := time.Now().UTC()
	claimToken := uuid.NewString()
	claimTTL := s.forgejoSuccessCommentClaimTTL()
	staleBefore := now.Add(-claimTTL)
	claim := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where("id = ? AND action_generation = ? AND action_intent_id = ? AND phase NOT IN ? AND success_comment_dispatch_id = ? AND success_commented_at IS NULL AND (success_comment_claim_token = ? OR success_comment_claimed_at IS NULL OR success_comment_claimed_at < ?)",
			jobID, binding.ActionGeneration, binding.IntentID, []string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal}, dispatchID, "", staleBefore).
		Updates(map[string]any{"success_comment_claim_token": claimToken, "success_comment_claimed_at": &now})
	if claim.Error != nil {
		return fmt.Errorf("claim Forgejo success-comment dispatch: %w", claim.Error)
	}
	if claim.RowsAffected != 1 {
		// Another live attempt owns the write. Persist an exact lease-expiry retry
		// coordinate; startup recovery schedules future failed_retryable jobs and
		// the in-process worker does not consume its ordinary retry budget.
		if err := s.DBForCtx(ctx).Where("id = ? AND action_generation = ? AND action_intent_id = ?", jobID, binding.ActionGeneration, binding.IntentID).First(&job).Error; err != nil {
			return fmt.Errorf("reload claimed Forgejo success-comment dispatch: %w", err)
		}
		if job.SuccessCommentedAt != nil && job.SuccessCommentDispatchID == dispatchID {
			return nil
		}
		if job.SuccessCommentClaimedAt == nil || job.SuccessCommentClaimToken == "" {
			// The winning writer can clear the claim and commit success between our
			// failed UPDATE and reload. Recheck the deterministic provider marker;
			// otherwise schedule a short durable observation retry.
			confirmed, readErr := s.ForgejoIntegration.HasPullRequestComment(ctx, event.RepoFullName, event.PRNumber, marker)
			if readErr != nil {
				return fmt.Errorf("recheck concurrent Forgejo success comment: %w", readErr)
			}
			if confirmed {
				return s.confirmForgejoActionRebaseSuccessComment(ctx, jobID, dispatchID, "")
			}
			retryAt := now.Add(250 * time.Millisecond)
			if err := s.updateForgejoActionRebaseJob(ctx, jobID, ForgejoProjectionPhaseFailedRetryable, map[string]any{
				"last_error_type": "success_comment_claim_busy", "last_error": "waiting for concurrent success-comment confirmation", "next_run_at": &retryAt,
			}); err != nil {
				return fmt.Errorf("schedule concurrent Forgejo success-comment observation: %w", err)
			}
			return &forgejoSuccessCommentClaimBusyError{retryAt: retryAt}
		}
		retryAt := job.SuccessCommentClaimedAt.UTC().Add(claimTTL)
		if retryAt.Before(now) {
			retryAt = now
		}
		if err := s.updateForgejoActionRebaseJob(ctx, jobID, ForgejoProjectionPhaseFailedRetryable, map[string]any{
			"last_error_type": "success_comment_claim_busy", "last_error": "waiting for durable success-comment claim expiry", "next_run_at": &retryAt,
		}); err != nil {
			return fmt.Errorf("schedule Forgejo success-comment claim recovery: %w", err)
		}
		return &forgejoSuccessCommentClaimBusyError{retryAt: retryAt}
	}

	if s.testForgejoSuccessCommentBeforeWrite != nil {
		if err := s.testForgejoSuccessCommentBeforeWrite(); err != nil {
			return err
		}
	}
	if err := s.createForgejoPullRequestComment(ctx, event.RepoFullName, event.PRNumber, comment); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			_ = s.terminalDenyForgejoAction(ctx, "", jobID, DelegatedSessionDenialReason(err), "delegated authority changed at success-comment provider seam", sha)
		}
		return err
	}
	if s.testForgejoSuccessCommentAfterWrite != nil {
		if err := s.testForgejoSuccessCommentAfterWrite(); err != nil {
			return err
		}
	}
	confirmed, err = s.ForgejoIntegration.HasPullRequestComment(ctx, event.RepoFullName, event.PRNumber, marker)
	if err != nil {
		return fmt.Errorf("read back written Forgejo success comment: %w", err)
	}
	if !confirmed {
		return fmt.Errorf("Forgejo success comment was not confirmed by provider readback")
	}
	return s.confirmForgejoActionRebaseSuccessComment(ctx, jobID, dispatchID, claimToken)
}

func (s *Service) confirmForgejoActionRebaseSuccessComment(ctx context.Context, jobID uint, dispatchID, claimToken string) error {
	binding, bound := forgejoActionBindingFromContext(ctx)
	if !bound || binding.JobID != jobID || binding.ActionGeneration == 0 {
		return fmt.Errorf("missing exact Forgejo success-comment binding")
	}
	now := time.Now().UTC()
	query := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where("id = ? AND action_generation = ? AND action_intent_id = ? AND phase NOT IN ? AND success_comment_dispatch_id = ? AND success_commented_at IS NULL",
			jobID, binding.ActionGeneration, binding.IntentID, []string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal}, dispatchID)
	if claimToken != "" {
		query = query.Where("success_comment_claim_token = ?", claimToken)
	}
	result := query.Updates(map[string]any{
		"success_commented_at": &now, "success_comment_claim_token": "", "success_comment_claimed_at": nil,
	})
	if result.Error != nil {
		return fmt.Errorf("confirm Forgejo success comment: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var job db.PullRequestProjectionJob
	if err := s.DBForCtx(ctx).Where("id = ? AND action_generation = ? AND action_intent_id = ?", jobID, binding.ActionGeneration, binding.IntentID).First(&job).Error; err != nil {
		return err
	}
	if job.SuccessCommentDispatchID == dispatchID && job.SuccessCommentedAt != nil {
		return nil
	}
	return fmt.Errorf("Forgejo success-comment confirmation changed concurrently")
}

func (s *Service) updateForgejoActionRebaseJob(ctx context.Context, jobID uint, phase string, updates map[string]any) error {
	binding, ok := forgejoActionBindingFromContext(ctx)
	if jobID == 0 || !ok || binding.JobID != jobID || binding.ActionGeneration == 0 {
		return fmt.Errorf("missing exact Forgejo rebase job binding")
	}
	if updates == nil {
		updates = map[string]any{}
	}
	updates["phase"] = phase
	updates["updated_at"] = time.Now().UTC()
	result := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where(clause.Eq{Column: "trigger", Value: ForgejoProjectionTriggerActionRebase}).
		Where("id = ? AND action_generation = ? AND action_intent_id = ? AND phase NOT IN ?",
			jobID, binding.ActionGeneration, binding.IntentID,
			[]string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal}).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("Forgejo rebase job generation changed or became terminal")
	}
	return nil
}

func (s *Service) markForgejoActionRebaseJobFailed(ctx context.Context, jobID uint, ref, desiredSHA string, err error, defaultRetryable bool) {
	if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		if terminalErr := s.terminalizeBoundActionProviderDenial(ctx, err, "delegated authority changed at post-job provider seam", desiredSHA); terminalErr != nil && !errors.Is(terminalErr, ErrDelegatedSessionUseTimeDenied) {
			slog.WarnContext(ctx, "terminalize post-job provider denial failed", "job_id", jobID, "error", terminalErr)
		}
		return
	}
	failure := newProjectionJobFailure(ref, desiredSHA, err, defaultRetryable)
	if failure == nil {
		return
	}
	phase := ForgejoProjectionPhaseFailedRetryable
	var nextRunAt *time.Time
	if !failure.retryable {
		phase = ForgejoProjectionPhaseFailedTerminal
	} else {
		next := time.Now().UTC().Add(s.forgejoProjectionRetryDelay())
		nextRunAt = &next
	}
	updates := map[string]any{
		"last_error_type": failure.failureType, "last_error": failure.summary,
		"observed_forgejo_head_sha": failure.remoteSHA, "next_run_at": nextRunAt,
	}
	if phase == ForgejoProjectionPhaseFailedTerminal {
		now := time.Now().UTC()
		updates["finished_at"] = &now
	}
	_ = s.updateForgejoActionRebaseJob(ctx, jobID, phase, updates)
}

func (s *Service) completeForgejoActionRebaseJob(ctx context.Context, jobID uint, desiredSHA string) error {
	binding, bound := forgejoActionBindingFromContext(ctx)
	if !bound || binding.JobID != jobID || binding.ActionGeneration == 0 {
		return fmt.Errorf("missing exact Forgejo rebase completion binding")
	}
	now := time.Now().UTC()
	deniedAtBoundary := false
	allowedPhases := []string{
		ForgejoProjectionPhasePreflight, ForgejoProjectionPhaseRebasing, ForgejoProjectionPhaseProjectionResume,
		ForgejoProjectionPhasePushingRef, ForgejoProjectionPhaseVerifyingRef, ForgejoProjectionPhaseVerifyingPR,
		ForgejoProjectionPhaseEnsuringPR, ForgejoProjectionPhaseRecordingProjection, ForgejoProjectionPhaseFailedRetryable,
	}
	err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		// Completion and denial use one lock order: job, then action intent.
		var job db.PullRequestProjectionJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND action_generation = ? AND action_intent_id = ?", jobID, binding.ActionGeneration, binding.IntentID).
			First(&job).Error; err != nil {
			return err
		}
		if job.Trigger != ForgejoProjectionTriggerActionRebase || job.SuccessCommentedAt == nil {
			return fmt.Errorf("projection job %d lacks provider-confirmed rebase success", jobID)
		}
		if !stringInSlice(job.Phase, allowedPhases) {
			return fmt.Errorf("projection job %d cannot complete from phase %s", jobID, job.Phase)
		}
		var intent db.PullRequestActionIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&intent, "id = ?", binding.IntentID).Error; err != nil {
			return err
		}
		if intent.Action != "pr.rebase" || intent.PullRequestID != job.PullRequestID || intent.RepositoryID != job.RepositoryID ||
			!sameOptionalString(intent.AgentSessionID, job.AgentSessionID) {
			return fmt.Errorf("Forgejo rebase completion authority facts mismatch")
		}
		if intent.AgentSessionID != nil {
			if _, err := s.validateDelegatedEffectBoundaryForAction(ContextWithDB(ctx, tx), intent); err != nil {
				return fmt.Errorf("Forgejo rebase completion Boundary Receipt integrity failed: %w", err)
			}
		}
		activeStates := []string{ForgejoActionIntentAccepted, ForgejoActionIntentRunning, ForgejoActionIntentRecovery}
		intentAlreadyCompleted := intent.State == ForgejoActionIntentCompleted && strings.EqualFold(intent.ResultSHA, desiredSHA)
		if (!stringInSlice(intent.State, activeStates) && !intentAlreadyCompleted) || (!intentAlreadyCompleted && !intent.ExpiresAt.After(now)) {
			if intent.State != ForgejoActionIntentCompleted {
				if err := tx.Model(&db.PullRequestActionIntent{}).Where("id = ? AND state <> ?", intent.ID, ForgejoActionIntentCompleted).Updates(map[string]any{
					"state": ForgejoActionIntentDenied, "failure_code": "provider_admission_denied",
					"failure_summary": "action intent was not active at completion boundary", "result_sha": desiredSHA, "finished_at": &now,
				}).Error; err != nil {
					return err
				}
				jobDenial := tx.Model(&db.PullRequestProjectionJob{}).
					Where("id = ? AND action_generation = ? AND action_intent_id = ? AND phase NOT IN ?", job.ID, binding.ActionGeneration, binding.IntentID, []string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal}).
					Updates(map[string]any{
						"phase": ForgejoProjectionPhaseFailedTerminal, "last_error_type": ProjectionFailureProviderAdmissionDenied,
						"last_error": "action intent was not active at completion boundary", "next_run_at": nil, "finished_at": &now,
					})
				if jobDenial.Error != nil {
					return jobDenial.Error
				}
				if jobDenial.RowsAffected != 1 {
					return fmt.Errorf("Forgejo rebase completion job generation changed")
				}
			}
			deniedAtBoundary = true
			return nil
		}
		jobUpdate := tx.Model(&db.PullRequestProjectionJob{}).
			Where("id = ? AND action_generation = ? AND action_intent_id = ? AND phase IN ? AND success_commented_at IS NOT NULL",
				jobID, binding.ActionGeneration, binding.IntentID, allowedPhases).
			Updates(map[string]any{
				"phase": ForgejoProjectionPhaseProjected, "head_sha": desiredSHA, "desired_ags_head_sha": desiredSHA,
				"observed_forgejo_head_sha": desiredSHA, "remote_sha": desiredSHA,
				"last_error_type": "", "last_error": "", "next_run_at": nil, "finished_at": &now, "updated_at": now,
			})
		if jobUpdate.Error != nil {
			return jobUpdate.Error
		}
		if jobUpdate.RowsAffected != 1 {
			return fmt.Errorf("Forgejo rebase job completion changed concurrently")
		}
		if intentAlreadyCompleted {
			return nil
		}
		intentUpdate := tx.Model(&db.PullRequestActionIntent{}).
			Where("id = ? AND state IN ? AND expires_at > ?", intent.ID, activeStates, now).
			Updates(map[string]any{
				"state": ForgejoActionIntentCompleted, "provider_effect_status": ProviderEffectStatusVerifiedCompleted,
				"failure_code": "", "failure_summary": "", "result_sha": desiredSHA, "finished_at": &now,
			})
		if intentUpdate.Error != nil {
			return intentUpdate.Error
		}
		if intentUpdate.RowsAffected != 1 {
			return fmt.Errorf("Forgejo rebase intent completion changed concurrently")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if deniedAtBoundary {
		return fmt.Errorf("Forgejo rebase completion denied by terminal or expired action intent")
	}
	return nil
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
