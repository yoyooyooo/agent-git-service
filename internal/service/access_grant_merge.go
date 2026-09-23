package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/operationconstraints"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AccessGrantPRMergeInput struct {
	InvocationID     string
	AGSPRNumber      int
	ProviderPRNumber int
	ExpectedHeadSHA  string
	MergeMethod      string
}

type accessGrantPRMergePreflight struct {
	PR              db.PullRequest
	Binding         forgejointegration.ProviderMergeBinding
	ProviderRepo    string
	BaseSHA         string
	ProviderBaseSHA string
	BaseRef         string
}

// ExecuteAccessGrantPRMerge owns one exact provider-effect attempt. A repeated
// exact request resolves the existing durable intent and never emits a second
// provider POST after dispatch has begun.
func (s *Service) ExecuteAccessGrantPRMerge(ctx context.Context, rawGrant string, input AccessGrantPRMergeInput) (AccessGrantInvocationReceipt, error) {
	invocationID := strings.TrimSpace(input.InvocationID)
	parsedInvocationID, parseErr := uuid.Parse(invocationID)
	if parseErr != nil || parsedInvocationID.String() != invocationID {
		return AccessGrantInvocationReceipt{}, fmt.Errorf("%w: canonical invocation_id is required", ErrValidation)
	}
	grant, _, _, err := s.resolveActiveAccessGrant(ctx, rawGrant)
	if err != nil {
		return AccessGrantInvocationReceipt{}, err
	}
	if !containsAccessGrantOperation(grant.EffectiveOperations, "pr.merge") {
		return AccessGrantInvocationReceipt{}, fmt.Errorf("%w: pr.merge is outside the grant", ErrAccessGrantDenied)
	}
	constraints, err := operationconstraints.NormalizeStored("pr.merge", map[string]string{
		"pull_request_number":         strconv.Itoa(input.AGSPRNumber),
		"forgejo_pull_request_number": strconv.Itoa(input.ProviderPRNumber),
		"expected_head_sha":           strings.TrimSpace(input.ExpectedHeadSHA),
		"merge_method":                strings.TrimSpace(input.MergeMethod),
	})
	if err != nil {
		return AccessGrantInvocationReceipt{}, fmt.Errorf("%w: pr.merge constraints are invalid", ErrValidation)
	}
	effectKey, err := accessGrantMergeEffectKey(grant.RepositoryFullName, constraints)
	if err != nil {
		return AccessGrantInvocationReceipt{}, ErrAccessGrantUnavailable
	}
	if existing, loadErr := s.loadAccessGrantInvocationByID(ctx, invocationID); loadErr == nil {
		if existing.EffectKey == nil || *existing.EffectKey != effectKey || !sameAccessGrantPRMergeInvocation(grant, existing, constraints) {
			return AccessGrantInvocationReceipt{}, accessGrantPRMergeLocatorCollision(grant, existing, "invocation_id belongs to different effect facts")
		}
		return s.resumeAccessGrantPRMerge(ctx, grant, existing, constraints)
	} else if !errors.Is(loadErr, gorm.ErrRecordNotFound) {
		return AccessGrantInvocationReceipt{}, loadErr
	}
	if existing, found, loadErr := s.loadAccessGrantInvocationByEffectKey(ctx, effectKey); loadErr != nil {
		return AccessGrantInvocationReceipt{}, loadErr
	} else if found {
		if existing.ID != invocationID || existing.GrantID != grant.ID {
			return AccessGrantInvocationReceipt{}, accessGrantPRMergeLocatorCollision(grant, existing, "exact provider effect belongs to another invocation or grant")
		}
		return s.resumeAccessGrantPRMerge(ctx, grant, existing, constraints)
	}

	preflight, err := s.accessGrantPRMergePreflight(ctx, grant, constraints)
	if err != nil {
		if existing, loadErr := s.loadAccessGrantInvocationByID(ctx, invocationID); loadErr == nil {
			if sameAccessGrantPRMergeInvocation(grant, existing, constraints) {
				return s.resumeAccessGrantPRMerge(ctx, grant, existing, constraints)
			}
			return AccessGrantInvocationReceipt{}, accessGrantPRMergeLocatorCollision(grant, existing, "invocation_id belongs to different effect facts")
		}
		if existing, found, loadErr := s.loadAccessGrantInvocationByEffectKey(ctx, effectKey); loadErr == nil && found {
			if existing.ID != invocationID || existing.GrantID != grant.ID {
				return AccessGrantInvocationReceipt{}, accessGrantPRMergeLocatorCollision(grant, existing, "exact provider effect belongs to another invocation or grant")
			}
			if sameAccessGrantPRMergeInvocation(grant, existing, constraints) {
				return s.resumeAccessGrantPRMerge(ctx, grant, existing, constraints)
			}
		}
		if errors.Is(err, ErrAccessGrantConflict) {
			return s.persistAccessGrantPRMergeConflict(ctx, grant, preflight, constraints, effectKey, invocationID, accessGrantPRMergePreflightDenialCode(err))
		}
		return AccessGrantInvocationReceipt{}, err
	}
	invocation, err := s.persistAccessGrantPRMergeIntent(ctx, grant, preflight, constraints, effectKey, invocationID)
	if err != nil {
		if existing, found, loadErr := s.loadAccessGrantInvocationByEffectKey(ctx, effectKey); loadErr == nil && found {
			if existing.ID == invocationID && existing.GrantID == grant.ID && sameAccessGrantPRMergeInvocation(grant, existing, constraints) {
				return s.resumeAccessGrantPRMerge(ctx, grant, existing, constraints)
			}
			return AccessGrantInvocationReceipt{}, accessGrantPRMergeLocatorCollision(grant, existing, "exact provider effect belongs to another invocation or grant")
		}
		if errors.Is(err, ErrAccessGrantConflict) {
			return s.persistAccessGrantPRMergeConflict(ctx, grant, preflight, constraints, effectKey, invocationID, "intent_fact_drift")
		}
		return AccessGrantInvocationReceipt{}, err
	}
	if !sameAccessGrantPRMergeInvocation(grant, invocation, constraints) {
		return AccessGrantInvocationReceipt{}, ErrAccessGrantUnavailable
	}
	return s.dispatchAccessGrantPRMerge(ctx, grant, invocation, constraints)
}

// GetAccessGrantInvocation is an exact GET/reconcile path. The originating
// credential or a verified renewed descendant may inspect the same immutable
// provider-effect receipt after expiry or revocation; this method never
// dispatches a provider write.
func (s *Service) GetAccessGrantInvocation(ctx context.Context, rawGrant, invocationID string) (AccessGrantInvocationReceipt, error) {
	grant, _, _, err := s.loadAccessGrantCredential(ctx, rawGrant)
	if err != nil {
		return AccessGrantInvocationReceipt{}, err
	}
	invocationID = strings.TrimSpace(invocationID)
	parsedInvocationID, parseErr := uuid.Parse(invocationID)
	if parseErr != nil || parsedInvocationID.String() != invocationID {
		return AccessGrantInvocationReceipt{}, ErrAccessGrantDenied
	}
	invocation, err := s.loadAccessGrantInvocationByID(ctx, invocationID)
	if err != nil {
		return AccessGrantInvocationReceipt{}, ErrAccessGrantDenied
	}
	allowed, err := s.accessGrantLineageContains(ctx, grant, invocation.GrantID)
	if err != nil || !allowed {
		return AccessGrantInvocationReceipt{}, ErrAccessGrantDenied
	}
	if invocation.Operation == "pr.merge" && (invocation.State == ForgejoActionIntentDispatching || invocation.State == ForgejoActionIntentRecovery) {
		return s.reconcileAccessGrantPRMerge(ctx, invocation)
	}
	return accessGrantInvocationReceipt(invocation), nil
}

func (s *Service) accessGrantLineageContains(ctx context.Context, grant db.AccessGrant, ancestorID string) (bool, error) {
	if grant.ID == ancestorID {
		return true, nil
	}
	cursor := strings.TrimSpace(grant.RenewedFromGrantID)
	for depth := 0; cursor != "" && depth < 64; depth++ {
		if cursor == ancestorID {
			return true, nil
		}
		var parent db.AccessGrant
		if err := s.DBForCtx(ctx).Select("id", "renewed_from_grant_id").First(&parent, "id = ?", cursor).Error; err != nil {
			return false, err
		}
		cursor = strings.TrimSpace(parent.RenewedFromGrantID)
	}
	return false, nil
}

func accessGrantPRMergeLocatorCollision(grant db.AccessGrant, existing db.AccessGrantInvocation, detail string) error {
	if existing.GrantID != grant.ID {
		return fmt.Errorf("%w: provider effect locator is outside this grant", ErrAccessGrantDenied)
	}
	return fmt.Errorf("%w: %s", ErrValidation, detail)
}

func (s *Service) persistAccessGrantPRMergeConflict(
	ctx context.Context,
	grant db.AccessGrant,
	preflight accessGrantPRMergePreflight,
	constraints map[string]string,
	effectKey, invocationID, denialCode string,
) (AccessGrantInvocationReceipt, error) {
	constraintsJSON, err := json.Marshal(constraints)
	if err != nil {
		return AccessGrantInvocationReceipt{}, ErrAccessGrantUnavailable
	}
	now := time.Now().UTC()
	key := effectKey
	invocation := db.AccessGrantInvocation{
		ID: invocationID, EffectKey: &key, GrantID: grant.ID,
		ActorUserID: grant.ActorUserID, ExecutorUserID: grant.ExecutorUserID,
		RepositoryID: grant.RepositoryID, Repository: grant.RepositoryFullName,
		Operation: "pr.merge", ConstraintsJSON: string(constraintsJSON), AuthorityRevision: grant.AuthorityRevision,
		AGSPRNumber: preflightAGSNumber(constraints), Provider: ProjectionProviderForgejo,
		ProviderRepo: preflight.ProviderRepo, ProviderPRNumber: preflightPRNumber(constraints),
		ExpectedHeadSHA: constraints["expected_head_sha"], ExpectedBaseSHA: preflight.BaseSHA,
		BaseRef: preflight.BaseRef, EffectMethod: constraints["merge_method"],
		State: ForgejoActionIntentConflict, AuthorizationOutcome: "denied",
		ProviderAttempt: "not_attempted", ProviderOutcome: "not_attempted", DenialCode: denialCode,
		CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
	}
	if err := s.DBForCtx(ctx).Create(&invocation).Error; err == nil {
		return accessGrantInvocationReceipt(invocation), nil
	}
	if existing, loadErr := s.loadAccessGrantInvocationByID(ctx, invocationID); loadErr == nil {
		if sameAccessGrantPRMergeInvocation(grant, existing, constraints) {
			return s.resumeAccessGrantPRMerge(ctx, grant, existing, constraints)
		}
		return AccessGrantInvocationReceipt{}, accessGrantPRMergeLocatorCollision(grant, existing, "invocation_id belongs to different effect facts")
	}
	if existing, found, loadErr := s.loadAccessGrantInvocationByEffectKey(ctx, effectKey); loadErr == nil && found {
		if existing.ID == invocationID && existing.GrantID == grant.ID && sameAccessGrantPRMergeInvocation(grant, existing, constraints) {
			return s.resumeAccessGrantPRMerge(ctx, grant, existing, constraints)
		}
		return AccessGrantInvocationReceipt{}, accessGrantPRMergeLocatorCollision(grant, existing, "exact provider effect belongs to another invocation or grant")
	}
	return AccessGrantInvocationReceipt{}, ErrAccessGrantUnavailable
}

func (s *Service) persistAccessGrantPRMergeIntent(ctx context.Context, grant db.AccessGrant, preflight accessGrantPRMergePreflight, constraints map[string]string, effectKey, invocationID string) (db.AccessGrantInvocation, error) {
	constraintsJSON, err := json.Marshal(constraints)
	if err != nil {
		return db.AccessGrantInvocation{}, ErrAccessGrantUnavailable
	}
	now := time.Now().UTC()
	key := effectKey
	invocation := db.AccessGrantInvocation{
		ID: invocationID, EffectKey: &key, GrantID: grant.ID,
		ActorUserID: grant.ActorUserID, ExecutorUserID: grant.ExecutorUserID,
		RepositoryID: grant.RepositoryID, Repository: grant.RepositoryFullName,
		Operation: "pr.merge", ConstraintsJSON: string(constraintsJSON), AuthorityRevision: grant.AuthorityRevision,
		AGSPRNumber: preflight.PR.Number, Provider: ProjectionProviderForgejo,
		ProviderRepo: preflight.ProviderRepo, ProviderPRNumber: preflightPRNumber(constraints),
		ExpectedHeadSHA: constraints["expected_head_sha"], ExpectedBaseSHA: preflight.BaseSHA,
		BaseRef: preflight.BaseRef, EffectMethod: constraints["merge_method"],
		State: ForgejoActionIntentPlanned, AuthorizationOutcome: "allowed",
		ProviderAttempt: "not_attempted", ProviderOutcome: "not_attempted",
		CreatedAt: now, UpdatedAt: now,
	}
	err = s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		var currentGrant db.AccessGrant
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&currentGrant, "id = ?", grant.ID).Error; err != nil {
			return err
		}
		if currentGrant.RevokedAt != nil || !currentGrant.ExpiresAt.After(time.Now().UTC()) || currentGrant.AuthorityRevision != grant.AuthorityRevision {
			return ErrAccessGrantConflict
		}
		var currentPR db.PullRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select(
			"id", "number", "repository_id", "state", "merged", "head_ref", "head_sha", "base_ref",
		).First(&currentPR, preflight.PR.ID).Error; err != nil {
			return err
		}
		if currentPR.RepositoryID != grant.RepositoryID || currentPR.Number != invocation.AGSPRNumber ||
			currentPR.State != db.StateOpen || currentPR.Merged ||
			!exactGitSHA(currentPR.HeadSHA, invocation.ExpectedHeadSHA) || currentPR.BaseRef != invocation.BaseRef {
			return ErrAccessGrantConflict
		}
		var projection db.PullRequestProjection
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"pull_request_id = ? AND provider = ?", currentPR.ID, ProjectionProviderForgejo,
		).First(&projection).Error; err != nil {
			return err
		}
		if projection.ExternalRepo != invocation.ProviderRepo || projection.ExternalNumber != invocation.ProviderPRNumber {
			return ErrAccessGrantConflict
		}
		return tx.Create(&invocation).Error
	})
	if err != nil {
		if existing, found, loadErr := s.loadAccessGrantInvocationByEffectKey(ctx, effectKey); loadErr == nil && found {
			if existing.ID != invocationID || existing.GrantID != grant.ID || !sameAccessGrantPRMergeInvocation(grant, existing, constraints) {
				return db.AccessGrantInvocation{}, accessGrantPRMergeLocatorCollision(grant, existing, "exact provider effect belongs to another invocation or grant")
			}
			return existing, nil
		}
		if errors.Is(err, ErrAccessGrantConflict) {
			return db.AccessGrantInvocation{}, fmt.Errorf("%w: exact pr.merge facts changed", ErrAccessGrantConflict)
		}
		return db.AccessGrantInvocation{}, wrapErr(err)
	}
	return invocation, nil
}

func (s *Service) resumeAccessGrantPRMerge(ctx context.Context, grant db.AccessGrant, invocation db.AccessGrantInvocation, constraints map[string]string) (AccessGrantInvocationReceipt, error) {
	if !sameAccessGrantPRMergeInvocation(grant, invocation, constraints) {
		return AccessGrantInvocationReceipt{}, accessGrantPRMergeLocatorCollision(grant, invocation, "provider effect facts changed")
	}
	switch invocation.State {
	case ForgejoActionIntentPlanned:
		return s.dispatchAccessGrantPRMerge(ctx, grant, invocation, constraints)
	case ForgejoActionIntentDispatching, ForgejoActionIntentRecovery:
		return s.reconcileAccessGrantPRMerge(ctx, invocation)
	case ForgejoActionIntentCompleted, ForgejoActionIntentDenied, ForgejoActionIntentConflict:
		return accessGrantInvocationReceipt(invocation), nil
	default:
		return AccessGrantInvocationReceipt{}, ErrAccessGrantUnavailable
	}
}

func (s *Service) dispatchAccessGrantPRMerge(ctx context.Context, grant db.AccessGrant, invocation db.AccessGrantInvocation, constraints map[string]string) (AccessGrantInvocationReceipt, error) {
	current, err := s.loadAccessGrantInvocationByID(ctx, invocation.ID)
	if err != nil {
		return AccessGrantInvocationReceipt{}, err
	}
	if current.State != ForgejoActionIntentPlanned {
		return s.resumeAccessGrantPRMerge(ctx, grant, current, constraints)
	}
	invocation = current
	preflight, err := s.accessGrantPRMergePreflight(ctx, grant, constraints)
	if err != nil || !accessGrantPRMergePreflightMatchesInvocation(preflight, invocation) {
		if current, loadErr := s.loadAccessGrantInvocationByID(ctx, invocation.ID); loadErr == nil && current.State != ForgejoActionIntentPlanned {
			return s.resumeAccessGrantPRMerge(ctx, grant, current, constraints)
		}
		terminal, finishErr := s.finishAccessGrantPRMergeWithoutDispatch(ctx, invocation.ID, "use_time_fact_drift")
		if finishErr != nil {
			return AccessGrantInvocationReceipt{}, finishErr
		}
		return accessGrantInvocationReceipt(terminal), nil
	}
	if s.fastForwardAckEligible(invocation.Repository, invocation.EffectMethod, preflight.ProviderBaseSHA, invocation.ExpectedHeadSHA) {
		sides := providerMergeSides{
			BaseRef: preflight.BaseRef, Head: invocation.ExpectedHeadSHA,
			AGSBase: preflight.BaseSHA, ProviderBase: preflight.ProviderBaseSHA,
		}
		if _, ackErr := s.acknowledgeFastForwardMerge(ctx, HumanProviderMergeReceipt{Repository: invocation.Repository}, preflight.PR, ProviderEvidenceBinding{}, sides); ackErr != nil {
			terminal, finishErr := s.finishAccessGrantPRMergeWithoutDispatch(ctx, invocation.ID, "fast_forward_ack_conflict")
			if finishErr != nil {
				return AccessGrantInvocationReceipt{}, finishErr
			}
			return accessGrantInvocationReceipt(terminal), nil
		}
		if err := s.completeAccessGrantPRMergeFastForwardAck(ctx, invocation.ID, invocation.ExpectedHeadSHA); err != nil {
			return AccessGrantInvocationReceipt{}, err
		}
		current, err = s.loadAccessGrantInvocationByID(ctx, invocation.ID)
		if err != nil {
			return AccessGrantInvocationReceipt{}, err
		}
		_ = s.DBForCtx(ctx).Model(&db.AccessGrant{}).Where("id = ?", grant.ID).Update("last_used_at", time.Now().UTC()).Error
		return accessGrantInvocationReceipt(current), nil
	}
	current, dispatch, err := s.beginAccessGrantPRMergeDispatch(ctx, grant, invocation)
	if err != nil {
		if errors.Is(err, ErrAccessGrantConflict) {
			terminal, finishErr := s.finishAccessGrantPRMergeWithoutDispatch(ctx, invocation.ID, "final_dispatch_fact_drift")
			if finishErr != nil {
				return AccessGrantInvocationReceipt{}, finishErr
			}
			return accessGrantInvocationReceipt(terminal), nil
		}
		return AccessGrantInvocationReceipt{}, err
	}
	if !dispatch {
		if current.State == ForgejoActionIntentDispatching || current.State == ForgejoActionIntentRecovery {
			return s.reconcileAccessGrantPRMerge(ctx, current)
		}
		return accessGrantInvocationReceipt(current), nil
	}

	result, mergeErr := s.ForgejoIntegration.MergePullRequest(ctx, invocation.Repository, invocation.ProviderRepo, invocation.ProviderPRNumber,
		forgejointegration.PullRequestMergeRequest{Method: invocation.EffectMethod, ExpectedHead: invocation.ExpectedHeadSHA, DeleteBranch: false})
	state := ForgejoActionIntentRecovery
	providerOutcome := "outcome_unknown"
	merged := false
	mergeSHA := ""
	if canonicalSHA, confirmed := canonicalProviderMergeResult(result, mergeErr, invocation.ExpectedHeadSHA); confirmed {
		state, providerOutcome, merged, mergeSHA = ForgejoActionIntentCompleted, result.ProviderOutcome, true, canonicalSHA
	}
	if err := s.completeAccessGrantPRMergeDispatch(ctx, invocation.ID, state, providerOutcome, merged, mergeSHA); err != nil {
		return AccessGrantInvocationReceipt{}, err
	}
	current, err = s.loadAccessGrantInvocationByID(ctx, invocation.ID)
	if err != nil {
		return AccessGrantInvocationReceipt{}, err
	}
	_ = s.DBForCtx(ctx).Model(&db.AccessGrant{}).Where("id = ?", grant.ID).Update("last_used_at", time.Now().UTC()).Error
	return accessGrantInvocationReceipt(current), nil
}

func (s *Service) beginAccessGrantPRMergeDispatch(ctx context.Context, grant db.AccessGrant, invocation db.AccessGrantInvocation) (db.AccessGrantInvocation, bool, error) {
	now := time.Now().UTC()
	var dispatched bool
	err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		// Use the same lock order as intent creation: grant -> PR -> projection
		// -> invocation. Locking the invocation first deadlocks with a duplicate
		// creator holding the grant/PR while its unique-key INSERT waits here.
		// This transaction only claims the effect; the provider POST stays outside
		// it and is never retried as a database recovery action.
		var currentGrant db.AccessGrant
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&currentGrant, "id = ?", grant.ID).Error; err != nil {
			return err
		}
		var currentPR db.PullRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select(
			"id", "number", "repository_id", "state", "merged", "draft", "head_sha", "base_ref",
		).First(&currentPR, "repository_id = ? AND number = ?", invocation.RepositoryID, invocation.AGSPRNumber).Error; err != nil {
			return err
		}
		var projection db.PullRequestProjection
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
			"pull_request_id = ? AND provider = ?", currentPR.ID, ProjectionProviderForgejo,
		).First(&projection).Error; err != nil {
			return err
		}
		var currentInvocation db.AccessGrantInvocation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&currentInvocation, "id = ?", invocation.ID).Error; err != nil {
			return err
		}
		if currentInvocation.State != ForgejoActionIntentPlanned {
			return nil
		}
		now = time.Now().UTC() // Lock waiting cannot extend grant lifetime.
		if currentGrant.RevokedAt != nil || !currentGrant.ExpiresAt.After(now) ||
			currentGrant.AuthorityRevision != grant.AuthorityRevision || currentGrant.ID != invocation.GrantID {
			return ErrAccessGrantConflict
		}
		if currentPR.State != db.StateOpen || currentPR.Merged || currentPR.Draft ||
			!exactGitSHA(currentPR.HeadSHA, invocation.ExpectedHeadSHA) || currentPR.BaseRef != invocation.BaseRef {
			return ErrAccessGrantConflict
		}
		if projection.ExternalRepo != invocation.ProviderRepo || projection.ExternalNumber != invocation.ProviderPRNumber {
			return ErrAccessGrantConflict
		}
		result := tx.Model(&db.AccessGrantInvocation{}).
			Where("id = ? AND operation = ? AND state = ?", invocation.ID, "pr.merge", ForgejoActionIntentPlanned).
			Updates(map[string]any{
				"state": ForgejoActionIntentDispatching, "provider_attempt": "attempted",
				"provider_outcome": "outcome_unknown", "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		dispatched = result.RowsAffected == 1
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrAccessGrantConflict) {
			return db.AccessGrantInvocation{}, false, fmt.Errorf("%w: final dispatch facts changed", ErrAccessGrantConflict)
		}
		return db.AccessGrantInvocation{}, false, err
	}
	current, err := s.loadAccessGrantInvocationByID(ctx, invocation.ID)
	return current, dispatched, err
}

func (s *Service) completeAccessGrantPRMergeFastForwardAck(ctx context.Context, invocationID, mergeSHA string) error {
	if !canonicalActionSHA(mergeSHA) {
		return fmt.Errorf("fast-forward ack SHA is noncanonical")
	}
	now := time.Now().UTC()
	result := s.DBForCtx(ctx).Model(&db.AccessGrantInvocation{}).
		Where("id = ? AND operation = ? AND state = ?", invocationID, "pr.merge", ForgejoActionIntentPlanned).
		Updates(map[string]any{
			"state": ForgejoActionIntentCompleted, "authorization_outcome": "allowed",
			"provider_attempt": "not_attempted", "provider_outcome": providerMergeOutcomeSourceSuccess,
			"provider_merged": false, "provider_merge_sha": mergeSHA,
			"updated_at": now, "finished_at": &now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("access-grant fast-forward ack state changed")
	}
	return nil
}

func (s *Service) completeAccessGrantPRMergeDispatch(ctx context.Context, invocationID, state, providerOutcome string, merged bool, mergeSHA string) error {
	if state == ForgejoActionIntentCompleted {
		if !canonicalActionSHA(mergeSHA) {
			return fmt.Errorf("completed access-grant merge result is noncanonical")
		}
		if merged {
			if providerOutcome != "confirmed" && providerOutcome != "reconciled_after_error" {
				return fmt.Errorf("completed access-grant merge result is noncanonical")
			}
		} else if providerOutcome != providerMergeOutcomeSourceSuccess {
			return fmt.Errorf("completed access-grant merge result is noncanonical")
		}
	} else if merged || mergeSHA != "" || state != ForgejoActionIntentRecovery {
		return fmt.Errorf("unknown access-grant merge result is noncanonical")
	}
	now := time.Now().UTC()
	updates := map[string]any{
		"state": state, "provider_outcome": providerOutcome, "provider_merged": merged,
		"provider_merge_sha": mergeSHA, "updated_at": now,
	}
	if state == ForgejoActionIntentCompleted {
		updates["finished_at"] = &now
	}
	result := s.DBForCtx(ctx).Model(&db.AccessGrantInvocation{}).
		Where("id = ? AND operation = ? AND state IN ?", invocationID, "pr.merge", []string{ForgejoActionIntentDispatching, ForgejoActionIntentRecovery}).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		current, err := s.loadAccessGrantInvocationByID(ctx, invocationID)
		if err != nil {
			return err
		}
		if current.State == ForgejoActionIntentCompleted && current.ProviderMerged && canonicalActionSHA(current.ProviderMergeSHA) &&
			(current.ProviderOutcome == "confirmed" || current.ProviderOutcome == "reconciled_after_error") {
			return nil
		}
		if current.State == state && current.ProviderOutcome == providerOutcome && current.ProviderMerged == merged && current.ProviderMergeSHA == mergeSHA {
			return nil
		}
		return fmt.Errorf("access-grant merge dispatch state changed")
	}
	return nil
}

func (s *Service) finishAccessGrantPRMergeWithoutDispatch(ctx context.Context, invocationID, code string) (db.AccessGrantInvocation, error) {
	now := time.Now().UTC()
	result := s.DBForCtx(ctx).Model(&db.AccessGrantInvocation{}).
		Where("id = ? AND operation = ? AND state = ?", invocationID, "pr.merge", ForgejoActionIntentPlanned).
		Updates(map[string]any{
			"state": ForgejoActionIntentConflict, "authorization_outcome": "denied",
			"provider_attempt": "not_attempted", "provider_outcome": "not_attempted",
			"denial_code": code, "updated_at": now, "finished_at": &now,
		})
	if result.Error != nil {
		return db.AccessGrantInvocation{}, result.Error
	}
	current, err := s.loadAccessGrantInvocationByID(ctx, invocationID)
	if err != nil {
		return db.AccessGrantInvocation{}, err
	}
	if result.RowsAffected == 0 && current.State == ForgejoActionIntentPlanned {
		return db.AccessGrantInvocation{}, fmt.Errorf("access-grant merge terminal state changed")
	}
	return current, nil
}

func sameAccessGrantPRMergeInvocation(grant db.AccessGrant, invocation db.AccessGrantInvocation, constraints map[string]string) bool {
	var stored map[string]string
	if json.Unmarshal([]byte(invocation.ConstraintsJSON), &stored) != nil || !operationconstraints.Match("pr.merge", stored, constraints) {
		return false
	}
	return invocation.GrantID == grant.ID && invocation.ActorUserID == grant.ActorUserID &&
		invocation.ExecutorUserID == grant.ExecutorUserID && invocation.RepositoryID == grant.RepositoryID &&
		invocation.Repository == grant.RepositoryFullName && invocation.Operation == "pr.merge" &&
		invocation.AuthorityRevision == grant.AuthorityRevision && invocation.AGSPRNumber == preflightAGSNumber(constraints) &&
		invocation.ProviderPRNumber == preflightPRNumber(constraints) &&
		exactGitSHA(invocation.ExpectedHeadSHA, constraints["expected_head_sha"]) &&
		invocation.EffectMethod == constraints["merge_method"]
}

func accessGrantPRMergePreflightMatchesInvocation(preflight accessGrantPRMergePreflight, invocation db.AccessGrantInvocation) bool {
	return preflight.PR.ID != 0 && preflight.PR.Number == invocation.AGSPRNumber &&
		preflight.ProviderRepo == invocation.ProviderRepo && preflight.BaseRef == invocation.BaseRef &&
		exactGitSHA(preflight.PR.HeadSHA, invocation.ExpectedHeadSHA) &&
		exactGitSHA(preflight.BaseSHA, invocation.ExpectedBaseSHA) &&
		preflight.Binding.MergeMethod == invocation.EffectMethod
}

func (s *Service) loadAccessGrantInvocationByEffectKey(ctx context.Context, effectKey string) (db.AccessGrantInvocation, bool, error) {
	var invocation db.AccessGrantInvocation
	err := s.DBForCtx(ctx).Where("effect_key = ?", effectKey).First(&invocation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return db.AccessGrantInvocation{}, false, nil
	}
	return invocation, err == nil, err
}

func (s *Service) loadAccessGrantInvocationByID(ctx context.Context, id string) (db.AccessGrantInvocation, error) {
	var invocation db.AccessGrantInvocation
	err := s.DBForCtx(ctx).First(&invocation, "id = ?", id).Error
	return invocation, err
}

func accessGrantMergeEffectKey(repository string, constraints map[string]string) (string, error) {
	normalized, err := operationconstraints.NormalizeStored("pr.merge", constraints)
	if err != nil || strings.Trim(strings.TrimSpace(repository), "/") == "" {
		return "", ErrValidation
	}
	body, err := json.Marshal(struct {
		Schema           string `json:"schema"`
		Repository       string `json:"repository"`
		AGSPRNumber      string `json:"ags_pr_number"`
		ProviderPRNumber string `json:"provider_pr_number"`
		ExpectedHeadSHA  string `json:"expected_head_sha"`
		MergeMethod      string `json:"merge_method"`
	}{
		Schema: "ags.access-grant-pr-merge-effect.v1", Repository: strings.Trim(repository, "/"),
		AGSPRNumber: normalized["pull_request_number"], ProviderPRNumber: normalized["forgejo_pull_request_number"],
		ExpectedHeadSHA: normalized["expected_head_sha"], MergeMethod: normalized["merge_method"],
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func preflightAGSNumber(constraints map[string]string) int {
	value, _ := strconv.Atoi(constraints["pull_request_number"])
	return value
}

func preflightPRNumber(constraints map[string]string) int {
	value, _ := strconv.Atoi(constraints["forgejo_pull_request_number"])
	return value
}
