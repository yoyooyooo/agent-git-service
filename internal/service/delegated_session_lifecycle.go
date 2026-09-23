package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/operationconstraints"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"github.com/ngaut/agent-git-service/internal/workloadidentity"
)

const (
	DelegatedSessionLifecycleSchema     = "ags.delegated-session-lifecycle.v1"
	DelegatedSessionLifecycleClaimLimit = "This readback proves the persisted lifecycle state and expiry-audited marker for one delegated Session; it does not expose authentication material or prove external provider effects."
)

var (
	lifecycleIssueKeyRE          = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[1-9][0-9]*$`)
	lifecycleGrantRevisionRE     = regexp.MustCompile(`^grant-rev:[a-f0-9]{64}$`)
	lifecycleAuthorityRevisionRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	lifecycleRepositoryRE        = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	lifecycleSecretValueRE       = regexp.MustCompile(`(mat_[A-Za-z0-9_-]+|ags_sess_[A-Za-z0-9_-]+|eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
)

type DelegatedSessionLifecycleAuthority struct {
	ContractRevision    string                             `json:"contract_revision"`
	Principal           DelegatedSessionPrincipal          `json:"principal"`
	TeamIdentityID      string                             `json:"team_identity_id"`
	TeamBindingRevision string                             `json:"team_binding_revision"`
	PolicyClass         string                             `json:"policy_class"`
	MembershipEpoch     int64                              `json:"membership_epoch"`
	IssuerInstanceID    string                             `json:"issuer_instance_id"`
	Target              DelegatedSessionTarget             `json:"target"`
	Resource            DelegatedSessionResource           `json:"resource"`
	Operation           DelegatedSessionOperation          `json:"operation"`
	AuthorizationBasis  DelegatedSessionAuthorizationBasis `json:"authorization_basis"`
}

type DelegatedSessionLifecycleProvenance struct {
	Schema        string `json:"schema"`
	WorkspaceID   string `json:"workspace_id"`
	AgentID       string `json:"agent_id"`
	SquadID       string `json:"squad_id,omitempty"`
	IssueID       string `json:"issue_id,omitempty"`
	IssueKey      string `json:"issue_key,omitempty"`
	TaskID        string `json:"task_id"`
	RunID         string `json:"run_id"`
	CorrelationID string `json:"correlation_id"`
	TriggerID     string `json:"trigger_id,omitempty"`
	RuntimeID     string `json:"runtime_id,omitempty"`
}

type DelegatedSessionLifecycleReadback struct {
	Schema          string                              `json:"schema"`
	SessionID       string                              `json:"session_id"`
	State           string                              `json:"state"`
	CreatedAt       time.Time                           `json:"created_at"`
	ExpiresAt       time.Time                           `json:"expires_at"`
	RevokedAt       *time.Time                          `json:"revoked_at,omitempty"`
	ExpiryAuditedAt *time.Time                          `json:"expiry_audited_at,omitempty"`
	Authority       DelegatedSessionLifecycleAuthority  `json:"authority"`
	Provenance      DelegatedSessionLifecycleProvenance `json:"provenance"`
	ClaimLimit      string                              `json:"claim_limit"`
}

// GetDelegatedSessionLifecycle returns one closed, credential-free lifecycle
// projection for a canonical team-v4 Session. It is deliberately unavailable
// to delegated credentials even when their immutable principal is a site
// administrator: this is a durable-operator inspection surface, not a new
// delegated Session capability.
func (s *Service) GetDelegatedSessionLifecycle(ctx context.Context, sessionID string) (DelegatedSessionLifecycleReadback, error) {
	if _, delegated := DelegatedSessionFromContext(ctx); delegated {
		return DelegatedSessionLifecycleReadback{}, ErrForbidden
	}
	contextViewer, ok := UserFromContext(ctx)
	if !ok || contextViewer.ID == 0 {
		return DelegatedSessionLifecycleReadback{}, ErrForbidden
	}

	database := s.DBForCtx(ctx)
	var viewer db.User
	if err := database.Select("id", "type", "status", "site_admin").First(&viewer, "id = ?", contextViewer.ID).Error; err != nil || viewer.Type != db.TypeUser || !viewer.SiteAdmin || !isUserStatusActive(viewer.Status) {
		return DelegatedSessionLifecycleReadback{}, ErrForbidden
	}

	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return DelegatedSessionLifecycleReadback{}, ErrNotFound
	}
	var session db.DelegatedAgentSession
	if err := database.Select(
		"id", "principal_user_id", "access_grant_id", "access_grant_authority_revision", "actor_user_id",
		"contract_revision", "credential_mode", "team_identity_id", "team_binding_revision", "policy_class", "membership_epoch",
		"issuer_instance_id", "trust_revision", "issuer_workspace_id", "external_agent_id", "external_squad_id", "external_issue_id", "external_issue_key",
		"external_task_id", "external_run_id", "correlation_id", "external_trigger_id", "external_runtime_id", "workload_context_schema",
		"target_instance", "resource_service", "repository_id", "operation_name", "operation_constraints", "native_grant_revision",
		"resource_policy_revision", "policy_version", "created_at", "expires_at", "revoked_at", "expiry_audited_at",
	).First(&session, "id = ?", sessionID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return DelegatedSessionLifecycleReadback{}, ErrNotFound
		}
		return DelegatedSessionLifecycleReadback{}, err
	}

	var principal db.User
	if err := database.Select("id", "login").First(&principal, "id = ?", session.PrincipalUserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return DelegatedSessionLifecycleReadback{}, invalidLifecycleRow()
		}
		return DelegatedSessionLifecycleReadback{}, err
	}
	var repository db.Repository
	if err := database.Select("id", "full_name").First(&repository, "id = ?", session.RepositoryID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return DelegatedSessionLifecycleReadback{}, invalidLifecycleRow()
		}
		return DelegatedSessionLifecycleReadback{}, err
	}

	now := time.Now().UTC()
	if err := validateCanonicalLifecycleSession(session, principal, repository, now); err != nil {
		return DelegatedSessionLifecycleReadback{}, err
	}
	createdAt := session.CreatedAt.UTC()
	expiresAt := session.ExpiresAt.UTC()
	revokedAt := utcTimePointer(session.RevokedAt)
	expiryAuditedAt := utcTimePointer(session.ExpiryAuditedAt)
	requestedScope := session.OperationName + ":" + repository.FullName
	return DelegatedSessionLifecycleReadback{
		Schema: DelegatedSessionLifecycleSchema, SessionID: session.ID,
		State: delegatedSessionState(session, now), CreatedAt: createdAt, ExpiresAt: expiresAt,
		RevokedAt: revokedAt, ExpiryAuditedAt: expiryAuditedAt,
		Authority: DelegatedSessionLifecycleAuthority{
			ContractRevision: session.ContractRevision,
			Principal:        DelegatedSessionPrincipal{ID: principal.ID, Login: principal.Login},
			TeamIdentityID:   session.TeamIdentityID, TeamBindingRevision: session.TeamBindingRevision,
			PolicyClass: session.PolicyClass, MembershipEpoch: session.MembershipEpoch, IssuerInstanceID: session.IssuerInstanceID,
			Target:   DelegatedSessionTarget{Instance: session.TargetInstance, Repository: repository.FullName},
			Resource: DelegatedSessionResource{Service: session.ResourceService, Repository: repository.FullName},
			// Lifecycle v1 deliberately omits stored operation constraints. They
			// are validated before this response is built, but provider-effect
			// attribution belongs to owning operation receipts, not this surface.
			Operation: DelegatedSessionOperation{Name: session.OperationName},
			AuthorizationBasis: DelegatedSessionAuthorizationBasis{
				TrustRevision: session.TrustRevision, NativeGrantRevision: session.NativeGrantRevision,
				ResourcePolicyRevision: session.ResourcePolicyRevision, PolicyClassRevision: session.PolicyVersion,
				RequestedOperationScope: requestedScope,
			},
		},
		Provenance: DelegatedSessionLifecycleProvenance{
			Schema: session.WorkloadContextSchema, WorkspaceID: session.IssuerWorkspaceID,
			AgentID: session.ExternalAgentID, SquadID: session.ExternalSquadID,
			IssueID: session.ExternalIssueID, IssueKey: session.ExternalIssueKey,
			TaskID: session.ExternalTaskID, RunID: session.ExternalRunID, CorrelationID: session.CorrelationID,
			TriggerID: session.ExternalTriggerID, RuntimeID: session.ExternalRuntimeID,
		},
		ClaimLimit: DelegatedSessionLifecycleClaimLimit,
	}, nil
}

func invalidLifecycleRow() error {
	return fmt.Errorf("%w: canonical team-v4 delegated Session lifecycle is unavailable", ErrValidation)
}

func validateCanonicalLifecycleSession(session db.DelegatedAgentSession, principal db.User, repository db.Repository, now time.Time) error {
	invalid := invalidLifecycleRow
	credentialModeValid := session.CredentialMode == "delegated_session" || session.CredentialMode == accessGrantTransportCredentialMode
	transportAuthorityValid := session.CredentialMode != accessGrantTransportCredentialMode ||
		(canonicalLifecycleUUID(session.AccessGrantID) && lifecycleAuthorityRevisionRE.MatchString(session.AccessGrantAuthorityRevision) &&
			session.ActorUserID > 0 && uint64(session.ActorUserID) <= uint64(operationconstraints.MaxJSONSafePositiveInteger))
	if session.ContractRevision != sessionauthority.ContractRevision || !credentialModeValid || !transportAuthorityValid ||
		!canonicalLifecycleUUID(session.ID) || principal.ID == 0 || uint64(principal.ID) > uint64(operationconstraints.MaxJSONSafePositiveInteger) || !canonicalLifecycleText(principal.Login) ||
		!canonicalLifecycleUUID(session.TeamIdentityID) || !canonicalLifecycleText(session.TeamBindingRevision) ||
		!canonicalLifecycleText(session.PolicyClass) || session.MembershipEpoch <= 0 || session.MembershipEpoch > operationconstraints.MaxJSONSafePositiveInteger || !canonicalLifecycleText(session.IssuerInstanceID) ||
		!canonicalLifecycleText(session.TrustRevision) || !canonicalLifecycleUUID(session.IssuerWorkspaceID) ||
		!canonicalLifecycleUUID(session.ExternalAgentID) || !canonicalLifecycleUUID(session.ExternalTaskID) ||
		!canonicalLifecycleUUID(session.ExternalRunID) || !canonicalLifecycleUUID(session.CorrelationID) ||
		session.WorkloadContextSchema != workloadidentity.WorkloadContextSchema || !canonicalLifecycleText(session.TargetInstance) ||
		session.ResourceService != "ags" || !canonicalLifecycleText(repository.FullName) || !lifecycleRepositoryRE.MatchString(repository.FullName) ||
		!operationconstraints.IsDefaultOperation(session.OperationName) || !canonicalLifecycleText(session.ResourcePolicyRevision) ||
		!canonicalLifecycleText(session.PolicyVersion) || !lifecycleGrantRevisionRE.MatchString(session.NativeGrantRevision) {
		return invalid()
	}
	if _, err := operationconstraints.NormalizeStored(session.OperationName, session.OperationConstraints); err != nil {
		return invalid()
	}
	for _, optionalUUID := range []string{session.ExternalSquadID, session.ExternalTriggerID, session.ExternalRuntimeID} {
		if optionalUUID != "" && !canonicalLifecycleUUID(optionalUUID) {
			return invalid()
		}
	}
	if (session.ExternalIssueID == "") != (session.ExternalIssueKey == "") {
		return invalid()
	}
	if session.ExternalIssueID != "" && (!canonicalLifecycleUUID(session.ExternalIssueID) || !lifecycleIssueKeyRE.MatchString(session.ExternalIssueKey)) {
		return invalid()
	}
	createdAt := session.CreatedAt.UTC()
	expiresAt := session.ExpiresAt.UTC()
	if createdAt.IsZero() || expiresAt.IsZero() || !expiresAt.After(createdAt) {
		return invalid()
	}
	if session.RevokedAt != nil && session.RevokedAt.UTC().Before(createdAt) {
		return invalid()
	}
	if session.ExpiryAuditedAt != nil && session.ExpiryAuditedAt.UTC().Before(expiresAt) {
		return invalid()
	}
	state := delegatedSessionState(session, now)
	if state == "active" && session.ExpiryAuditedAt != nil {
		return invalid()
	}
	if state == "expired" && session.RevokedAt != nil {
		return invalid()
	}
	return nil
}

func canonicalLifecycleUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func canonicalLifecycleText(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "\r\n\x00") &&
		!sessionauthority.IsSecretShapedValue(value) && !lifecycleSecretValueRE.MatchString(value)
}

func utcTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}
