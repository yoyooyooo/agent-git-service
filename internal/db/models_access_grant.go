package db

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

var (
	errAccessGrantAuthorityImmutable   = errors.New("access grant authority is immutable")
	errAccessGrantDeleteForbidden      = errors.New("access grants cannot be deleted")
	errAccessInvocationFactsImmutable  = errors.New("access grant invocation facts are immutable")
	errAccessInvocationDeleteForbidden = errors.New("access grant invocations cannot be deleted")
)

// AccessGrant is an AGS-owned, task/repository-scoped authority snapshot. The
// bearer is returned once; only its SHA-256 hash and a non-authenticating prefix
// are persisted. ActorUserID is the canonical runtime actor while
// ExecutorUserID is the server-owned compatibility principal selected by the
// accepted policy binding.
type AccessGrant struct {
	ID               string `gorm:"type:char(36);primaryKey" json:"id"`
	CredentialHash   string `gorm:"size:64;not null;uniqueIndex" json:"-"`
	CredentialPrefix string `gorm:"size:32;not null;index" json:"credential_prefix"`

	ActorUserID    uint   `gorm:"not null;index" json:"actor_user_id"`
	ExecutorUserID uint   `gorm:"not null;index" json:"executor_user_id"`
	SnapshotID     string `gorm:"type:char(36);not null;index" json:"snapshot_id"`

	SourceInstanceID    string `gorm:"size:64;not null;index" json:"source_instance_id"`
	ExternalWorkspaceID string `gorm:"size:255;not null;index" json:"external_workspace_id"`
	ExternalAgentID     string `gorm:"size:255;not null;index" json:"external_agent_id"`
	ExternalTaskID      string `gorm:"size:255;not null;index" json:"external_task_id"`
	ExternalRunID       string `gorm:"size:255;not null" json:"external_run_id"`
	ExternalIssueID     string `gorm:"size:255" json:"external_issue_id,omitempty"`
	ExternalRuntimeID   string `gorm:"size:255" json:"external_runtime_id,omitempty"`

	RepositoryID       uint   `gorm:"not null;index" json:"repository_id"`
	RepositoryFullName string `gorm:"size:512;not null;index" json:"repository"`
	TargetInstance     string `gorm:"size:64;not null;index" json:"target_instance"`

	AgentSelector          string   `gorm:"size:255" json:"agent_selector,omitempty"`
	AgentSelectorOutcome   string   `gorm:"size:32;not null" json:"agent_selector_outcome"`
	DefaultPolicyClass     string   `gorm:"size:255;not null" json:"default_policy_class"`
	RequestedPolicyClass   string   `gorm:"size:255" json:"requested_policy_class,omitempty"`
	PolicyClassOutcome     string   `gorm:"size:32;not null" json:"policy_class_outcome"`
	EffectivePolicyClasses []string `gorm:"serializer:json;not null" json:"effective_policy_classes"`
	RequestedOperations    []string `gorm:"serializer:json;not null" json:"requested_operations"`
	EffectiveOperations    []string `gorm:"serializer:json;not null" json:"effective_operations"`
	Warnings               []string `gorm:"serializer:json;not null" json:"warnings"`

	DefaultBindingRevision  string `gorm:"size:255;not null" json:"default_binding_revision"`
	ElevatedBindingRevision string `gorm:"size:255" json:"elevated_binding_revision,omitempty"`
	ResourcePolicyRevision  string `gorm:"size:255;not null" json:"resource_policy_revision"`
	AuthorityRevision       string `gorm:"size:71;not null;index" json:"authority_revision"`

	RenewedFromGrantID string     `gorm:"type:char(36);index" json:"renewed_from_grant_id,omitempty"`
	CreatedAt          time.Time  `gorm:"not null;index" json:"created_at"`
	ExpiresAt          time.Time  `gorm:"not null;index" json:"expires_at"`
	LastUsedAt         *time.Time `json:"last_used_at,omitempty"`
	RevokedAt          *time.Time `gorm:"index" json:"revoked_at,omitempty"`
	RevocationReason   string     `gorm:"size:255" json:"revocation_reason,omitempty"`
}

func (AccessGrant) TableName() string { return "access_grants" }

func (*AccessGrant) BeforeUpdate(tx *gorm.DB) error {
	for _, field := range []string{
		"ID", "CredentialHash", "CredentialPrefix", "ActorUserID", "ExecutorUserID", "SnapshotID",
		"SourceInstanceID", "ExternalWorkspaceID", "ExternalAgentID", "ExternalTaskID", "ExternalRunID",
		"ExternalIssueID", "ExternalRuntimeID", "RepositoryID", "RepositoryFullName", "TargetInstance",
		"AgentSelector", "AgentSelectorOutcome", "DefaultPolicyClass", "RequestedPolicyClass",
		"PolicyClassOutcome", "EffectivePolicyClasses", "RequestedOperations", "EffectiveOperations",
		"Warnings", "DefaultBindingRevision", "ElevatedBindingRevision", "ResourcePolicyRevision",
		"AuthorityRevision", "RenewedFromGrantID", "CreatedAt", "ExpiresAt",
	} {
		if tx.Statement.Changed(field) {
			return errAccessGrantAuthorityImmutable
		}
	}
	return nil
}

func (*AccessGrant) BeforeDelete(*gorm.DB) error { return errAccessGrantDeleteForbidden }

// AccessGrantInvocation is both an exact operation authorization receipt and,
// for provider effects, the durable single-attempt intent/recovery locator.
type AccessGrantInvocation struct {
	ID                string  `gorm:"type:char(36);primaryKey" json:"id"`
	EffectKey         *string `gorm:"size:71;uniqueIndex" json:"effect_key,omitempty"`
	GrantID           string  `gorm:"type:char(36);not null;index" json:"grant_id"`
	ActorUserID       uint    `gorm:"not null;index" json:"actor_user_id"`
	ExecutorUserID    uint    `gorm:"not null;index" json:"executor_user_id"`
	RepositoryID      uint    `gorm:"not null;index" json:"repository_id"`
	Repository        string  `gorm:"size:512;not null;index" json:"repository"`
	Operation         string  `gorm:"size:64;not null;index" json:"operation"`
	ConstraintsJSON   string  `gorm:"type:text;not null" json:"-"`
	AuthorityRevision string  `gorm:"size:71;not null;index" json:"authority_revision"`

	AGSPRNumber      int    `gorm:"index" json:"ags_pr_number,omitempty"`
	Provider         string `gorm:"size:32" json:"provider,omitempty"`
	ProviderRepo     string `gorm:"size:512" json:"provider_repository,omitempty"`
	ProviderPRNumber int    `gorm:"index" json:"provider_pr_number,omitempty"`
	ExpectedHeadSHA  string `gorm:"size:40" json:"expected_head_sha,omitempty"`
	ExpectedBaseSHA  string `gorm:"size:40" json:"expected_base_sha,omitempty"`
	BaseRef          string `gorm:"size:255" json:"base_ref,omitempty"`
	EffectMethod     string `gorm:"size:32" json:"effect_method,omitempty"`

	State                string `gorm:"size:32;not null;index" json:"state"`
	AuthorizationOutcome string `gorm:"size:32;not null" json:"authorization_outcome"`
	ProviderAttempt      string `gorm:"size:32;not null" json:"provider_attempt"`
	ProviderOutcome      string `gorm:"size:64;not null" json:"provider_outcome"`
	ProviderMerged       bool   `gorm:"not null;default:false" json:"provider_merged"`
	ProviderMergeSHA     string `gorm:"size:40" json:"provider_merge_sha,omitempty"`
	DenialCode           string `gorm:"size:64" json:"denial_code,omitempty"`

	CreatedAt  time.Time  `gorm:"not null;index" json:"created_at"`
	UpdatedAt  time.Time  `gorm:"not null" json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func (AccessGrantInvocation) TableName() string { return "access_grant_invocations" }

func (*AccessGrantInvocation) BeforeUpdate(tx *gorm.DB) error {
	for _, field := range []string{
		"ID", "EffectKey", "GrantID", "ActorUserID", "ExecutorUserID", "RepositoryID", "Repository",
		"Operation", "ConstraintsJSON", "AuthorityRevision", "AGSPRNumber", "Provider", "ProviderRepo",
		"ProviderPRNumber", "ExpectedHeadSHA", "ExpectedBaseSHA", "BaseRef", "EffectMethod", "CreatedAt",
	} {
		if tx.Statement.Changed(field) {
			return errAccessInvocationFactsImmutable
		}
	}
	return nil
}

func (*AccessGrantInvocation) BeforeDelete(*gorm.DB) error { return errAccessInvocationDeleteForbidden }
