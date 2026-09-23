package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	AuthorityBoundaryReceiptLegacyCaptureV1Kind = "legacy_authority_capture.v1"
	AuthorityBoundaryReceiptLegacyCaptureKind   = "legacy_authority_capture.v2"
	AuthorityBoundaryReceiptDelegatedEffectKind = "delegated_effect.v1"

	legacyAuthorityCaptureClaimLimit = "exact loaded secret-safe authority projection and AGS readback captured; Session issuance, provider state, and operation success are not claimed"
	delegatedEffectClaimLimit        = "exact delegated authority admission and AGS readback captured; provider state and operation success are not claimed"
	legacyAuthorityCaptureV1Schema   = "ags.authority-boundary.legacy-capture.v1"
	legacyAuthorityCaptureSchema     = "ags.authority-boundary.legacy-capture.v2"
	delegatedEffectSchema            = "ags.authority-boundary.delegated-effect.v1"

	// The complete transaction, including ambiguous commit results, is bounded.
	// This is a source contract; live TiDB lock timing remains separately proven.
	authorityBoundaryTransactionAttempts = 5
	authorityBoundaryTransactionBudget   = 2 * time.Second
)

var (
	ErrAuthorityBoundaryReceiptDenied  = errors.New("authority boundary receipt denied")
	ErrAuthorityBoundaryReceiptCorrupt = errors.New("authority boundary receipt digest mismatch")
	ErrAuthorityBoundaryReceiptSource  = errors.New("authority boundary receipt source revision unavailable")
	ErrAuthorityBoundaryReceiptEpoch   = errors.New("authority boundary receipt authority epoch mismatch")
	ErrAuthorityBoundaryReceiptSecret  = errors.New("authority boundary receipt payload is not secret-safe")
)

type AuthorityBoundaryReceipt struct {
	ReceiptID       string          `json:"receipt_id"`
	Kind            string          `json:"kind"`
	ParentDigest    string          `json:"parent_digest,omitempty"`
	AuthorityEpoch  string          `json:"authority_epoch"`
	SnapshotDigest  string          `json:"snapshot_digest"`
	SourceRevision  string          `json:"source_revision"`
	ClaimLimit      string          `json:"claim_limit"`
	Payload         json.RawMessage `json:"payload"`
	CreatedByUserID uint            `json:"created_by_user_id"`
	CreatedAt       time.Time       `json:"created_at"`
}

type authorityIssuerProjection struct {
	ID            string `json:"id"`
	Issuer        string `json:"issuer"`
	Status        string `json:"status"`
	TrustRevision string `json:"trust_revision"`
}

type authorityPrincipalReadback struct {
	ID        uint   `json:"id"`
	Login     string `json:"login"`
	Status    string `json:"status"`
	SiteAdmin bool   `json:"site_admin"`
}

type legacyAuthorityCapturePayloadV1 struct {
	Schema                  string                              `json:"schema"`
	Version                 int                                 `json:"version"`
	ContractRevision        string                              `json:"contract_revision"`
	LegacyCompatibilityMode string                              `json:"legacy_compatibility_mode,omitempty"`
	TrustedIssuers          []authorityIssuerProjection         `json:"trusted_issuers"`
	Bindings                []sessionauthority.PrincipalBinding `json:"bindings"`
	TeamBindings            []sessionauthority.TeamBinding      `json:"team_bindings"`
	PolicyClasses           []sessionauthority.PolicyClass      `json:"policy_classes"`
	Resources               []sessionauthority.ResourcePolicy   `json:"resources"`
	Principals              []authorityPrincipalReadback        `json:"principals"`
}

type legacyAuthorityCapturePayload struct {
	Schema                  string                              `json:"schema"`
	Version                 int                                 `json:"version"`
	ContractRevision        string                              `json:"contract_revision"`
	LegacyCompatibilityMode string                              `json:"legacy_compatibility_mode,omitempty"`
	TrustedIssuers          []authorityIssuerProjection         `json:"trusted_issuers"`
	Bindings                []sessionauthority.PrincipalBinding `json:"bindings"`
	TeamBindings            []sessionauthority.TeamBinding      `json:"team_bindings"`
	PolicyClasses           []sessionauthority.PolicyClass      `json:"policy_classes"`
	ResourceDefaults        []sessionauthority.ResourceDefault  `json:"resource_defaults"`
	Resources               []sessionauthority.ResourcePolicy   `json:"resources"`
	Principals              []authorityPrincipalReadback        `json:"principals"`
}

type delegatedEffectPayload struct {
	Schema              string            `json:"schema"`
	SessionID           string            `json:"session_id"`
	PrincipalID         uint              `json:"principal_id"`
	RepositoryID        uint              `json:"repository_id"`
	Repository          string            `json:"repository"`
	Operation           string            `json:"operation"`
	Capabilities        []string          `json:"capabilities"`
	Constraints         map[string]string `json:"constraints,omitempty"`
	PolicySnapshotHash  string            `json:"policy_snapshot_hash"`
	NativeGrantRevision string            `json:"native_grant_revision"`
	MembershipEpoch     int64             `json:"membership_epoch,omitempty"`
}

// CurrentAuthorityEpoch hashes the complete normalized authority config. The
// digest covers key identifiers without exposing them in receipt payloads.
func (s *Service) CurrentAuthorityEpoch() (string, error) {
	if s == nil || s.PrincipalSessions == nil {
		return "", ErrAuthorityBoundaryReceiptEpoch
	}
	return authorityEpochForConfig(s.PrincipalSessions.Config())
}

func authorityEpochForConfig(config sessionauthority.Config) (string, error) {
	config = canonicalAuthorityConfig(config)
	if config.Version == 0 || strings.TrimSpace(config.ContractRevision) == "" {
		return "", ErrAuthorityBoundaryReceiptEpoch
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return sha256Hex(encoded), nil
}

func canonicalAuthorityConfig(config sessionauthority.Config) sessionauthority.Config {
	for index := range config.TrustedIssuers {
		sort.Strings(config.TrustedIssuers[index].KeyIDs)
	}
	for index := range config.PolicyClasses {
		sort.Strings(config.PolicyClasses[index].Operations)
	}
	sort.Slice(config.TrustedIssuers, func(i, j int) bool { return config.TrustedIssuers[i].ID < config.TrustedIssuers[j].ID })
	sort.Slice(config.Bindings, func(i, j int) bool { return config.Bindings[i].ID < config.Bindings[j].ID })
	sort.Slice(config.TeamBindings, func(i, j int) bool { return config.TeamBindings[i].ID < config.TeamBindings[j].ID })
	sort.Slice(config.PolicyClasses, func(i, j int) bool { return config.PolicyClasses[i].ID < config.PolicyClasses[j].ID })
	sort.Slice(config.ResourceDefaults, func(i, j int) bool { return config.ResourceDefaults[i].ID < config.ResourceDefaults[j].ID })
	sort.Slice(config.Resources, func(i, j int) bool { return config.Resources[i].ID < config.Resources[j].ID })
	return config
}

// CaptureLegacyAuthorityBoundary creates or returns the immutable capture for
// the exact current config/DB/source. Callers submit only the expected epoch.
func (s *Service) CaptureLegacyAuthorityBoundary(ctx context.Context, expectedEpoch string) (AuthorityBoundaryReceipt, error) {
	return s.captureAuthorityBoundaryWithRetry(ctx, func(txCtx context.Context) (AuthorityBoundaryReceipt, error) {
		return s.captureLegacyAuthorityBoundary(txCtx, expectedEpoch)
	})
}

func (s *Service) captureLegacyAuthorityBoundary(ctx context.Context, expectedEpoch string) (AuthorityBoundaryReceipt, error) {
	contextViewer, ok := UserFromContext(ctx)
	if !ok || contextViewer.ID == 0 {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	var viewer db.User
	if err := s.DBForCtx(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&viewer, "id = ?", contextViewer.ID).Error; err != nil || !viewer.SiteAdmin || !isUserStatusActive(viewer.Status) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	config := canonicalAuthorityConfig(s.PrincipalSessions.Config())
	authorityEpoch, err := authorityEpochForConfig(config)
	if err != nil {
		return AuthorityBoundaryReceipt{}, err
	}
	principalIDs := make(map[uint]struct{})
	for _, binding := range config.Bindings {
		principalIDs[binding.PrincipalID] = struct{}{}
	}
	for _, binding := range config.TeamBindings {
		principalIDs[binding.PrincipalID] = struct{}{}
	}
	ids := make([]uint, 0, len(principalIDs))
	for id := range principalIDs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var users []db.User
	if len(ids) != 0 {
		if err := s.DBForCtx(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", ids).Find(&users).Error; err != nil {
			return AuthorityBoundaryReceipt{}, err
		}
	}
	byID := make(map[uint]db.User, len(users))
	for _, user := range users {
		byID[user.ID] = user
	}
	principals := make([]authorityPrincipalReadback, 0, len(ids))
	for _, id := range ids {
		user, found := byID[id]
		if !found || user.ID == 0 || !isUserStatusActive(user.Status) {
			return AuthorityBoundaryReceipt{}, fmt.Errorf("%w: principal %d is unavailable", ErrAuthorityBoundaryReceiptDenied, id)
		}
		principals = append(principals, authorityPrincipalReadback{ID: user.ID, Login: user.Login, Status: user.Status, SiteAdmin: user.SiteAdmin})
	}
	payload := legacyAuthorityCapturePayloadForConfig(config, principals)
	return s.createAuthorityBoundaryReceipt(ctx, viewer.ID, AuthorityBoundaryReceiptLegacyCaptureKind, "", authorityEpoch, expectedEpoch, legacyAuthorityCaptureClaimLimit, payload)
}

func legacyAuthorityCapturePayloadForConfig(config sessionauthority.Config, principals []authorityPrincipalReadback) legacyAuthorityCapturePayload {
	issuers := make([]authorityIssuerProjection, 0, len(config.TrustedIssuers))
	for _, issuer := range config.TrustedIssuers {
		issuers = append(issuers, authorityIssuerProjection{ID: issuer.ID, Issuer: issuer.Issuer, Status: issuer.Status, TrustRevision: issuer.TrustRevision})
	}
	resourceDefaults := append([]sessionauthority.ResourceDefault{}, config.ResourceDefaults...)
	resources := append([]sessionauthority.ResourcePolicy{}, config.Resources...)
	return legacyAuthorityCapturePayload{
		Schema: legacyAuthorityCaptureSchema, Version: config.Version, ContractRevision: config.ContractRevision,
		LegacyCompatibilityMode: config.LegacyCompatibilityMode, TrustedIssuers: issuers,
		Bindings: config.Bindings, TeamBindings: config.TeamBindings, PolicyClasses: config.PolicyClasses,
		ResourceDefaults: resourceDefaults, Resources: resources, Principals: principals,
	}
}

// CaptureDelegatedEffectBoundary captures fresh delegated authority at an
// effect boundary. It deliberately does not claim provider state or success.
func (s *Service) CaptureDelegatedEffectBoundary(ctx context.Context, expectedEpoch string) (AuthorityBoundaryReceipt, error) {
	return s.captureAuthorityBoundaryWithRetry(ctx, func(txCtx context.Context) (AuthorityBoundaryReceipt, error) {
		return s.captureDelegatedEffectBoundary(txCtx, expectedEpoch)
	})
}

func (s *Service) captureAuthorityBoundaryWithRetry(ctx context.Context, capture func(context.Context) (AuthorityBoundaryReceipt, error)) (AuthorityBoundaryReceipt, error) {
	budgetCtx, cancel := context.WithTimeout(ctx, authorityBoundaryTransactionBudget)
	defer cancel()
	// Freeze the tenant-aware database for the complete retry/readback budget.
	// An ambiguous commit must never fall back to the service's default DB.
	database := s.DBForCtx(budgetCtx).WithContext(budgetCtx)
	var lastErr error
	for attempt := 0; attempt < authorityBoundaryTransactionAttempts; attempt++ {
		var receipt AuthorityBoundaryReceipt
		run := func(tx *gorm.DB) error {
			var err error
			receipt, err = capture(ContextWithDB(budgetCtx, tx.WithContext(budgetCtx)))
			return err
		}
		if s.testAuthorityBoundaryTransaction != nil {
			lastErr = s.testAuthorityBoundaryTransaction(attempt, run)
		} else {
			lastErr = database.Transaction(run)
		}
		if lastErr == nil {
			return receipt, nil
		}
		if authorityBoundaryReceiptRetryable(lastErr) && strings.TrimSpace(receipt.ReceiptID) != "" {
			// A retryable commit result may be ambiguous: the row can already be
			// durable even though the driver returned an error. Read through the
			// base handle (never the finished transaction) and accept only the
			// complete digest/schema/source integrity contract.
			baseCtx := ContextWithDB(budgetCtx, database)
			committed, readErr := s.GetAuthorityBoundaryReceipt(baseCtx, receipt.ReceiptID)
			if readErr == nil {
				return committed, nil
			}
			if !errors.Is(readErr, ErrNotFound) {
				return AuthorityBoundaryReceipt{}, readErr
			}
		}
		if !authorityBoundaryReceiptRetryable(lastErr) || attempt+1 == authorityBoundaryTransactionAttempts {
			return AuthorityBoundaryReceipt{}, lastErr
		}
		delay := retryDelay(attempt)
		select {
		case <-budgetCtx.Done():
			return AuthorityBoundaryReceipt{}, budgetCtx.Err()
		case <-time.After(delay):
		}
	}
	return AuthorityBoundaryReceipt{}, lastErr
}

func (s *Service) captureDelegatedEffectBoundary(ctx context.Context, expectedEpoch string) (AuthorityBoundaryReceipt, error) {
	sessionID, ok := DelegatedSessionIDFromContext(ctx)
	if !ok {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	var coordinate db.DelegatedAgentSession
	if err := s.DBForCtx(ctx).Select("id", "repository_id", "operation_name").First(&coordinate, "id = ?", sessionID).Error; err != nil {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	fresh, err := s.RevalidateDelegatedSession(ctx, coordinate.RepositoryID, coordinate.OperationName, "", nil)
	if err != nil {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	return s.createDelegatedEffectBoundaryReceipt(ctx, fresh, expectedEpoch)
}

// captureDelegatedEffectBoundaryForActionTx is the only exact pr.rebase
// Boundary capture kernel. The locked intent supplies every coordinate; no
// client epoch or payload is accepted. The caller must run this after locking
// PullRequest -> PullRequestProjection -> PullRequestActionIntent and before the
// intent becomes eligible for provider dispatch.
func (s *Service) captureDelegatedEffectBoundaryForActionTx(ctx context.Context, intent db.PullRequestActionIntent) (AuthorityBoundaryReceipt, error) {
	if intent.AgentSessionID == nil || strings.TrimSpace(*intent.AgentSessionID) == "" || intent.Action != "pr.rebase" {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	sessionID, ok := DelegatedSessionIDFromContext(ctx)
	if !ok || sessionID != strings.TrimSpace(*intent.AgentSessionID) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	constraints := forgejoRebaseSessionConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA)
	fresh, err := s.RevalidateDelegatedSession(ctx, intent.RepositoryID, "pr.rebase", "repo:write", constraints)
	if err != nil || fresh.Session.ID != sessionID || fresh.Principal.ID != intent.PrincipalID ||
		fresh.Repository.ID != intent.RepositoryID || fresh.Repository.FullName != intent.Repository ||
		fresh.Session.OperationName != "pr.rebase" || !sameStringMap(fresh.Session.OperationConstraints, constraints) ||
		fresh.Session.PolicySnapshotHash != intent.AuthorityRev || fresh.Session.TeamIdentityID != intent.TeamIdentityID ||
		fresh.Session.PolicyClass != intent.PolicyClass || fresh.Session.MembershipEpoch != intent.MembershipEpoch {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	return s.createDelegatedEffectBoundaryReceipt(ctx, fresh, fresh.AuthorityEpoch)
}

func (s *Service) createDelegatedEffectBoundaryReceipt(ctx context.Context, fresh FreshDelegatedAuthority, expectedEpoch string) (AuthorityBoundaryReceipt, error) {
	capabilities := append([]string(nil), fresh.Session.GrantedCapabilities...)
	sort.Strings(capabilities)
	payload := delegatedEffectPayload{
		Schema: delegatedEffectSchema, SessionID: fresh.Session.ID, PrincipalID: fresh.Principal.ID,
		RepositoryID: fresh.Repository.ID, Repository: fresh.Repository.FullName, Operation: fresh.Session.OperationName,
		Capabilities: capabilities, Constraints: cloneStringMap(fresh.Session.OperationConstraints), PolicySnapshotHash: fresh.Session.PolicySnapshotHash,
		NativeGrantRevision: fresh.Session.NativeGrantRevision, MembershipEpoch: fresh.Session.MembershipEpoch,
	}
	return s.createAuthorityBoundaryReceipt(ctx, fresh.Principal.ID, AuthorityBoundaryReceiptDelegatedEffectKind, fresh.Session.PolicySnapshotHash, fresh.AuthorityEpoch, expectedEpoch, delegatedEffectClaimLimit, payload)
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range right {
		if left[key] != value {
			return false
		}
	}
	return true
}

func (s *Service) createAuthorityBoundaryReceipt(ctx context.Context, creatorID uint, kind, parentDigest, authorityEpoch, expectedEpoch, claimLimit string, payload any) (AuthorityBoundaryReceipt, error) {
	expectedClaimLimit := expectedAuthorityBoundaryClaimLimit(kind)
	if expectedClaimLimit == "" || expectedClaimLimit != claimLimit || (isLegacyAuthorityCaptureKind(kind) && parentDigest != "") || (kind == AuthorityBoundaryReceiptDelegatedEffectKind && !isFullSHA256Digest(parentDigest)) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	source := strings.ToLower(strings.TrimSpace(s.SourceRevision))
	if !isFullHexRevision(source) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptSource
	}
	epoch := strings.ToLower(strings.TrimSpace(authorityEpoch))
	if !isFullSHA256Digest(epoch) || strings.TrimSpace(expectedEpoch) == "" || !strings.EqualFold(strings.TrimSpace(expectedEpoch), epoch) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptEpoch
	}
	canonicalPayload, err := json.Marshal(payload)
	if err != nil {
		return AuthorityBoundaryReceipt{}, err
	}
	if err := validateSecretSafeReceiptJSON(canonicalPayload); err != nil {
		return AuthorityBoundaryReceipt{}, err
	}
	snapshotDigest := sha256Hex(canonicalPayload)
	identity := struct {
		Kind           string `json:"kind"`
		ParentDigest   string `json:"parent_digest,omitempty"`
		AuthorityEpoch string `json:"authority_epoch"`
		SnapshotDigest string `json:"snapshot_digest"`
		SourceRevision string `json:"source_revision"`
		ClaimLimit     string `json:"claim_limit"`
	}{kind, parentDigest, epoch, snapshotDigest, source, claimLimit}
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return AuthorityBoundaryReceipt{}, err
	}
	row := db.AuthorityBoundaryReceipt{
		ReceiptID: "abr_" + sha256Hex(identityJSON), Kind: kind, ParentDigest: parentDigest, AuthorityEpoch: epoch,
		SnapshotDigest: snapshotDigest, SourceRevision: source, ClaimLimit: claimLimit, Payload: string(canonicalPayload),
		CreatedByUserID: creatorID, CreatedAt: time.Now().UTC(),
	}
	if err := s.DBForCtx(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "receipt_id"}}, DoNothing: true}).Create(&row).Error; err != nil {
		return AuthorityBoundaryReceipt{}, err
	}
	// A concurrent idempotent insert may wait for another transaction. Under
	// repeatable read its committed row is outside our original snapshot, so
	// read the acknowledged row with a current read before returning it.
	return s.getAuthorityBoundaryReceipt(ctx, row.ReceiptID, false, true)
}

func (s *Service) GetAuthorityBoundaryReceipt(ctx context.Context, receiptID string) (AuthorityBoundaryReceipt, error) {
	return s.getAuthorityBoundaryReceipt(ctx, receiptID, false, false)
}

// GetCurrentDelegatedEffectBoundaryReceipt is the Session-scoped readback for
// delegated_effect.v1. A durable credential for the same principal is not an
// originating Session and cannot use this method.
func (s *Service) GetCurrentDelegatedEffectBoundaryReceipt(ctx context.Context, receiptID string) (AuthorityBoundaryReceipt, error) {
	if sessionID, ok := DelegatedSessionIDFromContext(ctx); !ok || strings.TrimSpace(sessionID) == "" {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	receipt, err := s.getAuthorityBoundaryReceipt(ctx, receiptID, true, false)
	if err != nil {
		return AuthorityBoundaryReceipt{}, err
	}
	if receipt.Kind != AuthorityBoundaryReceiptDelegatedEffectKind {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	return receipt, nil
}

func (s *Service) getAuthorityBoundaryReceipt(ctx context.Context, receiptID string, requireCurrentSession, currentRead bool) (AuthorityBoundaryReceipt, error) {
	contextViewer, ok := UserFromContext(ctx)
	if !ok || contextViewer.ID == 0 {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	var viewer db.User
	if err := s.DBForCtx(ctx).First(&viewer, "id = ?", contextViewer.ID).Error; err != nil || !isUserStatusActive(viewer.Status) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
	}
	ref := strings.TrimSpace(receiptID)
	if !isCanonicalReceiptID(ref) {
		return AuthorityBoundaryReceipt{}, ErrNotFound
	}
	var row db.AuthorityBoundaryReceipt
	query := s.DBForCtx(ctx)
	if currentRead {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := query.First(&row, "receipt_id = ?", ref).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AuthorityBoundaryReceipt{}, ErrNotFound
		}
		return AuthorityBoundaryReceipt{}, err
	}
	expectedClaimLimit := expectedAuthorityBoundaryClaimLimit(row.Kind)
	if expectedClaimLimit == "" || expectedClaimLimit != row.ClaimLimit || (isLegacyAuthorityCaptureKind(row.Kind) && row.ParentDigest != "") || (row.Kind == AuthorityBoundaryReceiptDelegatedEffectKind && !isFullSHA256Digest(row.ParentDigest)) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptCorrupt
	}
	payload := []byte(row.Payload)
	canonicalPayload, err := canonicalReceiptJSON(row.Kind, payload)
	if err != nil || string(canonicalPayload) != row.Payload || !isCanonicalSHA256Digest(row.AuthorityEpoch) ||
		!isCanonicalSHA256Digest(row.SnapshotDigest) || !isCanonicalSourceRevision(row.SourceRevision) ||
		(row.ParentDigest != "" && !isCanonicalSHA256Digest(row.ParentDigest)) || sha256Hex(payload) != row.SnapshotDigest {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptCorrupt
	}
	identity := struct {
		Kind           string `json:"kind"`
		ParentDigest   string `json:"parent_digest,omitempty"`
		AuthorityEpoch string `json:"authority_epoch"`
		SnapshotDigest string `json:"snapshot_digest"`
		SourceRevision string `json:"source_revision"`
		ClaimLimit     string `json:"claim_limit"`
	}{row.Kind, row.ParentDigest, row.AuthorityEpoch, row.SnapshotDigest, row.SourceRevision, row.ClaimLimit}
	identityJSON, err := json.Marshal(identity)
	if err != nil || row.ReceiptID != "abr_"+sha256Hex(identityJSON) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptCorrupt
	}
	if err := validateSecretSafeReceiptJSON(payload); err != nil {
		return AuthorityBoundaryReceipt{}, err
	}

	switch row.Kind {
	case AuthorityBoundaryReceiptDelegatedEffectKind:
		var delegated delegatedEffectPayload
		if err := json.Unmarshal(payload, &delegated); err != nil {
			return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptCorrupt
		}
		sessionID, hasSession := DelegatedSessionIDFromContext(ctx)
		originatingSession := hasSession && strings.TrimSpace(sessionID) == delegated.SessionID && viewer.ID == delegated.PrincipalID
		if requireCurrentSession && !originatingSession {
			return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
		}
		if !viewer.SiteAdmin && !originatingSession {
			return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
		}
	case AuthorityBoundaryReceiptLegacyCaptureV1Kind, AuthorityBoundaryReceiptLegacyCaptureKind:
		if requireCurrentSession || (!viewer.SiteAdmin && viewer.ID != row.CreatedByUserID) {
			return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptDenied
		}
	default:
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptCorrupt
	}
	return AuthorityBoundaryReceipt{
		ReceiptID: row.ReceiptID, Kind: row.Kind, ParentDigest: row.ParentDigest, AuthorityEpoch: row.AuthorityEpoch,
		SnapshotDigest: row.SnapshotDigest, SourceRevision: row.SourceRevision, ClaimLimit: row.ClaimLimit,
		Payload: append(json.RawMessage(nil), payload...), CreatedByUserID: row.CreatedByUserID, CreatedAt: row.CreatedAt,
	}, nil
}

func (s *Service) validateDelegatedEffectBoundaryForAction(ctx context.Context, intent db.PullRequestActionIntent) (AuthorityBoundaryReceipt, error) {
	if intent.AgentSessionID == nil || intent.BoundaryProtocol != AuthorityBoundaryReceiptDelegatedEffectKind ||
		intent.BoundaryReceiptID == nil || !isCanonicalReceiptID(strings.TrimSpace(*intent.BoundaryReceiptID)) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptCorrupt
	}
	receipt, err := s.GetCurrentDelegatedEffectBoundaryReceipt(ctx, strings.TrimSpace(*intent.BoundaryReceiptID))
	if err != nil {
		return AuthorityBoundaryReceipt{}, err
	}
	var payload delegatedEffectPayload
	if err := json.Unmarshal(receipt.Payload, &payload); err != nil {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptCorrupt
	}
	expectedConstraints := forgejoRebaseSessionConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA)
	if receipt.ParentDigest != intent.AuthorityRev || payload.Schema != delegatedEffectSchema ||
		payload.SessionID != strings.TrimSpace(*intent.AgentSessionID) || payload.PrincipalID != intent.PrincipalID ||
		payload.RepositoryID != intent.RepositoryID || payload.Repository != intent.Repository || payload.Operation != "pr.rebase" ||
		payload.PolicySnapshotHash != intent.AuthorityRev || payload.MembershipEpoch != intent.MembershipEpoch ||
		!sameStringMap(payload.Constraints, expectedConstraints) {
		return AuthorityBoundaryReceipt{}, ErrAuthorityBoundaryReceiptCorrupt
	}
	return receipt, nil
}

func authorityBoundaryReceiptRetryable(err error) bool {
	if isSQLiteLockErr(err) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "write conflict") || strings.Contains(message, "deadlock") || strings.Contains(message, "try again")
}

func isLegacyAuthorityCaptureKind(kind string) bool {
	return kind == AuthorityBoundaryReceiptLegacyCaptureV1Kind || kind == AuthorityBoundaryReceiptLegacyCaptureKind
}

func expectedAuthorityBoundaryClaimLimit(kind string) string {
	switch kind {
	case AuthorityBoundaryReceiptLegacyCaptureV1Kind, AuthorityBoundaryReceiptLegacyCaptureKind:
		return legacyAuthorityCaptureClaimLimit
	case AuthorityBoundaryReceiptDelegatedEffectKind:
		return delegatedEffectClaimLimit
	default:
		return ""
	}
}

func canonicalReceiptJSON(kind string, encoded []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var value any
	switch kind {
	case AuthorityBoundaryReceiptLegacyCaptureV1Kind:
		payload := &legacyAuthorityCapturePayloadV1{}
		value = payload
		if err := decoder.Decode(value); err != nil || payload.Schema != legacyAuthorityCaptureV1Schema {
			return nil, ErrAuthorityBoundaryReceiptCorrupt
		}
	case AuthorityBoundaryReceiptLegacyCaptureKind:
		payload := &legacyAuthorityCapturePayload{}
		value = payload
		if err := decoder.Decode(value); err != nil || payload.Schema != legacyAuthorityCaptureSchema {
			return nil, ErrAuthorityBoundaryReceiptCorrupt
		}
	case AuthorityBoundaryReceiptDelegatedEffectKind:
		payload := &delegatedEffectPayload{}
		value = payload
		if err := decoder.Decode(value); err != nil || payload.Schema != delegatedEffectSchema {
			return nil, ErrAuthorityBoundaryReceiptCorrupt
		}
	default:
		return nil, ErrAuthorityBoundaryReceiptCorrupt
	}
	return json.Marshal(value)
}

func validateSecretSafeReceiptJSON(encoded []byte) error {
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return ErrAuthorityBoundaryReceiptSecret
	}
	var inspect func(any) error
	inspect = func(current any) error {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				lower := strings.ToLower(key)
				if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "private_key") || strings.Contains(lower, "credential_hash") {
					return ErrAuthorityBoundaryReceiptSecret
				}
				if err := inspect(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := inspect(child); err != nil {
					return err
				}
			}
		case string:
			if safeDelegatedAuthorityText(typed) != strings.TrimSpace(typed) {
				return ErrAuthorityBoundaryReceiptSecret
			}
		}
		return nil
	}
	return inspect(value)
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func isFullSHA256Digest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isCanonicalSHA256Digest(value string) bool {
	return value == strings.ToLower(value) && isFullSHA256Digest(value)
}

func isCanonicalReceiptID(value string) bool {
	return strings.HasPrefix(value, "abr_") && len(value) == 68 && isCanonicalSHA256Digest(strings.TrimPrefix(value, "abr_"))
}

func isFullHexRevision(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isCanonicalSourceRevision(value string) bool {
	return value == strings.ToLower(value) && isFullHexRevision(value)
}
