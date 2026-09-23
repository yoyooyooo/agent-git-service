package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

type accessGrantPRMergePreflightFailure struct {
	code string
	err  error
}

func (e *accessGrantPRMergePreflightFailure) Error() string { return e.err.Error() }
func (e *accessGrantPRMergePreflightFailure) Unwrap() error { return e.err }

func failAccessGrantPRMergePreflight(result accessGrantPRMergePreflight, code string, err error) (accessGrantPRMergePreflight, error) {
	return result, &accessGrantPRMergePreflightFailure{code: code, err: err}
}

func accessGrantPRMergePreflightDenialCode(err error) string {
	var failure *accessGrantPRMergePreflightFailure
	if errors.As(err, &failure) {
		return failure.code
	}
	return "pre_dispatch_fact_drift"
}

// accessGrantPRMergePreflight re-evaluates every mutable use-time fact before
// the durable dispatch CAS. Actor/executor authority, exact AGS and provider PR
// coordinates, base/head, CI, review/mergeability, and protected provider
// authority all fail closed here. Partial exact facts are retained so a
// definitive pre-dispatch conflict can be returned and GET-read as a terminal
// provider_attempt=not_attempted receipt.
func (s *Service) accessGrantPRMergePreflight(ctx context.Context, grant db.AccessGrant, constraints map[string]string) (accessGrantPRMergePreflight, error) {
	result := accessGrantPRMergePreflight{}
	if s == nil || s.ForgejoIntegration == nil || s.Git == nil {
		return failAccessGrantPRMergePreflight(result, "merge_executor_unavailable", fmt.Errorf("%w: pr.merge executor is unavailable", ErrAccessGrantDenied))
	}
	_, executor, repository, err := s.revalidateAccessGrant(ctx, grant)
	if err != nil {
		return failAccessGrantPRMergePreflight(result, "grant_authority_drift", err)
	}
	agsPRNumber, err := strconv.Atoi(constraints["pull_request_number"])
	if err != nil || agsPRNumber <= 0 {
		return failAccessGrantPRMergePreflight(result, "ags_pr_invalid", fmt.Errorf("%w: AGS pull request is invalid", ErrValidation))
	}
	providerPRNumber, err := strconv.Atoi(constraints["forgejo_pull_request_number"])
	if err != nil || providerPRNumber <= 0 {
		return failAccessGrantPRMergePreflight(result, "provider_pr_invalid", fmt.Errorf("%w: provider pull request is invalid", ErrValidation))
	}

	var pr db.PullRequest
	if err := s.DBForCtx(ctx).Select(
		"id", "number", "repository_id", "title", "body", "state", "merged", "draft",
		"head_ref", "head_sha", "base_ref", "base_sha",
	).First(&pr, "repository_id = ? AND number = ?", repository.ID, agsPRNumber).Error; err != nil {
		return failAccessGrantPRMergePreflight(result, "ags_pr_unavailable", fmt.Errorf("%w: AGS pull request is unavailable", ErrAccessGrantConflict))
	}
	pr.Repository = repository
	result.PR = pr
	result.BaseRef = pr.BaseRef
	result.BaseSHA = pr.BaseSHA
	if pr.State != db.StateOpen || pr.Merged || pr.Draft || !exactGitSHA(pr.HeadSHA, constraints["expected_head_sha"]) {
		return failAccessGrantPRMergePreflight(result, "ags_pr_head_state_drift", fmt.Errorf("%w: AGS pull request head/state drift", ErrAccessGrantConflict))
	}
	if err := s.enforceMergePolicy(ctx, executor, &pr); err != nil {
		return failAccessGrantPRMergePreflight(result, "ags_merge_policy_denied", err)
	}

	projection, err := s.forgejoEvidenceBinding(ctx, pr)
	if err != nil || projection.Kind != ProjectionProviderForgejo || projection.ExternalNumber != providerPRNumber {
		return failAccessGrantPRMergePreflight(result, "provider_projection_unavailable", fmt.Errorf("%w: exact provider projection is unavailable", ErrAccessGrantConflict))
	}
	result.ProviderRepo = projection.ExternalRepo
	binding, err := s.ForgejoIntegration.ProviderMergeBindingForBase(repository.FullName, repository.ID, grant.TargetInstance, pr.BaseRef)
	if err != nil || binding.CanonicalRepository != repository.FullName || binding.TargetInstance != grant.TargetInstance ||
		binding.ProviderRepository != projection.ExternalRepo {
		return failAccessGrantPRMergePreflight(result, "provider_merge_binding_unavailable", fmt.Errorf("%w: provider merge binding is unavailable", ErrAccessGrantConflict))
	}
	result.Binding = binding
	if binding.BaseRef != pr.BaseRef || binding.MergeMethod != constraints["merge_method"] {
		return failAccessGrantPRMergePreflight(result, "provider_merge_method_base_drift", fmt.Errorf("%w: provider merge method/base drift", ErrAccessGrantConflict))
	}

	sides, err := s.loadProviderMergeSides(ctx, repository.FullName, pr.BaseRef, pr.HeadSHA)
	if err != nil {
		return failAccessGrantPRMergePreflight(result, "ags_current_base_unavailable", fmt.Errorf("%w: current AGS or Forgejo base is unavailable", ErrAccessGrantConflict))
	}
	result.BaseSHA = sides.AGSBase
	result.ProviderBaseSHA = sides.ProviderBase
	ffAck := s.fastForwardAckEligible(repository.FullName, constraints["merge_method"], sides.ProviderBase, constraints["expected_head_sha"])
	if !ffAck {
		headContainsBase, err := s.Git.IsAncestor(ctx, repository.FullName, sides.AGSBase, pr.HeadSHA)
		if err != nil || !headContainsBase {
			return failAccessGrantPRMergePreflight(result, "ags_head_does_not_contain_current_base", fmt.Errorf("%w: AGS pull request head does not contain current base", ErrAccessGrantConflict))
		}
		if _, err := s.providerMergePreflight(ctx, repository.FullName, binding.ProviderRepository,
			providerPRNumber, constraints["expected_head_sha"], pr.BaseRef, sides.AGSBase, sides.ProviderBase); err != nil {
			return failAccessGrantPRMergePreflight(result, "provider_pr_drift", fmt.Errorf("%w: exact provider pull request drift", ErrAccessGrantConflict))
		}
	}
	if _, err := s.ForgejoIntegration.VerifyPullRequestMergeAuthority(ctx, repository.FullName, pr.BaseRef); err != nil {
		return failAccessGrantPRMergePreflight(result, "provider_merge_authority_unavailable", fmt.Errorf("%w: protected provider merge authority is unavailable", ErrAccessGrantDenied))
	}
	if s.accessGrantRequiresCI(repository.FullName) {
		runs, supported, err := s.ForgejoIntegration.WorkflowRunsForPullRequest(
			ctx, repository.FullName, binding.ProviderRepository, providerPRNumber, pr.HeadRef, constraints["expected_head_sha"], 100,
		)
		if err != nil || !supported || !latestAccessGrantWorkflowRunsPass(runs) {
			return failAccessGrantPRMergePreflight(result, "exact_head_ci_unsuccessful", fmt.Errorf("%w: exact-head CI is not successful", ErrAccessGrantDenied))
		}
	}
	return result, nil
}

func (s *Service) accessGrantRequiresCI(repoFullName string) bool {
	if s == nil || s.AccessGrantRequireCI == nil {
		return true
	}
	return s.AccessGrantRequireCI(repoFullName)
}

// latestAccessGrantWorkflowRunsPass evaluates only the newest exact-head run
// per workflow identity. At least one exact-head workflow is required; stale
// failures cannot override a newer success and a pending/failed newest run
// cannot be hidden by older success.
func latestAccessGrantWorkflowRunsPass(runs []forgejointegration.WorkflowRun) bool {
	if len(runs) == 0 {
		return false
	}
	type candidate struct {
		run forgejointegration.WorkflowRun
		at  time.Time
	}
	latest := make(map[string]candidate, len(runs))
	for _, run := range runs {
		key := strings.TrimSpace(run.WorkflowID)
		if key == "" {
			key = strings.TrimSpace(run.Name)
		}
		if key == "" || !canonicalActionSHA(strings.ToLower(strings.TrimSpace(run.HeadSHA))) {
			return false
		}
		updated, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(run.UpdatedAt))
		current, found := latest[key]
		if !found || run.ID > current.run.ID || (run.ID == current.run.ID && updated.After(current.at)) {
			latest[key] = candidate{run: run, at: updated}
		}
	}
	if len(latest) == 0 {
		return false
	}
	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.ToLower(strings.TrimSpace(latest[key].run.Status)) != "success" {
			return false
		}
	}
	return true
}

// reconcileAccessGrantPRMerge is GET-only after the dispatch boundary. It may
// advance the durable receipt from unknown to an exact observed result, but it
// never calls the provider merge endpoint.
func (s *Service) reconcileAccessGrantPRMerge(ctx context.Context, invocation db.AccessGrantInvocation) (AccessGrantInvocationReceipt, error) {
	switch invocation.State {
	case ForgejoActionIntentCompleted, ForgejoActionIntentDenied, ForgejoActionIntentConflict, ForgejoActionIntentPlanned:
		return accessGrantInvocationReceipt(invocation), nil
	case ForgejoActionIntentDispatching, ForgejoActionIntentRecovery:
	default:
		return AccessGrantInvocationReceipt{}, ErrAccessGrantUnavailable
	}
	if s == nil || s.ForgejoIntegration == nil {
		return accessGrantInvocationReceipt(invocation), nil
	}
	snapshot, found, err := s.ForgejoIntegration.ExactPullRequestSnapshot(
		ctx, invocation.Repository, invocation.ProviderRepo, invocation.ProviderPRNumber,
	)
	if err != nil || !found {
		current, markErr := s.markAccessGrantPRMergeRecovery(ctx, invocation.ID)
		if markErr != nil {
			return AccessGrantInvocationReceipt{}, markErr
		}
		return accessGrantInvocationReceipt(current), nil
	}
	if snapshot.Merged {
		if exactGitSHA(snapshot.HeadSHA, invocation.ExpectedHeadSHA) {
			if mergeSHA, canonical := canonicalProviderMergeSnapshotSHA(snapshot, invocation.ExpectedHeadSHA); canonical {
				current, completeErr := s.completeAccessGrantPRMergeReconciliation(ctx, invocation.ID, mergeSHA)
				if completeErr != nil {
					return AccessGrantInvocationReceipt{}, completeErr
				}
				return accessGrantInvocationReceipt(current), nil
			}
		}
		current, finishErr := s.finishAccessGrantPRMergeAfterDispatch(ctx, invocation.ID, "provider_merged_fact_drift", "merged_fact_drift")
		if finishErr != nil {
			return AccessGrantInvocationReceipt{}, finishErr
		}
		return accessGrantInvocationReceipt(current), nil
	}
	if snapshot.State == "open" && exactGitSHA(snapshot.HeadSHA, invocation.ExpectedHeadSHA) &&
		snapshot.BaseRef == invocation.BaseRef && exactGitSHA(snapshot.BaseSHA, invocation.ExpectedBaseSHA) {
		current, markErr := s.markAccessGrantPRMergeRecovery(ctx, invocation.ID)
		if markErr != nil {
			return AccessGrantInvocationReceipt{}, markErr
		}
		return accessGrantInvocationReceipt(current), nil
	}
	current, finishErr := s.finishAccessGrantPRMergeAfterDispatch(ctx, invocation.ID, "provider_state_drift", "confirmed_not_merged")
	if finishErr != nil {
		return AccessGrantInvocationReceipt{}, finishErr
	}
	return accessGrantInvocationReceipt(current), nil
}

func (s *Service) markAccessGrantPRMergeRecovery(ctx context.Context, invocationID string) (db.AccessGrantInvocation, error) {
	now := time.Now().UTC()
	result := s.DBForCtx(ctx).Model(&db.AccessGrantInvocation{}).
		Where("id = ? AND operation = ? AND state IN ?", invocationID, "pr.merge", []string{ForgejoActionIntentDispatching, ForgejoActionIntentRecovery}).
		Updates(map[string]any{
			"state": ForgejoActionIntentRecovery, "provider_attempt": "attempted",
			"provider_outcome": "outcome_unknown", "updated_at": now,
		})
	if result.Error != nil {
		return db.AccessGrantInvocation{}, result.Error
	}
	return s.loadAccessGrantInvocationByID(ctx, invocationID)
}

func (s *Service) completeAccessGrantPRMergeReconciliation(ctx context.Context, invocationID, mergeSHA string) (db.AccessGrantInvocation, error) {
	if !canonicalActionSHA(mergeSHA) {
		return db.AccessGrantInvocation{}, fmt.Errorf("access-grant merge reconciliation SHA is noncanonical")
	}
	now := time.Now().UTC()
	result := s.DBForCtx(ctx).Model(&db.AccessGrantInvocation{}).
		Where("id = ? AND operation = ? AND state IN ?", invocationID, "pr.merge", []string{ForgejoActionIntentDispatching, ForgejoActionIntentRecovery}).
		Updates(map[string]any{
			"state": ForgejoActionIntentCompleted, "authorization_outcome": "allowed",
			"provider_attempt": "attempted", "provider_outcome": "reconciled_after_error",
			"provider_merged": true, "provider_merge_sha": mergeSHA,
			"denial_code": "", "updated_at": now, "finished_at": &now,
		})
	if result.Error != nil {
		return db.AccessGrantInvocation{}, result.Error
	}
	current, err := s.loadAccessGrantInvocationByID(ctx, invocationID)
	if err != nil {
		return db.AccessGrantInvocation{}, err
	}
	if current.State != ForgejoActionIntentCompleted || !current.ProviderMerged || !exactGitSHA(current.ProviderMergeSHA, mergeSHA) {
		return db.AccessGrantInvocation{}, fmt.Errorf("access-grant merge reconciliation state changed")
	}
	return current, nil
}

func (s *Service) finishAccessGrantPRMergeAfterDispatch(ctx context.Context, invocationID, code, providerOutcome string) (db.AccessGrantInvocation, error) {
	now := time.Now().UTC()
	result := s.DBForCtx(ctx).Model(&db.AccessGrantInvocation{}).
		Where("id = ? AND operation = ? AND state IN ?", invocationID, "pr.merge", []string{ForgejoActionIntentDispatching, ForgejoActionIntentRecovery}).
		Updates(map[string]any{
			"state": ForgejoActionIntentConflict, "provider_attempt": "attempted",
			"provider_outcome": providerOutcome, "provider_merged": false,
			"provider_merge_sha": "", "denial_code": code,
			"updated_at": now, "finished_at": &now,
		})
	if result.Error != nil {
		return db.AccessGrantInvocation{}, result.Error
	}
	current, err := s.loadAccessGrantInvocationByID(ctx, invocationID)
	if err != nil {
		return db.AccessGrantInvocation{}, err
	}
	if result.RowsAffected == 0 && current.State != ForgejoActionIntentConflict && current.State != ForgejoActionIntentCompleted {
		return db.AccessGrantInvocation{}, fmt.Errorf("access-grant merge recovery state changed")
	}
	return current, nil
}
