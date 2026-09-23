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
	"sort"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/operationconstraints"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"github.com/ngaut/agent-git-service/internal/workloadidentity"
)

const delegatedSessionTokenPrefix = "ags_sess_"

var (
	ErrDelegatedSessionCredentialInvalid         = errors.New("delegated session credential invalid")
	ErrDelegatedSessionOperationConstraintDenied = errors.New("delegated session operation constraint denied")
)

var delegatedSessionCapabilities = map[string]struct{}{
	"repo:read":  {},
	"repo:write": {},
	"pr:create":  {},
}

type DelegatedSessionPrincipal struct {
	ID    uint   `json:"id"`
	Login string `json:"login"`
}

type DelegatedSessionActor struct {
	Provider    string `json:"provider"`
	WorkspaceID string `json:"workspace_id"`
	Workspace   string `json:"workspace,omitempty"`
	AgentID     string `json:"agent_id"`
	AgentName   string `json:"agent_name"`
	TaskID      string `json:"task_id"`
	RunID       string `json:"run_id,omitempty"`
	IssueID     string `json:"issue_id,omitempty"`
	IssueKey    string `json:"issue_key,omitempty"`
}

type DelegatedSessionTarget struct {
	Instance   string `json:"instance"`
	Repository string `json:"repository"`
}

type DelegatedSessionResource struct {
	Service    string `json:"service"`
	Repository string `json:"repository"`
}

type DelegatedSessionOperation struct {
	Name        string            `json:"name"`
	Constraints map[string]string `json:"constraints,omitempty"`
}

type DelegatedSessionAuthorizationBasis struct {
	TrustRevision           string `json:"trust_revision"`
	NativeGrantRevision     string `json:"native_grant_revision"`
	ResourcePolicyRevision  string `json:"resource_policy_revision"`
	PolicyClassRevision     string `json:"policy_class_revision,omitempty"`
	RequestedOperationScope string `json:"requested_operation_scope"`
}

type DelegatedSessionReceipt struct {
	ContractRevision    string                             `json:"contract_revision,omitempty"`
	ID                  string                             `json:"id"`
	Principal           DelegatedSessionPrincipal          `json:"principal"`
	BindingID           string                             `json:"binding_id,omitempty"`
	TeamIdentityID      string                             `json:"team_identity_id,omitempty"`
	TeamBindingRevision string                             `json:"team_binding_revision,omitempty"`
	PolicyClass         string                             `json:"policy_class,omitempty"`
	MembershipEpoch     int64                              `json:"membership_epoch,omitempty"`
	CredentialMode      string                             `json:"credential_mode,omitempty"`
	Actor               DelegatedSessionActor              `json:"actor"`
	WorkloadContext     workloadidentity.WorkloadContext   `json:"workload_context"`
	TraceQuality        string                             `json:"trace_quality,omitempty"`
	MissingFields       []string                           `json:"missing_fields"`
	Target              DelegatedSessionTarget             `json:"target"`
	Resource            DelegatedSessionResource           `json:"resource"`
	Operation           DelegatedSessionOperation          `json:"operation"`
	EffectiveTTL        string                             `json:"effective_ttl,omitempty"`
	AuthorizationBasis  DelegatedSessionAuthorizationBasis `json:"authorization_basis"`
	GrantedCapabilities []string                           `json:"granted_capabilities"`
	PolicyVersion       string                             `json:"policy_version"`
	State               string                             `json:"state"`
	CreatedAt           time.Time                          `json:"created_at"`
	ExpiresAt           time.Time                          `json:"expires_at"`
	LastUsedAt          *time.Time                         `json:"last_used_at,omitempty"`
	RevokedAt           *time.Time                         `json:"revoked_at,omitempty"`
	RevokedByUserID     *uint                              `json:"revoked_by_user_id,omitempty"`
	RevocationReason    string                             `json:"revocation_reason,omitempty"`
}

func ContextWithDelegatedSession(ctx context.Context, session db.DelegatedAgentSession) context.Context {
	return context.WithValue(ctx, ctxKeyDelegatedSession, session)
}

func DelegatedSessionFromContext(ctx context.Context) (db.DelegatedAgentSession, bool) {
	session, ok := ctx.Value(ctxKeyDelegatedSession).(db.DelegatedAgentSession)
	return session, ok
}

func IsDelegatedSessionCredential(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), delegatedSessionTokenPrefix)
}

func DelegatedSessionHasCapability(session db.DelegatedAgentSession, capability string) bool {
	capability = strings.ToLower(strings.TrimSpace(capability))
	for _, granted := range session.GrantedCapabilities {
		if strings.ToLower(strings.TrimSpace(granted)) == capability {
			return true
		}
	}
	return false
}

// ValidateDelegatedSessionOperationConstraints enforces signed narrowing at the
// point where concrete request facts are available. Non-session principals are
// unaffected. Unknown constraints on persisted credentials fail closed.
func ValidateDelegatedSessionOperationConstraints(ctx context.Context, operation string, actual map[string]string) error {
	session, ok := DelegatedSessionFromContext(ctx)
	if !ok {
		return nil
	}
	if strings.TrimSpace(operation) != session.OperationName || !delegatedOperationConstraintsMatch(session.OperationName, session.OperationConstraints, actual) {
		return ErrDelegatedSessionOperationConstraintDenied
	}
	return nil
}

func delegatedOperationConstraintsValid(operation string, expected map[string]string) bool {
	if operationconstraints.IsDefaultOperation(operation) {
		_, err := operationconstraints.NormalizeStored(operation, expected)
		return err == nil
	}
	// Deferred Session operations retain their existing empty vector.
	return len(expected) == 0
}

func delegatedOperationConstraintsMatch(operation string, expected, actual map[string]string) bool {
	if operationconstraints.IsDefaultOperation(operation) {
		return operationconstraints.Match(operation, expected, actual)
	}
	return len(expected) == 0 && len(actual) == 0
}

func (s *Service) delegatedSessionDelegatorSnapshot(ctx context.Context, principal db.User) (*uint, string, error) {
	if principal.UserKind == db.UserKindHuman {
		id := principal.ID
		return &id, principal.Login, nil
	}
	humanID, ok, err := s.boundHumanIDForAgent(ctx, principal.ID)
	if err != nil || !ok {
		return nil, "", err
	}
	var human db.User
	if err := s.DBForCtx(ctx).Select("id", "login").First(&human, "id = ?", humanID).Error; err != nil {
		return nil, "", err
	}
	id := human.ID
	return &id, human.Login, nil
}

func operationCapabilities(requested, operationCapabilities []string) ([]string, error) {
	allowed := make(map[string]struct{}, len(operationCapabilities))
	for _, capability := range operationCapabilities {
		allowed[strings.ToLower(strings.TrimSpace(capability))] = struct{}{}
	}
	if len(requested) == 0 {
		requested = operationCapabilities
	}
	granted := make([]string, 0, len(requested))
	for _, raw := range requested {
		capability := strings.ToLower(strings.TrimSpace(raw))
		if _, safe := delegatedSessionCapabilities[capability]; !safe {
			return nil, fmt.Errorf("%w: operation uses unsupported transport capability %q", ErrAccessGrantConflict, capability)
		}
		if _, ok := allowed[capability]; !ok {
			return nil, fmt.Errorf("%w: transport capability %q does not belong to the requested operation", ErrAccessGrantConflict, capability)
		}
		granted = append(granted, capability)
	}
	if len(granted) == 0 {
		return nil, fmt.Errorf("%w: requested operation scope is empty", ErrAccessGrantConflict)
	}
	sort.Strings(granted)
	return granted, nil
}

func repoPermissionForSessionAuthority(permission sessionauthority.Permission) RepoPermission {
	switch permission {
	case sessionauthority.PermissionAdmin:
		return RepoPermissionAdmin
	case sessionauthority.PermissionWrite:
		return RepoPermissionWrite
	default:
		return RepoPermissionRead
	}
}

func newDelegatedSessionCredential() (raw, hash, fingerprint string, err error) {
	bytes := make([]byte, 32)
	if _, err = rand.Read(bytes); err != nil {
		return "", "", "", fmt.Errorf("generate delegated session credential: %w", err)
	}
	raw = delegatedSessionTokenPrefix + base64.RawURLEncoding.EncodeToString(bytes)
	digest := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(digest[:])
	fingerprint = "sha256:" + hash[:16]
	return raw, hash, fingerprint, nil
}

func nativeGrantRevision(principalID, repositoryID uint, permission RepoPermission) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("principal=%d\nrepository=%d\npermission=%s", principalID, repositoryID, permission.Effective().String())))
	return "grant-rev:" + hex.EncodeToString(digest[:])
}

func principalSessionSnapshotHash(resolved sessionauthority.Resolved, nativeGrantRevision string, membershipEpoch int64, grantedCapabilities []string, operationConstraints map[string]string) (string, error) {
	capabilities := append([]string(nil), grantedCapabilities...)
	sort.Strings(capabilities)
	snapshot := struct {
		ContractRevision       string            `json:"contract_revision"`
		IssuerInstanceID       string            `json:"issuer_instance_id"`
		TrustRevision          string            `json:"trust_revision"`
		BindingID              string            `json:"binding_id"`
		BindingRevision        string            `json:"binding_revision"`
		TeamIdentityID         string            `json:"team_identity_id,omitempty"`
		PolicyClass            string            `json:"policy_class,omitempty"`
		PolicyClassRevision    string            `json:"policy_class_revision,omitempty"`
		MembershipEpoch        int64             `json:"membership_epoch,omitempty"`
		PrincipalID            uint              `json:"principal_id"`
		Target                 string            `json:"target"`
		Service                string            `json:"service"`
		Repository             string            `json:"repository"`
		ResourcePolicyRevision string            `json:"resource_policy_revision"`
		Operation              string            `json:"operation"`
		GrantedCapabilities    []string          `json:"granted_capabilities"`
		OperationConstraints   map[string]string `json:"operation_constraints,omitempty"`
		NativeGrantRevision    string            `json:"native_grant_revision"`
	}{
		ContractRevision: sessionauthority.ContractRevision,
		IssuerInstanceID: resolved.Issuer.ID, TrustRevision: resolved.Issuer.TrustRevision,
		BindingID: resolved.Binding.ID, BindingRevision: resolved.Binding.BindingRevision,
		TeamIdentityID: resolved.TeamBinding.TeamIdentityID, PolicyClass: resolved.TeamBinding.PolicyClass,
		PolicyClassRevision: resolved.PolicyClass.PolicyRevision, MembershipEpoch: membershipEpoch, PrincipalID: resolved.PrincipalID,
		Target: resolved.Resource.Target, Service: resolved.Resource.Service, Repository: resolved.Resource.Repository,
		ResourcePolicyRevision: resolved.Resource.PolicyRevision, Operation: resolved.Operation.Name,
		GrantedCapabilities: capabilities, OperationConstraints: cloneStringMap(operationConstraints),
		NativeGrantRevision: nativeGrantRevision,
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func policyVersion(resolved sessionauthority.Resolved) string {
	if resolved.PolicyClass.PolicyRevision != "" {
		return resolved.PolicyClass.PolicyRevision
	}
	return resolved.Resource.PolicyRevision
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

// ResolveDelegatedSessionCredential authenticates one presented session token.
// Expired, revoked, malformed, stale-authority and unknown values share one
// fail-closed caller-facing error. The same fresh evaluator used by effects owns
// all current authority checks; last_used_at advances only after it succeeds.
func (s *Service) ResolveDelegatedSessionCredential(ctx context.Context, raw string) (db.User, db.DelegatedAgentSession, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, delegatedSessionTokenPrefix) {
		return db.User{}, db.DelegatedAgentSession{}, ErrDelegatedSessionCredentialInvalid
	}
	digest := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(digest[:])
	var identified db.DelegatedAgentSession
	if err := s.DBForCtx(ctx).Select("id", "principal_user_id", "repository_id", "operation_name").First(&identified, "credential_hash = ?", hash).Error; err != nil {
		return db.User{}, db.DelegatedAgentSession{}, ErrDelegatedSessionCredentialInvalid
	}
	var principal db.User
	if err := s.DBForCtx(ctx).First(&principal, "id = ?", identified.PrincipalUserID).Error; err != nil {
		return db.User{}, db.DelegatedAgentSession{}, ErrDelegatedSessionCredentialInvalid
	}
	authCtx := ContextWithDelegatedSession(ContextWithUser(ctx, principal), db.DelegatedAgentSession{ID: identified.ID})
	credentialOperation := strings.TrimSpace(identified.OperationName)
	if credentialOperation == "" {
		return db.User{}, db.DelegatedAgentSession{}, ErrDelegatedSessionCredentialInvalid
	}
	fresh, err := s.RevalidateDelegatedSession(authCtx, identified.RepositoryID, credentialOperation, "", nil)
	if err != nil {
		return db.User{}, db.DelegatedAgentSession{}, ErrDelegatedSessionCredentialInvalid
	}
	now := time.Now().UTC()
	used := s.DBForCtx(ctx).Model(&db.DelegatedAgentSession{}).
		Where("id = ? AND revoked_at IS NULL AND expires_at > ?", fresh.Session.ID, now).
		Update("last_used_at", now)
	if used.Error != nil || used.RowsAffected != 1 {
		return db.User{}, db.DelegatedAgentSession{}, ErrDelegatedSessionCredentialInvalid
	}
	fresh.Session.LastUsedAt = &now
	return fresh.Principal, fresh.Session, nil
}

func delegatedSessionReceipt(session db.DelegatedAgentSession, principal db.User, repository db.Repository) DelegatedSessionReceipt {
	session = redactDelegatedSessionOutput(session)
	principal.Login = safeDelegatedAuthorityText(principal.Login)
	repository.FullName = safeDelegatedAuthorityText(repository.FullName)
	workloadContext := workloadidentity.WorkloadContext{
		Schema: session.WorkloadContextSchema, IssuerInstanceID: session.IssuerInstanceID,
		Subject: session.IssuerSubject, CorrelationID: session.CorrelationID,
		WorkspaceID: session.IssuerWorkspaceID, AgentID: session.ExternalAgentID, SquadID: session.ExternalSquadID,
		IssueID: session.ExternalIssueID, IssueKey: session.ExternalIssueKey,
		TaskID: session.ExternalTaskID, RunID: session.ExternalRunID,
		TriggerID: session.ExternalTriggerID, RuntimeID: session.ExternalRuntimeID, Role: session.ExternalRole,
	}
	requestedScope := session.OperationName + ":" + repository.FullName
	return DelegatedSessionReceipt{
		ContractRevision: session.ContractRevision, ID: session.ID,
		Principal: DelegatedSessionPrincipal{ID: principal.ID, Login: principal.Login},
		BindingID: session.BindingID, TeamIdentityID: session.TeamIdentityID, TeamBindingRevision: session.TeamBindingRevision,
		PolicyClass: session.PolicyClass, MembershipEpoch: session.MembershipEpoch, CredentialMode: session.CredentialMode,
		Actor: DelegatedSessionActor{
			Provider: workloadidentity.DefaultIssuer, WorkspaceID: session.IssuerWorkspaceID, Workspace: session.IssuerWorkspace, AgentID: session.ExternalAgentID,
			AgentName: session.ExternalAgentName, TaskID: session.ExternalTaskID, RunID: session.ExternalRunID,
			IssueID: session.ExternalIssueID, IssueKey: session.ExternalIssueKey,
		},
		WorkloadContext: workloadContext, TraceQuality: session.TraceQuality,
		MissingFields: append([]string{}, session.MissingFields...),
		Target:        DelegatedSessionTarget{Instance: session.TargetInstance, Repository: repository.FullName},
		Resource:      DelegatedSessionResource{Service: session.ResourceService, Repository: repository.FullName},
		Operation:     DelegatedSessionOperation{Name: session.OperationName, Constraints: cloneStringMap(session.OperationConstraints)},
		EffectiveTTL:  session.ExpiresAt.Sub(session.CreatedAt).String(),
		AuthorizationBasis: DelegatedSessionAuthorizationBasis{
			TrustRevision: session.TrustRevision, NativeGrantRevision: session.NativeGrantRevision, ResourcePolicyRevision: session.ResourcePolicyRevision,
			PolicyClassRevision: session.PolicyVersion, RequestedOperationScope: requestedScope,
		},
		GrantedCapabilities: append([]string(nil), session.GrantedCapabilities...),
		PolicyVersion:       session.PolicyVersion, State: delegatedSessionState(session, time.Now().UTC()),
		CreatedAt: session.CreatedAt, ExpiresAt: session.ExpiresAt, LastUsedAt: session.LastUsedAt,
		RevokedAt: session.RevokedAt, RevokedByUserID: session.RevokedByUserID, RevocationReason: session.RevocationReason,
	}
}

func delegatedSessionState(session db.DelegatedAgentSession, now time.Time) string {
	if session.RevokedAt != nil {
		return "revoked"
	}
	if !session.ExpiresAt.After(now) {
		return "expired"
	}
	return "active"
}

type DelegatedSessionAuditEvent struct {
	Action    string
	Operation string
	Outcome   string
	Reason    string
	PRNumber  int
}

// LogCurrentDelegatedSessionAudit appends one redacted audit fact for the
// delegated session in ctx. It never serializes credentials, hashes, or token
// fingerprints.
func (s *Service) LogCurrentDelegatedSessionAudit(ctx context.Context, event DelegatedSessionAuditEvent) error {
	session, ok := DelegatedSessionFromContext(ctx)
	if !ok {
		return nil
	}
	session = redactDelegatedSessionOutput(session)
	principal := session.PrincipalUser
	if principal.ID == 0 {
		if actor, ok := UserFromContext(ctx); ok && actor.ID == session.PrincipalUserID {
			principal = actor
		} else if err := s.DBForCtx(ctx).First(&principal, session.PrincipalUserID).Error; err != nil {
			return err
		}
	}
	repository := session.Repository
	if repository.ID == 0 && session.RepositoryID != 0 {
		if err := s.DBForCtx(ctx).First(&repository, session.RepositoryID).Error; err != nil {
			return err
		}
	}
	principalLogin := strings.TrimSpace(session.PrincipalLogin)
	if principalLogin == "" {
		principalLogin = principal.Login
	}
	principalLogin = safeDelegatedAuthorityText(principalLogin)
	repository.FullName = safeDelegatedAuthorityText(repository.FullName)
	details, err := json.Marshal(struct {
		ContractRevision           string            `json:"contract_revision,omitempty"`
		SessionID                  string            `json:"session_id"`
		PrincipalUserID            uint              `json:"principal_user_id"`
		PrincipalLogin             string            `json:"principal_login"`
		BindingID                  string            `json:"binding_id,omitempty"`
		BindingRevision            string            `json:"binding_revision,omitempty"`
		TeamIdentityID             string            `json:"team_identity_id,omitempty"`
		TeamBindingRevision        string            `json:"team_binding_revision,omitempty"`
		PolicyClass                string            `json:"policy_class,omitempty"`
		MembershipEpoch            int64             `json:"membership_epoch,omitempty"`
		IssuerInstanceID           string            `json:"issuer_instance_id,omitempty"`
		TrustRevision              string            `json:"trust_revision,omitempty"`
		AssertionKeyID             string            `json:"assertion_key_id,omitempty"`
		IssuerSubject              string            `json:"issuer_subject,omitempty"`
		CorrelationID              string            `json:"correlation_id,omitempty"`
		DelegatedByUserID          *uint             `json:"delegated_by_user_id,omitempty"`
		DelegatedByLogin           string            `json:"delegated_by_login,omitempty"`
		DelegatedBySource          string            `json:"delegated_by_source"`
		WorkspaceID                string            `json:"workspace_id,omitempty"`
		Workspace                  string            `json:"workspace,omitempty"`
		AgentID                    string            `json:"agent_id,omitempty"`
		AgentName                  string            `json:"agent_name,omitempty"`
		SquadID                    string            `json:"squad_id,omitempty"`
		TaskID                     string            `json:"task_id,omitempty"`
		RunID                      string            `json:"run_id,omitempty"`
		IssueID                    string            `json:"issue_id,omitempty"`
		IssueKey                   string            `json:"issue_key,omitempty"`
		TriggerID                  string            `json:"trigger_id,omitempty"`
		RuntimeID                  string            `json:"runtime_id,omitempty"`
		Role                       string            `json:"role,omitempty"`
		TraceQuality               string            `json:"trace_quality,omitempty"`
		MissingFields              []string          `json:"missing_fields,omitempty"`
		TargetInstance             string            `json:"target_instance,omitempty"`
		ResourceService            string            `json:"resource_service,omitempty"`
		RepositoryID               uint              `json:"repository_id"`
		SessionOperation           string            `json:"session_operation,omitempty"`
		OperationConstraints       map[string]string `json:"operation_constraints,omitempty"`
		MergeDelegationID          string            `json:"merge_delegation_id,omitempty"`
		MergeDelegationRevision    string            `json:"merge_delegation_revision,omitempty"`
		MergeDelegationFactsDigest string            `json:"merge_delegation_facts_digest,omitempty"`
		GrantedCapabilities        []string          `json:"granted_capabilities,omitempty"`
		NativeGrantRevision        string            `json:"native_grant_revision,omitempty"`
		ResourcePolicyRevision     string            `json:"resource_policy_revision,omitempty"`
		PolicyVersion              string            `json:"policy_version"`
		ExpiresAt                  time.Time         `json:"expires_at"`
		RevokedAt                  *time.Time        `json:"revoked_at,omitempty"`
		Operation                  string            `json:"operation"`
		Outcome                    string            `json:"outcome"`
		Reason                     string            `json:"reason,omitempty"`
		PullRequestNumber          int               `json:"pull_request_number,omitempty"`
	}{
		ContractRevision: session.ContractRevision, SessionID: session.ID,
		PrincipalUserID: session.PrincipalUserID, PrincipalLogin: principalLogin,
		BindingID: session.BindingID, BindingRevision: session.BindingRevision,
		TeamIdentityID: session.TeamIdentityID, TeamBindingRevision: session.TeamBindingRevision,
		PolicyClass: session.PolicyClass, MembershipEpoch: session.MembershipEpoch,
		IssuerInstanceID: session.IssuerInstanceID, TrustRevision: session.TrustRevision,
		AssertionKeyID: session.AssertionKeyID, IssuerSubject: session.IssuerSubject, CorrelationID: session.CorrelationID,
		DelegatedByUserID: session.DelegatedByUserID, DelegatedByLogin: session.DelegatedByLogin, DelegatedBySource: session.DelegatedBySource,
		WorkspaceID: session.IssuerWorkspaceID, Workspace: session.IssuerWorkspace,
		AgentID: session.ExternalAgentID, AgentName: session.ExternalAgentName, SquadID: session.ExternalSquadID, TaskID: session.ExternalTaskID,
		RunID: session.ExternalRunID, IssueID: session.ExternalIssueID, IssueKey: session.ExternalIssueKey,
		TriggerID: session.ExternalTriggerID, RuntimeID: session.ExternalRuntimeID, Role: session.ExternalRole,
		TraceQuality: session.TraceQuality, MissingFields: append([]string(nil), session.MissingFields...),
		TargetInstance: session.TargetInstance, ResourceService: session.ResourceService,
		RepositoryID: session.RepositoryID, SessionOperation: session.OperationName,
		OperationConstraints: cloneStringMap(session.OperationConstraints), MergeDelegationID: session.MergeDelegationID,
		MergeDelegationRevision: session.MergeDelegationRevision, MergeDelegationFactsDigest: session.MergeDelegationFactsDigest,
		GrantedCapabilities: append([]string(nil), session.GrantedCapabilities...),
		NativeGrantRevision: session.NativeGrantRevision, ResourcePolicyRevision: session.ResourcePolicyRevision,
		PolicyVersion: session.PolicyVersion, ExpiresAt: session.ExpiresAt, RevokedAt: session.RevokedAt,
		Operation: strings.TrimSpace(event.Operation), Outcome: strings.TrimSpace(event.Outcome),
		Reason: safeDelegatedAuditText(event.Reason), PullRequestNumber: event.PRNumber,
	})
	if err != nil {
		return err
	}
	userID := session.PrincipalUserID
	return s.LogAudit(ctx, AuditEvent{
		UserID: &userID, Action: event.Action, RepositoryFullName: repository.FullName,
		TargetLogin: session.ExternalAgentName, Details: string(details),
	})
}

func safeDelegatedAuthorityText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "ags_sess_") || strings.Contains(lower, "mat_") || strings.Contains(lower, "-----begin") || strings.Contains(lower, "private key") || (strings.HasPrefix(value, "eyJ") && strings.Count(value, ".") == 2) {
		return "redacted"
	}
	return value
}

func safeDelegatedAuditText(value string) string {
	value = safeDelegatedAuthorityText(value)
	if len(value) > 160 {
		return value[:160]
	}
	return value
}

func redactDelegatedStringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = safeDelegatedAuthorityText(value)
	}
	return out
}

func redactDelegatedStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[safeDelegatedAuthorityText(key)] = safeDelegatedAuthorityText(value)
	}
	return out
}

// redactDelegatedSessionOutput makes receipt and audit serialization safe even
// if a malformed historical row predates current configuration validation.
func redactDelegatedSessionOutput(session db.DelegatedAgentSession) db.DelegatedAgentSession {
	session.PrincipalLogin = safeDelegatedAuthorityText(session.PrincipalLogin)
	session.DelegatedByLogin = safeDelegatedAuthorityText(session.DelegatedByLogin)
	session.Issuer = safeDelegatedAuthorityText(session.Issuer)
	session.AssertionPurpose = safeDelegatedAuthorityText(session.AssertionPurpose)
	session.AssertionJTI = safeDelegatedAuthorityText(session.AssertionJTI)
	session.AssertionAudience = safeDelegatedAuthorityText(session.AssertionAudience)
	session.AssertionKeyID = safeDelegatedAuthorityText(session.AssertionKeyID)
	session.ContractRevision = safeDelegatedAuthorityText(session.ContractRevision)
	session.CredentialMode = safeDelegatedAuthorityText(session.CredentialMode)
	session.BindingID = safeDelegatedAuthorityText(session.BindingID)
	session.BindingRevision = safeDelegatedAuthorityText(session.BindingRevision)
	session.TeamIdentityID = safeDelegatedAuthorityText(session.TeamIdentityID)
	session.TeamBindingRevision = safeDelegatedAuthorityText(session.TeamBindingRevision)
	session.PolicyClass = safeDelegatedAuthorityText(session.PolicyClass)
	session.IssuerInstanceID = safeDelegatedAuthorityText(session.IssuerInstanceID)
	session.TrustRevision = safeDelegatedAuthorityText(session.TrustRevision)
	session.IssuerSubject = safeDelegatedAuthorityText(session.IssuerSubject)
	session.CorrelationID = safeDelegatedAuthorityText(session.CorrelationID)
	session.IssuerWorkspaceID = safeDelegatedAuthorityText(session.IssuerWorkspaceID)
	session.IssuerWorkspace = safeDelegatedAuthorityText(session.IssuerWorkspace)
	session.ExternalAgentID = safeDelegatedAuthorityText(session.ExternalAgentID)
	session.ExternalAgentName = safeDelegatedAuthorityText(session.ExternalAgentName)
	session.ExternalSquadID = safeDelegatedAuthorityText(session.ExternalSquadID)
	session.ExternalTaskID = safeDelegatedAuthorityText(session.ExternalTaskID)
	session.ExternalRunID = safeDelegatedAuthorityText(session.ExternalRunID)
	session.ExternalIssueID = safeDelegatedAuthorityText(session.ExternalIssueID)
	session.ExternalIssueKey = safeDelegatedAuthorityText(session.ExternalIssueKey)
	session.ExternalTriggerID = safeDelegatedAuthorityText(session.ExternalTriggerID)
	session.ExternalRuntimeID = safeDelegatedAuthorityText(session.ExternalRuntimeID)
	session.ExternalRole = safeDelegatedAuthorityText(session.ExternalRole)
	session.WorkloadContextSchema = safeDelegatedAuthorityText(session.WorkloadContextSchema)
	session.TraceQuality = safeDelegatedAuthorityText(session.TraceQuality)
	session.MissingFields = redactDelegatedStringSlice(session.MissingFields)
	session.TargetInstance = safeDelegatedAuthorityText(session.TargetInstance)
	session.ResourceService = safeDelegatedAuthorityText(session.ResourceService)
	session.OperationName = safeDelegatedAuthorityText(session.OperationName)
	session.OperationConstraints = redactDelegatedStringMap(session.OperationConstraints)
	session.MergeDelegationID = safeDelegatedAuthorityText(session.MergeDelegationID)
	session.MergeDelegationRevision = safeDelegatedAuthorityText(session.MergeDelegationRevision)
	session.MergeDelegationFactsDigest = safeDelegatedAuthorityText(session.MergeDelegationFactsDigest)
	session.MergeDelegationTargetInstance = safeDelegatedAuthorityText(session.MergeDelegationTargetInstance)
	session.MergeDelegationCanonicalRepositoryID = safeDelegatedAuthorityText(session.MergeDelegationCanonicalRepositoryID)
	session.MergeDelegationProvider = safeDelegatedAuthorityText(session.MergeDelegationProvider)
	session.MergeDelegationProviderBindingID = safeDelegatedAuthorityText(session.MergeDelegationProviderBindingID)
	session.MergeDelegationProviderBindingRevision = safeDelegatedAuthorityText(session.MergeDelegationProviderBindingRevision)
	session.MergeDelegationProviderRepository = safeDelegatedAuthorityText(session.MergeDelegationProviderRepository)
	session.MergeDelegationExpectedHeadSHA = safeDelegatedAuthorityText(session.MergeDelegationExpectedHeadSHA)
	session.MergeDelegationExpectedBaseSHA = safeDelegatedAuthorityText(session.MergeDelegationExpectedBaseSHA)
	session.MergeDelegationBaseRef = safeDelegatedAuthorityText(session.MergeDelegationBaseRef)
	session.MergeDelegationMethod = safeDelegatedAuthorityText(session.MergeDelegationMethod)
	session.MergeDelegationProjectionRevision = safeDelegatedAuthorityText(session.MergeDelegationProjectionRevision)
	session.GrantedCapabilities = redactDelegatedStringSlice(session.GrantedCapabilities)
	session.PolicyVersion = safeDelegatedAuthorityText(session.PolicyVersion)
	session.NativeGrantRevision = safeDelegatedAuthorityText(session.NativeGrantRevision)
	session.ResourcePolicyRevision = safeDelegatedAuthorityText(session.ResourcePolicyRevision)
	session.RevocationReason = safeDelegatedAuthorityText(session.RevocationReason)
	return session
}

// DelegatedSessionReceiptForTest wraps delegatedSessionReceipt for use by
// external test packages. Production transport issuance calls the private
// helper directly; public dynamic Session lifecycle surfaces are retired.
func DelegatedSessionReceiptForTest(session db.DelegatedAgentSession, principal db.User, repository db.Repository) DelegatedSessionReceipt {
	return delegatedSessionReceipt(session, principal, repository)
}
