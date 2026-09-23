package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
	"github.com/ngaut/agent-git-service/internal/gittransport"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
)

// ForgejoWebhookResult describes the result of a Forgejo webhook callback.
type ForgejoWebhookResult struct {
	Handled                   bool
	RepoFullName              string
	PRNumber                  int
	BaseBranch                string
	SyncedSHA                 string
	GitLabBackupHandled       bool
	GitLabTargetBranch        string
	GitLabShadowClosed        bool
	GitLabShadowMRURL         string
	GitHubBackupHandled       bool
	GitHubTargetBranch        string
	GitHubShadowClosed        bool
	GitHubShadowPRURL         string
	AGSPRNumber               int
	BranchDeleted             bool
	DeletedBranch             string
	AGSSourceBranchDeleted    bool
	GitLabSourceBranchDeleted bool
	GitHubSourceBranchDeleted bool
	WorkflowAction            string
	WorkflowLabel             string
	WorkflowStatus            string
}

// HandleForgejoWebhook validates and applies Forgejo merge-authority callbacks.
func (s *Service) HandleForgejoWebhook(ctx context.Context, headers http.Header, body []byte) (ForgejoWebhookResult, error) {
	if s == nil || s.ForgejoIntegration == nil {
		return ForgejoWebhookResult{}, fmt.Errorf("forgejo integration is not configured")
	}
	signature := forgejoWebhookSignatureFromHeaders(headers)
	closedEvent, closedOK, err := s.ForgejoIntegration.VerifyAndParseClosedPullRequestWebhook(signature, body)
	if err != nil {
		return ForgejoWebhookResult{}, err
	}
	if !closedOK {
		deletedEvent, deletedOK, deletedErr := s.ForgejoIntegration.VerifyAndParseDeletedBranchWebhook(signature, body)
		if deletedErr != nil {
			return ForgejoWebhookResult{}, deletedErr
		}
		if deletedOK {
			return s.handleForgejoDeletedBranch(ctx, deletedEvent)
		}
		actionEvent, actionOK, actionErr := s.ForgejoIntegration.VerifyAndParsePullRequestActionLabelWebhook(signature, body)
		if actionErr != nil {
			return ForgejoWebhookResult{}, actionErr
		}
		if !actionOK {
			return ForgejoWebhookResult{Handled: false}, nil
		}
		actionEvent.CorrelationID = firstNonEmpty(
			strings.TrimSpace(headers.Get("X-Forgejo-Delivery")),
			strings.TrimSpace(headers.Get("X-Gitea-Delivery")),
			strings.TrimSpace(headers.Get("X-GitHub-Delivery")),
		)
		return s.handleForgejoPullRequestActionLabel(ctx, actionEvent)
	}
	if !closedEvent.Merged {
		agsPR, err := s.MarkPullRequestClosedFromProjection(ctx, ProjectionProviderForgejo, closedEvent.RepoFullName, closedEvent.PRNumber, "forgejo")
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				slog.WarnContext(ctx, "ignore closed Forgejo PR without AGS projection", "forgejo_repo", closedEvent.RepoFullName, "forgejo_pr", closedEvent.PRNumber)
				return ForgejoWebhookResult{Handled: false, RepoFullName: closedEvent.RepoFullName, PRNumber: closedEvent.PRNumber, BaseBranch: closedEvent.BaseBranch}, nil
			}
			return ForgejoWebhookResult{}, fmt.Errorf("record AGS PR close from Forgejo projection: %w", err)
		}
		return ForgejoWebhookResult{Handled: true, RepoFullName: closedEvent.RepoFullName, PRNumber: closedEvent.PRNumber, BaseBranch: closedEvent.BaseBranch, AGSPRNumber: agsPR.Number}, nil
	}
	event := forgejointegration.MergedPullRequestEvent{
		RepoFullName: closedEvent.RepoFullName,
		PRNumber:     closedEvent.PRNumber,
		PRURL:        closedEvent.PRURL,
		HeadBranch:   closedEvent.HeadBranch,
		BaseBranch:   closedEvent.BaseBranch,
	}
	agsPR, err := s.FindPullRequestByProjection(ctx, ProjectionProviderForgejo, event.RepoFullName, event.PRNumber)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ForgejoWebhookResult{}, fmt.Errorf("resolve AGS PR from Forgejo projection: %w", err)
	}
	if errors.Is(err, ErrNotFound) {
		return s.handleUnmappedForgejoMergedPullRequest(ctx, event)
	}
	agsEvent := event
	agsEvent.RepoFullName = agsPR.Repository.FullName
	sha, err := s.syncForgejoMergedBranchToAGS(ctx, agsEvent)
	if err != nil {
		return ForgejoWebhookResult{}, err
	}
	agsPR, err = s.MarkPullRequestMergedFromProjection(ctx, ProjectionProviderForgejo, event.RepoFullName, event.PRNumber, sha, "forgejo")
	if err != nil {
		return ForgejoWebhookResult{}, fmt.Errorf("record AGS PR merge from Forgejo projection: %w", err)
	}
	shadowResult, shadowClosed, err := s.closeGitLabShadowMergeRequest(ctx, agsEvent, sha, agsPR)
	if err != nil {
		slog.WarnContext(ctx, "close GitLab shadow MR after Forgejo merge failed", "repo", agsEvent.RepoFullName, "forgejo_pr", event.PRNumber, "base", agsEvent.BaseBranch, "sha", sha, "error", err)
		shadowClosed = false
	}
	gitHubShadowResult, gitHubShadowClosed, gitHubShadowErr := s.closeGitHubShadowPullRequest(ctx, agsEvent, sha, agsPR)
	if gitHubShadowErr != nil {
		slog.WarnContext(ctx, "close GitHub shadow PR after Forgejo merge failed", "repo", agsEvent.RepoFullName, "forgejo_pr", event.PRNumber, "base", agsEvent.BaseBranch, "sha", sha, "error", gitHubShadowErr)
		gitHubShadowClosed = false
	}
	gitLabResult, gitLabHandled, err := s.pushForgejoMergedBranchToGitLabBackup(ctx, agsEvent, sha)
	if err != nil {
		slog.WarnContext(ctx, "push Forgejo merged branch to GitLab backup failed", "repo", agsEvent.RepoFullName, "forgejo_pr", event.PRNumber, "base", agsEvent.BaseBranch, "sha", sha, "error", err)
		gitLabHandled = false
	}
	gitHubResult, gitHubHandled, gitHubErr := s.pushForgejoMergedBranchToGitHubBackup(ctx, agsEvent, sha)
	if gitHubErr != nil {
		slog.WarnContext(ctx, "push Forgejo merged branch to GitHub backup failed", "repo", agsEvent.RepoFullName, "forgejo_pr", event.PRNumber, "base", agsEvent.BaseBranch, "sha", sha, "error", gitHubErr)
		gitHubHandled = false
	} else if gitHubHandled {
		slog.InfoContext(ctx, "github backup pushed after Forgejo merge", "repo", agsEvent.RepoFullName, "base", agsEvent.BaseBranch, "sha", sha, "target", gitHubResult.TargetRepo)
	}
	// MarkPullRequestMergedFromProjection durably enqueues the typed Multica
	// terminal projection in the same transaction as the AGS merge fact. Do not
	// reintroduce a post-commit goroutine here.
	cleanupResult, cleanupErr := s.cleanupMergedSourceBranchIfForgejoDeleted(ctx, agsPR, event.RepoFullName, event.HeadBranch)
	if cleanupErr != nil {
		slog.WarnContext(ctx, "cleanup merged source branch after Forgejo merge failed", "repo", agsEvent.RepoFullName, "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "branch", event.HeadBranch, "error", cleanupErr)
	}
	return ForgejoWebhookResult{
		Handled:                   true,
		RepoFullName:              event.RepoFullName,
		PRNumber:                  event.PRNumber,
		BaseBranch:                event.BaseBranch,
		SyncedSHA:                 sha,
		GitLabBackupHandled:       gitLabHandled,
		GitLabTargetBranch:        gitLabResult.TargetBranch,
		GitLabShadowClosed:        shadowClosed,
		GitLabShadowMRURL:         shadowResult.WebURL,
		GitHubBackupHandled:       gitHubHandled,
		GitHubTargetBranch:        gitHubResult.TargetBranch,
		GitHubShadowClosed:        gitHubShadowClosed,
		GitHubShadowPRURL:         gitHubShadowResult.WebURL,
		AGSPRNumber:               agsPR.Number,
		DeletedBranch:             cleanupResult.DeletedBranch,
		AGSSourceBranchDeleted:    cleanupResult.AGSSourceBranchDeleted,
		GitLabSourceBranchDeleted: cleanupResult.GitLabSourceBranchDeleted,
		GitHubSourceBranchDeleted: cleanupResult.GitHubSourceBranchDeleted,
	}, nil
}

func (s *Service) handleUnmappedForgejoMergedPullRequest(ctx context.Context, event forgejointegration.MergedPullRequestEvent) (ForgejoWebhookResult, error) {
	if s == nil || s.ForgejoIntegration == nil {
		return ForgejoWebhookResult{}, fmt.Errorf("forgejo integration is not configured")
	}
	repoFullName, ok := s.ForgejoIntegration.ResolveMergedPullRequestRepo(event)
	if !ok {
		slog.WarnContext(ctx, "ignore merged Forgejo PR without AGS projection or allowed repo mapping", "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "head", event.HeadBranch, "base", event.BaseBranch)
		return ForgejoWebhookResult{Handled: false, RepoFullName: event.RepoFullName, PRNumber: event.PRNumber, BaseBranch: event.BaseBranch}, nil
	}
	agsEvent := event
	agsEvent.RepoFullName = repoFullName
	sha, err := s.syncForgejoMergedBranchToAGS(ctx, agsEvent)
	if err != nil {
		return ForgejoWebhookResult{}, err
	}
	slog.InfoContext(ctx, "synced unmapped Forgejo merged PR base to AGS", "repo", repoFullName, "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "base", event.BaseBranch, "sha", sha)
	return ForgejoWebhookResult{Handled: true, RepoFullName: event.RepoFullName, PRNumber: event.PRNumber, BaseBranch: event.BaseBranch, SyncedSHA: sha}, nil
}

type forgejoActionRebasePreflight struct {
	Projection            db.PullRequestProjection
	RepoFullName          string
	RepoPath              string
	Ref                   string
	HeadSHA               string
	BaseSHA               string
	ForgejoHeadSHA        string
	ForgejoPRHeadSHA      string
	ForgejoPRURL          string
	ForgejoBaseSHA        string
	AlreadyConverged      bool
	ResumeExistingAGSHead bool
	RebaseExistingAGSHead bool
}

func (s *Service) handleForgejoPullRequestActionLabel(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent) (ForgejoWebhookResult, error) {
	result := ForgejoWebhookResult{Handled: true, RepoFullName: event.RepoFullName, PRNumber: event.PRNumber, BaseBranch: event.BaseBranch, WorkflowAction: "rebase", WorkflowLabel: event.LabelName, WorkflowStatus: "queued"}
	if event.LabelName != forgejointegration.AGSActionRebaseLabel {
		result.WorkflowStatus = "ignored"
		return result, nil
	}

	mu := s.getForgejoWorkflowActionMu(event.RepoFullName, event.PRNumber)
	mu.Lock()
	defer mu.Unlock()

	// The webhook is not an authority channel. Only a matching exact intent may
	// enter the leased rebase kernel. A signed direct trigger with no AGS-owned
	// authority is server-owned invalid-input cleanup, not a delegated effect.
	intent, accepted, denial, consumeErr := s.consumeForgejoActionIntent(ctx, event)
	if consumeErr != nil {
		result.WorkflowStatus = "failed"
		return result, consumeErr
	}
	if !accepted && denial == forgejoActionIntentDenialMissingAuthority {
		admission, admissionErr := s.admitForgejoLabelActionIntent(ctx, event)
		if admissionErr != nil {
			result.WorkflowStatus = "failed"
			return result, admissionErr
		}
		if admission.Stale {
			result.WorkflowStatus = "ignored"
			return result, nil
		}
		if admission.Replay {
			result.AGSPRNumber = admission.Intent.AGSPRNumber
			result.SyncedSHA = admission.Intent.ResultSHA
			switch admission.Intent.State {
			case ForgejoActionIntentCompleted:
				result.WorkflowStatus = "rebased"
			case ForgejoActionIntentConflict:
				result.WorkflowStatus = "conflict"
			case ForgejoActionIntentDenied, ForgejoActionIntentProjection:
				result.WorkflowStatus = "denied"
			case ForgejoActionIntentRecovery:
				result.WorkflowStatus = "recovery_needed"
			default:
				result.WorkflowStatus = "queued"
			}
			return result, nil
		}
		if admission.Admitted {
			intent, accepted, denial, consumeErr = s.consumeForgejoActionIntent(ctx, event)
			if consumeErr != nil {
				result.WorkflowStatus = "failed"
				return result, consumeErr
			}
		} else {
			result.WorkflowStatus = "denied"
			if admission.Intent.ID != "" {
				result.AGSPRNumber = admission.Intent.AGSPRNumber
			}
			if err := s.cleanupInvalidForgejoActionTrigger(ctx, event.RepoFullName, event.PRNumber, event.LabelName); err != nil {
				slog.WarnContext(ctx, "quarantine invalid Forgejo action trigger failed", "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "error", err)
				return result, fmt.Errorf("quarantine invalid Forgejo action trigger: %w", err)
			}
			if err := s.publishInvalidForgejoActionStatus(ctx, event.RepoFullName, event.PRNumber); err != nil {
				slog.WarnContext(ctx, "publish invalid Forgejo action status failed", "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "error", err)
				return result, fmt.Errorf("publish invalid Forgejo action status: %w", err)
			}
			if admission.MappedActor && strings.TrimSpace(admission.DenialReason) != "" {
				slog.WarnContext(ctx, "deny mapped Forgejo label action", "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "actor", event.SenderLogin, "reason", admission.DenialReason)
			}
			return result, nil
		}
	}
	if !accepted {
		result.WorkflowStatus = "denied"
		if intent.ID != "" {
			result.AGSPRNumber = intent.AGSPRNumber
		}
		return result, nil
	}
	if intent.AgentSessionID != nil {
		delegatedCtx, err := s.ContextForDelegatedSessionID(ctx, *intent.AgentSessionID)
		if err == nil {
			_, err = s.RevalidateDelegatedSession(delegatedCtx, intent.RepositoryID, "pr.rebase", "repo:write", forgejoRebaseSessionConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA))
		}
		if err != nil {
			code := DelegatedSessionDenialReason(err)
			_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, code, "delegated authority changed before webhook effect", "")
			result.WorkflowStatus = "denied"
			result.AGSPRNumber = intent.AGSPRNumber
			return result, nil
		}
		ctx = delegatedCtx
	}
	ctx = contextWithForgejoActionIntent(ctx, intent.ID)
	var agsPR db.PullRequest
	if err := s.DBForCtx(ctx).Preload("Repository").First(&agsPR, intent.PullRequestID).Error; err != nil {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "projection_missing", "projected pull request is unavailable", "")
		result.WorkflowStatus = "denied"
		return result, nil
	}
	result.AGSPRNumber = agsPR.Number
	if agsPR.Repository.FullName != "" {
		result.RepoFullName = agsPR.Repository.FullName
	}
	preflight, err := s.preflightForgejoActionRebase(ctx, event, agsPR, intent)
	if err != nil {
		if terminalErr := s.terminalDenyForgejoAction(ctx, intent.ID, 0, "exact_action_fact_drift", "live action facts do not match intent", ""); terminalErr != nil {
			return result, fmt.Errorf("terminalize exact action fact drift: %w", terminalErr)
		}
		result.WorkflowStatus = "denied"
		if ackErr := s.acknowledgeForgejoActionRequest(ctx, event.RepoFullName, event.PRNumber, "AGS rebase request was blocked: live action facts do not match the exact intent. Use `ags-cli pr update-branch --rebase` or retry the label after AGS and Forgejo heads agree."); ackErr != nil {
			slog.WarnContext(ctx, "acknowledge failed Forgejo action-rebase request", "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "error", ackErr)
		}
		return result, nil
	}
	if err := s.removeForgejoPullRequestLabel(ctx, event.RepoFullName, event.PRNumber, forgejointegration.AGSActionRebaseLabel); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			code := DelegatedSessionDenialReason(err)
			_ = s.terminalDenyForgejoAction(ctx, intent.ID, 0, code, "delegated authority changed at provider action-label removal", "")
			result.WorkflowStatus = "denied"
			return result, err
		}
		s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "label_remove_failed", "provider action label removal failed", "")
		return result, nil
	}
	if agsPR.ID != intent.PullRequestID || agsPR.RepositoryID != intent.RepositoryID || agsPR.Number != intent.AGSPRNumber {
		s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "projection_drift", "projected pull request does not match intent", "")
		result.WorkflowStatus = "denied"
		return result, nil
	}
	if agsPR.State != db.StateOpen || agsPR.Merged {
		result.WorkflowStatus = "blocked"
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "pr_not_open", "pull request is not open or already merged", "")
		if notifyErr := s.markForgejoActionBlocked(ctx, event, "pull request is not open"); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, intent.ID, 0, notifyErr, "delegated authority changed while publishing blocked status", "")
		}
		return result, nil
	}
	if actionJob, resumable, err := s.findResumableForgejoActionRebaseJob(ctx, event, agsPR, intent); err != nil {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "resumable_job_lookup", "load resumable rebase action failed", "")
		return result, fmt.Errorf("load resumable Forgejo rebase action: %w", err)
	} else if resumable {
		if err := s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRunning, "", "", ""); err != nil {
			return result, fmt.Errorf("transition intent to running before resume: %w", err)
		}
		ctx = contextWithForgejoActionJob(ctx, actionJob)
		res, err := s.resumeForgejoActionRebaseJob(ctx, event, agsPR, actionJob, result)
		if err != nil {
			state, code := ForgejoActionIntentRecovery, "resume_failed"
			if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
				state, code = ForgejoActionIntentDenied, DelegatedSessionDenialReason(err)
			}
			_ = s.actionIntentState(ctx, intent.ID, state, code, "rebase resume admission failed", "")
			return res, err
		}
		switch res.WorkflowStatus {
		case "rebased":
			// completeForgejoActionRebaseJob owns the atomic job+intent success fact.
		case "conflict":
			_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentConflict, "conflict", "rebase conflict", res.SyncedSHA)
		case "projection_failed", "needs_rebase":
			_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "projection_failed", "resume projection failed", res.SyncedSHA)
		default:
			_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "resume_unknown_state", fmt.Sprintf("resume completed in unexpected state: %s", res.WorkflowStatus), res.SyncedSHA)
		}
		return res, nil
	}
	if preflight.AlreadyConverged {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRunning, "", "", "")
		res, err := s.reconcileAlreadyConvergedForgejoAction(ctx, event, agsPR, intent, preflight, result)
		if err != nil {
			_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, delegatedActionFailureCode(err, "reconcile_failed"), "reconcile admission failed", "")
		}
		return res, err
	}
	if preflight.RebaseExistingAGSHead {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRunning, "", "", "")
		res, err := s.rebaseExistingAGSHeadForgejoAction(ctx, event, agsPR, intent, preflight, result)
		if err != nil {
			_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, delegatedActionFailureCode(err, "rebase_existing_failed"), "rebase admission failed", "")
		}
		return res, err
	}
	if preflight.ResumeExistingAGSHead {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRunning, "", "", "")
		res, err := s.resumeExistingAGSHeadForgejoAction(ctx, event, agsPR, intent, preflight, result)
		if err != nil {
			_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, delegatedActionFailureCode(err, "resume_existing_failed"), "resume admission failed", "")
		}
		return res, err
	}
	if err := s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRunning, "", "", ""); err != nil {
		return result, fmt.Errorf("transition intent to running: %w", err)
	}
	actionJob, err := s.beginForgejoActionRebaseJob(ctx, event, agsPR, intent, preflight)
	if err != nil {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "begin_job_failed", "begin durable rebase action failed", "")
		return result, fmt.Errorf("begin durable Forgejo rebase action: %w", err)
	}
	ctx = contextWithForgejoActionJob(ctx, actionJob)
	if err := s.revalidateCurrentDelegatedProviderWrite(ctx, event.RepoFullName, event.PRNumber); err != nil {
		code := DelegatedSessionDenialReason(err)
		if terminalErr := s.terminalDenyForgejoAction(ctx, intent.ID, actionJob.ID, code, "delegated authority changed during action preflight", preflight.HeadSHA); terminalErr != nil {
			return result, fmt.Errorf("terminalize post-job action preflight denial: %w", terminalErr)
		}
		result.WorkflowStatus = "denied"
		return result, nil
	}
	if err := s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseRebasing, nil); err != nil {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "phase_update_failed", "persist rebase phase failed", "")
		return result, fmt.Errorf("persist Forgejo rebase phase: %w", err)
	}
	if err := s.clearForgejoWorkflowStatusLabels(ctx, event.RepoFullName, event.PRNumber); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			_ = s.terminalDenyForgejoAction(ctx, intent.ID, actionJob.ID, DelegatedSessionDenialReason(err), "delegated authority changed while clearing provider status labels", "")
			result.WorkflowStatus = "denied"
		}
		return result, err
	}
	if err := s.addForgejoPullRequestLabels(ctx, event.RepoFullName, event.PRNumber, []string{forgejointegration.AGSStatusRebasingLabel}); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			_ = s.terminalDenyForgejoAction(ctx, intent.ID, actionJob.ID, DelegatedSessionDenialReason(err), "delegated authority changed while publishing provider rebase status", "")
			result.WorkflowStatus = "denied"
			return result, err
		}
		slog.WarnContext(ctx, "add Forgejo PR rebasing status label failed", "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "error", err)
	}
	if err := s.revalidateCurrentDelegatedProviderWrite(ctx, event.RepoFullName, event.PRNumber); err != nil {
		code := DelegatedSessionDenialReason(err)
		if terminalErr := s.terminalDenyForgejoAction(ctx, intent.ID, actionJob.ID, code, "delegated authority changed before AGS rebase", preflight.HeadSHA); terminalErr != nil {
			return result, fmt.Errorf("terminalize post-job AGS rebase denial: %w", terminalErr)
		}
		result.WorkflowStatus = "denied"
		return result, nil
	}
	updatedPR, sha, err := s.rebaseAGSPrBranchFromForgejoLabel(ctx, agsPR)
	if err != nil {
		result.WorkflowStatus = "failed"
		failureCode := "rebase_failed"
		if isLikelyRebaseConflict(err) {
			result.WorkflowStatus = "conflict"
			failureCode = "conflict"
		}
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, failureCode, err.Error(), "")
		s.markForgejoActionRebaseJobFailed(ctx, actionJob.ID, preflight.Ref, preflight.HeadSHA, err, false)
		if notifyErr := s.markForgejoActionFailed(ctx, event, err); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, intent.ID, actionJob.ID, notifyErr, "delegated authority changed while publishing rebase failure", "")
		}
		return result, nil
	}
	if err := s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseProjectionResume, map[string]any{
		"head_sha": sha, "desired_ags_head_sha": sha, "observed_forgejo_head_sha": preflight.ForgejoHeadSHA,
		"remote_sha": preflight.ForgejoHeadSHA, "attempt": actionJob.Attempt + 1,
	}); err != nil {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "phase_update_failed", "persist rebased head for recovery failed", "")
		return result, fmt.Errorf("persist rebased AGS head for recovery: %w", err)
	}
	result.SyncedSHA = sha
	if moved, detail, checkErr := s.forgejoActionBaseChanged(ctx, preflight, sha); checkErr != nil {
		result.WorkflowStatus = "projection_failed"
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "projection_failed", "post-rebase base verification failed", sha)
		provider := PullRequestProviderResult{Provider: ProjectionProviderForgejo, Required: true, Attempted: true, DesiredSHA: sha, Err: checkErr}
		failure, recordErr := s.recordForgejoActionProjectionFailure(ctx, event, updatedPR, sha, provider, "post_rebase_base_verify")
		if recordErr != nil {
			return result, fmt.Errorf("persist Forgejo base verification failure: %w", recordErr)
		}
		s.markForgejoActionRebaseJobFailed(ctx, actionJob.ID, preflight.Ref, sha, checkErr, true)
		if notifyErr := s.markForgejoActionProjectionFailed(ctx, event, updatedPR, failure, true); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, intent.ID, actionJob.ID, notifyErr, "delegated authority changed while publishing projection failure", sha)
		}
		return result, nil
	} else if moved {
		result.WorkflowStatus = "needs_rebase"
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "base_moved", detail, sha)
		_ = s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseNeedsRebase, map[string]any{"last_error_type": "base_moved", "last_error": detail})
		if notifyErr := s.markForgejoActionNeedsRebase(ctx, event, updatedPR, sha, detail); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, intent.ID, actionJob.ID, notifyErr, "delegated authority changed while publishing needs-rebase status", sha)
		}
		return result, nil
	}
	if err := s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhasePushingRef, nil); err != nil {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "phase_update_failed", "persist push phase failed", sha)
		return result, fmt.Errorf("persist Forgejo action push phase: %w", err)
	}
	dispatch, err := s.DispatchPullRequestIntegrationsWithPolicy(ctx, updatedPR, PullRequestIntegrationPolicy{
		RequireForgejo: true,
		ForgejoRewrite: &ForgejoPullRequestRewritePolicy{
			ExternalRepo: preflight.Projection.ExternalRepo, ExternalNumber: preflight.Projection.ExternalNumber,
			ExpectedOldHeadSHA: preflight.HeadSHA,
		},
	})
	if err != nil {
		result.WorkflowStatus = "projection_failed"
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			if terminalErr := s.terminalDenyForgejoAction(ctx, intent.ID, actionJob.ID, DelegatedSessionDenialReason(err), "delegated authority changed during provider projection", sha); terminalErr != nil {
				return result, fmt.Errorf("terminalize provider projection denial: %w", terminalErr)
			}
			result.WorkflowStatus = "denied"
			return result, err
		}
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "projection_failed", "dispatch push failed", sha)
		providerFailure := dispatch.Forgejo
		if providerFailure.Err == nil {
			providerFailure.Err = err
		}
		failure, recordErr := s.recordForgejoActionProjectionFailure(ctx, event, updatedPR, sha, providerFailure, "push")
		if recordErr != nil {
			return result, fmt.Errorf("persist Forgejo action projection failure: %w", recordErr)
		}
		s.markForgejoActionRebaseJobFailed(ctx, actionJob.ID, preflight.Ref, sha, providerFailure.Err, true)
		if notifyErr := s.markForgejoActionProjectionFailed(ctx, event, updatedPR, failure, true); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, intent.ID, actionJob.ID, notifyErr, "delegated authority changed while publishing projection failure", sha)
		}
		return result, nil
	}
	if err := s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseVerifyingRef, map[string]any{"observed_forgejo_head_sha": sha, "remote_sha": sha}); err != nil {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "phase_update_failed", "persist ref verification phase failed", sha)
		return result, fmt.Errorf("persist Forgejo action ref verification phase: %w", err)
	}
	if moved, detail, checkErr := s.forgejoActionBaseChanged(ctx, preflight, sha); checkErr != nil {
		result.WorkflowStatus = "projection_failed"
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "projection_failed", "post-push base verification failed", sha)
		provider := dispatch.Forgejo
		provider.Err = checkErr
		failure, recordErr := s.recordForgejoActionProjectionFailure(ctx, event, updatedPR, sha, provider, "post_push_base_verify")
		if recordErr != nil {
			return result, fmt.Errorf("persist Forgejo post-push base verification failure: %w", recordErr)
		}
		s.markForgejoActionRebaseJobFailed(ctx, actionJob.ID, preflight.Ref, sha, checkErr, true)
		if notifyErr := s.markForgejoActionProjectionFailed(ctx, event, updatedPR, failure, true); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, intent.ID, actionJob.ID, notifyErr, "delegated authority changed while publishing projection failure", sha)
		}
		return result, nil
	} else if moved {
		result.WorkflowStatus = "needs_rebase"
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "base_moved", detail, sha)
		_ = s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseNeedsRebase, map[string]any{"last_error_type": "base_moved", "last_error": detail})
		if notifyErr := s.markForgejoActionNeedsRebase(ctx, event, updatedPR, sha, detail); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, intent.ID, actionJob.ID, notifyErr, "delegated authority changed while publishing needs-rebase status", sha)
		}
		return result, nil
	}
	if err := s.updateForgejoActionRebaseJob(ctx, actionJob.ID, ForgejoProjectionPhaseVerifyingPR, nil); err != nil {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "phase_update_failed", "persist PR verification phase failed", sha)
		return result, fmt.Errorf("persist Forgejo action PR verification phase: %w", err)
	}
	if err := s.verifyForgejoActionConvergence(ctx, event, updatedPR, preflight, sha); err != nil {
		result.WorkflowStatus = "projection_failed"
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "projection_failed", "convergence verification failed", sha)
		provider := dispatch.Forgejo
		provider.Err = err
		failure, recordErr := s.recordForgejoActionProjectionFailure(ctx, event, updatedPR, sha, provider, "convergence_verify")
		if recordErr != nil {
			return result, fmt.Errorf("persist Forgejo exact convergence failure: %w", recordErr)
		}
		s.markForgejoActionRebaseJobFailed(ctx, actionJob.ID, preflight.Ref, sha, err, true)
		if notifyErr := s.markForgejoActionProjectionFailed(ctx, event, updatedPR, failure, true); notifyErr != nil {
			return result, s.handleForgejoStatusNotificationError(ctx, intent.ID, actionJob.ID, notifyErr, "delegated authority changed while publishing projection failure", sha)
		}
		return result, nil
	}
	if err := s.ResolveProjectionRefState(ctx, preflight.RepoFullName, ProjectionProviderForgejo, preflight.Ref, sha, sha, time.Now().UTC()); err != nil {
		if IsProjectionAlertingError(err) {
			slog.WarnContext(ctx, "Forgejo action resolved notification degraded", "repo", preflight.RepoFullName, "forgejo_repo", event.RepoFullName, "ags_pr", updatedPR.Number, "forgejo_pr", event.PRNumber, "error", err)
		} else {
			_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "projection_resolve_failed", "resolve projection drift failed", sha)
			return result, fmt.Errorf("resolve Forgejo action projection drift: %w", err)
		}
	}
	if err := s.clearForgejoWorkflowStatusLabels(ctx, event.RepoFullName, event.PRNumber); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			_ = s.terminalDenyForgejoAction(ctx, intent.ID, actionJob.ID, DelegatedSessionDenialReason(err), "delegated authority changed while clearing provider status labels", sha)
			result.WorkflowStatus = "denied"
		}
		return result, err
	}
	if err := s.publishForgejoActionRebaseSuccess(ctx, event, updatedPR, actionJob.ID, sha); err != nil {
		if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			code := DelegatedSessionDenialReason(err)
			_ = s.terminalDenyForgejoAction(ctx, intent.ID, actionJob.ID, code, "delegated authority changed at success-comment provider seam", sha)
			result.WorkflowStatus = "denied"
		}
		return result, err
	}
	if err := s.completeForgejoActionRebaseJob(ctx, actionJob.ID, sha); err != nil {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentRecovery, "complete_job_failed", "persist completed rebase action failed", sha)
		return result, fmt.Errorf("persist completed Forgejo rebase action: %w", err)
	}
	result.WorkflowStatus = "rebased"
	slog.InfoContext(ctx, "processed Forgejo PR action label", "repo", result.RepoFullName, "ags_pr", updatedPR.Number, "forgejo_repo", event.RepoFullName, "forgejo_pr", event.PRNumber, "label", event.LabelName, "sha", sha)
	return result, nil
}

func (s *Service) preflightForgejoActionRebase(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, intent db.PullRequestActionIntent) (forgejoActionRebasePreflight, error) {
	if s == nil || s.ForgejoIntegration == nil {
		return forgejoActionRebasePreflight{}, fmt.Errorf("Forgejo integration is not configured")
	}
	if intent.Action != "pr.rebase" || intent.PullRequestID != pr.ID || intent.RepositoryID != pr.RepositoryID ||
		pr.State != db.StateOpen || pr.Merged || intent.AGSPRNumber != pr.Number || intent.ForgejoRepo != strings.TrimSpace(event.RepoFullName) ||
		intent.ForgejoPRNumber != event.PRNumber || intent.HeadRef != pr.HeadRef || intent.BaseRef != pr.BaseRef {
		return forgejoActionRebasePreflight{}, forgejoActionProjectionError(pr, event, intent.ExpectedHeadSHA, pr.HeadSHA, forgejointegration.ProjectionFailurePullRequestAuthorityMissing, "live action coordinate does not match the exact intent")
	}
	projection, ok := s.forgejoProjectionForPullRequest(ctx, pr.ID)
	if !ok {
		return forgejoActionRebasePreflight{}, forgejoActionProjectionError(pr, event, pr.HeadSHA, "", forgejointegration.ProjectionFailurePullRequestProjectionMissing, "durable Forgejo pull request mapping is missing")
	}
	repoFullName := strings.TrimSpace(pr.Repository.FullName)
	if repoFullName == "" {
		repo, err := s.GetRepoByID(ctx, fmt.Sprint(pr.RepositoryID))
		if err != nil {
			return forgejoActionRebasePreflight{}, fmt.Errorf("lookup PR repository for rebase preflight: %w", err)
		}
		pr.Repository = repo
		repoFullName = repo.FullName
	}
	head := strings.TrimSpace(pr.HeadRef)
	base := strings.TrimSpace(pr.BaseRef)
	ref := "refs/heads/" + head
	if projection.Provider != ProjectionProviderForgejo || projection.ExternalRepo != event.RepoFullName || projection.ExternalNumber != event.PRNumber || projection.SourceBranch != head || projection.TargetBranch != base || projection.State != ProjectionStateOpen {
		return forgejoActionRebasePreflight{}, forgejoActionProjectionError(pr, event, pr.HeadSHA, projection.LastSyncedSHA, forgejointegration.ProjectionFailurePullRequestAuthorityMissing, "Forgejo action does not match the durable PR projection mapping")
	}
	if strings.TrimSpace(event.HeadBranch) != head || strings.TrimSpace(event.BaseBranch) != base {
		return forgejoActionRebasePreflight{}, forgejoActionProjectionError(pr, event, pr.HeadSHA, event.HeadSHA, forgejointegration.ProjectionFailurePullRequestAuthorityMissing, "Forgejo webhook head/base does not match the mapped AGS PR")
	}
	defaultBranch := strings.TrimSpace(pr.Repository.DefaultBranch)
	if head == "" || base == "" || head == base || head == defaultBranch || strings.HasPrefix(head, "refs/") || strings.Contains(head, ":") {
		return forgejoActionRebasePreflight{}, forgejoActionProjectionError(pr, event, pr.HeadSHA, "", forgejointegration.ProjectionFailureProtectedBranch, "mapped ref is not an eligible non-base PR work branch")
	}
	if pr.HeadRepositoryID != 0 && pr.HeadRepositoryID != pr.RepositoryID {
		return forgejoActionRebasePreflight{}, forgejoActionProjectionError(pr, event, pr.HeadSHA, "", forgejointegration.ProjectionFailurePullRequestAuthorityMissing, "cross-repository PR heads are not eligible for lease rewrite")
	}
	repoPath, err := s.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		return forgejoActionRebasePreflight{}, fmt.Errorf("lookup AGS repo path for rebase preflight: %w", err)
	}
	headSHA, err := s.Git.HeadSHA(ctx, repoFullName, head)
	if err != nil {
		return forgejoActionRebasePreflight{}, fmt.Errorf("read AGS PR branch for rebase preflight: %w", err)
	}
	baseSHA, err := s.Git.HeadSHA(ctx, repoFullName, base)
	if err != nil {
		return forgejoActionRebasePreflight{}, fmt.Errorf("read AGS base branch for rebase preflight: %w", err)
	}
	accepted := strings.TrimSpace(intent.ExpectedHeadSHA)
	forgejoHead, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, repoFullName, repoPath, head)
	if err != nil || !checked {
		return forgejoActionRebasePreflight{}, fmt.Errorf("read Forgejo PR branch for rebase preflight: %w", firstNonNil(err, fmt.Errorf("live remote ref inspection unavailable")))
	}
	forgejoPR, found, err := s.ForgejoIntegration.ExactPullRequestSnapshot(ctx, repoFullName, projection.ExternalRepo, event.PRNumber)
	if err != nil {
		return forgejoActionRebasePreflight{}, fmt.Errorf("read Forgejo PR for rebase preflight: %w", err)
	}
	if !found || forgejoPR.State != "open" || forgejoPR.HeadRef != head || forgejoPR.BaseRef != base {
		actual := ""
		if found {
			actual = forgejoPR.HeadSHA
		}
		return forgejoActionRebasePreflight{}, forgejoActionProjectionError(pr, event, accepted, actual, forgejointegration.ProjectionFailurePullRequestStateDrift, "Forgejo PR identity does not match the accepted projection")
	}
	forgejoBase, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, repoFullName, repoPath, base)
	if err != nil || !checked {
		return forgejoActionRebasePreflight{}, fmt.Errorf("read Forgejo base for rebase preflight: %w", firstNonNil(err, fmt.Errorf("live remote base inspection unavailable")))
	}
	liveBase, err := agreeLiveRebaseBase(ctx, repoPath, intent.ExpectedBaseSHA, baseSHA, forgejoBase)
	if err != nil {
		return forgejoActionRebasePreflight{}, forgejoActionProjectionError(pr, event, intent.ExpectedBaseSHA, firstNonEmpty(baseSHA, forgejoBase), forgejointegration.ProjectionFailureSHADrift, "live AGS and Forgejo base facts are not a fast-forward of the exact intent: "+err.Error())
	}
	for label, actual := range map[string]string{
		"accepted projection": projection.LastSyncedSHA, "AGS branch": headSHA, "AGS PR": pr.HeadSHA,
		"Forgejo webhook": event.HeadSHA, "Forgejo branch": forgejoHead, "Forgejo PR": forgejoPR.HeadSHA,
	} {
		if !exactGitSHA(actual, accepted) {
			return forgejoActionRebasePreflight{}, forgejoActionProjectionError(pr, event, accepted, actual, forgejointegration.ProjectionFailureSHADrift, label+" head does not match the exact intent")
		}
	}
	return forgejoActionRebasePreflight{
		Projection: projection, RepoFullName: repoFullName, RepoPath: repoPath, Ref: ref,
		HeadSHA: accepted, BaseSHA: liveBase, ForgejoHeadSHA: forgejoHead,
		ForgejoPRHeadSHA: forgejoPR.HeadSHA, ForgejoPRURL: forgejoPR.URL, ForgejoBaseSHA: liveBase,
	}, nil
}

func forgejoActionProjectionError(pr db.PullRequest, event forgejointegration.PullRequestActionLabelEvent, expected, actual, failureType, detail string) error {
	ref := "refs/heads/" + strings.TrimSpace(pr.HeadRef)
	return &forgejointegration.ProjectionError{
		Type: failureType, Repo: strings.TrimSpace(pr.Repository.FullName), TargetRepo: strings.TrimSpace(event.RepoFullName),
		Ref: ref, Branch: strings.TrimSpace(pr.HeadRef), ExpectedSHA: strings.TrimSpace(expected), ActualSHA: strings.TrimSpace(actual),
		ErrorSummary: fmt.Sprintf("%s; expected %s, observed %s", strings.TrimSpace(detail), firstNonEmpty(strings.TrimSpace(expected), "<unknown>"), firstNonEmpty(strings.TrimSpace(actual), "<unknown>")),
	}
}

func firstNonNil(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func exactGitSHA(a, b string) bool {
	return strings.TrimSpace(a) != "" && strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func (s *Service) forgejoActionBaseChanged(ctx context.Context, preflight forgejoActionRebasePreflight, headSHA string) (bool, string, error) {
	agsBase, err := s.Git.HeadSHA(ctx, preflight.RepoFullName, preflight.Projection.TargetBranch)
	if err != nil {
		return false, "", fmt.Errorf("read AGS base after rebase: %w", err)
	}
	forgejoBase, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, preflight.RepoFullName, preflight.RepoPath, preflight.Projection.TargetBranch)
	if err != nil || !checked {
		return false, "", fmt.Errorf("read Forgejo base after rebase: %w", firstNonNil(err, fmt.Errorf("live remote base inspection unavailable")))
	}
	if !exactGitSHA(agsBase, preflight.BaseSHA) || !exactGitSHA(forgejoBase, preflight.ForgejoBaseSHA) || !exactGitSHA(agsBase, forgejoBase) {
		return true, fmt.Sprintf("Base `%s` changed during the action window: preflight AGS/Forgejo `%s`, current AGS `%s`, current Forgejo `%s`.", preflight.Projection.TargetBranch, preflight.BaseSHA, agsBase, forgejoBase), nil
	}
	ancestor, err := gitCommitIsAncestor(ctx, preflight.RepoPath, agsBase, headSHA)
	if err != nil {
		return false, "", err
	}
	if !ancestor {
		return true, fmt.Sprintf("Base `%s` at `%s` is not an ancestor of rebased head `%s`.", preflight.Projection.TargetBranch, agsBase, headSHA), nil
	}
	return false, "", nil
}

func (s *Service) verifyForgejoActionConvergence(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, preflight forgejoActionRebasePreflight, expectedSHA string) error {
	agsBranch, err := s.Git.HeadSHA(ctx, preflight.RepoFullName, pr.HeadRef)
	if err != nil {
		return fmt.Errorf("read AGS branch during convergence verification: %w", err)
	}
	if !exactGitSHA(agsBranch, expectedSHA) {
		return forgejoActionProjectionError(pr, event, expectedSHA, agsBranch, forgejointegration.ProjectionFailureSHADrift, "AGS branch changed before convergence verification")
	}
	freshPR, err := s.GetPR(ctx, preflight.RepoFullName, pr.Number)
	if err != nil {
		return fmt.Errorf("read AGS PR during convergence verification: %w", err)
	}
	if !exactGitSHA(freshPR.HeadSHA, expectedSHA) {
		return forgejoActionProjectionError(pr, event, expectedSHA, freshPR.HeadSHA, forgejointegration.ProjectionFailureSHADrift, "AGS PR head did not converge")
	}
	forgejoBranch, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, preflight.RepoFullName, preflight.RepoPath, pr.HeadRef)
	if err != nil || !checked {
		return fmt.Errorf("read Forgejo branch during convergence verification: %w", firstNonNil(err, fmt.Errorf("live remote ref inspection unavailable")))
	}
	if !exactGitSHA(forgejoBranch, expectedSHA) {
		return forgejoActionProjectionError(pr, event, expectedSHA, forgejoBranch, forgejointegration.ProjectionFailureSHADrift, "Forgejo branch did not converge")
	}
	forgejoPR, found, err := s.ForgejoIntegration.PullRequestSnapshot(ctx, preflight.RepoFullName, preflight.Projection.ExternalRepo, event.PRNumber)
	if err != nil {
		return fmt.Errorf("read Forgejo PR during convergence verification: %w", err)
	}
	if !found || forgejoPR.HeadRef != pr.HeadRef || forgejoPR.BaseRef != pr.BaseRef || !exactGitSHA(forgejoPR.HeadSHA, expectedSHA) {
		actual := ""
		if found {
			actual = forgejoPR.HeadSHA
		}
		return forgejoActionProjectionError(pr, event, expectedSHA, actual, forgejointegration.ProjectionFailurePullRequestStateDrift, "Forgejo PR head did not converge")
	}
	projection, ok := s.forgejoProjectionForPullRequest(ctx, pr.ID)
	if !ok || projection.ExternalRepo != event.RepoFullName || projection.ExternalNumber != event.PRNumber || !exactGitSHA(projection.LastSyncedSHA, expectedSHA) {
		actual := ""
		if ok {
			actual = projection.LastSyncedSHA
		}
		return forgejoActionProjectionError(pr, event, expectedSHA, actual, forgejointegration.ProjectionFailureSHADrift, "durable projection row did not converge")
	}
	return nil
}

func (s *Service) rebaseAGSPrBranchFromForgejoLabel(ctx context.Context, pr db.PullRequest) (db.PullRequest, string, error) {
	repoFullName := pr.Repository.FullName
	if repoFullName == "" {
		repo, err := s.GetRepoByID(ctx, fmt.Sprint(pr.RepositoryID))
		if err != nil {
			return db.PullRequest{}, "", fmt.Errorf("lookup PR repository: %w", err)
		}
		pr.Repository = repo
		repoFullName = repo.FullName
	}
	headRepo := pr.HeadRepository
	if headRepo.ID == 0 {
		headRepo = pr.Repository
	}
	if headRepo.FullName == "" && headRepo.ID != 0 {
		repo, err := s.GetRepoByID(ctx, fmt.Sprint(headRepo.ID))
		if err != nil {
			return db.PullRequest{}, "", fmt.Errorf("lookup PR head repository: %w", err)
		}
		headRepo = repo
	}
	sha, err := s.UpdatePRBranch(ctx, gitstore.UpdatePRBranchOptions{
		FullName:     headRepo.FullName,
		BaseBranch:   pr.BaseRef,
		HeadBranch:   pr.HeadRef,
		Committer:    "ags-forgejo-bot",
		Email:        "ags-forgejo-bot@localhost",
		UpdateMethod: "rebase",
	})
	if err != nil {
		return db.PullRequest{}, "", err
	}
	if err := s.UpdatePRFields(ctx, pr.ID, map[string]any{"head_sha": sha}); err != nil {
		return db.PullRequest{}, "", fmt.Errorf("record rebased AGS PR head: %w", err)
	}
	// Git rebase already landed. Sibling open-PR refresh is derived state; keep
	// the action job as the durable owner instead of failing the rebase.
	if err := s.SyncOpenPRHeadsForBranch(ctx, headRepo.ID, headRepo.FullName, pr.HeadRef); err != nil {
		slog.ErrorContext(ctx, "refresh open PR heads after Forgejo rebase failed",
			"repo", headRepo.FullName, "branch", pr.HeadRef, "pr", pr.Number, "error", err)
	}
	updatedPR, err := s.GetPR(ctx, repoFullName, pr.Number)
	if err != nil {
		return db.PullRequest{}, "", err
	}
	return updatedPR, sha, nil
}

func gitCommitIsAncestor(ctx context.Context, repoPath, ancestorSHA, descendantSHA string) (bool, error) {
	ancestorSHA = strings.TrimSpace(ancestorSHA)
	descendantSHA = strings.TrimSpace(descendantSHA)
	if repoPath == "" || ancestorSHA == "" || descendantSHA == "" {
		return false, fmt.Errorf("missing repo path, ancestor, or descendant")
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "merge-base", "--is-ancestor", ancestorSHA, descendantSHA)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor failed: %w: %s", err, string(out))
}

func (s *Service) markForgejoActionBlocked(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, reason string) error {
	ctx = contextWithForgejoActionNotification(ctx)
	if err := s.clearForgejoWorkflowStatusLabels(ctx, event.RepoFullName, event.PRNumber); err != nil {
		return err
	}
	if err := s.addForgejoPullRequestLabels(ctx, event.RepoFullName, event.PRNumber, []string{forgejointegration.AGSStatusBlockedLabel}); err != nil {
		return err
	}
	comment := fmt.Sprintf("AGS rebase request was blocked: %s", strings.TrimSpace(reason))
	return s.createForgejoPullRequestComment(ctx, event.RepoFullName, event.PRNumber, comment)
}

func (s *Service) markForgejoActionFailed(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, err error) error {
	ctx = contextWithForgejoActionNotification(ctx)
	if clearErr := s.clearForgejoWorkflowStatusLabels(ctx, event.RepoFullName, event.PRNumber); clearErr != nil {
		return clearErr
	}
	status := forgejointegration.AGSStatusBlockedLabel
	if isLikelyRebaseConflict(err) {
		status = forgejointegration.AGSStatusRebaseConflictLabel
	}
	if addErr := s.addForgejoPullRequestLabels(ctx, event.RepoFullName, event.PRNumber, []string{status}); addErr != nil {
		return addErr
	}
	comment := fmt.Sprintf("AGS rebase request failed: %s", strings.TrimSpace(err.Error()))
	return s.createForgejoPullRequestComment(ctx, event.RepoFullName, event.PRNumber, comment)
}

func (s *Service) recordForgejoActionProjectionFailure(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, desiredSHA string, provider PullRequestProviderResult, actionPhase string) (forgejointegration.ProjectionError, error) {
	ref := "refs/heads/" + strings.TrimSpace(pr.HeadRef)
	failure := forgejointegration.ClassifyProjectionError(ref, desiredSHA, provider.Err)
	failure.Repo = strings.TrimSpace(pr.Repository.FullName)
	failure.TargetRepo = strings.TrimSpace(event.RepoFullName)
	failure.Ref = ref
	failure.Branch = strings.TrimSpace(pr.HeadRef)
	failure.ExpectedSHA = strings.TrimSpace(desiredSHA)
	failure.ActualSHA = firstNonEmpty(strings.TrimSpace(failure.ActualSHA), strings.TrimSpace(provider.ObservedSHA))
	if projection, ok := s.forgejoProjectionForPullRequest(ctx, pr.ID); ok {
		failure.TargetRepo = firstNonEmpty(failure.TargetRepo, projection.ExternalRepo)
	}
	metadata := ProjectionFailureAlertMetadata{
		AGSPRNumber:        pr.Number,
		AGSPRURL:           s.absAGSURL(fmt.Sprintf("/%s/pull/%d", failure.Repo, pr.Number)),
		ForgejoPRNumber:    event.PRNumber,
		ForgejoPRURL:       strings.TrimSpace(event.PRURL),
		Actor:              strings.TrimSpace(event.SenderLogin),
		ActionPhase:        strings.TrimSpace(actionPhase),
		AGSOldSHA:          strings.TrimSpace(event.HeadSHA),
		AGSNewSHA:          strings.TrimSpace(desiredSHA),
		ExpectedForgejoSHA: strings.TrimSpace(desiredSHA),
		ActualForgejoSHA:   strings.TrimSpace(failure.ActualSHA),
		CorrelationID:      strings.TrimSpace(event.CorrelationID),
		RecoveryHint:       "retry ags/action-rebase only after the Forgejo head is reconciled; never use an unconditional force push",
	}
	if err := s.RecordForgejoProjectionFailureWithAlert(ctx, failure.Repo, []ForgejoRefChange{{Ref: ref, After: desiredSHA}}, &failure, metadata); err != nil {
		if IsProjectionAlertingError(err) {
			slog.WarnContext(ctx, "Forgejo action projection alerting degraded", "repo", failure.Repo, "forgejo_repo", event.RepoFullName, "ags_pr", pr.Number, "forgejo_pr", event.PRNumber, "phase", actionPhase, "error", err)
			return failure, nil
		}
		return failure, err
	}
	return failure, nil
}

func (s *Service) markForgejoActionProjectionFailed(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, failure forgejointegration.ProjectionError, rebased bool) error {
	ctx = contextWithForgejoActionNotification(ctx)
	if err := s.clearForgejoWorkflowStatusLabels(ctx, event.RepoFullName, event.PRNumber); err != nil {
		return err
	}
	if addErr := s.addForgejoPullRequestLabels(ctx, event.RepoFullName, event.PRNumber, []string{forgejointegration.AGSStatusProjectionDriftLabel}); addErr != nil {
		return addErr
	}
	observed := "its head could not be independently observed"
	if observedSHA := strings.TrimSpace(failure.ActualSHA); observedSHA != "" {
		observed = fmt.Sprintf("its head was observed at `%s`", observedSHA)
	}
	authority := fmt.Sprintf("AGS PR #%d has been updated to `%s`", pr.Number, failure.ExpectedSHA)
	if !rebased {
		authority = fmt.Sprintf("AGS PR #%d remains at `%s`; rebase stopped during safety preflight", pr.Number, failure.ExpectedSHA)
	}
	comment := fmt.Sprintf("%s, but Forgejo PR #%d has not converged; %s. Required Forgejo projection failed (%s): %s", authority, event.PRNumber, observed, failure.Type, strings.TrimSpace(failure.ErrorSummary))
	return s.createForgejoPullRequestComment(ctx, event.RepoFullName, event.PRNumber, comment)
}

func (s *Service) markForgejoActionNeedsRebase(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest, sha, detail string) error {
	ctx = contextWithForgejoActionNotification(ctx)
	if err := s.clearForgejoWorkflowStatusLabels(ctx, event.RepoFullName, event.PRNumber); err != nil {
		return err
	}
	if addErr := s.addForgejoPullRequestLabels(ctx, event.RepoFullName, event.PRNumber, []string{forgejointegration.AGSStatusNeedsRebaseLabel}); addErr != nil {
		return addErr
	}
	comment := fmt.Sprintf("AGS rebase completed at `%s`, but the base branch moved before the result could be considered current. Trigger `%s` again after the latest base sync completes.", sha, forgejointegration.AGSActionRebaseLabel)
	if trimmed := strings.TrimSpace(detail); trimmed != "" {
		comment += " " + trimmed
	}
	return s.createForgejoPullRequestComment(ctx, event.RepoFullName, event.PRNumber, comment)
}

func (s *Service) handleForgejoStatusNotificationError(ctx context.Context, intentID string, jobID uint, err error, detail, sha string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		if terminalErr := s.terminalDenyForgejoAction(ctx, intentID, jobID, DelegatedSessionDenialReason(err), detail, sha); terminalErr != nil {
			return fmt.Errorf("terminalize Forgejo status-notification denial: %w (provider error: %v)", terminalErr, err)
		}
	}
	return err
}

func (s *Service) clearForgejoWorkflowStatusLabels(ctx context.Context, externalRepo string, prNumber int) error {
	for _, label := range forgejoWorkflowStatusLabels() {
		if err := s.removeForgejoPullRequestLabel(ctx, externalRepo, prNumber, label); err != nil {
			slog.WarnContext(ctx, "remove Forgejo PR status label failed", "forgejo_repo", externalRepo, "forgejo_pr", prNumber, "label", label, "error", err)
			return err
		}
	}
	return nil
}

func forgejoWorkflowStatusLabels() []string {
	return []string{
		forgejointegration.AGSStatusRebasingLabel,
		forgejointegration.AGSStatusProjectionDriftLabel,
		forgejointegration.AGSStatusRebaseConflictLabel,
		forgejointegration.AGSStatusBlockedLabel,
		forgejointegration.AGSStatusNeedsRebaseLabel,
	}
}

func isLikelyRebaseConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "conflict") || strings.Contains(msg, "could not apply") || strings.Contains(msg, "resolve all conflicts")
}

func (s *Service) handleForgejoDeletedBranch(ctx context.Context, event forgejointegration.DeletedBranchEvent) (ForgejoWebhookResult, error) {
	branch := strings.TrimSpace(event.BranchName)
	result := ForgejoWebhookResult{Handled: true, RepoFullName: event.RepoFullName, BranchDeleted: true, DeletedBranch: branch}
	if branch == "" {
		return result, nil
	}
	projection, pr, ok := s.findPullRequestByForgejoSourceBranch(ctx, event.RepoFullName, branch)
	if !ok {
		slog.InfoContext(ctx, "ignore Forgejo branch delete without AGS PR projection", "forgejo_repo", event.RepoFullName, "branch", branch)
		return result, nil
	}
	result.AGSPRNumber = pr.Number
	if pr.Repository.FullName != "" {
		result.RepoFullName = pr.Repository.FullName
	}
	if !sourceBranchDeletionAllowed(pr, branch) {
		slog.WarnContext(ctx, "skip Forgejo source branch cleanup because PR is not an eligible merged source branch", "repo", result.RepoFullName, "pr_number", pr.Number, "branch", branch, "merged", pr.Merged)
		return result, nil
	}
	openPRs, err := s.ListOpenPRsByHead(ctx, pr.RepositoryID, branch)
	if err != nil {
		return result, fmt.Errorf("check open PRs for source branch cleanup: %w", err)
	}
	for _, openPR := range openPRs {
		if openPR.ID != pr.ID {
			slog.WarnContext(ctx, "skip Forgejo source branch cleanup because another open PR uses the branch", "repo", result.RepoFullName, "branch", branch, "open_pr", openPR.Number)
			return result, nil
		}
	}
	if pr.Repository.FullName == "" {
		repo, err := s.GetRepoByID(ctx, fmt.Sprint(pr.RepositoryID))
		if err != nil {
			return result, fmt.Errorf("lookup PR repo for source branch cleanup: %w", err)
		}
		pr.Repository = repo
		result.RepoFullName = repo.FullName
	}
	ref := "refs/heads/" + branch
	if _, err := s.Git.HeadSHA(ctx, pr.Repository.FullName, branch); err == nil {
		if err := s.Git.DeleteRef(ctx, pr.Repository.FullName, ref); err != nil {
			return result, fmt.Errorf("delete AGS source branch %s: %w", branch, err)
		}
		result.AGSSourceBranchDeleted = true
	} else {
		slog.InfoContext(ctx, "AGS source branch already absent during Forgejo cleanup", "repo", pr.Repository.FullName, "branch", branch, "error", err)
	}
	if err := s.ResolveProjectionRefState(ctx, pr.Repository.FullName, ProjectionProviderForgejo, ref, "", "", time.Now().UTC()); err != nil {
		slog.WarnContext(ctx, "resolve Forgejo source branch projection state after cleanup failed", "repo", pr.Repository.FullName, "branch", branch, "error", err)
	}
	repoPath := ""
	if s.GitLabIntegration != nil || s.GitHubIntegration != nil {
		path, pathErr := s.Git.GetRepoPath(ctx, pr.Repository.FullName)
		if pathErr != nil {
			slog.WarnContext(ctx, "lookup AGS repo path for shadow source branch cleanup failed", "repo", pr.Repository.FullName, "branch", branch, "error", pathErr)
		} else {
			repoPath = path
		}
	}
	if s.GitLabIntegration != nil && repoPath != "" {
		if _, handled, err := s.GitLabIntegration.DeleteSourceBranch(ctx, gitlabintegration.DeleteSourceBranchRequest{RepoFullName: pr.Repository.FullName, RepoPath: repoPath, HeadBranch: branch}); err != nil {
			slog.WarnContext(ctx, "delete GitLab source branch failed", "repo", pr.Repository.FullName, "branch", branch, "error", err)
		} else {
			result.GitLabSourceBranchDeleted = handled
		}
	}
	if s.GitHubIntegration != nil && repoPath != "" {
		if _, handled, err := s.GitHubIntegration.DeleteSourceBranch(ctx, githubintegration.DeleteSourceBranchRequest{RepoFullName: pr.Repository.FullName, RepoPath: repoPath, HeadBranch: branch}); err != nil {
			slog.WarnContext(ctx, "delete GitHub source branch failed", "repo", pr.Repository.FullName, "branch", branch, "error", err)
		} else {
			result.GitHubSourceBranchDeleted = handled
		}
	}
	s.projectSourceBranchDeletedToMultica(ctx, pr, projection, result.GitLabSourceBranchDeleted, result.GitHubSourceBranchDeleted)
	slog.InfoContext(ctx, "processed Forgejo source branch delete", "repo", pr.Repository.FullName, "pr_number", pr.Number, "branch", branch, "ags_deleted", result.AGSSourceBranchDeleted, "gitlab_deleted", result.GitLabSourceBranchDeleted, "github_deleted", result.GitHubSourceBranchDeleted)
	return result, nil
}

func (s *Service) cleanupMergedSourceBranchIfForgejoDeleted(ctx context.Context, pr db.PullRequest, externalRepo, branch string) (ForgejoWebhookResult, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" || !sourceBranchDeletionAllowed(pr, branch) || s == nil || s.ForgejoIntegration == nil {
		return ForgejoWebhookResult{}, nil
	}
	if pr.Repository.FullName == "" {
		repo, err := s.GetRepoByID(ctx, fmt.Sprint(pr.RepositoryID))
		if err != nil {
			return ForgejoWebhookResult{}, fmt.Errorf("lookup PR repo for post-merge source branch cleanup: %w", err)
		}
		pr.Repository = repo
	}
	repoPath, err := s.Git.GetRepoPath(ctx, pr.Repository.FullName)
	if err != nil {
		return ForgejoWebhookResult{}, fmt.Errorf("lookup AGS repo path for post-merge source branch cleanup: %w", err)
	}
	remoteSHA, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, pr.Repository.FullName, repoPath, branch)
	if err != nil {
		return ForgejoWebhookResult{}, fmt.Errorf("check Forgejo source branch after merge: %w", err)
	}
	if !checked || strings.TrimSpace(remoteSHA) != "" {
		return ForgejoWebhookResult{}, nil
	}
	return s.handleForgejoDeletedBranch(ctx, forgejointegration.DeletedBranchEvent{RepoFullName: externalRepo, BranchName: branch})
}

func (s *Service) findPullRequestByForgejoSourceBranch(ctx context.Context, externalRepo, branch string) (db.PullRequestProjection, db.PullRequest, bool) {
	var rows []db.PullRequestProjection
	if err := s.DBForCtx(ctx).
		Preload("PullRequest").
		Preload("PullRequest.Repository").
		Preload("PullRequest.HeadRepository").
		Where("provider = ? AND external_repo = ? AND source_branch = ?", ProjectionProviderForgejo, strings.TrimSpace(externalRepo), strings.TrimSpace(branch)).
		Order("updated_at DESC, id DESC").
		Limit(1).
		Find(&rows).Error; err != nil {
		slog.WarnContext(ctx, "lookup AGS PR projection for Forgejo branch delete failed", "forgejo_repo", externalRepo, "branch", branch, "error", err)
		return db.PullRequestProjection{}, db.PullRequest{}, false
	}
	if len(rows) == 0 {
		return db.PullRequestProjection{}, db.PullRequest{}, false
	}
	return rows[0], rows[0].PullRequest, true
}

func sourceBranchDeletionAllowed(pr db.PullRequest, branch string) bool {
	branch = strings.TrimSpace(branch)
	if !pr.Merged || branch == "" || strings.HasPrefix(branch, "refs/") {
		return false
	}
	if branch != strings.TrimSpace(pr.HeadRef) {
		return false
	}
	base := strings.TrimSpace(pr.BaseRef)
	if branch == base || branch == "main" || branch == "master" {
		return false
	}
	return true
}

func (s *Service) projectSourceBranchDeletedToMultica(ctx context.Context, pr db.PullRequest, projection db.PullRequestProjection, gitLabDeleted, gitHubDeleted bool) {
	if s == nil || s.MulticaProjection == nil {
		return
	}
	issueRef := s.multicaIssueRefForPR(ctx, pr.Repository.FullName, pr)
	if issueRef.IssueKey == "" {
		return
	}
	req := multicaprojection.Request{
		IssueKey:              issueRef.IssueKey,
		Workspace:             issueRef.Workspace,
		WorkspaceID:           issueRef.WorkspaceID,
		RepoFullName:          pr.Repository.FullName,
		AGSPRNumber:           pr.Number,
		AGSPRURL:              s.absAGSURL(fmt.Sprintf("/%s/pull/%d", pr.Repository.FullName, pr.Number)),
		HeadBranch:            pr.HeadRef,
		HeadSHA:               pr.HeadSHA,
		ForgejoNumber:         projection.ExternalNumber,
		ForgejoURL:            projection.ExternalURL,
		CIState:               "passed",
		MergeState:            "merged",
		MergedSHA:             pr.MergeCommitSHA,
		SourceBranchState:     "deleted",
		SourceBranchDeletedAt: time.Now().Format(time.RFC3339),
		MulticaURL:            issueRef.URL,
	}
	if gitLabDeleted {
		req.GitLabSourceBranchState = "deleted"
	}
	if gitHubDeleted {
		req.GitHubSourceBranchState = "deleted"
	}
	s.addGitLabProjectionToMulticaRequest(ctx, pr.ID, &req)
	s.addGitHubProjectionToMulticaRequest(ctx, pr.ID, &req)
	s.dispatchMulticaProjection(ctx, "Forgejo source branch deleted", req)
}

func (s *Service) pushForgejoMergedBranchToGitLabBackup(ctx context.Context, event forgejointegration.MergedPullRequestEvent, sha string) (gitlabintegration.PushResult, bool, error) {
	if s == nil || s.GitLabIntegration == nil {
		return gitlabintegration.PushResult{}, false, nil
	}
	repoPath, err := s.Git.GetRepoPath(ctx, event.RepoFullName)
	if err != nil {
		return gitlabintegration.PushResult{}, false, fmt.Errorf("lookup AGS repo path for GitLab backup: %w", err)
	}
	return s.GitLabIntegration.PushBackup(ctx, gitlabintegration.PushRequest{
		RepoFullName: event.RepoFullName,
		RepoPath:     repoPath,
		SourceSHA:    sha,
		BaseBranch:   event.BaseBranch,
	})
}

func (s *Service) pushForgejoMergedBranchToGitHubBackup(ctx context.Context, event forgejointegration.MergedPullRequestEvent, sha string) (githubintegration.PushResult, bool, error) {
	if s == nil || s.GitHubIntegration == nil {
		return githubintegration.PushResult{}, false, nil
	}
	repoPath, err := s.Git.GetRepoPath(ctx, event.RepoFullName)
	if err != nil {
		return githubintegration.PushResult{}, false, fmt.Errorf("lookup AGS repo path for GitHub backup: %w", err)
	}
	return s.GitHubIntegration.PushBackup(ctx, githubintegration.PushRequest{
		RepoFullName: event.RepoFullName,
		RepoPath:     repoPath,
		SourceSHA:    sha,
		BaseBranch:   event.BaseBranch,
	})
}

func (s *Service) closeGitLabShadowMergeRequest(ctx context.Context, event forgejointegration.MergedPullRequestEvent, sha string, pr db.PullRequest) (gitlabintegration.CloseShadowMergeRequestResult, bool, error) {
	if s == nil || s.GitLabIntegration == nil {
		return gitlabintegration.CloseShadowMergeRequestResult{}, false, nil
	}
	result, handled, err := s.GitLabIntegration.CloseShadowMergeRequest(ctx, gitlabintegration.CloseShadowMergeRequestRequest{
		RepoFullName: event.RepoFullName,
		HeadBranch:   event.HeadBranch,
		BaseBranch:   event.BaseBranch,
		PRNumber:     event.PRNumber,
		MergedSHA:    sha,
	})
	if err != nil {
		return result, handled, err
	}
	// CloseShadowMergeRequest returning handled=false is the ancestry-merged
	// case (no open GitLab MR). AGS still records the projection as closed
	// with the Forgejo merged SHA; GitLab is never merge authority.
	if recordErr := s.recordGitLabShadowProjectionClosed(ctx, pr, result, sha); recordErr != nil {
		slog.WarnContext(ctx, "record GitLab shadow projection closed failed", "repo", event.RepoFullName, "ags_pr", pr.Number, "sha", sha, "error", recordErr)
	}
	return result, handled, nil
}

func (s *Service) closeGitHubShadowPullRequest(ctx context.Context, event forgejointegration.MergedPullRequestEvent, sha string, pr db.PullRequest) (githubintegration.CloseShadowPullRequestResult, bool, error) {
	if s == nil || s.GitHubIntegration == nil {
		return githubintegration.CloseShadowPullRequestResult{}, false, nil
	}
	result, handled, err := s.GitHubIntegration.CloseShadowPullRequest(ctx, githubintegration.CloseShadowPullRequestRequest{
		RepoFullName: event.RepoFullName,
		HeadBranch:   event.HeadBranch,
		BaseBranch:   event.BaseBranch,
		PRNumber:     event.PRNumber,
		MergedSHA:    sha,
	})
	if err != nil {
		return result, handled, err
	}
	// CloseShadowPullRequest returning handled=false is the ancestry-merged
	// case (no open GitHub PR). AGS still records the projection as closed
	// with the Forgejo merged SHA; GitHub is never merge authority.
	if recordErr := s.recordGitHubShadowProjectionClosed(ctx, pr, result, sha); recordErr != nil {
		slog.WarnContext(ctx, "record GitHub shadow projection closed failed", "repo", event.RepoFullName, "ags_pr", pr.Number, "sha", sha, "error", recordErr)
	}
	return result, handled, nil
}

func forgejoWebhookSignatureFromHeaders(headers http.Header) string {
	if signature := strings.TrimSpace(headers.Get("X-Hub-Signature-256")); signature != "" {
		return signature
	}
	return strings.TrimSpace(headers.Get("X-Hub-Signature"))
}

func gitLSRemoteSHAForRef(output, ref string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ref {
			return fields[0], true
		}
	}
	return "", false
}

func (s *Service) syncForgejoMergedBranchToAGS(ctx context.Context, event forgejointegration.MergedPullRequestEvent) (string, error) {
	if s.Git != nil {
		mutationCtx, release, err := s.Git.BeginMutation(ctx)
		if err != nil {
			return "", err
		}
		defer release()
		ctx = mutationCtx
	}
	if event.RepoFullName == "" || event.BaseBranch == "" {
		return "", fmt.Errorf("forgejo webhook missing repo or base branch")
	}
	repoPath, err := s.Git.GetRepoPath(ctx, event.RepoFullName)
	if err != nil {
		return "", fmt.Errorf("lookup AGS repo path: %w", err)
	}
	remoteURL, err := s.ForgejoIntegration.RemoteURLForRepo(event.RepoFullName)
	if err != nil {
		return "", err
	}
	ref := "refs/heads/" + event.BaseBranch
	currentOut, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", "--verify", ref).Output()
	if err != nil {
		return "", fmt.Errorf("read current AGS base branch SHA: %w", err)
	}
	currentSHA := strings.TrimSpace(string(currentOut))
	remoteOut, err := gittransport.Run(ctx, remoteURL, "", "-C", repoPath, "ls-remote", "--heads", remoteURL, ref)
	if err != nil {
		return "", fmt.Errorf("read Forgejo merged branch: %w", err)
	}
	sha, ok := gitLSRemoteSHAForRef(string(remoteOut), ref)
	if !ok {
		return "", fmt.Errorf("forgejo merged base branch %s is missing", event.BaseBranch)
	}
	if _, err := gittransport.Run(ctx, remoteURL, "", "-C", repoPath, "fetch", "--no-tags", remoteURL, sha); err != nil {
		return "", fmt.Errorf("fetch Forgejo merged commit: %w", err)
	}
	fastForward, err := gitCommitIsAncestor(ctx, repoPath, currentSHA, sha)
	if err != nil {
		return "", fmt.Errorf("validate Forgejo merged base update: %w", err)
	}
	if !fastForward {
		return "", fmt.Errorf("reject non-fast-forward Forgejo base update for %s: AGS %s is not an ancestor of Forgejo %s", event.BaseBranch, currentSHA, sha)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "update-ref", ref, sha, currentSHA).CombinedOutput(); err != nil {
		return "", fmt.Errorf("advance AGS base branch atomically: %w: %s", err, string(out))
	}
	repo, err := s.GetRepo(ctx, event.RepoFullName)
	if err != nil {
		return "", fmt.Errorf("lookup AGS repo after merged base advance: %w", err)
	}
	if updateErr := s.UpdateRepositoryPushedAt(ctx, repo.ID, time.Now()); updateErr != nil {
		slog.WarnContext(ctx, "forgejo webhook pushed_at update failed", "repo", event.RepoFullName, "error", updateErr)
	}
	if err := s.SyncWorkflowsFromRepo(ctx, event.RepoFullName); err != nil {
		slog.WarnContext(ctx, "forgejo webhook workflow sync failed", "repo", event.RepoFullName, "error", err)
	}
	// Base tip advanced via update-ref (not receive-pack). Refresh open PRs whose
	// head_ref is this base so stacked PR rows converge with the live tip.
	if err := s.SyncOpenPRHeadsForBranch(ctx, repo.ID, event.RepoFullName, event.BaseBranch); err != nil {
		return "", fmt.Errorf("refresh open PR heads after merged base advance: %w", err)
	}
	return sha, nil
}
