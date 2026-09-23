package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ForgejoActionIntentPlanned     = "planned"
	ForgejoActionIntentDispatching = "dispatching"
	ForgejoActionIntentDispatched  = "dispatched"
	ForgejoActionIntentAccepted    = "accepted"
	ForgejoActionIntentRunning     = "running"
	ForgejoActionIntentCompleted   = "completed"
	ForgejoActionIntentDenied      = "denied"
	ForgejoActionIntentConflict    = "conflict"
	ForgejoActionIntentProjection  = "projection_failed"
	ForgejoActionIntentRecovery    = "recovery_needed"

	ProviderEffectStatusNotAttempted      = "not_attempted"
	ProviderEffectStatusOutcomeUnknown    = "outcome_unknown"
	ProviderEffectStatusVerifiedCompleted = "verified_completed"
	ProviderEffectStatusNotRecorded       = "not_recorded"
)

type ForgejoActionIntentRequest struct {
	IdempotencyKey  string
	Repository      string
	AGSPRNumber     int
	ForgejoPRNumber int
	ExpectedHeadSHA string
	ExpectedBaseSHA string
	ExpectedLabels  []string
	PostLabels      []string
	ExpiresIn       time.Duration
}

type ForgejoActionIntentReceipt struct {
	ID                   string    `json:"id"`
	Action               string    `json:"action"`
	State                string    `json:"state"`
	AGSPRNumber          int       `json:"ags_pr_number"`
	ForgejoPRNumber      int       `json:"forgejo_pr_number"`
	ExpectedHeadSHA      string    `json:"expected_head_sha"`
	ExpectedBaseSHA      string    `json:"expected_base_sha"`
	BoundaryReceiptID    string    `json:"boundary_receipt_id,omitempty"`
	ProviderEffectStatus string    `json:"provider_effect_status"`
	ResultSHA            string    `json:"result_sha,omitempty"`
	FailureCode          string    `json:"failure_code,omitempty"`
	ExpiresAt            time.Time `json:"expires_at"`
}

func actionLabelSet(labels []string) ([]string, error) {
	set := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" || len(label) > 255 {
			return nil, fmt.Errorf("%w: action labels are invalid", ErrValidation)
		}
		set[label] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for label := range set {
		out = append(out, label)
	}
	sort.Strings(out)
	return out, nil
}

func actionLabelsJSON(labels []string) (string, error) {
	labels, err := actionLabelSet(labels)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(labels)
	return string(encoded), err
}

func sameActionLabels(raw string, actual []string) bool {
	var expected []string
	if json.Unmarshal([]byte(raw), &expected) != nil {
		return false
	}
	want, err := actionLabelSet(expected)
	if err != nil {
		return false
	}
	got, err := actionLabelSet(actual)
	if err != nil {
		return false
	}
	return strings.Join(want, "\x00") == strings.Join(got, "\x00")
}

func actionIntentReceipt(intent db.PullRequestActionIntent) (ForgejoActionIntentReceipt, error) {
	if !isValidGitSHA(intent.ExpectedHeadSHA) || !isValidGitSHA(intent.ExpectedBaseSHA) {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("durable Forgejo action intent contains non-canonical expected SHA")
	}
	status := strings.TrimSpace(intent.ProviderEffectStatus)
	if status == "" {
		status = ProviderEffectStatusNotRecorded
	}
	boundaryReceiptID := ""
	if intent.BoundaryReceiptID != nil {
		boundaryReceiptID = strings.TrimSpace(*intent.BoundaryReceiptID)
	}
	if err := validateActionIntentBoundaryWire(intent, status, boundaryReceiptID); err != nil {
		return ForgejoActionIntentReceipt{}, err
	}
	return ForgejoActionIntentReceipt{
		ID: intent.ID, Action: intent.Action, State: intent.State, AGSPRNumber: intent.AGSPRNumber,
		ForgejoPRNumber: intent.ForgejoPRNumber, ExpectedHeadSHA: intent.ExpectedHeadSHA, ExpectedBaseSHA: intent.ExpectedBaseSHA,
		BoundaryReceiptID: boundaryReceiptID, ProviderEffectStatus: status, ResultSHA: intent.ResultSHA,
		FailureCode: intent.FailureCode, ExpiresAt: intent.ExpiresAt,
	}, nil
}

func validateActionIntentBoundaryWire(intent db.PullRequestActionIntent, status, receiptID string) error {
	if !stringInSlice(status, []string{ProviderEffectStatusNotAttempted, ProviderEffectStatusOutcomeUnknown, ProviderEffectStatusVerifiedCompleted, ProviderEffectStatusNotRecorded}) {
		return fmt.Errorf("durable Forgejo action intent contains invalid provider effect status")
	}
	delegated := intent.AgentSessionID != nil
	if !delegated {
		if intent.BoundaryProtocol != "" || receiptID != "" {
			return fmt.Errorf("durable-principal action intent contains delegated Boundary lineage")
		}
	} else if intent.BoundaryProtocol != "" && intent.BoundaryProtocol != AuthorityBoundaryReceiptDelegatedEffectKind {
		return fmt.Errorf("delegated action intent contains invalid Boundary protocol")
	}
	if receiptID != "" && !isCanonicalReceiptID(receiptID) {
		return fmt.Errorf("delegated action intent contains invalid Boundary Receipt ID")
	}
	if status == ProviderEffectStatusNotRecorded {
		// Historical rows are intentionally not inferred or backfilled.
		return nil
	}
	if status == ProviderEffectStatusVerifiedCompleted && intent.State != ForgejoActionIntentCompleted {
		return fmt.Errorf("provider effect completion is not bound to completed intent")
	}
	if delegated && intent.State == ForgejoActionIntentPlanned {
		if intent.BoundaryProtocol != AuthorityBoundaryReceiptDelegatedEffectKind || status != ProviderEffectStatusNotAttempted || receiptID != "" {
			return fmt.Errorf("planned delegated action intent Boundary state is invalid")
		}
	}
	if delegated && intent.State == ForgejoActionIntentDenied && status == ProviderEffectStatusNotAttempted {
		if intent.BoundaryProtocol != AuthorityBoundaryReceiptDelegatedEffectKind || receiptID != "" {
			return fmt.Errorf("pre-dispatch denied delegated action intent Boundary state is invalid")
		}
		return nil
	}
	if delegated && (intent.State == ForgejoActionIntentDispatching || forgejoActionStateAfterDispatch(intent.State)) {
		if intent.BoundaryProtocol != AuthorityBoundaryReceiptDelegatedEffectKind || receiptID == "" || status == ProviderEffectStatusNotAttempted {
			return fmt.Errorf("dispatch-eligible delegated action intent lacks Boundary Receipt lineage")
		}
	}
	return nil
}

// isValidGitSHA accepts only the canonical full SHA format bound into a
// pr.rebase operation constraint. Object existence is verified separately.
func isValidGitSHA(v string) bool { return canonicalActionSHA(v) }

func forgejoRebaseSessionConstraints(agsPRNumber, forgejoPRNumber int, expectedHeadSHA, expectedBaseSHA string) map[string]string {
	return map[string]string{
		"pull_request_number":         fmt.Sprint(agsPRNumber),
		"forgejo_pull_request_number": fmt.Sprint(forgejoPRNumber),
		"expected_head_sha":           expectedHeadSHA,
		"expected_base_sha":           expectedBaseSHA,
	}
}

func forgejoRebaseDurableConstraints(agsPRNumber, forgejoPRNumber int, expectedHeadSHA, expectedBaseSHA string) map[string]any {
	return map[string]any{
		"pull_request_number":         json.Number(fmt.Sprint(agsPRNumber)),
		"forgejo_pull_request_number": json.Number(fmt.Sprint(forgejoPRNumber)),
		"expected_head_sha":           expectedHeadSHA,
		"expected_base_sha":           expectedBaseSHA,
	}
}

func durableActionAuthorityRevision(result DurableOperationAuthorizationResult) string {
	snapshot := struct {
		ContractRevision       string `json:"contract_revision"`
		PrincipalID            uint   `json:"principal_id"`
		Repository             string `json:"repository"`
		Operation              string `json:"operation"`
		TeamIdentityID         string `json:"team_identity_id"`
		MembershipEpoch        int64  `json:"membership_epoch"`
		TeamBindingRevision    string `json:"team_binding_revision"`
		PolicyClass            string `json:"policy_class"`
		PolicyClassRevision    string `json:"policy_class_revision"`
		NativeGrantRevision    string `json:"native_grant_revision"`
		ResourcePolicyRevision string `json:"resource_policy_revision"`
	}{
		ContractRevision: result.ContractRevision, PrincipalID: result.PrincipalID,
		Repository: result.Resource.Repository, Operation: result.Operation.Name,
		TeamIdentityID: result.AuthorizationBasis.TeamIdentityID, MembershipEpoch: result.AuthorizationBasis.MembershipEpoch,
		TeamBindingRevision: result.AuthorizationBasis.TeamBindingRevision,
		PolicyClass:         result.AuthorizationBasis.PolicyClass, PolicyClassRevision: result.AuthorizationBasis.PolicyClassRevision,
		NativeGrantRevision:    result.AuthorizationBasis.NativeGrantRevision,
		ResourcePolicyRevision: result.AuthorizationBasis.ResourcePolicyRevision,
	}
	encoded, _ := json.Marshal(snapshot)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func sameDurableRebaseConstraints(actual map[string]any, intent db.PullRequestActionIntent) bool {
	expected := forgejoRebaseDurableConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA)
	if len(actual) != len(expected) {
		return false
	}
	for key, want := range expected {
		if fmt.Sprint(actual[key]) != fmt.Sprint(want) {
			return false
		}
	}
	return true
}

func (s *Service) authorizeCurrentDurableActionIntent(ctx context.Context, intent db.PullRequestActionIntent) error {
	if intent.RequestSource == forgejoLabelActionRequestSource {
		principalID, revision, ok := s.ForgejoIntegration.ActionPrincipalBinding(intent.RequestActor)
		if !ok || principalID != intent.PrincipalID || revision != intent.RequestBindingRev {
			return delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged)
		}
	}
	var principal db.User
	if err := s.DBForCtx(ctx).Where("id = ? AND type = ?", intent.PrincipalID, db.TypeUser).First(&principal).Error; err != nil || !isUserStatusActive(principal.Status) {
		return delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged)
	}
	result, err := s.AuthorizeDurableOperation(ctx, principal, "ags", intent.Repository, "pr.rebase",
		forgejoRebaseDurableConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA))
	if err != nil || !result.Authorized || result.PrincipalID != intent.PrincipalID || result.Resource.Service != "ags" ||
		result.Resource.Repository != intent.Repository || result.Operation.Name != "pr.rebase" || !sameDurableRebaseConstraints(result.Operation.Constraints, intent) ||
		result.AuthorizationBasis.TeamIdentityID != intent.TeamIdentityID || result.AuthorizationBasis.PolicyClass != intent.PolicyClass ||
		result.AuthorizationBasis.MembershipEpoch != intent.MembershipEpoch || durableActionAuthorityRevision(result) != intent.AuthorityRev {
		return delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged)
	}
	return nil
}

// RequestForgejoActionRebase creates the authority record before touching the
// provider. The authenticated AGS principal is the only authority input.
func (s *Service) RequestForgejoActionRebase(ctx context.Context, viewer db.User, request ForgejoActionIntentRequest) (ForgejoActionIntentReceipt, error) {
	if s == nil || s.ForgejoIntegration == nil {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: Forgejo integration is unavailable", ErrForbidden)
	}
	key := strings.TrimSpace(request.IdempotencyKey)
	request.Repository = strings.TrimSpace(request.Repository)
	if key == "" || len(key) > 255 || request.Repository == "" || request.AGSPRNumber <= 0 || int64(request.AGSPRNumber) > maxActionPullRequestNumber ||
		request.ForgejoPRNumber <= 0 || int64(request.ForgejoPRNumber) > maxActionPullRequestNumber ||
		!isValidGitSHA(request.ExpectedHeadSHA) || !isValidGitSHA(request.ExpectedBaseSHA) {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: action request is invalid", ErrValidation)
	}
	preLabels, err := actionLabelsJSON(request.ExpectedLabels)
	if err != nil {
		return ForgejoActionIntentReceipt{}, err
	}
	postLabels, err := actionLabelsJSON(request.PostLabels)
	if err != nil {
		return ForgejoActionIntentReceipt{}, err
	}
	if strings.Contains(preLabels, forgejointegration.AGSActionRebaseLabel) || !strings.Contains(postLabels, forgejointegration.AGSActionRebaseLabel) {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: exact action label transition is invalid", ErrValidation)
	}
	var repository db.Repository
	if err := s.DBForCtx(ctx).Where("full_name = ?", request.Repository).First(&repository).Error; err != nil {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: repository is unavailable", ErrNotFound)
	}
	var pr db.PullRequest
	if err := s.DBForCtx(ctx).Preload("Repository").Where("repository_id = ? AND number = ?", repository.ID, request.AGSPRNumber).First(&pr).Error; err != nil {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: pull request is unavailable", ErrNotFound)
	}
	if pr.Repository.FullName == "" || pr.State != db.StateOpen || pr.Merged {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: pull request is unavailable", ErrForbidden)
	}
	projection, ok := s.forgejoProjectionForPullRequest(ctx, pr.ID)
	if !ok || projection.Provider != ProjectionProviderForgejo || projection.ExternalNumber != request.ForgejoPRNumber {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: Forgejo projection is unavailable", ErrForbidden)
	}
	if !exactGitSHA(pr.HeadSHA, request.ExpectedHeadSHA) {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: expected head does not match current pull request", ErrForbidden)
	}
	teamID, policyClass, authorityRev := "", "", ""
	membershipEpoch := int64(0)
	var agentSessionID *string
	if _, delegated := DelegatedSessionIDFromContext(ctx); delegated {
		fresh, err := s.RevalidateDelegatedSession(ctx, pr.RepositoryID, "pr.rebase", "repo:write", forgejoRebaseSessionConstraints(request.AGSPRNumber, request.ForgejoPRNumber, request.ExpectedHeadSHA, request.ExpectedBaseSHA))
		if err != nil || fresh.Principal.ID != viewer.ID {
			return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: delegated action authority is invalid", ErrForbidden)
		}
		session := fresh.Session
		teamID, policyClass, membershipEpoch = session.TeamIdentityID, session.PolicyClass, session.MembershipEpoch
		authorityRev = session.PolicySnapshotHash
		agentSessionID = &session.ID
	} else {
		result, err := s.AuthorizeDurableOperation(ctx, viewer, "ags", pr.Repository.FullName, "pr.rebase", forgejoRebaseDurableConstraints(request.AGSPRNumber, request.ForgejoPRNumber, request.ExpectedHeadSHA, request.ExpectedBaseSHA))
		if err != nil || !result.Authorized {
			return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: action authority denied", ErrForbidden)
		}
		teamID, policyClass, membershipEpoch = result.AuthorizationBasis.TeamIdentityID, result.AuthorizationBasis.PolicyClass, result.AuthorizationBasis.MembershipEpoch
		authorityRev = durableActionAuthorityRevision(result)
	}
	labels, checked, err := s.ForgejoIntegration.ListPullRequestLabels(ctx, projection.ExternalRepo, projection.ExternalNumber)
	if err != nil || !checked || !sameActionLabels(preLabels, labels) {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: Forgejo exact pre-label state is unavailable or changed", ErrForbidden)
	}
	expiresIn := request.ExpiresIn
	if expiresIn <= 0 || expiresIn > 15*time.Minute {
		expiresIn = 15 * time.Minute
	}
	expiresIn = time.Duration(expiresIn/time.Second) * time.Second
	now := time.Now().UTC()
	boundaryProtocol := ""
	if agentSessionID != nil {
		boundaryProtocol = AuthorityBoundaryReceiptDelegatedEffectKind
	}
	intent := db.PullRequestActionIntent{ID: uuid.NewString(), IdempotencyKey: key, Action: "pr.rebase", State: ForgejoActionIntentPlanned, PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AGSPRNumber: pr.Number, Repository: pr.Repository.FullName, ForgejoRepo: projection.ExternalRepo, ForgejoPRNumber: projection.ExternalNumber, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, ExpectedHeadSHA: request.ExpectedHeadSHA, ExpectedBaseSHA: request.ExpectedBaseSHA, ExpectedLabels: preLabels, PostLabels: postLabels, PrincipalID: viewer.ID, AgentSessionID: agentSessionID, TeamIdentityID: teamID, PolicyClass: policyClass, MembershipEpoch: membershipEpoch, AuthorityRev: authorityRev, BoundaryProtocol: boundaryProtocol, ProviderEffectStatus: ProviderEffectStatusNotAttempted, RequestSource: "ags_client", ExpiresInSeconds: int64(expiresIn / time.Second), ExpiresAt: now.Add(expiresIn)}
	event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber, HeadBranch: pr.HeadRef, HeadSHA: request.ExpectedHeadSHA, BaseBranch: pr.BaseRef}
	preflight, err := s.preflightForgejoActionRebase(ctx, event, pr, intent)
	if err != nil || !exactGitSHA(preflight.BaseSHA, request.ExpectedBaseSHA) {
		return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: action preflight facts changed", ErrForbidden)
	}
	if err := s.commitForgejoActionIntent(ctx, &intent); err != nil {
		return ForgejoActionIntentReceipt{}, err
	}
	if intent.State != ForgejoActionIntentPlanned {
		return actionIntentReceipt(intent)
	}
	if intent.AgentSessionID != nil {
		if _, err := s.RevalidateDelegatedSession(ctx, intent.RepositoryID, "pr.rebase", "repo:write", forgejoRebaseSessionConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA)); err != nil {
			code := DelegatedSessionDenialReason(err)
			if stateErr := s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, code, "delegated authority changed before provider dispatch", ""); stateErr != nil {
				return ForgejoActionIntentReceipt{}, stateErr
			}
			intent.State, intent.FailureCode = ForgejoActionIntentDenied, code
			return actionIntentReceipt(intent)
		}
	}
	intent, err = s.beginForgejoActionIntentDispatch(ctx, intent.ID)
	if err != nil {
		return ForgejoActionIntentReceipt{}, err
	}
	if intent.State != ForgejoActionIntentDispatching {
		return actionIntentReceipt(intent)
	}
	if err := s.dispatchForgejoActionIntentLabel(ctx, intent, "delegated authority changed at initial provider label dispatch"); err != nil {
		if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
			if stateErr := s.recordForgejoActionIntentDispatchUncertain(ctx, intent.ID, "provider label dispatch outcome is unknown"); stateErr != nil {
				return ForgejoActionIntentReceipt{}, stateErr
			}
		}
		intent, loadErr := s.loadForgejoActionIntent(ctx, intent.ID)
		if loadErr != nil {
			return ForgejoActionIntentReceipt{}, loadErr
		}
		return actionIntentReceipt(intent)
	}
	intent, err = s.completeForgejoActionIntentDispatch(ctx, intent.ID)
	if err != nil {
		return ForgejoActionIntentReceipt{}, err
	}
	return actionIntentReceipt(intent)
}

func (s *Service) commitForgejoActionIntent(ctx context.Context, intent *db.PullRequestActionIntent) error {
	if intent == nil {
		return fmt.Errorf("%w: action intent is required", ErrValidation)
	}
	if !isValidGitSHA(intent.ExpectedHeadSHA) || !isValidGitSHA(intent.ExpectedBaseSHA) {
		return fmt.Errorf("%w: action intent expected SHAs are invalid", ErrValidation)
	}
	return s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		// The AGS PR row is the durable active-slot mutex. Every create path locks
		// it before expiry reconciliation, idempotency comparison and insertion.
		var lockedPR db.PullRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Repository").First(&lockedPR, intent.PullRequestID).Error; err != nil {
			return err
		}
		if lockedPR.RepositoryID != intent.RepositoryID || lockedPR.Number != intent.AGSPRNumber ||
			lockedPR.Repository.FullName != intent.Repository || lockedPR.State != db.StateOpen || lockedPR.Merged ||
			lockedPR.HeadRef != intent.HeadRef || lockedPR.BaseRef != intent.BaseRef ||
			!exactGitSHA(lockedPR.HeadSHA, intent.ExpectedHeadSHA) {
			return fmt.Errorf("%w: pull request facts changed before intent commit", ErrForbidden)
		}
		var lockedProjection db.PullRequestProjection
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("pull_request_id = ? AND provider = ?", lockedPR.ID, ProjectionProviderForgejo).First(&lockedProjection).Error; err != nil {
			return err
		}
		if lockedProjection.ExternalRepo != intent.ForgejoRepo || lockedProjection.ExternalNumber != intent.ForgejoPRNumber ||
			lockedProjection.SourceBranch != intent.HeadRef || lockedProjection.TargetBranch != intent.BaseRef ||
			lockedProjection.State != ProjectionStateOpen || !exactGitSHA(lockedProjection.LastSyncedSHA, intent.ExpectedHeadSHA) {
			return fmt.Errorf("%w: projection facts changed before intent commit", ErrForbidden)
		}
		txNow := time.Now().UTC()
		if err := expireForgejoActionIntents(tx, lockedPR.ID, txNow); err != nil {
			return err
		}
		var same db.PullRequestActionIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("idempotency_key = ?", intent.IdempotencyKey).First(&same).Error; err == nil {
			if !sameForgejoActionIntentFacts(same, *intent) {
				return fmt.Errorf("%w: idempotency key facts changed", ErrForbidden)
			}
			*intent = same
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var active int64
		if err := tx.Model(&db.PullRequestActionIntent{}).
			Where("pull_request_id = ? AND action = ? AND state IN ? AND expires_at > ?", lockedPR.ID, "pr.rebase", forgejoActionActiveStates(), txNow).
			Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return fmt.Errorf("%w: another action intent is active", ErrForbidden)
		}
		return tx.Create(intent).Error
	})
}

func (s *Service) dispatchForgejoActionIntentLabel(ctx context.Context, intent db.PullRequestActionIntent, denialSummary string) error {
	ctx = contextWithForgejoActionIntent(ctx, intent.ID)
	err := s.addForgejoPullRequestLabels(ctx, intent.ForgejoRepo, intent.ForgejoPRNumber, []string{forgejointegration.AGSActionRebaseLabel})
	if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		_ = s.terminalDenyForgejoAction(ctx, intent.ID, 0, DelegatedSessionDenialReason(err), denialSummary, "")
	}
	return err
}

func (s *Service) loadForgejoActionIntent(ctx context.Context, id string) (db.PullRequestActionIntent, error) {
	var intent db.PullRequestActionIntent
	if err := s.DBForCtx(ctx).First(&intent, "id = ?", strings.TrimSpace(id)).Error; err != nil {
		return db.PullRequestActionIntent{}, err
	}
	return intent, nil
}

// beginForgejoActionIntentDispatch is the sole pre-effect transaction kernel.
// Lock order is PullRequest -> PullRequestProjection -> PullRequestActionIntent
// -> receipt insert. For delegated intents receipt insert/link, dispatching and
// outcome_unknown commit atomically. The provider remains outside this
// transaction and is eligible only after base-DB receipt readback succeeds.
func (s *Service) beginForgejoActionIntentDispatch(ctx context.Context, id string) (db.PullRequestActionIntent, error) {
	intentID := strings.TrimSpace(id)
	if intentID == "" {
		return db.PullRequestActionIntent{}, fmt.Errorf("Forgejo action intent ID is required")
	}
	budgetCtx, cancel := context.WithTimeout(ctx, authorityBoundaryTransactionBudget)
	defer cancel()
	database := s.DBForCtx(budgetCtx).WithContext(budgetCtx)
	var lastErr error
	for attempt := 0; attempt < authorityBoundaryTransactionAttempts; attempt++ {
		var candidate db.PullRequestActionIntent
		run := func(tx *gorm.DB) error {
			var coordinate db.PullRequestActionIntent
			if err := tx.Select("pull_request_id").First(&coordinate, "id = ?", intentID).Error; err != nil {
				return err
			}
			var lockedPR db.PullRequest
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedPR, coordinate.PullRequestID).Error; err != nil {
				return err
			}
			var lockedProjection db.PullRequestProjection
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("pull_request_id = ? AND provider = ?", lockedPR.ID, ProjectionProviderForgejo).First(&lockedProjection).Error; err != nil {
				return err
			}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&candidate, "id = ?", intentID).Error; err != nil {
				return err
			}
			if candidate.Action != "pr.rebase" || candidate.PullRequestID != lockedPR.ID || candidate.RepositoryID != lockedPR.RepositoryID ||
				candidate.AGSPRNumber != lockedPR.Number || candidate.HeadRef != lockedPR.HeadRef || candidate.BaseRef != lockedPR.BaseRef ||
				lockedPR.State != db.StateOpen || lockedPR.Merged || !exactGitSHA(candidate.ExpectedHeadSHA, lockedPR.HeadSHA) ||
				candidate.ForgejoRepo != lockedProjection.ExternalRepo || candidate.ForgejoPRNumber != lockedProjection.ExternalNumber ||
				candidate.HeadRef != lockedProjection.SourceBranch || candidate.BaseRef != lockedProjection.TargetBranch ||
				lockedProjection.State != ProjectionStateOpen || !exactGitSHA(candidate.ExpectedHeadSHA, lockedProjection.LastSyncedSHA) {
				return fmt.Errorf("Forgejo action dispatch coordinate changed")
			}
			if candidate.State != ForgejoActionIntentPlanned {
				if candidate.State == ForgejoActionIntentDispatching || forgejoActionStateAfterDispatch(candidate.State) {
					return nil
				}
				return fmt.Errorf("Forgejo action intent is not dispatchable from state %s", candidate.State)
			}
			if !candidate.ExpiresAt.After(time.Now().UTC()) {
				return fmt.Errorf("Forgejo action intent expired before dispatch")
			}
			updates := map[string]any{
				"state": ForgejoActionIntentDispatching, "provider_effect_status": ProviderEffectStatusOutcomeUnknown,
				"failure_code": "", "failure_summary": "",
			}
			if candidate.AgentSessionID != nil {
				if candidate.BoundaryProtocol != "" && candidate.BoundaryProtocol != AuthorityBoundaryReceiptDelegatedEffectKind {
					return ErrAuthorityBoundaryReceiptCorrupt
				}
				txCtx := ContextWithDB(budgetCtx, tx.WithContext(budgetCtx))
				receipt, err := s.captureDelegatedEffectBoundaryForActionTx(txCtx, candidate)
				if err != nil {
					return err
				}
				if candidate.BoundaryReceiptID != nil && strings.TrimSpace(*candidate.BoundaryReceiptID) != receipt.ReceiptID {
					return ErrAuthorityBoundaryReceiptCorrupt
				}
				candidate.BoundaryProtocol = AuthorityBoundaryReceiptDelegatedEffectKind
				candidate.BoundaryReceiptID = &receipt.ReceiptID
				updates["boundary_protocol"] = candidate.BoundaryProtocol
				updates["boundary_receipt_id"] = receipt.ReceiptID
			} else if candidate.BoundaryProtocol != "" || candidate.BoundaryReceiptID != nil {
				return ErrAuthorityBoundaryReceiptCorrupt
			}
			update := tx.Model(&db.PullRequestActionIntent{}).Where("id = ? AND state = ?", candidate.ID, ForgejoActionIntentPlanned).Updates(updates)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return fmt.Errorf("Forgejo action dispatch generation changed")
			}
			candidate.State = ForgejoActionIntentDispatching
			candidate.ProviderEffectStatus = ProviderEffectStatusOutcomeUnknown
			return nil
		}
		if s.testAuthorityBoundaryTransaction != nil {
			lastErr = s.testAuthorityBoundaryTransaction(attempt, run)
		} else {
			lastErr = database.Transaction(run)
		}
		baseCtx := ContextWithDB(budgetCtx, database)
		if lastErr == nil {
			return s.readBackForgejoActionDispatchEligibility(baseCtx, intentID)
		}
		if authorityBoundaryReceiptRetryable(lastErr) {
			if committed, readErr := s.readBackForgejoActionDispatchEligibility(baseCtx, intentID); readErr == nil {
				return committed, nil
			}
		}
		if !authorityBoundaryReceiptRetryable(lastErr) || attempt+1 == authorityBoundaryTransactionAttempts {
			return db.PullRequestActionIntent{}, lastErr
		}
		select {
		case <-budgetCtx.Done():
			return db.PullRequestActionIntent{}, budgetCtx.Err()
		case <-time.After(retryDelay(attempt)):
		}
	}
	return db.PullRequestActionIntent{}, lastErr
}

func (s *Service) readBackForgejoActionDispatchEligibility(ctx context.Context, intentID string) (db.PullRequestActionIntent, error) {
	intent, err := s.loadForgejoActionIntent(ctx, intentID)
	if err != nil {
		return db.PullRequestActionIntent{}, err
	}
	if intent.State != ForgejoActionIntentDispatching && !forgejoActionStateAfterDispatch(intent.State) {
		return db.PullRequestActionIntent{}, fmt.Errorf("Forgejo action intent is not dispatch eligible from state %s", intent.State)
	}
	if intent.AgentSessionID == nil {
		if intent.BoundaryProtocol != "" || intent.BoundaryReceiptID != nil {
			return db.PullRequestActionIntent{}, ErrAuthorityBoundaryReceiptCorrupt
		}
		return intent, nil
	}
	if intent.State == ForgejoActionIntentDenied && intent.ProviderEffectStatus == ProviderEffectStatusNotAttempted &&
		intent.BoundaryProtocol == AuthorityBoundaryReceiptDelegatedEffectKind && intent.BoundaryReceiptID == nil {
		return intent, nil
	}
	if intent.ProviderEffectStatus != ProviderEffectStatusOutcomeUnknown && intent.ProviderEffectStatus != ProviderEffectStatusVerifiedCompleted {
		return db.PullRequestActionIntent{}, ErrAuthorityBoundaryReceiptCorrupt
	}
	if _, err := s.validateDelegatedEffectBoundaryForAction(ctx, intent); err != nil {
		return db.PullRequestActionIntent{}, err
	}
	return intent, nil
}

func (s *Service) recordForgejoActionIntentDispatchUncertain(ctx context.Context, id, summary string) error {
	result := s.DBForCtx(ctx).Model(&db.PullRequestActionIntent{}).
		Where("id = ? AND state = ?", strings.TrimSpace(id), ForgejoActionIntentDispatching).
		Updates(map[string]any{"failure_code": "dispatch_outcome_unknown", "failure_summary": summary})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	current, err := s.loadForgejoActionIntent(ctx, id)
	if err != nil {
		return err
	}
	if forgejoActionStateAfterDispatch(current.State) {
		return nil
	}
	return fmt.Errorf("Forgejo action dispatch uncertainty lost generation ownership in state %s", current.State)
}

func (s *Service) completeForgejoActionIntentDispatch(ctx context.Context, id string) (db.PullRequestActionIntent, error) {
	intentID := strings.TrimSpace(id)
	dispatchedAt := time.Now().UTC()
	result := s.DBForCtx(ctx).Model(&db.PullRequestActionIntent{}).
		Where("id = ? AND state = ?", intentID, ForgejoActionIntentDispatching).
		Updates(map[string]any{"state": ForgejoActionIntentDispatched, "dispatched_at": &dispatchedAt, "failure_code": "", "failure_summary": ""})
	if result.Error != nil {
		return db.PullRequestActionIntent{}, result.Error
	}
	if result.RowsAffected > 1 {
		return db.PullRequestActionIntent{}, fmt.Errorf("Forgejo action dispatch acknowledgement updated multiple intents")
	}
	if result.RowsAffected == 1 && s.testForgejoActionDispatchAfterCAS != nil {
		s.testForgejoActionDispatchAfterCAS(intentID)
	}
	current, err := s.loadForgejoActionIntent(ctx, intentID)
	if err != nil {
		return db.PullRequestActionIntent{}, err
	}
	// The provider webhook may legitimately advance the exact intent after the
	// acknowledgement CAS and before this readback. Both CAS outcomes therefore
	// converge through the same monotonic-state check; neither may overwrite or
	// reject a valid later generation state.
	if current.ID == intentID && forgejoActionStateAfterDispatch(current.State) {
		return current, nil
	}
	return db.PullRequestActionIntent{}, fmt.Errorf("Forgejo action dispatch acknowledgement lost generation ownership in state %s", current.State)
}

func forgejoActionStateAfterDispatch(state string) bool {
	return stringInSlice(state, []string{
		ForgejoActionIntentDispatched, ForgejoActionIntentAccepted, ForgejoActionIntentRunning,
		ForgejoActionIntentCompleted, ForgejoActionIntentDenied, ForgejoActionIntentConflict,
		ForgejoActionIntentProjection, ForgejoActionIntentRecovery,
	})
}

func delegatedActionFailureCode(err error, fallback string) string {
	if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		return DelegatedSessionDenialReason(err)
	}
	return fallback
}

func sameOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func forgejoActionActiveStates() []string {
	return []string{ForgejoActionIntentPlanned, ForgejoActionIntentDispatching, ForgejoActionIntentDispatched, ForgejoActionIntentAccepted, ForgejoActionIntentRunning, ForgejoActionIntentRecovery}
}

func forgejoActionDispatchStates() []string {
	return []string{ForgejoActionIntentDispatching, ForgejoActionIntentDispatched}
}

func sameForgejoActionIntentFacts(left, right db.PullRequestActionIntent) bool {
	return left.IdempotencyKey == right.IdempotencyKey && left.Action == right.Action &&
		left.PullRequestID == right.PullRequestID && left.RepositoryID == right.RepositoryID && left.AGSPRNumber == right.AGSPRNumber &&
		left.Repository == right.Repository && left.ForgejoRepo == right.ForgejoRepo && left.ForgejoPRNumber == right.ForgejoPRNumber &&
		left.HeadRef == right.HeadRef && left.BaseRef == right.BaseRef && left.ExpectedHeadSHA == right.ExpectedHeadSHA &&
		left.ExpectedBaseSHA == right.ExpectedBaseSHA && left.ExpectedLabels == right.ExpectedLabels && left.PostLabels == right.PostLabels &&
		left.PrincipalID == right.PrincipalID && sameOptionalString(left.AgentSessionID, right.AgentSessionID) &&
		left.TeamIdentityID == right.TeamIdentityID && left.PolicyClass == right.PolicyClass && left.MembershipEpoch == right.MembershipEpoch &&
		left.AuthorityRev == right.AuthorityRev && left.RequestSource == right.RequestSource && left.RequestActor == right.RequestActor && left.RequestBindingRev == right.RequestBindingRev &&
		left.ExpiresInSeconds == right.ExpiresInSeconds
}

func expireForgejoActionIntents(tx *gorm.DB, pullRequestID uint, now time.Time) error {
	return tx.Model(&db.PullRequestActionIntent{}).
		Where("pull_request_id = ? AND action = ? AND state IN ? AND expires_at <= ?", pullRequestID, "pr.rebase", forgejoActionActiveStates(), now).
		Updates(map[string]any{"state": ForgejoActionIntentDenied, "failure_code": "expired", "failure_summary": "intent expired before exact action admission", "finished_at": &now}).Error
}

func (s *Service) actionIntentState(ctx context.Context, id, state, code, summary, result string) error {
	now := time.Now().UTC()
	// This compatibility transition helper is monotonic: it may move only an
	// active intent. Atomic success and provider denial have dedicated kernels
	// and can never be overwritten by a late webhook/error path.
	update := s.DBForCtx(ctx).Model(&db.PullRequestActionIntent{}).
		Where("id = ? AND state IN ?", id, forgejoActionActiveStates()).
		Updates(map[string]any{"state": state, "failure_code": code, "failure_summary": summary, "result_sha": result, "finished_at": &now})
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return fmt.Errorf("Forgejo action intent transition lost generation ownership")
	}
	return nil
}

// terminalDenyForgejoAction records a delegated provider-admission denial as
// one AGS-owned transaction. Jobs are bound to one exact action intent; this
// function never searches for the latest intent of a PR. Historical unbound
// jobs are terminalized locally without mutating any intent.
func (s *Service) terminalDenyForgejoAction(ctx context.Context, intentID string, jobID uint, code, summary, resultSHA string) error {
	now := time.Now().UTC()
	binding, hasBinding := forgejoActionBindingFromContext(ctx)
	intentID = strings.TrimSpace(intentID)
	if hasBinding && intentID != "" && intentID != binding.IntentID {
		return fmt.Errorf("Forgejo action denial intent binding mismatch")
	}
	return s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		var lockedJob *db.PullRequestProjectionJob
		if jobID != 0 {
			var job db.PullRequestProjectionJob
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&job, jobID).Error; err != nil {
				return err
			}
			if hasBinding && binding.JobID != 0 && !sameActionIntentJobBinding(job, binding) {
				// The row now belongs to another generation. Deny only the exact old
				// intent carried by the stale worker context; never touch the new row.
				intentID = binding.IntentID
				jobID = 0
			} else {
				lockedJob = &job
			}
			if lockedJob != nil && job.ActionIntentID != nil {
				jobIntentID := strings.TrimSpace(*job.ActionIntentID)
				if intentID != "" && intentID != jobIntentID {
					return fmt.Errorf("Forgejo action intent/job binding mismatch")
				}
				intentID = jobIntentID
			} else if lockedJob != nil {
				// Historical jobs have no trustworthy authority link. Terminalize the
				// job itself below, but never apply a caller-supplied/latest intent.
				intentID = ""
			}
		}
		if intentID == "" && hasBinding && (lockedJob == nil || lockedJob.ActionIntentID != nil) {
			intentID = binding.IntentID
		}

		var lockedIntent *db.PullRequestActionIntent
		if intentID != "" {
			// When no particular job was supplied, lock only jobs bound to this exact
			// intent before locking the intent, preserving the completion lock order.
			if lockedJob == nil {
				var jobs []db.PullRequestProjectionJob
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
					Where("action_intent_id = ?", intentID).
					Where(clause.Eq{Column: "trigger", Value: ForgejoProjectionTriggerActionRebase}).
					Order("id ASC").Find(&jobs).Error; err != nil {
					return err
				}
			}
			var intent db.PullRequestActionIntent
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&intent, "id = ?", intentID).Error; err != nil {
				return err
			}
			if lockedJob != nil && lockedJob.ActionIntentID != nil &&
				(lockedJob.PullRequestID != intent.PullRequestID || lockedJob.RepositoryID != intent.RepositoryID ||
					!sameOptionalString(lockedJob.AgentSessionID, intent.AgentSessionID)) {
				return fmt.Errorf("Forgejo action intent/job authority facts mismatch")
			}
			lockedIntent = &intent
		}

		if lockedIntent != nil && lockedIntent.State != ForgejoActionIntentCompleted {
			if err := tx.Model(&db.PullRequestActionIntent{}).
				Where("id = ? AND state <> ?", lockedIntent.ID, ForgejoActionIntentCompleted).
				Updates(map[string]any{"state": ForgejoActionIntentDenied, "failure_code": code, "failure_summary": summary, "result_sha": resultSHA, "finished_at": &now}).Error; err != nil {
				return err
			}
		}
		if s.testForgejoTerminalDenialBeforeCommit != nil {
			s.testForgejoTerminalDenialBeforeCommit()
		}

		jobs := tx.Model(&db.PullRequestProjectionJob{}).
			Where(clause.Eq{Column: "trigger", Value: ForgejoProjectionTriggerActionRebase}).
			Where("phase <> ?", ForgejoProjectionPhaseProjected)
		switch {
		case lockedJob != nil && lockedJob.ActionIntentID == nil:
			jobs = jobs.Where("id = ? AND action_intent_id IS NULL", lockedJob.ID)
		case lockedJob != nil:
			jobs = jobs.Where("id = ? AND action_generation = ? AND action_intent_id = ?", lockedJob.ID, lockedJob.ActionGeneration, intentID)
		case intentID != "":
			jobs = jobs.Where("action_intent_id = ?", intentID)
		default:
			return fmt.Errorf("Forgejo action denial has no exact job or intent binding")
		}
		jobUpdate := jobs.Updates(map[string]any{
			"phase": ForgejoProjectionPhaseFailedTerminal, "last_error_type": ProjectionFailureProviderAdmissionDenied,
			"last_error": code, "next_run_at": nil, "finished_at": &now,
			"success_comment_claim_token": "", "success_comment_claimed_at": nil, "success_commented_at": nil,
		})
		if jobUpdate.Error != nil {
			return jobUpdate.Error
		}
		if lockedJob != nil && jobUpdate.RowsAffected != 1 {
			return fmt.Errorf("Forgejo action denial job generation changed")
		}
		return nil
	})
}

func (s *Service) GetForgejoActionIntent(ctx context.Context, viewer db.User, repo string, number int, id string) (ForgejoActionIntentReceipt, error) {
	var intent db.PullRequestActionIntent
	err := s.DBForCtx(ctx).Where("repository = ? AND ags_pr_number = ? AND id = ?", repo, number, strings.TrimSpace(id)).First(&intent).Error
	if err != nil {
		return ForgejoActionIntentReceipt{}, err
	}
	if intent.AgentSessionID != nil {
		sessionID, delegated := DelegatedSessionIDFromContext(ctx)
		if !delegated || sessionID == "" || sessionID != strings.TrimSpace(*intent.AgentSessionID) {
			return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: action receipt is unavailable", ErrForbidden)
		}
		fresh, err := s.RevalidateDelegatedSession(ctx, intent.RepositoryID, "pr.rebase", "repo:write", forgejoRebaseSessionConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA))
		if err != nil || fresh.Principal.ID != viewer.ID || fresh.Session.ID != sessionID {
			return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: action receipt is unavailable", ErrForbidden)
		}
	} else {
		if _, delegated := DelegatedSessionIDFromContext(ctx); delegated || viewer.ID != intent.PrincipalID || s.authorizeCurrentDurableActionIntent(ctx, intent) != nil {
			return ForgejoActionIntentReceipt{}, fmt.Errorf("%w: action receipt is unavailable", ErrForbidden)
		}
	}
	return actionIntentReceipt(intent)
}

type forgejoActionIntentDenial string

const (
	forgejoActionIntentDenialNone             forgejoActionIntentDenial = ""
	forgejoActionIntentDenialMissingAuthority forgejoActionIntentDenial = "missing_authority"
	forgejoActionIntentDenialFactMismatch     forgejoActionIntentDenial = "fact_mismatch"
	forgejoActionIntentDenialDelegated        forgejoActionIntentDenial = "delegated_authority"
	forgejoActionIntentDenialLabelDrift       forgejoActionIntentDenial = "label_drift"
	forgejoActionIntentDenialConsumed         forgejoActionIntentDenial = "already_consumed"
)

// consumeForgejoActionIntent makes the webhook an event adapter only. It does
// not use sender login, Forgejo roles, or provider admin permissions as authority.
func (s *Service) consumeForgejoActionIntent(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent) (db.PullRequestActionIntent, bool, forgejoActionIntentDenial, error) {
	database := s.DBForCtx(ctx)
	var candidatePRIDs []uint
	if err := database.Model(&db.PullRequestActionIntent{}).Distinct("pull_request_id").
		Where("forgejo_repo = ? AND forgejo_pr_number = ? AND action = ? AND state IN ?", strings.TrimSpace(event.RepoFullName), event.PRNumber, "pr.rebase", forgejoActionDispatchStates()).
		Pluck("pull_request_id", &candidatePRIDs).Error; err != nil {
		return db.PullRequestActionIntent{}, false, forgejoActionIntentDenialNone, fmt.Errorf("locate exact action intent: %w", err)
	}
	if len(candidatePRIDs) == 0 {
		return db.PullRequestActionIntent{}, false, forgejoActionIntentDenialMissingAuthority, nil
	}
	if len(candidatePRIDs) != 1 {
		sort.Slice(candidatePRIDs, func(i, j int) bool { return candidatePRIDs[i] < candidatePRIDs[j] })
		err := database.Transaction(func(tx *gorm.DB) error {
			now := time.Now().UTC()
			for _, pullRequestID := range candidatePRIDs {
				var pr db.PullRequest
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&pr, pullRequestID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if pr.ID != 0 {
					if err := expireForgejoActionIntents(tx, pr.ID, now); err != nil {
						return err
					}
				}
			}
			return tx.Model(&db.PullRequestActionIntent{}).
				Where("forgejo_repo = ? AND forgejo_pr_number = ? AND action = ? AND state IN ?", strings.TrimSpace(event.RepoFullName), event.PRNumber, "pr.rebase", forgejoActionDispatchStates()).
				Updates(map[string]any{"state": ForgejoActionIntentDenied, "failure_code": "ambiguous_active_intent", "failure_summary": "provider coordinate matched multiple AGS pull requests", "finished_at": &now}).Error
		})
		if err != nil {
			return db.PullRequestActionIntent{}, false, forgejoActionIntentDenialNone, fmt.Errorf("terminalize ambiguous action intents: %w", err)
		}
		return db.PullRequestActionIntent{}, false, forgejoActionIntentDenialFactMismatch, nil
	}

	var intent db.PullRequestActionIntent
	denial := forgejoActionIntentDenialNone
	err := database.Transaction(func(tx *gorm.DB) error {
		var lockedPR db.PullRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&lockedPR, candidatePRIDs[0]).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				now := time.Now().UTC()
				if updateErr := tx.Model(&db.PullRequestActionIntent{}).
					Where("pull_request_id = ? AND forgejo_repo = ? AND forgejo_pr_number = ? AND action = ? AND state IN ?", candidatePRIDs[0], strings.TrimSpace(event.RepoFullName), event.PRNumber, "pr.rebase", forgejoActionDispatchStates()).
					Updates(map[string]any{"state": ForgejoActionIntentDenied, "failure_code": "projection_drift", "failure_summary": "intent pull request no longer exists", "finished_at": &now}).Error; updateErr != nil {
					return updateErr
				}
				denial = forgejoActionIntentDenialFactMismatch
				return nil
			}
			return err
		}
		now := time.Now().UTC()
		if err := expireForgejoActionIntents(tx, lockedPR.ID, now); err != nil {
			return err
		}
		var intents []db.PullRequestActionIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("pull_request_id = ? AND forgejo_repo = ? AND forgejo_pr_number = ? AND action = ? AND state IN ? AND expires_at > ?", lockedPR.ID, strings.TrimSpace(event.RepoFullName), event.PRNumber, "pr.rebase", forgejoActionDispatchStates(), now).
			Order("created_at ASC, id ASC").Limit(3).Find(&intents).Error; err != nil {
			return err
		}
		if len(intents) == 0 {
			denial = forgejoActionIntentDenialMissingAuthority
			return nil
		}
		if len(intents) != 1 {
			ids := make([]string, 0, len(intents))
			for _, duplicate := range intents {
				ids = append(ids, duplicate.ID)
			}
			if err := tx.Model(&db.PullRequestActionIntent{}).Where("id IN ? AND state IN ?", ids, forgejoActionDispatchStates()).
				Updates(map[string]any{"state": ForgejoActionIntentDenied, "failure_code": "ambiguous_active_intent", "failure_summary": "multiple dispatchable intents matched one provider event", "finished_at": &now}).Error; err != nil {
				return err
			}
			intent = intents[0]
			denial = forgejoActionIntentDenialFactMismatch
			return nil
		}
		intent = intents[0]
		if intent.PullRequestID != lockedPR.ID || intent.RepositoryID != lockedPR.RepositoryID || intent.AGSPRNumber != lockedPR.Number ||
			intent.HeadRef != strings.TrimSpace(event.HeadBranch) || intent.BaseRef != strings.TrimSpace(event.BaseBranch) ||
			!exactGitSHA(intent.ExpectedHeadSHA, event.HeadSHA) {
			if err := tx.Model(&db.PullRequestActionIntent{}).Where("id = ? AND state IN ?", intent.ID, forgejoActionDispatchStates()).
				Updates(map[string]any{"state": ForgejoActionIntentDenied, "failure_code": "webhook_mismatch", "failure_summary": "webhook facts do not match intent", "finished_at": &now}).Error; err != nil {
				return err
			}
			denial = forgejoActionIntentDenialFactMismatch
			return nil
		}
		res := tx.Model(&db.PullRequestActionIntent{}).Where("id = ? AND state IN ?", intent.ID, forgejoActionDispatchStates()).
			Updates(map[string]any{"state": ForgejoActionIntentAccepted, "accepted_at": &now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			denial = forgejoActionIntentDenialConsumed
			return nil
		}
		intent.State = ForgejoActionIntentAccepted
		return nil
	})
	if err != nil {
		return intent, false, forgejoActionIntentDenialNone, fmt.Errorf("consume exact action intent: %w", err)
	}
	if denial != forgejoActionIntentDenialNone {
		return intent, false, denial, nil
	}
	if intent.AgentSessionID != nil {
		delegatedCtx, err := s.ContextForDelegatedSessionID(ctx, *intent.AgentSessionID)
		if err == nil {
			_, err = s.RevalidateDelegatedSession(delegatedCtx, intent.RepositoryID, "pr.rebase", "repo:write", forgejoRebaseSessionConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA))
		}
		if err != nil {
			code := DelegatedSessionDenialReason(err)
			_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, code, "delegated authority changed before webhook provider read", "")
			return intent, false, forgejoActionIntentDenialDelegated, nil
		}
	}
	labels, checked, err := s.ForgejoIntegration.ListPullRequestLabels(ctx, intent.ForgejoRepo, intent.ForgejoPRNumber)
	if err != nil || !checked || !sameActionLabels(intent.PostLabels, labels) {
		_ = s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "label_drift", "Forgejo labels do not match intent", "")
		return intent, false, forgejoActionIntentDenialLabelDrift, nil
	}
	return intent, true, forgejoActionIntentDenialNone, nil
}

func (s *Service) lockForgejoActionIntentForRecovery(ctx context.Context, candidate db.PullRequestActionIntent) (db.PullRequestActionIntent, bool, error) {
	var current db.PullRequestActionIntent
	active := false
	err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		var pr db.PullRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&pr, candidate.PullRequestID).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := expireForgejoActionIntents(tx, pr.ID, now); err != nil {
			return err
		}
		var activeIntents []db.PullRequestActionIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("pull_request_id = ? AND action = ? AND state IN ? AND expires_at > ?", pr.ID, "pr.rebase", forgejoActionActiveStates(), now).
			Order("created_at ASC, id ASC").Limit(3).Find(&activeIntents).Error; err != nil {
			return err
		}
		if len(activeIntents) > 1 {
			ids := make([]string, 0, len(activeIntents))
			for _, duplicate := range activeIntents {
				ids = append(ids, duplicate.ID)
			}
			if err := tx.Model(&db.PullRequestActionIntent{}).Where("id IN ? AND state IN ?", ids, forgejoActionActiveStates()).
				Updates(map[string]any{"state": ForgejoActionIntentDenied, "failure_code": "ambiguous_active_intent", "failure_summary": "multiple active intents matched one AGS pull request", "finished_at": &now}).Error; err != nil {
				return err
			}
		}
		if err := tx.First(&current, "id = ?", candidate.ID).Error; err != nil {
			return err
		}
		active = len(activeIntents) == 1 && activeIntents[0].ID == current.ID && stringInSlice(current.State, forgejoActionActiveStates()) && current.ExpiresAt.After(now)
		return nil
	})
	return current, active, err
}

func (s *Service) quarantineForgejoActionIntent(ctx context.Context, id, code, summary string) error {
	result := s.DBForCtx(ctx).Model(&db.PullRequestActionIntent{}).
		Where("id = ? AND state IN ?", strings.TrimSpace(id), forgejoActionActiveStates()).
		Updates(map[string]any{"state": ForgejoActionIntentRecovery, "failure_code": code, "failure_summary": summary})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		current, err := s.loadForgejoActionIntent(ctx, id)
		if err != nil {
			return err
		}
		if current.State == ForgejoActionIntentRecovery && current.FailureCode == code {
			return nil
		}
		return fmt.Errorf("Forgejo action intent quarantine lost generation ownership")
	}
	return nil
}

// RecoverDurableActionIntents provides deterministic restart recovery for
// durable intents interrupted between commit, label dispatch, webhook and
// effect. It must be called once at service startup (or composition startup)
// and must be bounded, restart-safe and transactionally monotonic.
//
// Recovery rules:
//   - Expired/terminal intents are never resurrected.
//   - Non-terminal expired intents are marked denied.
//   - "planned" intents: atomically claim dispatching before any provider call.
//   - "dispatching" intents: idempotently ensure the provider label; an early
//     webhook may consume this durable authority before the provider call returns.
//   - "dispatched" intents: left safely pending for webhook/reconciliation.
//   - "accepted","running","recovery" intents: if expired, denied;
//     otherwise left pending for webhook/reconciliation or next job attempt.
func (s *Service) RecoverDurableActionIntents(ctx context.Context) error {
	if s == nil || s.ForgejoIntegration == nil {
		return nil
	}
	now := time.Now().UTC()
	// Historical action jobs predate exact intent binding. They are preserved for
	// audit but may never resume or select a newer intent.
	if err := s.DBForCtx(ctx).Model(&db.PullRequestProjectionJob{}).
		Where(clause.Eq{Column: "trigger", Value: ForgejoProjectionTriggerActionRebase}).
		Where("(action_intent_id IS NULL OR action_intent_id = ?) AND phase NOT IN ?", "", []string{ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal}).
		Updates(map[string]any{
			"phase": ForgejoProjectionPhaseFailedTerminal, "last_error_type": ProjectionFailureProviderAdmissionDenied,
			"last_error": "historical rebase job has no exact action intent binding", "next_run_at": nil, "finished_at": &now,
		}).Error; err != nil {
		return fmt.Errorf("terminalize unbound action jobs: %w", err)
	}
	var intents []db.PullRequestActionIntent
	if err := s.DBForCtx(ctx).
		Where("action = ? AND state IN ?", "pr.rebase", forgejoActionActiveStates()).
		Find(&intents).Error; err != nil {
		return fmt.Errorf("recover durable action intents: %w", err)
	}
	for _, candidate := range intents {
		intent, active, err := s.lockForgejoActionIntentForRecovery(ctx, candidate)
		if err != nil {
			return fmt.Errorf("lock action intent %s for recovery: %w", candidate.ID, err)
		}
		if !active {
			if intent.FailureCode == "expired" || intent.FailureCode == "ambiguous_active_intent" {
				if err := s.terminalDenyForgejoAction(contextWithForgejoActionIntent(ctx, intent.ID), intent.ID, 0, intent.FailureCode, "intent denied before recovery", ""); err != nil {
					return fmt.Errorf("terminalize inactive action intent %s: %w", intent.ID, err)
				}
			}
			continue
		}
		providerCtx := ctx
		if intent.AgentSessionID == nil {
			if err := s.authorizeCurrentDurableActionIntent(ctx, intent); err != nil {
				if terminalErr := s.terminalDenyForgejoAction(contextWithForgejoActionIntent(ctx, intent.ID), intent.ID, 0, DelegatedDenialAuthoritySnapshotChanged, "durable principal authority changed during startup recovery", ""); terminalErr != nil {
					return fmt.Errorf("deny recovery action intent %s: %w", intent.ID, terminalErr)
				}
				continue
			}
		} else {
			// Historical delegated dispatching+ rows cannot prove that a receipt
			// preceded the old provider effect. Never synthesize that history.
			if intent.State != ForgejoActionIntentPlanned && intent.BoundaryProtocol == "" {
				if err := s.quarantineForgejoActionIntent(ctx, intent.ID, "boundary_receipt_history_unproven", "historical provider effect has no proven pre-effect Boundary Receipt"); err != nil {
					return err
				}
				continue
			}
			if intent.BoundaryProtocol != "" && intent.BoundaryProtocol != AuthorityBoundaryReceiptDelegatedEffectKind {
				if err := s.quarantineForgejoActionIntent(ctx, intent.ID, "boundary_receipt_integrity_failure", "delegated action Boundary protocol is invalid"); err != nil {
					return err
				}
				continue
			}
			if intent.State != ForgejoActionIntentPlanned &&
				(intent.BoundaryReceiptID == nil || !isCanonicalReceiptID(strings.TrimSpace(*intent.BoundaryReceiptID)) ||
					(intent.ProviderEffectStatus != ProviderEffectStatusOutcomeUnknown && intent.ProviderEffectStatus != ProviderEffectStatusVerifiedCompleted)) {
				if err := s.quarantineForgejoActionIntent(ctx, intent.ID, "boundary_receipt_integrity_failure", "delegated action Boundary Receipt lineage is incomplete"); err != nil {
					return err
				}
				continue
			}
			providerCtx, err = s.ContextForDelegatedSessionID(ctx, *intent.AgentSessionID)
			if err == nil && intent.State != ForgejoActionIntentPlanned {
				// Integrity is checked before current-authority denial so a missing or
				// damaged new-protocol receipt cannot be misclassified as an ordinary
				// Session expiry/revocation.
				if _, receiptErr := s.validateDelegatedEffectBoundaryForAction(providerCtx, intent); receiptErr != nil {
					if quarantineErr := s.quarantineForgejoActionIntent(ctx, intent.ID, "boundary_receipt_integrity_failure", "delegated action Boundary Receipt readback failed"); quarantineErr != nil {
						return quarantineErr
					}
					continue
				}
			}
			if err == nil {
				_, err = s.RevalidateDelegatedSession(providerCtx, intent.RepositoryID, "pr.rebase", "repo:write", forgejoRebaseSessionConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA))
			}
			if err != nil {
				code := DelegatedSessionDenialReason(err)
				if stateErr := s.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, code, "delegated authority changed before recovery dispatch", ""); stateErr != nil {
					return fmt.Errorf("deny recovery action intent %s: %w", intent.ID, stateErr)
				}
				continue
			}
		}
		switch intent.State {
		case ForgejoActionIntentPlanned, ForgejoActionIntentDispatching:
			// Re-attempt label dispatch idempotently under exact intent facts and
			// fresh originating Session authority. Planned must atomically capture
			// and link its receipt before becoming dispatching.
			if intent.State == ForgejoActionIntentPlanned {
				intent, err = s.beginForgejoActionIntentDispatch(providerCtx, intent.ID)
				if err != nil {
					return fmt.Errorf("claim action intent %s dispatch: %w", candidate.ID, err)
				}
				if intent.State != ForgejoActionIntentDispatching {
					continue
				}
			}
			if err := s.dispatchForgejoActionIntentLabel(providerCtx, intent, "delegated authority changed at recovery provider dispatch"); err != nil {
				slog.WarnContext(ctx, "recover dispatching intent label ensure failed",
					"intent_id", intent.ID, "forgejo_repo", intent.ForgejoRepo, "forgejo_pr", intent.ForgejoPRNumber, "error", err)
				if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
					if stateErr := s.recordForgejoActionIntentDispatchUncertain(ctx, intent.ID, "provider label dispatch outcome is unknown during recovery"); stateErr != nil {
						return fmt.Errorf("record action intent %s dispatch uncertainty: %w", intent.ID, stateErr)
					}
				}
				continue
			}
			intent, err = s.completeForgejoActionIntentDispatch(ctx, intent.ID)
			if err != nil {
				return fmt.Errorf("complete action intent %s dispatch: %w", candidate.ID, err)
			}
			slog.InfoContext(ctx, "recovered action intent provider dispatch",
				"intent_id", intent.ID, "state", intent.State, "forgejo_repo", intent.ForgejoRepo, "forgejo_pr", intent.ForgejoPRNumber)
		case ForgejoActionIntentDispatched:
			// Left safely pending for webhook/reconciliation.
			slog.InfoContext(ctx, "recovery found dispatched intent, awaiting webhook",
				"intent_id", intent.ID, "forgejo_repo", intent.ForgejoRepo, "forgejo_pr", intent.ForgejoPRNumber)
		case ForgejoActionIntentAccepted, ForgejoActionIntentRunning, ForgejoActionIntentRecovery:
			// Non-terminal, non-expired. Left pending for webhook/reconciliation
			// or next job attempt.
			slog.InfoContext(ctx, "recovery found non-terminal intent, awaiting reconciliation",
				"intent_id", intent.ID, "state", intent.State, "forgejo_repo", intent.ForgejoRepo, "forgejo_pr", intent.ForgejoPRNumber)
		}
	}
	return nil
}
