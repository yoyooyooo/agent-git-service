package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm/clause"
)

const (
	DelegatedDenialSessionMissing             = "session_missing"
	DelegatedDenialSessionRevoked             = "session_revoked"
	DelegatedDenialSessionExpired             = "session_expired"
	DelegatedDenialCredentialModeInvalid      = "credential_mode_invalid"
	DelegatedDenialPrincipalMismatch          = "principal_mismatch"
	DelegatedDenialPrincipalInactive          = "principal_inactive"
	DelegatedDenialRepositoryMismatch         = "repository_mismatch"
	DelegatedDenialRepositoryUnavailable      = "repository_unavailable"
	DelegatedDenialOperationMismatch          = "operation_mismatch"
	DelegatedDenialCapabilityMismatch         = "capability_mismatch"
	DelegatedDenialConstraintMismatch         = "constraint_mismatch"
	DelegatedDenialAuthorityUnavailable       = "authority_unavailable"
	DelegatedDenialAuthoritySnapshotChanged   = "authority_snapshot_changed"
	DelegatedDenialNativeGrantRevisionChanged = "native_grant_revision_changed"
	DelegatedDenialMembershipEpochStale       = "membership_epoch_stale"
)

// ErrDelegatedSessionUseTimeDenied is the stable internal classification for a
// delegated credential that no longer satisfies its issuance authority. HTTP
// adapters must not reflect the detailed reason to callers.
var ErrDelegatedSessionUseTimeDenied = errors.New("delegated session use-time authority denied")

// DelegatedSessionUseTimeError carries one secret-safe, feature-local denial
// reason for durable intent/job state and audit facts.
type DelegatedSessionUseTimeError struct {
	Reason string
}

func (e *DelegatedSessionUseTimeError) Error() string {
	return ErrDelegatedSessionUseTimeDenied.Error() + ": " + e.Reason
}

func (e *DelegatedSessionUseTimeError) Unwrap() error { return ErrDelegatedSessionUseTimeDenied }

func delegatedUseTimeDenied(reason string) error {
	return &DelegatedSessionUseTimeError{Reason: reason}
}

// DelegatedSessionDenialReason returns a bounded stable reason without exposing
// provider, config or credential material.
func DelegatedSessionDenialReason(err error) string {
	var denied *DelegatedSessionUseTimeError
	if errors.As(err, &denied) && denied.Reason != "" {
		return denied.Reason
	}
	return DelegatedDenialAuthorityUnavailable
}

// FreshDelegatedAuthority is a DB/config readback produced by the shared
// use-time evaluator. No caller-provided context snapshot participates in the
// decision; context contributes only the persisted Session ID.
type FreshDelegatedAuthority struct {
	Session        db.DelegatedAgentSession
	Principal      db.User
	Repository     db.Repository
	Permission     RepoPermission
	AuthorityEpoch string
}

// DelegatedSessionIDFromContext extracts only the durable Session identifier.
// The boolean reports marker presence, not ID validity: an empty fabricated
// marker must stay on the delegated fail-closed path instead of falling through
// to native or internal authority. Callers must never use the rest of the
// context snapshot as authority.
func DelegatedSessionIDFromContext(ctx context.Context) (string, bool) {
	session, ok := DelegatedSessionFromContext(ctx)
	return strings.TrimSpace(session.ID), ok
}

// ContextForDelegatedSessionID rebuilds a request context from current DB facts.
// It is used by durable background jobs that persist the originating Session ID.
// CurrentDelegatedOperationScope reloads only the persisted operation shape
// needed by a transport adapter to assemble concrete facts. It is not an
// authorization result; callers must pass those facts to
// RevalidateDelegatedSession before returning data or performing an effect.
func (s *Service) CurrentDelegatedOperationScope(ctx context.Context) (string, map[string]string, error) {
	sessionID, ok := DelegatedSessionIDFromContext(ctx)
	if !ok || sessionID == "" {
		return "", nil, delegatedUseTimeDenied(DelegatedDenialSessionMissing)
	}
	var session db.DelegatedAgentSession
	if err := s.DBForCtx(ctx).Select("id", "operation_name", "operation_constraints").First(&session, "id = ?", sessionID).Error; err != nil {
		return "", nil, delegatedUseTimeDenied(DelegatedDenialSessionMissing)
	}
	return strings.ToLower(strings.TrimSpace(session.OperationName)), cloneStringMap(session.OperationConstraints), nil
}

func (s *Service) ContextForDelegatedSessionID(ctx context.Context, sessionID string) (context.Context, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ctx, delegatedUseTimeDenied(DelegatedDenialSessionMissing)
	}
	var session db.DelegatedAgentSession
	if err := s.DBForCtx(ctx).Preload("PrincipalUser").First(&session, "id = ?", sessionID).Error; err != nil {
		return ctx, delegatedUseTimeDenied(DelegatedDenialSessionMissing)
	}
	return ContextWithDelegatedSession(ContextWithUser(ctx, session.PrincipalUser), db.DelegatedAgentSession{ID: session.ID}), nil
}

// RevalidateDelegatedSession is the single fresh use-time authority evaluator.
// expectedRepositoryID and expectedOperation identify the concrete effect;
// expectedCapability and actualConstraints further narrow its transport facts.
// The evaluator reloads Session/principal/repository and recomputes the complete
// issuance snapshot before any caller may perform a provider or repository
// effect.
func (s *Service) RevalidateDelegatedSession(ctx context.Context, expectedRepositoryID uint, expectedOperation, expectedCapability string, actualConstraints map[string]string) (FreshDelegatedAuthority, error) {
	sessionID, ok := DelegatedSessionIDFromContext(ctx)
	if !ok || sessionID == "" {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialSessionMissing)
	}
	viewer, ok := UserFromContext(ctx)
	if !ok || viewer.ID == 0 {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialPrincipalMismatch)
	}

	var session db.DelegatedAgentSession
	if err := s.DBForCtx(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Preload("PrincipalUser").Preload("Repository").First(&session, "id = ?", sessionID).Error; err != nil {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialSessionMissing)
	}
	now := time.Now().UTC()
	if strings.TrimSpace(session.CredentialMode) != accessGrantTransportCredentialMode {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialCredentialModeInvalid)
	}
	if session.RevokedAt != nil {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialSessionRevoked)
	}
	if !session.ExpiresAt.After(now) {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialSessionExpired)
	}
	if viewer.ID != session.PrincipalUserID || session.PrincipalUser.ID != session.PrincipalUserID {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialPrincipalMismatch)
	}
	if !isUserStatusActive(session.PrincipalUser.Status) {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialPrincipalInactive)
	}
	if expectedRepositoryID == 0 || session.RepositoryID != expectedRepositoryID {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialRepositoryMismatch)
	}
	if session.Repository.ID == 0 || session.Repository.Disabled {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialRepositoryUnavailable)
	}
	expectedOperation = strings.ToLower(strings.TrimSpace(expectedOperation))
	storedOperation := strings.ToLower(strings.TrimSpace(session.OperationName))
	if expectedOperation == "" || storedOperation != expectedOperation {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialOperationMismatch)
	}
	if !retiredMergeDelegationFieldsEmpty(session) {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged)
	}
	expectedCapability = strings.ToLower(strings.TrimSpace(expectedCapability))
	if expectedCapability != "" && !DelegatedSessionHasCapability(session, expectedCapability) {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialCapabilityMismatch)
	}
	if !delegatedOperationConstraintsValid(storedOperation, session.OperationConstraints) {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialConstraintMismatch)
	}
	if actualConstraints != nil && !delegatedOperationConstraintsMatch(storedOperation, session.OperationConstraints, actualConstraints) {
		return FreshDelegatedAuthority{}, delegatedUseTimeDenied(DelegatedDenialConstraintMismatch)
	}
	return s.revalidateAccessGrantTransportSession(ctx, session, expectedOperation)
}

// confirmDelegatedSessionCommitBoundary narrows the DB-side TOCTOU window by
// re-reading the mutable Session row and effective native grant at the end of
// the evaluator. When the caller supplies a transaction, the earlier
// SELECT ... FOR UPDATE serializes Session revoke/expiry changes with the
// transaction commit. External provider state cannot share this transaction,
// so callers must still invoke the evaluator immediately before every remote
// mutation.
func (s *Service) confirmDelegatedSessionCommitBoundary(ctx context.Context, initial db.DelegatedAgentSession, required RepoPermission, compareStoredRevision bool) (RepoPermission, error) {
	// Capture a fresh clock at the commit boundary. Reusing the evaluator start
	// time can admit a Session that expires while the authority queries run.
	boundaryNow := time.Now().UTC()
	var current db.DelegatedAgentSession
	if err := s.DBForCtx(ctx).Select(
		"id", "principal_user_id", "repository_id", "access_grant_id", "access_grant_authority_revision", "actor_user_id", "credential_mode", "contract_revision", "operation_name",
		"binding_id", "binding_revision", "team_identity_id", "team_binding_revision", "policy_class", "membership_epoch",
		"policy_version", "policy_snapshot_hash", "native_grant_revision", "resource_policy_revision",
		"merge_delegation_id", "merge_delegation_revision", "merge_delegation_not_after", "merge_delegation_facts_digest",
		"merge_delegation_target_instance", "merge_delegation_canonical_repository_id", "merge_delegation_provider",
		"merge_delegation_provider_binding_id", "merge_delegation_provider_binding_revision", "merge_delegation_provider_repository",
		"merge_delegation_ags_pr_number", "merge_delegation_provider_pr_number", "merge_delegation_expected_head_sha",
		"merge_delegation_expected_base_sha", "merge_delegation_base_ref", "merge_delegation_method", "merge_delegation_projection_revision",
		"revoked_at", "expires_at",
	).First(&current, "id = ?", initial.ID).Error; err != nil {
		return RepoPermissionNone, delegatedUseTimeDenied(DelegatedDenialSessionMissing)
	}
	if current.RevokedAt != nil {
		return RepoPermissionNone, delegatedUseTimeDenied(DelegatedDenialSessionRevoked)
	}
	if !current.ExpiresAt.After(boundaryNow) {
		return RepoPermissionNone, delegatedUseTimeDenied(DelegatedDenialSessionExpired)
	}
	if current.PrincipalUserID != initial.PrincipalUserID || current.RepositoryID != initial.RepositoryID ||
		current.AccessGrantID != initial.AccessGrantID || current.AccessGrantAuthorityRevision != initial.AccessGrantAuthorityRevision ||
		current.ActorUserID != initial.ActorUserID || current.CredentialMode != initial.CredentialMode ||
		current.ContractRevision != initial.ContractRevision || current.OperationName != initial.OperationName ||
		current.BindingID != initial.BindingID || current.BindingRevision != initial.BindingRevision ||
		current.TeamIdentityID != initial.TeamIdentityID || current.TeamBindingRevision != initial.TeamBindingRevision ||
		current.PolicyClass != initial.PolicyClass || current.MembershipEpoch != initial.MembershipEpoch ||
		current.PolicyVersion != initial.PolicyVersion || current.PolicySnapshotHash != initial.PolicySnapshotHash ||
		current.ResourcePolicyRevision != initial.ResourcePolicyRevision ||
		!retiredMergeDelegationFieldsEmpty(current) || !retiredMergeDelegationFieldsEmpty(initial) {
		return RepoPermissionNone, delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged)
	}
	permission, err := s.currentRepoAccess(ctx, initial.RepositoryID, initial.PrincipalUserID)
	if err != nil {
		return RepoPermissionNone, delegatedUseTimeDenied(DelegatedDenialAuthorityUnavailable)
	}
	if !permission.AtLeast(required) {
		return RepoPermissionNone, delegatedUseTimeDenied(DelegatedDenialNativeGrantRevisionChanged)
	}
	if compareStoredRevision {
		currentRevision := nativeGrantRevision(initial.PrincipalUserID, initial.RepositoryID, permission)
		if current.NativeGrantRevision != initial.NativeGrantRevision || currentRevision != initial.NativeGrantRevision {
			return RepoPermissionNone, delegatedUseTimeDenied(DelegatedDenialNativeGrantRevisionChanged)
		}
	}
	return permission, nil
}

func retiredMergeDelegationFieldsEmpty(session db.DelegatedAgentSession) bool {
	return session.MergeDelegationID == "" && session.MergeDelegationRevision == "" &&
		session.MergeDelegationNotAfter == nil && session.MergeDelegationFactsDigest == "" &&
		session.MergeDelegationTargetInstance == "" && session.MergeDelegationCanonicalRepositoryID == "" &&
		session.MergeDelegationProvider == "" && session.MergeDelegationProviderBindingID == "" &&
		session.MergeDelegationProviderBindingRevision == "" && session.MergeDelegationProviderRepository == "" &&
		session.MergeDelegationAGSPRNumber == 0 && session.MergeDelegationProviderPRNumber == 0 &&
		session.MergeDelegationExpectedHeadSHA == "" && session.MergeDelegationExpectedBaseSHA == "" &&
		session.MergeDelegationBaseRef == "" && session.MergeDelegationMethod == "" &&
		session.MergeDelegationProjectionRevision == ""
}
