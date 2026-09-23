package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/operationcatalog"
	"github.com/ngaut/agent-git-service/internal/operationconstraints"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	AccessGrantIssueSchema              = "ags.access-grant.issue.v1"
	AccessGrantReceiptSchema            = "ags.access-grant.v1"
	AccessGrantInvocationSchema         = "ags.access-grant-invocation.v1"
	accessGrantTokenPrefix              = "ags_grant_"
	canonicalActorProvider              = "execution_context"
	invalidAccessGrantSelectorMarker    = "__ags_invalid_selector__"
	invalidAccessGrantPolicyClassMarker = "__ags_invalid_policy_class__"
	maximumAccessGrantTTL               = 30 * time.Minute
)

var (
	ErrAccessGrantUnavailable       = errors.New("access grant authority is unavailable")
	ErrAccessGrantCredentialInvalid = errors.New("access grant credential is invalid")
	ErrAccessGrantDenied            = errors.New("access grant is denied")
	ErrAccessGrantConflict          = errors.New("access grant authority changed")
	canonicalActorSlugRE            = regexp.MustCompile(`[^a-z0-9]+`)
)

// AccessGrantIssueInput acquires a new grant from one fresh, task-token-bound
// execution-context observation. Selector fields express intent only.
type AccessGrantIssueInput struct {
	ExecutionContext     ExecutionContextIntakeInput
	Repository           string
	AgentSelector        string
	PolicyClass          string
	AccessRole           string
	AdditionalOperations []string
}

type AccessGrantRenewInput struct {
	ExecutionContext ExecutionContextIntakeInput
}

type AccessGrantActorReceipt struct {
	ID       uint   `json:"id"`
	Login    string `json:"login"`
	Name     string `json:"name"`
	UserKind string `json:"user_kind"`
}

type AccessGrantExecutorReceipt struct {
	ID       uint   `json:"id"`
	Login    string `json:"login"`
	UserKind string `json:"user_kind"`
}

type AccessGrantSourceReceipt struct {
	InstanceID    string `json:"instance_id"`
	SnapshotID    string `json:"snapshot_id"`
	WorkspaceID   string `json:"workspace_id"`
	AgentID       string `json:"agent_id"`
	TaskID        string `json:"task_id"`
	RunID         string `json:"run_id"`
	IssueID       string `json:"issue_id,omitempty"`
	RuntimeID     string `json:"runtime_id,omitempty"`
	ContextDigest string `json:"context_digest"`
}

type AccessGrantRepositoryReceipt struct {
	ID       uint   `json:"id"`
	FullName string `json:"full_name"`
	Target   string `json:"target"`
}

type AccessGrantReceipt struct {
	Schema                 string                       `json:"schema"`
	ID                     string                       `json:"id"`
	Actor                  AccessGrantActorReceipt      `json:"actor"`
	Executor               AccessGrantExecutorReceipt   `json:"executor"`
	Source                 AccessGrantSourceReceipt     `json:"source"`
	Repository             AccessGrantRepositoryReceipt `json:"repository"`
	AgentSelectorOutcome   string                       `json:"agent_selector_outcome"`
	DefaultPolicyClass     string                       `json:"default_policy_class"`
	RequestedPolicyClass   string                       `json:"requested_policy_class,omitempty"`
	PolicyClassOutcome     string                       `json:"policy_class_outcome"`
	EffectivePolicyClasses []string                     `json:"effective_policy_classes"`
	RequestedOperations    []string                     `json:"requested_operations"`
	EffectiveOperations    []string                     `json:"effective_operations"`
	Warnings               []string                     `json:"warnings"`
	// AssociationStatus is observability only: linked|provisional|unlinked|conflict.
	// It never authorizes or blocks Git/PR collaboration operations.
	AssociationStatus string `json:"association_status"`
	// IdentityKind is observability only: human|durable_agent|temporary_agent.
	IdentityKind string `json:"identity_kind"`
	// CollaborationMode marks that this receipt is transport/attribution for
	// collaboration pass-through, not a fine-grained permission decision.
	CollaborationMode  string     `json:"collaboration_mode"`
	AuthorityRevision  string     `json:"authority_revision"`
	RenewedFromGrantID string     `json:"renewed_from_grant_id,omitempty"`
	State              string     `json:"state"`
	CreatedAt          time.Time  `json:"created_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	LastUsedAt         *time.Time `json:"last_used_at,omitempty"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	RevocationReason   string     `json:"revocation_reason,omitempty"`
}

type AccessGrantIssueResult struct {
	Schema     string             `json:"schema"`
	GrantToken string             `json:"grant_token"`
	Grant      AccessGrantReceipt `json:"grant"`
}

type AccessGrantOperationInput struct {
	Operation   string
	Constraints map[string]any
}

type AccessGrantInvocationReceipt struct {
	Schema               string         `json:"schema"`
	ID                   string         `json:"id"`
	GrantID              string         `json:"grant_id"`
	ActorUserID          uint           `json:"actor_user_id"`
	ExecutorUserID       uint           `json:"executor_user_id"`
	Repository           string         `json:"repository"`
	Operation            string         `json:"operation"`
	Constraints          map[string]any `json:"constraints"`
	AuthorityRevision    string         `json:"authority_revision"`
	AGSPRNumber          int            `json:"ags_pr_number,omitempty"`
	Provider             string         `json:"provider,omitempty"`
	ProviderRepository   string         `json:"provider_repository,omitempty"`
	ProviderPRNumber     int            `json:"provider_pr_number,omitempty"`
	ExpectedHeadSHA      string         `json:"expected_head_sha,omitempty"`
	ExpectedBaseSHA      string         `json:"expected_base_sha,omitempty"`
	BaseRef              string         `json:"base_ref,omitempty"`
	EffectMethod         string         `json:"effect_method,omitempty"`
	State                string         `json:"state"`
	AuthorizationOutcome string         `json:"authorization_outcome"`
	ProviderAttempt      string         `json:"provider_attempt"`
	ProviderOutcome      string         `json:"provider_outcome"`
	ProviderMerged       bool           `json:"provider_merged"`
	ProviderMergeSHA     string         `json:"provider_merge_sha,omitempty"`
	DenialCode           string         `json:"denial_code,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
	FinishedAt           *time.Time     `json:"finished_at,omitempty"`
}

type accessGrantAuthority struct {
	Actor                  db.User
	Executor               db.User
	Repository             db.Repository
	Default                sessionauthority.RuntimePolicyAuthority
	Elevated               *sessionauthority.RuntimePolicyAuthority
	AgentSelector          string
	RequestedPolicyClass   string
	AgentSelectorOutcome   string
	PolicyClassOutcome     string
	EffectivePolicyClasses []string
	RequestedOperations    []string
	EffectiveOperations    []string
	Warnings               []string
	Resource               sessionauthority.ResourcePolicy
	AuthorityRevision      string
}

func (s *Service) IssueAccessGrant(ctx context.Context, input AccessGrantIssueInput) (AccessGrantIssueResult, error) {
	input.Repository = strings.Trim(strings.TrimSpace(input.Repository), "/")
	if input.Repository == "" || sessionauthority.IsSecretShapedValue(input.Repository) {
		return AccessGrantIssueResult{}, fmt.Errorf("%w: invalid access grant request", ErrValidation)
	}
	repository, err := s.LookupRepoIdentity(ctx, input.Repository)
	if err != nil || repository.Disabled {
		return AccessGrantIssueResult{}, fmt.Errorf("%w: repository is unavailable", ErrAccessGrantDenied)
	}
	intake, err := s.intakeForAccessGrant(ctx, input.ExecutionContext)
	input.ExecutionContext.SourceToken = ""
	if err != nil {
		return AccessGrantIssueResult{}, err
	}
	record, token, actor, executor, err := s.buildAccessGrant(ctx, intake, repository, input.AgentSelector, input.PolicyClass, input.AccessRole, input.AdditionalOperations, "")
	if err != nil {
		return AccessGrantIssueResult{}, err
	}
	if intake.Provisional {
		record.Warnings = appendUniqueString(record.Warnings, "execution_context_unverified")
	}
	if err := s.DBForCtx(ctx).Create(&record).Error; err != nil {
		return AccessGrantIssueResult{}, ErrAccessGrantUnavailable
	}
	return AccessGrantIssueResult{
		Schema:     AccessGrantIssueSchema,
		GrantToken: token,
		Grant:      accessGrantReceipt(record, actor, executor, intake.ContextDigest, time.Now().UTC()),
	}, nil
}

func (s *Service) RenewAccessGrant(ctx context.Context, rawGrant string, input AccessGrantRenewInput) (AccessGrantIssueResult, error) {
	old, _, _, err := s.resolveActiveAccessGrant(ctx, rawGrant)
	if err != nil {
		return AccessGrantIssueResult{}, err
	}
	intake, err := s.IntakeExecutionContext(ctx, input.ExecutionContext)
	input.ExecutionContext.SourceToken = ""
	if isExecutionContextAvailabilityError(err) {
		intake = ExecutionContextIntakeResult{
			SnapshotID: old.SnapshotID,
			SourceRef: executioncontext.SourceRef{
				Schema: executioncontext.SourceRefSchema, SourceInstanceID: old.SourceInstanceID,
				WorkspaceID: old.ExternalWorkspaceID, AgentID: old.ExternalAgentID,
				TaskID: old.ExternalTaskID, RunID: old.ExternalRunID,
			},
			Provisional: true,
		}
		err = nil
	}
	if err != nil {
		return AccessGrantIssueResult{}, err
	}
	if intake.SourceRef.SourceInstanceID != old.SourceInstanceID || intake.SourceRef.WorkspaceID != old.ExternalWorkspaceID ||
		intake.SourceRef.AgentID != old.ExternalAgentID || intake.SourceRef.TaskID != old.ExternalTaskID || intake.SourceRef.RunID != old.ExternalRunID {
		return AccessGrantIssueResult{}, fmt.Errorf("%w: renewal source facts changed", ErrAccessGrantConflict)
	}
	repository, err := s.LookupRepoIdentity(ctx, old.RepositoryFullName)
	if err != nil || repository.ID != old.RepositoryID || repository.Disabled {
		return AccessGrantIssueResult{}, fmt.Errorf("%w: renewal repository changed", ErrAccessGrantConflict)
	}
	renewAccessRole := ""
	if old.ElevatedBindingRevision != "" || containsAccessGrantOperation(old.EffectiveOperations, "pr.merge") {
		renewAccessRole = operationcatalog.AccessRoleMaintainer
	}
	record, token, actor, executor, err := s.buildAccessGrant(ctx, intake, repository, old.AgentSelector, old.RequestedPolicyClass, renewAccessRole, old.RequestedOperations, old.ID)
	if err != nil {
		return AccessGrantIssueResult{}, err
	}
	if intake.Provisional {
		record.Warnings = appendUniqueString(record.Warnings, "execution_context_unverified")
	}
	if record.ActorUserID != old.ActorUserID || record.ExecutorUserID != old.ExecutorUserID ||
		record.RepositoryID != old.RepositoryID || record.TargetInstance != old.TargetInstance ||
		record.AuthorityRevision != old.AuthorityRevision ||
		!equalStringSlices(record.EffectiveOperations, old.EffectiveOperations) ||
		!equalStringSlices(record.EffectivePolicyClasses, old.EffectivePolicyClasses) {
		return AccessGrantIssueResult{}, fmt.Errorf("%w: renewal authority changed", ErrAccessGrantConflict)
	}
	now := time.Now().UTC()
	err = s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		var current db.AccessGrant
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", old.ID).Error; err != nil {
			return err
		}
		if current.RevokedAt != nil || !current.ExpiresAt.After(now) || current.AuthorityRevision != old.AuthorityRevision {
			return ErrAccessGrantConflict
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		if err := tx.Model(&db.AccessGrant{}).Where("id = ? AND revoked_at IS NULL", old.ID).
			Updates(map[string]any{"revoked_at": now, "revocation_reason": "renewed"}).Error; err != nil {
			return err
		}
		return revokeAccessGrantTransportSessions(tx, old.ID, now, "access_grant_renewed")
	})
	if err != nil {
		return AccessGrantIssueResult{}, wrapErr(err)
	}
	return AccessGrantIssueResult{Schema: AccessGrantIssueSchema, GrantToken: token,
		Grant: accessGrantReceipt(record, actor, executor, intake.ContextDigest, now)}, nil
}

func (s *Service) RevokeAccessGrant(ctx context.Context, rawGrant, reason string) (AccessGrantReceipt, error) {
	grant, actor, executor, err := s.loadAccessGrantCredential(ctx, rawGrant)
	if err != nil {
		return AccessGrantReceipt{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "caller_revoked"
	}
	if len(reason) > 255 || strings.ContainsAny(reason, "\r\n\x00") || sessionauthority.IsSecretShapedValue(reason) {
		return AccessGrantReceipt{}, fmt.Errorf("%w: invalid revocation reason", ErrValidation)
	}
	now := time.Now().UTC()
	if err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		var current db.AccessGrant
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", grant.ID).Error; err != nil {
			return err
		}
		revokedAt := now
		if current.RevokedAt == nil {
			if err := tx.Model(&db.AccessGrant{}).Where("id = ? AND revoked_at IS NULL", current.ID).
				Updates(map[string]any{"revoked_at": now, "revocation_reason": reason}).Error; err != nil {
				return err
			}
			current.RevokedAt = &now
			current.RevocationReason = reason
		} else {
			revokedAt = current.RevokedAt.UTC()
		}
		if err := revokeAccessGrantTransportSessions(tx, current.ID, revokedAt, "access_grant_revoked"); err != nil {
			return err
		}
		grant = current
		return nil
	}); err != nil {
		return AccessGrantReceipt{}, wrapErr(err)
	}
	digest := accessGrantSnapshotDigest(ctx, s, grant.SnapshotID)
	return accessGrantReceipt(grant, actor, executor, digest, now), nil
}

func revokeAccessGrantTransportSessions(tx *gorm.DB, grantID string, revokedAt time.Time, reason string) error {
	return tx.Model(&db.DelegatedAgentSession{}).
		Where("access_grant_id = ? AND credential_mode = ? AND revoked_at IS NULL", grantID, accessGrantTransportCredentialMode).
		Updates(map[string]any{"revoked_at": revokedAt, "revocation_reason": reason}).Error
}

func (s *Service) GetAccessGrant(ctx context.Context, rawGrant string) (AccessGrantReceipt, error) {
	grant, actor, executor, err := s.loadAccessGrantCredential(ctx, rawGrant)
	if err != nil {
		return AccessGrantReceipt{}, err
	}
	return accessGrantReceipt(grant, actor, executor, accessGrantSnapshotDigest(ctx, s, grant.SnapshotID), time.Now().UTC()), nil
}

func (s *Service) AuthorizeAccessGrantOperation(ctx context.Context, rawGrant string, input AccessGrantOperationInput) (AccessGrantInvocationReceipt, error) {
	grant, _, _, err := s.resolveActiveAccessGrant(ctx, rawGrant)
	if err != nil {
		return AccessGrantInvocationReceipt{}, err
	}
	operation := strings.ToLower(strings.TrimSpace(input.Operation))
	if !containsAccessGrantOperation(grant.EffectiveOperations, operation) {
		return AccessGrantInvocationReceipt{}, fmt.Errorf("%w: operation is outside the grant", ErrAccessGrantDenied)
	}
	constraints, err := operationconstraints.NormalizeJSON(operation, input.Constraints)
	if err != nil {
		return AccessGrantInvocationReceipt{}, fmt.Errorf("%w: operation constraints are invalid", ErrValidation)
	}
	if operation == "pr.merge" {
		return AccessGrantInvocationReceipt{}, fmt.Errorf("%w: pr.merge requires the effect endpoint", ErrAccessGrantDenied)
	}
	if _, _, _, err := s.revalidateAccessGrant(ctx, grant); err != nil {
		return AccessGrantInvocationReceipt{}, err
	}
	constraintsJSON, _ := json.Marshal(constraints)
	now := time.Now().UTC()
	invocation := db.AccessGrantInvocation{
		ID: uuid.NewString(), GrantID: grant.ID, ActorUserID: grant.ActorUserID, ExecutorUserID: grant.ExecutorUserID,
		RepositoryID: grant.RepositoryID, Repository: grant.RepositoryFullName, Operation: operation,
		ConstraintsJSON: string(constraintsJSON), AuthorityRevision: grant.AuthorityRevision,
		State: "completed", AuthorizationOutcome: "allowed", ProviderAttempt: "not_attempted", ProviderOutcome: "not_applicable",
		CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
	}
	if err := s.DBForCtx(ctx).Create(&invocation).Error; err != nil {
		return AccessGrantInvocationReceipt{}, ErrAccessGrantUnavailable
	}
	_ = s.DBForCtx(ctx).Model(&db.AccessGrant{}).Where("id = ?", grant.ID).Update("last_used_at", now).Error
	return accessGrantInvocationReceipt(invocation), nil
}

func (s *Service) buildAccessGrant(ctx context.Context, intake ExecutionContextIntakeResult, repository db.Repository, agentSelector, requestedClass, accessRole string, requestedOps []string, renewedFrom string) (db.AccessGrant, string, db.User, db.User, error) {
	if s.PrincipalSessions == nil {
		return db.AccessGrant{}, "", db.User{}, db.User{}, ErrAccessGrantUnavailable
	}
	actor, err := s.resolveOrCreateCanonicalRuntimeActor(ctx, intake.SourceRef, intake.ContextSnapshot)
	if err != nil {
		return db.AccessGrant{}, "", db.User{}, db.User{}, err
	}
	authority, err := s.evaluateAccessGrantAuthority(ctx, actor, intake.SourceRef, repository, agentSelector, requestedClass, accessRole, requestedOps)
	if err != nil {
		return db.AccessGrant{}, "", db.User{}, db.User{}, err
	}
	raw, hash, prefix, err := newAccessGrantCredential()
	if err != nil {
		return db.AccessGrant{}, "", db.User{}, db.User{}, ErrAccessGrantUnavailable
	}
	ttl := authority.Resource.MaxSessionDuration()
	if ttl <= 0 || ttl > maximumAccessGrantTTL {
		ttl = maximumAccessGrantTTL
	}
	now := time.Now().UTC()
	record := db.AccessGrant{
		ID: uuid.NewString(), CredentialHash: hash, CredentialPrefix: prefix,
		ActorUserID: actor.ID, ExecutorUserID: authority.Executor.ID, SnapshotID: intake.SnapshotID,
		SourceInstanceID: intake.SourceRef.SourceInstanceID, ExternalWorkspaceID: intake.SourceRef.WorkspaceID,
		ExternalAgentID: intake.SourceRef.AgentID, ExternalTaskID: intake.SourceRef.TaskID, ExternalRunID: intake.SourceRef.RunID,
		ExternalIssueID: intake.SourceRef.IssueID, ExternalRuntimeID: intake.SourceRef.RuntimeID,
		RepositoryID: repository.ID, RepositoryFullName: repository.FullName, TargetInstance: authority.Resource.Target,
		AgentSelector: authority.AgentSelector, AgentSelectorOutcome: authority.AgentSelectorOutcome,
		DefaultPolicyClass: authority.Default.PolicyClass.ID, RequestedPolicyClass: authority.RequestedPolicyClass,
		PolicyClassOutcome: authority.PolicyClassOutcome, EffectivePolicyClasses: authority.EffectivePolicyClasses,
		RequestedOperations: authority.RequestedOperations, EffectiveOperations: authority.EffectiveOperations, Warnings: authority.Warnings,
		DefaultBindingRevision: authority.Default.TeamBinding.BindingRevision,
		ResourcePolicyRevision: authority.Resource.PolicyRevision, AuthorityRevision: authority.AuthorityRevision,
		RenewedFromGrantID: renewedFrom, CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if authority.Elevated != nil {
		record.ElevatedBindingRevision = authority.Elevated.TeamBinding.BindingRevision
	}
	return record, raw, actor, authority.Executor, nil
}

func (s *Service) evaluateAccessGrantAuthority(ctx context.Context, actor db.User, source executioncontext.SourceRef, repository db.Repository, agentSelector, requestedClass, accessRole string, requestedOps []string) (accessGrantAuthority, error) {
	// Standard executor identity still comes from the trusted default team binding.
	// Operation risk and the default standard envelope come only from operationcatalog.
	defaultAuthority, err := s.PrincipalSessions.ResolveRuntimePolicyAuthority(source.SourceInstanceID, source.WorkspaceID, sessionauthority.DefaultDynamicPolicyClass)
	if err != nil {
		return accessGrantAuthority{}, fmt.Errorf("%w: default source authority is unavailable", ErrAccessGrantDenied)
	}
	var executor db.User
	if err := s.DBForCtx(ctx).First(&executor, "id = ?", defaultAuthority.TeamBinding.PrincipalID).Error; err != nil || !activeAccessGrantUser(executor) {
		return accessGrantAuthority{}, fmt.Errorf("%w: default executor is unavailable", ErrAccessGrantDenied)
	}
	standardOps := operationcatalog.StandardOperations()
	if !s.accessGrantPrincipalCoversOperations(ctx, repository.ID, executor.ID, standardOps) {
		return accessGrantAuthority{}, fmt.Errorf("%w: default executor native grant is unavailable", ErrAccessGrantDenied)
	}
	warnings := make([]string, 0, 4)
	selectorOutcome := "default"
	selector := strings.TrimSpace(agentSelector)
	if sessionauthority.IsSecretShapedValue(selector) {
		selector = invalidAccessGrantSelectorMarker
		selectorOutcome = "ignored_invalid"
		warnings = append(warnings, "agent_selector_ignored_invalid")
	} else if selector == invalidAccessGrantSelectorMarker {
		selectorOutcome = "ignored_invalid"
		warnings = append(warnings, "agent_selector_ignored_invalid")
	} else if selector != "" {
		if selector == source.AgentID || selector == actor.Login || selector == strconv.FormatUint(uint64(actor.ID), 10) {
			selectorOutcome = "accepted"
		} else {
			selectorOutcome = "ignored_unbound"
			warnings = append(warnings, "agent_selector_ignored_unbound")
		}
	}

	classes := []string{defaultAuthority.PolicyClass.ID}
	policyOutcome := "default"
	var elevated *sessionauthority.RuntimePolicyAuthority
	requestedClass = strings.TrimSpace(requestedClass)
	accessRole = strings.ToLower(strings.TrimSpace(accessRole))

	// Workload authority is intentionally one-dimensional: the Multica Agent
	// custom env role is the sole switch for the single extra operation pr.merge.
	// No allowlist, policy class, principal binding, or other privileged envelope
	// is consulted here. AGS still owns the merge correctness/effect gates.
	wantsElevation := operationcatalog.IsPRMergeRole(accessRole)
	if accessRole != "" && !wantsElevation {
		warnings = appendUniqueString(warnings, "access_role_ignored_invalid")
	}
	mergeEligible := wantsElevation
	if mergeEligible {
		policyOutcome = "accepted_role"
	}
	if sessionauthority.IsSecretShapedValue(requestedClass) {
		requestedClass = invalidAccessGrantPolicyClassMarker
		policyOutcome = "ignored_invalid"
		warnings = append(warnings, "policy_class_ignored_invalid")
	} else if requestedClass == invalidAccessGrantPolicyClassMarker {
		policyOutcome = "ignored_invalid"
		warnings = append(warnings, "policy_class_ignored_invalid")
	} else if requestedClass != "" && requestedClass != defaultAuthority.PolicyClass.ID {
		// Policy classes are retained only as observability input. They never
		// create a second privileged path.
		if !mergeEligible {
			policyOutcome = "ignored_unbound"
		}
		warnings = appendUniqueString(warnings, "policy_class_ignored")
	}

	normalizedRequested := normalizeAccessGrantOperations(requestedOps)
	safeRequested := normalizedRequested[:0]
	for _, operation := range normalizedRequested {
		operation = operationcatalog.CanonicalName(operation)
		if sessionauthority.IsSecretShapedValue(operation) {
			warnings = appendUniqueString(warnings, "operation_ignored_invalid")
			continue
		}
		safeRequested = append(safeRequested, operation)
	}
	normalizedRequested = normalizeAccessGrantOperations(safeRequested)

	allowed := make(map[string]struct{}, len(standardOps)+2)
	for _, operation := range standardOps {
		allowed[operation] = struct{}{}
	}
	if mergeEligible {
		allowed["pr.merge"] = struct{}{}
	}

	requested := make(map[string]struct{}, len(allowed)+len(normalizedRequested))
	for operation := range allowed {
		requested[operation] = struct{}{}
	}
	for _, operation := range normalizedRequested {
		if _, classAllowed := allowed[operation]; !classAllowed {
			warnings = appendUniqueString(warnings, "operation_ignored_outside_envelope")
			continue
		}
		if !operationconstraints.IsDefaultOperation(operation) || !operationcatalog.IsKnown(operation) {
			warnings = appendUniqueString(warnings, "operation_ignored_deferred")
			continue
		}
		requested[operation] = struct{}{}
	}

	resourceScope, err := s.PrincipalSessions.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
		Service: "ags", Repository: repository.FullName, Operation: "repo.read",
	})
	if err != nil {
		return accessGrantAuthority{}, fmt.Errorf("%w: repository resource policy is unavailable", ErrAccessGrantDenied)
	}
	effective := make([]string, 0, len(requested))
	for operation := range requested {
		operation = operationcatalog.CanonicalName(operation)
		if !operationconstraints.IsDefaultOperation(operation) || !operationcatalog.IsKnown(operation) {
			warnings = appendUniqueString(warnings, "operation_ignored_deferred")
			continue
		}
		// Unknown/privileged without elevation was already filtered via allowed.
		// Keep the accepted pass-through warning for ordinary operations when
		// resource-policy packaging lags the AGS-owned operation catalog.
		lowRisk := operationcatalog.IsStandard(operation)
		if _, err := s.PrincipalSessions.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
			Target: resourceScope.Resource.Target, Service: "ags", Repository: repository.FullName, Operation: operation,
		}); err != nil {
			if operationcatalog.IsPrivileged(operation) && elevated == nil {
				warnings = appendUniqueString(warnings, "operation_ignored_resource_policy")
				continue
			}
			if lowRisk {
				warnings = appendUniqueString(warnings, "resource_policy_softened_for_collaboration_passthrough")
			}
		}
		effective = append(effective, operation)
	}
	// Transport admits the hard low-risk allowlist even when packaging omits an op.
	// Do not rewrite the frozen grant envelope here; that would churn authority revision.
	sort.Strings(classes)
	sort.Strings(normalizedRequested)
	sort.Strings(effective)
	sort.Strings(warnings)
	if len(effective) == 0 {
		return accessGrantAuthority{}, fmt.Errorf("%w: no effective operations", ErrAccessGrantDenied)
	}
	revision, err := accessGrantAuthorityRevision(accessGrantAuthorityRevisionInput{
		ActorUserID: actor.ID, ExecutorUserID: executor.ID, SourceInstanceID: source.SourceInstanceID,
		WorkspaceID: source.WorkspaceID, AgentID: source.AgentID, RepositoryID: repository.ID,
		Repository: repository.FullName, Target: resourceScope.Resource.Target,
		AgentSelectorOutcome: selectorOutcome, DefaultPolicyClass: defaultAuthority.PolicyClass.ID,
		DefaultPolicyRevision:  defaultAuthority.PolicyClass.PolicyRevision,
		DefaultBindingRevision: defaultAuthority.TeamBinding.BindingRevision,
		RequestedPolicyClass:   requestedClass, PolicyClassOutcome: policyOutcome,
		EffectivePolicyClasses: classes, RequestedOperations: normalizedRequested, EffectiveOperations: effective,
		ResourcePolicyRevision: resourceScope.Resource.PolicyRevision,
		ElevatedBindingRevision: func() string {
			if elevated != nil {
				return elevated.TeamBinding.BindingRevision
			}
			return ""
		}(),
		ElevatedPolicyRevision: func() string {
			if elevated != nil {
				return elevated.PolicyClass.PolicyRevision
			}
			return ""
		}(),
	})
	if err != nil {
		return accessGrantAuthority{}, ErrAccessGrantUnavailable
	}
	return accessGrantAuthority{
		Actor: actor, Executor: executor, Repository: repository, Default: defaultAuthority, Elevated: elevated,
		AgentSelector: selector, RequestedPolicyClass: requestedClass,
		AgentSelectorOutcome: selectorOutcome, PolicyClassOutcome: policyOutcome, EffectivePolicyClasses: classes,
		RequestedOperations: normalizedRequested, EffectiveOperations: effective, Warnings: warnings,
		Resource: resourceScope.Resource, AuthorityRevision: revision,
	}, nil
}

type accessGrantAuthorityRevisionInput struct {
	Schema                  string   `json:"schema"`
	ActorUserID             uint     `json:"actor_user_id"`
	ExecutorUserID          uint     `json:"executor_user_id"`
	SourceInstanceID        string   `json:"source_instance_id"`
	WorkspaceID             string   `json:"workspace_id"`
	AgentID                 string   `json:"agent_id"`
	RepositoryID            uint     `json:"repository_id"`
	Repository              string   `json:"repository"`
	Target                  string   `json:"target"`
	AgentSelectorOutcome    string   `json:"agent_selector_outcome"`
	DefaultPolicyClass      string   `json:"default_policy_class"`
	DefaultPolicyRevision   string   `json:"default_policy_revision"`
	DefaultBindingRevision  string   `json:"default_binding_revision"`
	RequestedPolicyClass    string   `json:"requested_policy_class,omitempty"`
	PolicyClassOutcome      string   `json:"policy_class_outcome"`
	EffectivePolicyClasses  []string `json:"effective_policy_classes"`
	RequestedOperations     []string `json:"requested_operations"`
	EffectiveOperations     []string `json:"effective_operations"`
	ResourcePolicyRevision  string   `json:"resource_policy_revision"`
	ElevatedBindingRevision string   `json:"elevated_binding_revision,omitempty"`
	ElevatedPolicyRevision  string   `json:"elevated_policy_revision,omitempty"`
}

func accessGrantAuthorityRevision(input accessGrantAuthorityRevisionInput) (string, error) {
	input.Schema = "ags.access-grant-authority.v1"
	body, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (s *Service) resolveOrCreateCanonicalRuntimeActor(ctx context.Context, source executioncontext.SourceRef, current executioncontext.CurrentContext) (db.User, error) {
	provider := canonicalActorProvider
	subject := canonicalActorSubject(source.SourceInstanceID, source.AgentID)
	const maxAttempts = 4
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var actor db.User
		err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
			var identity db.UserIdentity
			err := tx.Preload("User").First(&identity, "provider = ? AND subject = ?", provider, subject).Error
			if err == nil {
				if !activeCanonicalActor(identity.User) {
					return ErrAccessGrantDenied
				}
				actor = identity.User
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			login := canonicalActorLogin(source.SourceInstanceID, source.AgentID, current.Agent.Name)
			var existing db.User
			if err := tx.First(&existing, "login = ?", login).Error; err == nil {
				login = "agent-" + canonicalActorDigest(source.SourceInstanceID, source.AgentID)[:20]
				if err := tx.First(&existing, "login = ?", login).Error; err == nil {
					return ErrAccessGrantConflict
				} else if !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			name := strings.TrimSpace(current.Agent.Name)
			if name == "" || sessionauthority.IsSecretShapedValue(name) {
				name = login
			}
			actor = db.User{Login: login, Name: name, Type: db.TypeUser, Status: "active", UserKind: db.UserKindAgent,
				DefaultRepositoryPermission: "none", IsAnonymous: false}
			if err := tx.Create(&actor).Error; err != nil {
				return err
			}
			return tx.Create(&db.UserIdentity{UserID: actor.ID, Provider: provider, Subject: subject}).Error
		})
		if err == nil {
			return actor, nil
		}
		if errors.Is(err, ErrAccessGrantDenied) {
			return db.User{}, fmt.Errorf("%w: canonical actor is inactive", ErrAccessGrantDenied)
		}
		if isDuplicateErr(err) || isSQLiteLockErr(err) || errors.Is(err, ErrAccessGrantConflict) {
			time.Sleep(retryDelay(attempt))
			continue
		}
		return db.User{}, wrapErr(err)
	}
	return db.User{}, ErrAccessGrantConflict
}

func (s *Service) revalidateAccessGrant(ctx context.Context, grant db.AccessGrant) (db.User, db.User, db.Repository, error) {
	if grant.RevokedAt != nil || !grant.ExpiresAt.After(time.Now().UTC()) {
		return db.User{}, db.User{}, db.Repository{}, ErrAccessGrantCredentialInvalid
	}
	var actor, executor db.User
	if err := s.DBForCtx(ctx).First(&actor, "id = ?", grant.ActorUserID).Error; err != nil || !activeCanonicalActor(actor) {
		return db.User{}, db.User{}, db.Repository{}, fmt.Errorf("%w: actor changed", ErrAccessGrantConflict)
	}
	if err := s.DBForCtx(ctx).First(&executor, "id = ?", grant.ExecutorUserID).Error; err != nil || !activeAccessGrantUser(executor) {
		return db.User{}, db.User{}, db.Repository{}, fmt.Errorf("%w: executor changed", ErrAccessGrantConflict)
	}
	var identity db.UserIdentity
	if err := s.DBForCtx(ctx).First(&identity, "provider = ? AND subject = ?", canonicalActorProvider,
		canonicalActorSubject(grant.SourceInstanceID, grant.ExternalAgentID)).Error; err != nil || identity.UserID != actor.ID {
		return db.User{}, db.User{}, db.Repository{}, fmt.Errorf("%w: actor binding changed", ErrAccessGrantConflict)
	}
	repository, err := s.LookupRepoIdentity(ctx, grant.RepositoryFullName)
	if err != nil || repository.ID != grant.RepositoryID || repository.Disabled {
		return db.User{}, db.User{}, db.Repository{}, fmt.Errorf("%w: repository changed", ErrAccessGrantConflict)
	}
	_, source, _, err := s.GetExecutionContextSnapshot(ctx, grant.SnapshotID)
	// RunID is association continuity only; Multica reinjection may change it
	// without revoking collaboration transport for the same task/agent.
	if err != nil || source.SourceInstanceID != grant.SourceInstanceID || source.WorkspaceID != grant.ExternalWorkspaceID ||
		source.AgentID != grant.ExternalAgentID || source.TaskID != grant.ExternalTaskID {
		return db.User{}, db.User{}, db.Repository{}, fmt.Errorf("%w: context snapshot changed", ErrAccessGrantConflict)
	}
	revalidateAccessRole := ""
	if grant.ElevatedBindingRevision != "" || containsAccessGrantOperation(grant.EffectiveOperations, "pr.merge") {
		revalidateAccessRole = operationcatalog.AccessRoleMaintainer
	}
	authority, err := s.evaluateAccessGrantAuthority(ctx, actor, source, repository, grant.AgentSelector, grant.RequestedPolicyClass, revalidateAccessRole, grant.RequestedOperations)
	if err != nil || authority.Executor.ID != executor.ID {
		return db.User{}, db.User{}, db.Repository{}, fmt.Errorf("%w: policy authority changed", ErrAccessGrantConflict)
	}
	// Collaboration pass-through: keep transport usable when the default
	// collaboration envelope still covers the grant's effective operations,
	// even if policy-class packaging or authority revision text drifted.
	if !equalStringSlices(authority.EffectiveOperations, grant.EffectiveOperations) {
		if !collaborationEnvelopeCovers(grant.EffectiveOperations, authority.EffectiveOperations) {
			return db.User{}, db.User{}, db.Repository{}, fmt.Errorf("%w: policy authority changed", ErrAccessGrantConflict)
		}
	}
	return actor, executor, repository, nil
}

// collaborationEnvelopeCovers reports whether live authority still admits every
// operation already frozen on the grant. Shrinking the envelope is conflict;
// packaging/revision-only drift with the same ops is allowed for transport.
func collaborationEnvelopeCovers(granted, live []string) bool {
	liveSet := make(map[string]struct{}, len(live))
	for _, operation := range live {
		liveSet[operation] = struct{}{}
	}
	for _, operation := range granted {
		if _, ok := liveSet[operation]; !ok {
			return false
		}
	}
	return true
}

func (s *Service) resolveActiveAccessGrant(ctx context.Context, raw string) (db.AccessGrant, db.User, db.User, error) {
	grant, actor, executor, err := s.loadAccessGrantCredential(ctx, raw)
	if err != nil {
		return db.AccessGrant{}, db.User{}, db.User{}, err
	}
	now := time.Now().UTC()
	if grant.RevokedAt != nil || !grant.ExpiresAt.After(now) {
		return db.AccessGrant{}, db.User{}, db.User{}, ErrAccessGrantCredentialInvalid
	}
	return grant, actor, executor, nil
}

func (s *Service) loadAccessGrantCredential(ctx context.Context, raw string) (db.AccessGrant, db.User, db.User, error) {
	if !strings.HasPrefix(raw, accessGrantTokenPrefix) || len(raw) < len(accessGrantTokenPrefix)+32 || len(raw) > 256 {
		return db.AccessGrant{}, db.User{}, db.User{}, ErrAccessGrantCredentialInvalid
	}
	digest := sha256.Sum256([]byte(raw))
	var grant db.AccessGrant
	if err := s.DBForCtx(ctx).First(&grant, "credential_hash = ?", hex.EncodeToString(digest[:])).Error; err != nil {
		return db.AccessGrant{}, db.User{}, db.User{}, ErrAccessGrantCredentialInvalid
	}
	var actor, executor db.User
	if err := s.DBForCtx(ctx).First(&actor, "id = ?", grant.ActorUserID).Error; err != nil {
		return db.AccessGrant{}, db.User{}, db.User{}, ErrAccessGrantCredentialInvalid
	}
	if err := s.DBForCtx(ctx).First(&executor, "id = ?", grant.ExecutorUserID).Error; err != nil {
		return db.AccessGrant{}, db.User{}, db.User{}, ErrAccessGrantCredentialInvalid
	}
	return grant, actor, executor, nil
}

func accessGrantReceipt(grant db.AccessGrant, actor, executor db.User, contextDigest string, now time.Time) AccessGrantReceipt {
	state := "active"
	if grant.RevokedAt != nil {
		state = "revoked"
	} else if !grant.ExpiresAt.After(now) {
		state = "expired"
	}
	requestedPolicyClass := grant.RequestedPolicyClass
	if requestedPolicyClass == invalidAccessGrantPolicyClassMarker {
		requestedPolicyClass = ""
	}
	association, identityKind := accessGrantAssociation(grant)
	warnings := append([]string{}, grant.Warnings...)
	warnings = appendUniqueString(warnings, "collaboration_passthrough_transport")
	sort.Strings(warnings)
	return AccessGrantReceipt{
		Schema: AccessGrantReceiptSchema, ID: grant.ID,
		Actor:    AccessGrantActorReceipt{ID: actor.ID, Login: actor.Login, Name: actor.Name, UserKind: actor.UserKind},
		Executor: AccessGrantExecutorReceipt{ID: executor.ID, Login: executor.Login, UserKind: executor.UserKind},
		Source: AccessGrantSourceReceipt{InstanceID: grant.SourceInstanceID, SnapshotID: grant.SnapshotID,
			WorkspaceID: grant.ExternalWorkspaceID, AgentID: grant.ExternalAgentID, TaskID: grant.ExternalTaskID,
			RunID: grant.ExternalRunID, IssueID: grant.ExternalIssueID, RuntimeID: grant.ExternalRuntimeID, ContextDigest: contextDigest},
		Repository:           AccessGrantRepositoryReceipt{ID: grant.RepositoryID, FullName: grant.RepositoryFullName, Target: grant.TargetInstance},
		AgentSelectorOutcome: grant.AgentSelectorOutcome, DefaultPolicyClass: grant.DefaultPolicyClass,
		RequestedPolicyClass: requestedPolicyClass, PolicyClassOutcome: grant.PolicyClassOutcome,
		EffectivePolicyClasses: append([]string{}, grant.EffectivePolicyClasses...),
		RequestedOperations:    append([]string{}, grant.RequestedOperations...),
		EffectiveOperations:    append([]string{}, grant.EffectiveOperations...), Warnings: warnings,
		AssociationStatus: association, IdentityKind: identityKind, CollaborationMode: "passthrough_transport",
		AuthorityRevision: grant.AuthorityRevision, RenewedFromGrantID: grant.RenewedFromGrantID,
		State: state, CreatedAt: grant.CreatedAt, ExpiresAt: grant.ExpiresAt, LastUsedAt: grant.LastUsedAt,
		RevokedAt: grant.RevokedAt, RevocationReason: grant.RevocationReason,
	}
}

// accessGrantAssociation derives observability-only association and identity
// labels. Incomplete association never removes collaboration operations.
func accessGrantAssociation(grant db.AccessGrant) (associationStatus, identityKind string) {
	hasCore := strings.TrimSpace(grant.ExternalWorkspaceID) != "" &&
		strings.TrimSpace(grant.ExternalAgentID) != "" &&
		strings.TrimSpace(grant.ExternalTaskID) != ""
	unverified := false
	for _, warning := range grant.Warnings {
		if warning == "execution_context_unverified" {
			unverified = true
			break
		}
	}
	switch {
	case strings.TrimSpace(grant.AgentSelector) != "" &&
		(grant.AgentSelectorOutcome == "ignored_unbound" || grant.AgentSelectorOutcome == "ignored_invalid"):
		associationStatus = "conflict"
	case !hasCore:
		associationStatus = "unlinked"
	case unverified:
		associationStatus = "provisional"
	case hasCore:
		associationStatus = "linked"
	default:
		associationStatus = "unlinked"
	}
	if grant.ActorUserID != 0 && grant.ActorUserID == grant.ExecutorUserID && associationStatus == "linked" {
		identityKind = "durable_agent"
	} else {
		identityKind = "temporary_agent"
	}
	return associationStatus, identityKind
}

func accessGrantInvocationReceipt(invocation db.AccessGrantInvocation) AccessGrantInvocationReceipt {
	constraints := map[string]any{}
	var stored map[string]string
	if json.Unmarshal([]byte(invocation.ConstraintsJSON), &stored) == nil {
		if converted, err := operationconstraints.ToJSON(invocation.Operation, stored); err == nil {
			constraints = converted
		}
	}
	return AccessGrantInvocationReceipt{
		Schema: AccessGrantInvocationSchema, ID: invocation.ID, GrantID: invocation.GrantID,
		ActorUserID: invocation.ActorUserID, ExecutorUserID: invocation.ExecutorUserID,
		Repository: invocation.Repository, Operation: invocation.Operation, Constraints: constraints,
		AuthorityRevision: invocation.AuthorityRevision, AGSPRNumber: invocation.AGSPRNumber,
		Provider: invocation.Provider, ProviderRepository: invocation.ProviderRepo, ProviderPRNumber: invocation.ProviderPRNumber,
		ExpectedHeadSHA: invocation.ExpectedHeadSHA, ExpectedBaseSHA: invocation.ExpectedBaseSHA,
		BaseRef: invocation.BaseRef, EffectMethod: invocation.EffectMethod, State: invocation.State,
		AuthorizationOutcome: invocation.AuthorizationOutcome, ProviderAttempt: invocation.ProviderAttempt,
		ProviderOutcome: invocation.ProviderOutcome, ProviderMerged: invocation.ProviderMerged,
		ProviderMergeSHA: invocation.ProviderMergeSHA, DenialCode: invocation.DenialCode,
		CreatedAt: invocation.CreatedAt, FinishedAt: invocation.FinishedAt,
	}
}

func newAccessGrantCredential() (raw, hash, prefix string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	raw = accessGrantTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	digest := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(digest[:])
	prefix = raw
	if len(prefix) > 20 {
		prefix = prefix[:20]
	}
	return raw, hash, prefix, nil
}

func canonicalActorSubject(sourceInstanceID, externalAgentID string) string {
	raw := strings.TrimSpace(sourceInstanceID) + "/" + strings.TrimSpace(externalAgentID)
	if len(raw) <= 255 {
		return raw
	}
	digest := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func canonicalActorDigest(sourceInstanceID, externalAgentID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(sourceInstanceID) + "\x00" + strings.TrimSpace(externalAgentID)))
	return hex.EncodeToString(digest[:])
}

func canonicalActorLogin(sourceInstanceID, externalAgentID, name string) string {
	slug := strings.ToLower(strings.TrimSpace(name))
	slug = strings.Trim(canonicalActorSlugRE.ReplaceAllString(slug, "-"), "-")
	if slug == "" {
		slug = "runtime"
	}
	if len(slug) > 18 {
		slug = strings.Trim(slug[:18], "-")
	}
	return "agent-" + slug + "-" + canonicalActorDigest(sourceInstanceID, externalAgentID)[:12]
}

func activeCanonicalActor(user db.User) bool {
	return activeAccessGrantUser(user) && user.Type == db.TypeUser && user.UserKind == db.UserKindAgent && !user.SiteAdmin && !user.IsAnonymous
}

func activeAccessGrantUser(user db.User) bool {
	return user.ID != 0 && user.Status == "active" && user.Type == db.TypeUser && !user.IsAnonymous
}

func (s *Service) accessGrantPrincipalCoversOperations(ctx context.Context, repositoryID, principalID uint, operations []string) bool {
	permission, err := s.HasRepoAccess(ctx, repositoryID, principalID)
	if err != nil {
		return false
	}
	for _, operationName := range operations {
		operation, ok := sessionauthority.LookupOperation(operationName)
		if !ok || !permission.AtLeast(repoPermissionForSessionAuthority(operation.RequiredPermission)) {
			return false
		}
	}
	return true
}

func normalizeAccessGrantOperations(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func containsAccessGrantOperation(values []string, wanted string) bool {
	index := sort.SearchStrings(values, wanted)
	return index < len(values) && values[index] == wanted
}

func appendUniqueString(values []string, wanted string) []string {
	for _, value := range values {
		if value == wanted {
			return values
		}
	}
	return append(values, wanted)
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func accessGrantSnapshotDigest(ctx context.Context, s *Service, snapshotID string) string {
	var snapshot db.ExecutionContextSnapshot
	if s != nil && s.DBForCtx(ctx).Select("context_digest").First(&snapshot, "id = ?", snapshotID).Error == nil {
		return snapshot.ContextDigest
	}
	return ""
}
