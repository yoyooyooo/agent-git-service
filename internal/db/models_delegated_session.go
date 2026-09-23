package db

import "time"

const (
	DelegatedBySourceSessionSnapshot   = "session_snapshot"
	DelegatedBySourceMigrationBackfill = "migration_backfill"
	DelegatedBySourcePrincipalOnly     = "principal_only"
)

// DelegatedAgentSession is the historical Session schema. New runtime rows are
// derived only from Access Grants and use credential_mode=access_grant_transport;
// assertion and merge-delegation columns remain migration/audit compatibility
// fields and cannot authorize use. Raw bearer credentials are never stored.
type DelegatedAgentSession struct {
	ID               string `gorm:"type:char(36);primaryKey" json:"id"`
	CredentialHash   string `gorm:"size:64;not null;uniqueIndex" json:"-"`
	CredentialPrefix string `gorm:"size:32;not null;index" json:"credential_prefix"`

	PrincipalUserID uint   `gorm:"not null;index" json:"principal_user_id"`
	PrincipalLogin  string `gorm:"size:255" json:"-"`
	PrincipalUser   User   `gorm:"foreignKey:PrincipalUserID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT" json:"-"`

	// AccessGrantID and ActorUserID are populated only for internal transport
	// Sessions derived from canonical Access Grants. PrincipalUserID remains the
	// executor used by existing Git and GitHub-compatible API adapters; ActorUserID is the immutable
	// workload identity projected into commits, PRs, and receipts.
	AccessGrantID                string `gorm:"type:char(36);index" json:"access_grant_id,omitempty"`
	AccessGrantAuthorityRevision string `gorm:"size:71" json:"access_grant_authority_revision,omitempty"`
	ActorUserID                  uint   `gorm:"index" json:"actor_user_id,omitempty"`

	// DelegatedBy* snapshots the AGS-owned human binding when the session is
	// issued. It is presentation/audit provenance only and never participates in
	// authorization. Existing sessions are backfilled from current AGS-owned
	// identity facts and labeled as migration-derived.
	DelegatedByUserID *uint  `gorm:"index" json:"-"`
	DelegatedByLogin  string `gorm:"size:255" json:"-"`
	DelegatedBySource string `gorm:"size:32" json:"-"`

	Issuer            string `gorm:"size:255;not null;uniqueIndex:idx_delegated_session_assertion,priority:1" json:"issuer"`
	AssertionVersion  int    `gorm:"not null" json:"assertion_version"`
	AssertionPurpose  string `gorm:"size:64;not null;uniqueIndex:idx_delegated_session_assertion,priority:2" json:"assertion_purpose"`
	AssertionJTI      string `gorm:"size:255;not null;uniqueIndex:idx_delegated_session_assertion,priority:3" json:"assertion_jti"`
	AssertionAudience string `gorm:"size:255;not null" json:"assertion_audience"`
	AssertionKeyID    string `gorm:"size:255" json:"assertion_key_id,omitempty"`

	ContractRevision    string `gorm:"size:255;index" json:"contract_revision,omitempty"`
	CredentialMode      string `gorm:"size:32" json:"credential_mode,omitempty"`
	BindingID           string `gorm:"size:255;index" json:"binding_id,omitempty"`
	BindingRevision     string `gorm:"size:255" json:"binding_revision,omitempty"`
	TeamIdentityID      string `gorm:"size:255;index" json:"team_identity_id,omitempty"`
	TeamBindingRevision string `gorm:"size:255" json:"team_binding_revision,omitempty"`
	PolicyClass         string `gorm:"size:255;index" json:"policy_class,omitempty"`
	MembershipEpoch     int64  `gorm:"index" json:"membership_epoch,omitempty"`
	IssuerInstanceID    string `gorm:"size:255;index" json:"issuer_instance_id,omitempty"`
	TrustRevision       string `gorm:"size:255" json:"trust_revision,omitempty"`
	IssuerSubject       string `gorm:"size:255;index" json:"issuer_subject,omitempty"`
	CorrelationID       string `gorm:"size:255;index" json:"correlation_id,omitempty"`

	IssuerWorkspaceID     string   `gorm:"size:255;index" json:"issuer_workspace_id,omitempty"`
	IssuerWorkspace       string   `gorm:"size:255" json:"-"`
	ExternalAgentID       string   `gorm:"size:255;index" json:"external_agent_id,omitempty"`
	ExternalAgentName     string   `gorm:"size:255" json:"external_agent_name,omitempty"`
	ExternalSquadID       string   `gorm:"size:255" json:"external_squad_id,omitempty"`
	ExternalTaskID        string   `gorm:"size:255;index" json:"external_task_id,omitempty"`
	ExternalRunID         string   `gorm:"size:255" json:"external_run_id,omitempty"`
	ExternalIssueID       string   `gorm:"size:255" json:"external_issue_id,omitempty"`
	ExternalIssueKey      string   `gorm:"size:255" json:"external_issue_key,omitempty"`
	ExternalTriggerID     string   `gorm:"size:255" json:"external_trigger_id,omitempty"`
	ExternalRuntimeID     string   `gorm:"size:255" json:"external_runtime_id,omitempty"`
	ExternalRole          string   `gorm:"size:255" json:"external_role,omitempty"`
	WorkloadContextSchema string   `gorm:"size:64" json:"workload_context_schema,omitempty"`
	TraceQuality          string   `gorm:"size:32" json:"trace_quality,omitempty"`
	MissingFields         []string `gorm:"serializer:json" json:"missing_fields,omitempty"`

	TargetInstance       string            `gorm:"size:255;not null" json:"target_instance"`
	ResourceService      string            `gorm:"size:64" json:"resource_service,omitempty"`
	RepositoryID         uint              `gorm:"not null;index" json:"repository_id"`
	Repository           Repository        `gorm:"foreignKey:RepositoryID;constraint:OnUpdate:CASCADE,OnDelete:RESTRICT" json:"-"`
	OperationName        string            `gorm:"size:64;index" json:"operation_name,omitempty"`
	OperationConstraints map[string]string `gorm:"serializer:json" json:"operation_constraints,omitempty"`

	MergeDelegationID                      string     `gorm:"size:36;index" json:"merge_delegation_id,omitempty"`
	MergeDelegationRevision                string     `gorm:"size:36" json:"merge_delegation_revision,omitempty"`
	MergeDelegationNotAfter                *time.Time `gorm:"index" json:"merge_delegation_not_after,omitempty"`
	MergeDelegationFactsDigest             string     `gorm:"size:71" json:"merge_delegation_facts_digest,omitempty"`
	MergeDelegationTargetInstance          string     `gorm:"size:64" json:"merge_delegation_target_instance,omitempty"`
	MergeDelegationCanonicalRepositoryID   string     `gorm:"size:71" json:"merge_delegation_canonical_repository_id,omitempty"`
	MergeDelegationProvider                string     `gorm:"size:32" json:"merge_delegation_provider,omitempty"`
	MergeDelegationProviderBindingID       string     `gorm:"size:71" json:"merge_delegation_provider_binding_id,omitempty"`
	MergeDelegationProviderBindingRevision string     `gorm:"size:71" json:"merge_delegation_provider_binding_revision,omitempty"`
	MergeDelegationProviderRepository      string     `gorm:"size:512" json:"merge_delegation_provider_repository,omitempty"`
	MergeDelegationAGSPRNumber             int        `gorm:"column:merge_delegation_ags_pr_number" json:"merge_delegation_ags_pr_number,omitempty"`
	MergeDelegationProviderPRNumber        int        `gorm:"column:merge_delegation_provider_pr_number" json:"merge_delegation_provider_pr_number,omitempty"`
	MergeDelegationExpectedHeadSHA         string     `gorm:"size:40" json:"merge_delegation_expected_head_sha,omitempty"`
	MergeDelegationExpectedBaseSHA         string     `gorm:"size:40" json:"merge_delegation_expected_base_sha,omitempty"`
	MergeDelegationBaseRef                 string     `gorm:"size:255" json:"merge_delegation_base_ref,omitempty"`
	MergeDelegationMethod                  string     `gorm:"size:32" json:"merge_delegation_method,omitempty"`
	MergeDelegationProjectionRevision      string     `gorm:"size:71" json:"merge_delegation_projection_revision,omitempty"`

	GrantedCapabilities    []string `gorm:"serializer:json;not null" json:"granted_capabilities"`
	PolicyVersion          string   `gorm:"size:255;not null" json:"policy_version"`
	PolicySnapshotHash     string   `gorm:"size:64;not null" json:"policy_snapshot_hash"`
	NativeGrantRevision    string   `gorm:"size:255" json:"native_grant_revision,omitempty"`
	ResourcePolicyRevision string   `gorm:"size:255" json:"resource_policy_revision,omitempty"`

	CreatedAt       time.Time  `gorm:"not null;index" json:"created_at"`
	ExpiresAt       time.Time  `gorm:"not null;index" json:"expires_at"`
	ExpiryAuditedAt *time.Time `gorm:"index" json:"-"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
	RevokedAt       *time.Time `gorm:"index" json:"revoked_at,omitempty"`

	RevokedByUserID  *uint  `gorm:"index" json:"revoked_by_user_id,omitempty"`
	RevokedByUser    *User  `gorm:"foreignKey:RevokedByUserID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"-"`
	RevocationReason string `gorm:"size:255" json:"revocation_reason,omitempty"`
}

func (DelegatedAgentSession) TableName() string { return "delegated_agent_sessions" }

// TeamAuthorityEpoch is the target-local monotonic revocation ledger. It is
// deliberately separate from desired-state configuration so an old config
// snapshot cannot resurrect an exchanged session after a floor advance.
type TeamAuthorityEpoch struct {
	ID               uint      `gorm:"primaryKey"`
	IssuerInstanceID string    `gorm:"size:255;not null;uniqueIndex:idx_team_authority_epoch,priority:1"`
	TeamIdentityID   string    `gorm:"size:255;not null;uniqueIndex:idx_team_authority_epoch,priority:2"`
	PolicyClass      string    `gorm:"size:255;not null;uniqueIndex:idx_team_authority_epoch,priority:3"`
	EpochFloor       int64     `gorm:"not null"`
	HighWater        int64     `gorm:"not null"`
	UpdatedAt        time.Time `gorm:"not null"`
}

func (TeamAuthorityEpoch) TableName() string { return "team_authority_epochs" }
