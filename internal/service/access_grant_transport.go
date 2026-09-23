package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	AccessGrantTransportSessionSchema  = "ags.access-grant-transport-session.v1"
	accessGrantTransportCredentialMode = "access_grant_transport"
	accessGrantTransportIssuer         = "ags.access-grant"
	accessGrantTransportPurpose        = "access_grant_transport"
	accessGrantTransportAudience       = "ags:internal:transport-session:v1"
	maximumAccessGrantTransportTTL     = 15 * time.Minute
)

func lowRiskTransportCapabilities(operation string) []string {
	switch strings.ToLower(strings.TrimSpace(operation)) {
	case "repo.read", "git.read", "pr.read", "review.read", "ci.read":
		return []string{"repo:read"}
	case "git.push", "pr.rebase":
		return []string{"repo:read", "repo:write"}
	case "pr.create":
		return []string{"repo:read", "pr:create"}
	default:
		return []string{"repo:read"}
	}
}

// AccessGrantTransportSessionResult is the internal adapter result used by
// existing Git and GitHub-compatible API transports. The bearer is returned exactly once and must
// never enter public command output, logs, PR bodies, or source snapshots.
type AccessGrantTransportSessionResult struct {
	Schema       string                       `json:"schema"`
	SessionToken string                       `json:"session_token"`
	Session      DelegatedSessionReceipt      `json:"session"`
	Invocation   AccessGrantInvocationReceipt `json:"invocation"`
}

// IssueAccessGrantTransportSession derives one exact operation-scoped transport
// Session from an already-authoritative Access Grant. Multica is not called and
// no workload assertion is accepted by this path.
func (s *Service) IssueAccessGrantTransportSession(ctx context.Context, rawGrant string, input AccessGrantOperationInput) (AccessGrantTransportSessionResult, error) {
	grant, actor, executor, err := s.resolveActiveAccessGrant(ctx, rawGrant)
	if err != nil {
		return AccessGrantTransportSessionResult{}, err
	}
	operationName := strings.ToLower(strings.TrimSpace(input.Operation))
	switch operationName {
	case "pr.merge":
		return AccessGrantTransportSessionResult{}, fmt.Errorf("%w: pr.merge requires the effect endpoint", ErrAccessGrantDenied)
	case "repo.create", "repo.admin", "repo.delete", "review.dismiss",
		"git.force_push", "protected_ref.write", "ref.delete", "branch_protection.write", "webhook.write":
		return AccessGrantTransportSessionResult{}, fmt.Errorf("%w: %s is not a generic transport operation", ErrAccessGrantDenied, operationName)
	}
	operationName = operationcatalog.CanonicalName(operationName)
	lowRiskOperation := operationcatalog.IsStandard(operationName)
	if !containsAccessGrantOperation(grant.EffectiveOperations, operationName) {
		// Collaboration pass-through: ordinary Git/PR ops stay transportable even
		// when an older grant envelope omitted them, as long as the op is in the
		// hard low-risk allowlist.
		if !lowRiskOperation {
			return AccessGrantTransportSessionResult{}, fmt.Errorf("%w: operation is outside the grant", ErrAccessGrantDenied)
		}
	}
	constraints, err := operationconstraints.NormalizeJSON(operationName, input.Constraints)
	if err != nil {
		return AccessGrantTransportSessionResult{}, fmt.Errorf("%w: operation constraints are invalid", ErrValidation)
	}
	if _, _, _, err := s.revalidateAccessGrant(ctx, grant); err != nil {
		return AccessGrantTransportSessionResult{}, err
	}
	_, source, current, err := s.GetExecutionContextSnapshot(ctx, grant.SnapshotID)
	if err != nil {
		return AccessGrantTransportSessionResult{}, ErrAccessGrantUnavailable
	}
	// RunID is association-only under pass-through; do not block transport when
	// Multica reinjects a new run for the same workspace/agent/task.
	if source.SourceInstanceID != grant.SourceInstanceID || source.WorkspaceID != grant.ExternalWorkspaceID ||
		source.AgentID != grant.ExternalAgentID || source.TaskID != grant.ExternalTaskID {
		return AccessGrantTransportSessionResult{}, ErrAccessGrantConflict
	}
	if s.PrincipalSessions == nil {
		return AccessGrantTransportSessionResult{}, ErrAccessGrantUnavailable
	}
	authorityClass := grant.DefaultPolicyClass
	authorityBindingRevision := grant.DefaultBindingRevision
	if grant.ElevatedBindingRevision != "" {
		if grant.PolicyClassOutcome != "accepted" || grant.RequestedPolicyClass == "" {
			return AccessGrantTransportSessionResult{}, fmt.Errorf("%w: transport elevated authority is inconsistent", ErrAccessGrantConflict)
		}
		authorityClass = grant.RequestedPolicyClass
		authorityBindingRevision = grant.ElevatedBindingRevision
	}
	authority, err := s.PrincipalSessions.ResolveRuntimePolicyAuthority(source.SourceInstanceID, source.WorkspaceID, authorityClass)
	if err != nil || authority.TeamBinding.PrincipalID != executor.ID {
		// Low-risk collaboration ops tolerate binding-revision packaging drift.
		if !lowRiskOperation || err != nil || authority.TeamBinding.PrincipalID != executor.ID {
			return AccessGrantTransportSessionResult{}, fmt.Errorf("%w: transport executor authority changed", ErrAccessGrantConflict)
		}
	} else if !lowRiskOperation && authority.TeamBinding.BindingRevision != authorityBindingRevision {
		return AccessGrantTransportSessionResult{}, fmt.Errorf("%w: transport executor authority changed", ErrAccessGrantConflict)
	}
	var resolvedOperation sessionauthority.ResolvedResourceOperation
	if lowRiskOperation {
		// Ordinary Git/PR transport uses a hard operation allowlist + grant
		// binding; association/policy packaging must not become a hot-path gate.
		resolvedOperation, err = s.PrincipalSessions.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
			Target: grant.TargetInstance, Service: "ags", Repository: grant.RepositoryFullName,
			Operation: operationName,
		})
		if err != nil {
			// Fall back to a minimal capability set for known low-risk ops.
			resolvedOperation = sessionauthority.ResolvedResourceOperation{
				Operation: sessionauthority.Operation{
					Name: operationName, RequiredPermission: sessionauthority.PermissionWrite,
					Capabilities: lowRiskTransportCapabilities(operationName),
				},
			}
			err = nil
		}
	} else {
		resolvedOperation, err = s.PrincipalSessions.ResolveResourceOperation(sessionauthority.ResourceOperationRequest{
			Target: grant.TargetInstance, Service: "ags", Repository: grant.RepositoryFullName,
			Operation: operationName,
		})
		if err != nil {
			return AccessGrantTransportSessionResult{}, fmt.Errorf("%w: transport operation authority changed", ErrAccessGrantConflict)
		}
	}
	permission, err := s.HasRepoAccess(ctx, grant.RepositoryID, executor.ID)
	if err != nil {
		return AccessGrantTransportSessionResult{}, ErrAccessGrantUnavailable
	}
	requiredPermission := repoPermissionForSessionAuthority(resolvedOperation.Operation.RequiredPermission)
	if !permission.AtLeast(requiredPermission) {
		// Low-risk ops already bound by a valid grant; native grant drift becomes
		// a warning path only when write is required and completely absent.
		if !lowRiskOperation || !permission.AtLeast(RepoPermissionRead) {
			return AccessGrantTransportSessionResult{}, fmt.Errorf("%w: transport executor native grant changed", ErrAccessGrantConflict)
		}
	}
	grantedCapabilities, err := operationCapabilities(nil, resolvedOperation.Operation.Capabilities)
	if err != nil {
		return AccessGrantTransportSessionResult{}, err
	}

	now := time.Now().UTC()
	ttl := resolvedOperation.Resource.MaxSessionDuration()
	if ttl <= 0 || ttl > maximumAccessGrantTransportTTL {
		ttl = maximumAccessGrantTransportTTL
	}
	if remaining := grant.ExpiresAt.Sub(now); remaining <= 0 {
		return AccessGrantTransportSessionResult{}, ErrAccessGrantCredentialInvalid
	} else if remaining < ttl {
		ttl = remaining
	}
	rawSession, sessionHash, sessionPrefix, err := newDelegatedSessionCredential()
	if err != nil {
		return AccessGrantTransportSessionResult{}, ErrAccessGrantUnavailable
	}
	delegatedByUserID, delegatedByLogin, err := s.delegatedSessionDelegatorSnapshot(ctx, actor)
	if err != nil {
		return AccessGrantTransportSessionResult{}, ErrAccessGrantUnavailable
	}
	delegatedBySource := db.DelegatedBySourcePrincipalOnly
	if delegatedByUserID != nil {
		delegatedBySource = db.DelegatedBySourceSessionSnapshot
	}

	invocationID := uuid.NewString()
	constraintsJSON, _ := json.Marshal(constraints)
	invocation := db.AccessGrantInvocation{
		ID: invocationID, GrantID: grant.ID, ActorUserID: actor.ID, ExecutorUserID: executor.ID,
		RepositoryID: grant.RepositoryID, Repository: grant.RepositoryFullName, Operation: operationName,
		ConstraintsJSON: string(constraintsJSON), AuthorityRevision: grant.AuthorityRevision,
		State: "completed", AuthorizationOutcome: "allowed", ProviderAttempt: "not_attempted", ProviderOutcome: "not_applicable",
		CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
	}
	missingFields, traceQuality := accessGrantTransportTrace(current)
	membershipEpoch := authority.TeamBinding.EpochFloor
	if membershipEpoch < 1 {
		membershipEpoch = 1
	}
	session := db.DelegatedAgentSession{
		ID: uuid.NewString(), CredentialHash: sessionHash, CredentialPrefix: sessionPrefix,
		PrincipalUserID: executor.ID, PrincipalLogin: executor.Login,
		AccessGrantID: grant.ID, AccessGrantAuthorityRevision: grant.AuthorityRevision, ActorUserID: actor.ID,
		DelegatedByUserID: delegatedByUserID, DelegatedByLogin: delegatedByLogin, DelegatedBySource: delegatedBySource,
		Issuer: accessGrantTransportIssuer, AssertionVersion: 1, AssertionPurpose: accessGrantTransportPurpose,
		AssertionJTI: invocationID, AssertionAudience: accessGrantTransportAudience,
		ContractRevision: sessionauthority.ContractRevision, CredentialMode: accessGrantTransportCredentialMode,
		TeamIdentityID: authority.TeamBinding.TeamIdentityID, TeamBindingRevision: authority.TeamBinding.BindingRevision,
		PolicyClass: authority.PolicyClass.ID, MembershipEpoch: membershipEpoch,
		IssuerInstanceID: source.SourceInstanceID, TrustRevision: grant.AuthorityRevision,
		IssuerSubject: "urn:multica:agent:" + source.AgentID, CorrelationID: grant.ID,
		IssuerWorkspaceID: source.WorkspaceID, IssuerWorkspace: current.Workspace.Slug,
		ExternalAgentID: source.AgentID, ExternalAgentName: current.Agent.Name,
		ExternalTaskID: source.TaskID, ExternalRunID: source.RunID,
		WorkloadContextSchema: "workload.context.v1", TraceQuality: traceQuality, MissingFields: missingFields,
		TargetInstance: grant.TargetInstance, ResourceService: "ags", RepositoryID: grant.RepositoryID,
		OperationName: operationName, OperationConstraints: cloneStringMap(constraints),
		GrantedCapabilities: grantedCapabilities, PolicyVersion: authority.PolicyClass.PolicyRevision,
		PolicySnapshotHash:     strings.TrimPrefix(grant.AuthorityRevision, "sha256:"),
		NativeGrantRevision:    nativeGrantRevision(executor.ID, grant.RepositoryID, permission),
		ResourcePolicyRevision: grant.ResourcePolicyRevision,
		CreatedAt:              now, ExpiresAt: now.Add(ttl),
	}
	if current.Squad != nil {
		session.ExternalSquadID = current.Squad.ID
	}
	if current.Issue != nil {
		session.ExternalIssueID = current.Issue.ID
		session.ExternalIssueKey = current.Issue.Key
	}
	if current.Trigger != nil {
		session.ExternalTriggerID = current.Trigger.ID
	}
	if current.Runtime != nil {
		session.ExternalRuntimeID = current.Runtime.ID
	}

	database := s.DBForCtx(ctx)
	if err := database.Transaction(func(tx *gorm.DB) error {
		var locked db.AccessGrant
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, "id = ?", grant.ID).Error; err != nil {
			return ErrAccessGrantCredentialInvalid
		}
		boundaryNow := time.Now().UTC()
		if locked.RevokedAt != nil || !locked.ExpiresAt.After(boundaryNow) || locked.AuthorityRevision != grant.AuthorityRevision {
			return ErrAccessGrantConflict
		}
		if err := tx.Create(&invocation).Error; err != nil {
			return err
		}
		if err := tx.Create(&session).Error; err != nil {
			return err
		}
		if err := tx.Model(&db.AccessGrant{}).Where("id = ?", grant.ID).Update("last_used_at", boundaryNow).Error; err != nil {
			return err
		}
		auditCtx := ContextWithDB(ContextWithDelegatedSession(ContextWithUser(ctx, executor), session), tx)
		return s.LogCurrentDelegatedSessionAudit(auditCtx, DelegatedSessionAuditEvent{
			Action: AuditActionAccessGrantTransportSession, Operation: operationName, Outcome: "success",
		})
	}); err != nil {
		if errors.Is(err, ErrAccessGrantCredentialInvalid) || errors.Is(err, ErrAccessGrantConflict) {
			return AccessGrantTransportSessionResult{}, err
		}
		return AccessGrantTransportSessionResult{}, ErrAccessGrantUnavailable
	}
	return AccessGrantTransportSessionResult{
		Schema: AccessGrantTransportSessionSchema, SessionToken: rawSession,
		Session:    delegatedSessionReceipt(session, executor, db.Repository{ID: grant.RepositoryID, FullName: grant.RepositoryFullName}),
		Invocation: accessGrantInvocationReceipt(invocation),
	}, nil
}

func accessGrantTransportTrace(current executioncontext.CurrentContext) ([]string, string) {
	missing := make([]string, 0, 5)
	if current.Squad == nil || strings.TrimSpace(current.Squad.ID) == "" {
		missing = append(missing, "squad_id")
	}
	if current.Issue == nil || strings.TrimSpace(current.Issue.ID) == "" {
		missing = append(missing, "issue_id")
	}
	if current.Issue == nil || strings.TrimSpace(current.Issue.Key) == "" {
		missing = append(missing, "issue_key")
	}
	if current.Trigger == nil || strings.TrimSpace(current.Trigger.ID) == "" {
		missing = append(missing, "trigger_id")
	}
	if current.Runtime == nil || strings.TrimSpace(current.Runtime.ID) == "" {
		missing = append(missing, "runtime_id")
	}
	if len(missing) == 0 {
		return []string{}, "complete"
	}
	return missing, "trace_degraded"
}

func (s *Service) revalidateAccessGrantTransportSession(ctx context.Context, session db.DelegatedAgentSession, expectedOperation string) (FreshDelegatedAuthority, error) {
	if session.AccessGrantID == "" || session.AccessGrantAuthorityRevision == "" || session.ActorUserID == 0 || session.CredentialMode != accessGrantTransportCredentialMode {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged)
	}
	var grant db.AccessGrant
	if err := s.DBForCtx(ctx).First(&grant, "id = ?", session.AccessGrantID).Error; err != nil {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialAuthorityUnavailable)
	}
	actor, executor, repository, err := s.revalidateAccessGrant(ctx, grant)
	if err != nil {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged)
	}
	if actor.ID != session.ActorUserID || executor.ID != session.PrincipalUserID || repository.ID != session.RepositoryID ||
		grant.RepositoryFullName != session.Repository.FullName || grant.TargetInstance != session.TargetInstance ||
		grant.ExternalWorkspaceID != session.IssuerWorkspaceID || grant.ExternalAgentID != session.ExternalAgentID ||
		grant.ExternalTaskID != session.ExternalTaskID || grant.ExternalRunID != session.ExternalRunID ||
		grant.AuthorityRevision != session.AccessGrantAuthorityRevision || grant.AuthorityRevision != session.TrustRevision ||
		grant.ResourcePolicyRevision != session.ResourcePolicyRevision || !containsAccessGrantOperation(grant.EffectiveOperations, expectedOperation) ||
		session.ExpiresAt.After(grant.ExpiresAt) {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged)
	}
	operation, ok := sessionauthority.LookupOperation(expectedOperation)
	if !ok {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialOperationMismatch)
	}
	required := repoPermissionForSessionAuthority(operation.RequiredPermission)
	permission, err := s.currentRepoAccess(ctx, session.RepositoryID, session.PrincipalUserID)
	if err != nil || !permission.AtLeast(required) {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialNativeGrantRevisionChanged)
	}
	if nativeGrantRevision(session.PrincipalUserID, session.RepositoryID, permission) != session.NativeGrantRevision {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialNativeGrantRevisionChanged)
	}
	permission, err = s.confirmDelegatedSessionCommitBoundary(ctx, session, required, true)
	if err != nil {
		return FreshDelegatedAuthority{}, err
	}
	epoch := ""
	if s.PrincipalSessions != nil {
		epoch, _ = authorityEpochForConfig(s.PrincipalSessions.Config())
	}
	return FreshDelegatedAuthority{Session: session, Principal: executor, Repository: repository, Permission: permission, AuthorityEpoch: epoch}, nil
}
