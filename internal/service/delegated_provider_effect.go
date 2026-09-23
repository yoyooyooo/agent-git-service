package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

type forgejoActionEffectBindingContextKey struct{}

type forgejoActionEffectBinding struct {
	IntentID                  string
	JobID                     uint
	ActionGeneration          uint
	AllowTerminalNotification bool
}

func contextWithForgejoActionIntent(ctx context.Context, intentID string) context.Context {
	binding := forgejoActionEffectBinding{IntentID: strings.TrimSpace(intentID)}
	return context.WithValue(ctx, forgejoActionEffectBindingContextKey{}, binding)
}

func contextWithForgejoActionJob(ctx context.Context, job db.PullRequestProjectionJob) context.Context {
	intentID := ""
	if job.ActionIntentID != nil {
		intentID = strings.TrimSpace(*job.ActionIntentID)
	}
	binding := forgejoActionEffectBinding{IntentID: intentID, JobID: job.ID, ActionGeneration: job.ActionGeneration}
	return context.WithValue(ctx, forgejoActionEffectBindingContextKey{}, binding)
}

func contextWithForgejoActionNotification(ctx context.Context) context.Context {
	binding, ok := forgejoActionBindingFromContext(ctx)
	if !ok {
		return ctx
	}
	binding.AllowTerminalNotification = true
	return context.WithValue(ctx, forgejoActionEffectBindingContextKey{}, binding)
}

func forgejoActionBindingFromContext(ctx context.Context) (forgejoActionEffectBinding, bool) {
	binding, ok := ctx.Value(forgejoActionEffectBindingContextKey{}).(forgejoActionEffectBinding)
	return binding, ok && binding.IntentID != ""
}

func sameActionIntentJobBinding(job db.PullRequestProjectionJob, binding forgejoActionEffectBinding) bool {
	return binding.JobID != 0 && job.ID == binding.JobID && job.ActionGeneration == binding.ActionGeneration &&
		job.ActionIntentID != nil && strings.TrimSpace(*job.ActionIntentID) == binding.IntentID
}

func exactActionHeadAllowed(actual, expected, projected string) bool {
	return exactGitSHA(actual, expected) || (strings.TrimSpace(projected) != "" && exactGitSHA(actual, projected))
}

// validateForgejoActionLiveFacts reloads every mutable AGS/provider coordinate
// immediately before an exact action effect. Before AGS rebasing, all heads must
// equal the intent's expected head. Once the bound job has durably recorded its
// exact rebased head, AGS must equal that generation and Forgejo may only be at
// the old or exact new head while the provider transaction converges.
func (s *Service) validateForgejoActionLiveFacts(ctx context.Context, intent db.PullRequestActionIntent, job *db.PullRequestProjectionJob) error {
	if s == nil || s.Git == nil || s.ForgejoIntegration == nil {
		return fmt.Errorf("exact action live-fact readers are unavailable")
	}
	var pr db.PullRequest
	if err := s.DBForCtx(ctx).Preload("Repository").First(&pr, intent.PullRequestID).Error; err != nil {
		return fmt.Errorf("reload exact action pull request: %w", err)
	}
	if pr.RepositoryID != intent.RepositoryID || pr.Number != intent.AGSPRNumber || pr.Repository.FullName != intent.Repository ||
		pr.State != db.StateOpen || pr.Merged || pr.HeadRef != intent.HeadRef || pr.BaseRef != intent.BaseRef {
		return fmt.Errorf("exact action AGS coordinate drift")
	}
	var projection db.PullRequestProjection
	if err := s.DBForCtx(ctx).Where("pull_request_id = ? AND provider = ?", intent.PullRequestID, ProjectionProviderForgejo).First(&projection).Error; err != nil {
		return fmt.Errorf("reload exact action projection: %w", err)
	}
	if projection.ExternalRepo != intent.ForgejoRepo || projection.ExternalNumber != intent.ForgejoPRNumber ||
		projection.SourceBranch != intent.HeadRef || projection.TargetBranch != intent.BaseRef || projection.State != ProjectionStateOpen {
		return fmt.Errorf("exact action projection coordinate drift")
	}
	repoPath, err := s.Git.GetRepoPath(ctx, intent.Repository)
	if err != nil {
		return fmt.Errorf("resolve exact action repository: %w", err)
	}
	expectedAGSHead := intent.ExpectedHeadSHA
	projectedHead := ""
	if job != nil && strings.TrimSpace(job.DesiredAGSHeadSHA) != "" {
		projectedHead = strings.TrimSpace(job.DesiredAGSHeadSHA)
		expectedAGSHead = projectedHead
	}
	if !exactActionHeadAllowed(projection.LastSyncedSHA, intent.ExpectedHeadSHA, projectedHead) {
		return fmt.Errorf("exact action projection head drift")
	}
	headSHA, err := s.Git.HeadSHA(ctx, intent.Repository, intent.HeadRef)
	if err != nil {
		return fmt.Errorf("read exact action AGS head: %w", err)
	}
	baseSHA, err := s.Git.HeadSHA(ctx, intent.Repository, intent.BaseRef)
	if err != nil {
		return fmt.Errorf("read exact action AGS base: %w", err)
	}
	if !exactGitSHA(pr.HeadSHA, expectedAGSHead) || !exactGitSHA(headSHA, expectedAGSHead) {
		return fmt.Errorf("exact action AGS head/base drift")
	}
	forgejoHead, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, intent.Repository, repoPath, intent.HeadRef)
	if err != nil || !checked {
		return fmt.Errorf("read exact action Forgejo head: %w", firstNonNil(err, fmt.Errorf("live remote ref inspection unavailable")))
	}
	forgejoBase, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, intent.Repository, repoPath, intent.BaseRef)
	if err != nil || !checked {
		return fmt.Errorf("read exact action Forgejo base: %w", firstNonNil(err, fmt.Errorf("live remote base inspection unavailable")))
	}
	forgejoPR, found, err := s.ForgejoIntegration.ExactPullRequestSnapshot(ctx, intent.Repository, intent.ForgejoRepo, intent.ForgejoPRNumber)
	if err != nil || !found {
		return fmt.Errorf("read exact action Forgejo pull request: %w", firstNonNil(err, fmt.Errorf("live pull request unavailable")))
	}
	if forgejoPR.Number != intent.ForgejoPRNumber || forgejoPR.State != "open" || forgejoPR.HeadRef != intent.HeadRef || forgejoPR.BaseRef != intent.BaseRef ||
		!exactActionHeadAllowed(forgejoHead, intent.ExpectedHeadSHA, projectedHead) ||
		!exactActionHeadAllowed(forgejoPR.HeadSHA, intent.ExpectedHeadSHA, projectedHead) || !exactGitSHA(forgejoHead, forgejoPR.HeadSHA) {
		return fmt.Errorf("exact action Forgejo head/base drift")
	}
	if _, err := agreeLiveRebaseBase(ctx, repoPath, intent.ExpectedBaseSHA, baseSHA, forgejoBase); err != nil {
		return fmt.Errorf("exact action Forgejo head/base drift")
	}
	return nil
}

// revalidateCurrentDelegatedProviderWrite is the final exact action-intent and
// principal-authority check for every rebase provider effect. The action binding
// is mandatory for durable and delegated callers: an old job generation must
// never use a newer intent for the same PR. Every seam reloads the originating
// principal/repository authority and exact live facts before the external write.
func (s *Service) revalidateCurrentDelegatedProviderWrite(ctx context.Context, forgejoRepo string, forgejoPRNumber int) error {
	binding, bound := forgejoActionBindingFromContext(ctx)
	forgejoRepo = strings.TrimSpace(forgejoRepo)
	if !bound || forgejoRepo == "" || forgejoPRNumber <= 0 {
		return fmt.Errorf("missing exact action effect binding: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
	}

	database := s.DBForCtx(ctx)
	var intent db.PullRequestActionIntent
	if err := database.First(&intent, "id = ?", binding.IntentID).Error; err != nil {
		return fmt.Errorf("exact action intent unavailable: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
	}
	activeStates := forgejoActionActiveStates()
	if intent.Action != "pr.rebase" || intent.ForgejoRepo != forgejoRepo || intent.ForgejoPRNumber != forgejoPRNumber {
		return fmt.Errorf("exact action intent coordinate mismatch: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
	}
	var boundJob *db.PullRequestProjectionJob
	if binding.JobID != 0 {
		var job db.PullRequestProjectionJob
		if err := database.First(&job, "id = ?", binding.JobID).Error; err != nil || !sameActionIntentJobBinding(job, binding) ||
			job.Trigger != ForgejoProjectionTriggerActionRebase || job.PullRequestID != intent.PullRequestID ||
			job.RepositoryID != intent.RepositoryID || !sameOptionalString(job.AgentSessionID, intent.AgentSessionID) {
			return fmt.Errorf("exact action job generation mismatch: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
		}
		boundJob = &job
	}
	stateAllowed := stringInSlice(intent.State, activeStates)
	if binding.AllowTerminalNotification && stringInSlice(intent.State, []string{ForgejoActionIntentDenied, ForgejoActionIntentConflict, ForgejoActionIntentProjection}) {
		stateAllowed = true
	}
	if intent.State == ForgejoActionIntentCompleted && boundJob != nil {
		// Exact completed generations may replay provider readback/recovery
		// idempotently, but can never authorize another intent or generation.
		stateAllowed = isFullHexRevision(intent.ResultSHA) &&
			(exactGitSHA(intent.ResultSHA, boundJob.DesiredAGSHeadSHA) || exactGitSHA(intent.ResultSHA, boundJob.HeadSHA))
	}
	if !stateAllowed {
		return fmt.Errorf("exact action intent state mismatch (%s): %w", intent.State, delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
	}
	if !intent.ExpiresAt.After(time.Now().UTC()) {
		denial := delegatedUseTimeDenied(DelegatedDenialConstraintMismatch)
		_ = s.terminalDenyForgejoAction(ctx, intent.ID, binding.JobID, "action_intent_expired", "delegated action intent expired before provider effect", "")
		return denial
	}
	if intent.AgentSessionID == nil {
		if err := s.authorizeCurrentDurableActionIntent(ctx, intent); err != nil {
			_ = s.terminalDenyForgejoAction(ctx, intent.ID, binding.JobID, DelegatedDenialAuthoritySnapshotChanged, "durable principal authority changed before provider effect", "")
			return err
		}
	} else {
		sessionID, delegated := DelegatedSessionIDFromContext(ctx)
		if !delegated || sessionID == "" || sessionID != *intent.AgentSessionID {
			return delegatedUseTimeDenied(DelegatedDenialSessionMissing)
		}
		if _, err := s.validateDelegatedEffectBoundaryForAction(ctx, intent); err != nil {
			return fmt.Errorf("exact action Boundary Receipt unavailable or corrupt: %w", delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged))
		}
		var coordinate db.DelegatedAgentSession
		if err := database.Select("id", "repository_id", "principal_user_id").First(&coordinate, "id = ?", sessionID).Error; err != nil ||
			coordinate.RepositoryID != intent.RepositoryID || coordinate.PrincipalUserID != intent.PrincipalID {
			return delegatedUseTimeDenied(DelegatedDenialSessionMissing)
		}
		if _, err := s.RevalidateDelegatedSession(ctx, coordinate.RepositoryID, "pr.rebase", "repo:write", forgejoRebaseSessionConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA)); err != nil {
			return err
		}
	}
	if s.testDelegatedProviderWrite != nil {
		if err := s.testDelegatedProviderWrite(); err != nil {
			return err
		}
	}
	if err := s.validateForgejoActionLiveFacts(ctx, intent, boundJob); err != nil {
		denial := delegatedUseTimeDenied(DelegatedDenialConstraintMismatch)
		_ = s.terminalDenyForgejoAction(ctx, intent.ID, binding.JobID, "exact_action_fact_drift", "live AGS or Forgejo facts changed before provider effect", "")
		return fmt.Errorf("%v: %w", err, denial)
	}
	return nil
}

func (s *Service) terminalizeBoundActionProviderDenial(ctx context.Context, err error, detail, sha string) error {
	if err == nil || !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		return err
	}
	binding, bound := forgejoActionBindingFromContext(ctx)
	if !bound || binding.JobID == 0 || binding.ActionGeneration == 0 {
		return fmt.Errorf("post-job provider denial has no exact action binding: %w", err)
	}
	if terminalErr := s.terminalDenyForgejoAction(ctx, binding.IntentID, binding.JobID, DelegatedSessionDenialReason(err), detail, sha); terminalErr != nil {
		return fmt.Errorf("terminalize exact action denial: %w (provider denial: %v)", terminalErr, err)
	}
	return err
}

func (s *Service) revalidateBoundActionProviderWrite(ctx context.Context) error {
	binding, bound := forgejoActionBindingFromContext(ctx)
	if !bound || binding.JobID == 0 || binding.ActionGeneration == 0 {
		return fmt.Errorf("missing exact action effect binding: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
	}
	var intent db.PullRequestActionIntent
	if err := s.DBForCtx(ctx).Select("id", "forgejo_repo", "forgejo_pr_number").First(&intent, "id = ?", binding.IntentID).Error; err != nil {
		return fmt.Errorf("exact action intent unavailable: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
	}
	return s.revalidateCurrentDelegatedProviderWrite(ctx, intent.ForgejoRepo, intent.ForgejoPRNumber)
}

func (s *Service) addForgejoPullRequestLabels(ctx context.Context, repo string, number int, labels []string) error {
	if err := s.revalidateCurrentDelegatedProviderWrite(ctx, repo, number); err != nil {
		return err
	}
	return s.ForgejoIntegration.AddPullRequestLabels(ctx, repo, number, labels)
}

func (s *Service) removeForgejoPullRequestLabel(ctx context.Context, repo string, number int, label string) error {
	if err := s.revalidateCurrentDelegatedProviderWrite(ctx, repo, number); err != nil {
		return err
	}
	return s.ForgejoIntegration.RemovePullRequestLabel(ctx, repo, number, label)
}

// cleanupInvalidForgejoActionTrigger is a server-owned cleanup capability for
// one already signature-verified webhook that contains the reserved trigger but
// has no AGS action intent. It cannot add labels, create an intent/job, or run a
// rebase, and is deliberately separate from the exact delegated effect wrapper.
func (s *Service) cleanupInvalidForgejoActionTrigger(ctx context.Context, repo string, number int, label string) error {
	if s == nil || s.ForgejoIntegration == nil || strings.TrimSpace(repo) == "" || number <= 0 || strings.TrimSpace(label) != forgejointegration.AGSActionRebaseLabel {
		return fmt.Errorf("invalid Forgejo action-trigger cleanup coordinate")
	}
	return s.ForgejoIntegration.RemovePullRequestLabel(ctx, strings.TrimSpace(repo), number, forgejointegration.AGSActionRebaseLabel)
}

// publishInvalidForgejoActionStatus is an idempotent server-owned denial
// projection for one signed reserved-label event. It cannot create an intent,
// job or Git effect; it only converges workflow status labels to blocked.
func (s *Service) publishInvalidForgejoActionStatus(ctx context.Context, repo string, number int) error {
	if s == nil || s.ForgejoIntegration == nil || strings.TrimSpace(repo) == "" || number <= 0 {
		return fmt.Errorf("invalid Forgejo action-trigger status coordinate")
	}
	repo = strings.TrimSpace(repo)
	for _, label := range []string{
		forgejointegration.AGSStatusRebasingLabel,
		forgejointegration.AGSStatusProjectionDriftLabel,
		forgejointegration.AGSStatusRebaseConflictLabel,
		forgejointegration.AGSStatusNeedsRebaseLabel,
	} {
		if err := s.ForgejoIntegration.RemovePullRequestLabel(ctx, repo, number, label); err != nil {
			return err
		}
	}
	return s.ForgejoIntegration.AddPullRequestLabels(ctx, repo, number, []string{forgejointegration.AGSStatusBlockedLabel})
}

// acknowledgeForgejoActionRequest is server-owned cleanup after an already
// admitted label request fails exact preflight. It must not require the same
// live SHA snapshot that just failed, or the action label stays stuck.
func (s *Service) acknowledgeForgejoActionRequest(ctx context.Context, repo string, number int, comment string) error {
	if err := s.cleanupInvalidForgejoActionTrigger(ctx, repo, number, forgejointegration.AGSActionRebaseLabel); err != nil {
		return err
	}
	if err := s.publishInvalidForgejoActionStatus(ctx, repo, number); err != nil {
		return err
	}
	if strings.TrimSpace(comment) == "" || s.ForgejoIntegration == nil {
		return nil
	}
	return s.ForgejoIntegration.CreatePullRequestComment(ctx, strings.TrimSpace(repo), number, strings.TrimSpace(comment))
}

func (s *Service) createForgejoPullRequestComment(ctx context.Context, repo string, number int, body string) error {
	if err := s.revalidateCurrentDelegatedProviderWrite(ctx, repo, number); err != nil {
		return err
	}
	return s.ForgejoIntegration.CreatePullRequestComment(ctx, repo, number, body)
}
