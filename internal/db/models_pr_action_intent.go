package db

import "time"

// PullRequestActionIntent is the AGS-owned durable authority record for an
// external workflow action. Provider labels are only delivery events; they are
// never an authorization source. The nonterminal dispatching state is committed
// before a provider label write and remains retryable when its outcome is unknown.
type PullRequestActionIntent struct {
	ID                   string  `gorm:"primaryKey;size:64"`
	IdempotencyKey       string  `gorm:"size:255;not null;uniqueIndex:idx_pr_action_intent_idempotency"`
	Action               string  `gorm:"size:64;not null;index"`
	State                string  `gorm:"size:32;not null;index"`
	PullRequestID        uint    `gorm:"not null;index"`
	RepositoryID         uint    `gorm:"not null;index"`
	AGSPRNumber          int     `gorm:"column:ags_pr_number;not null"`
	Repository           string  `gorm:"size:512;not null"`
	ForgejoRepo          string  `gorm:"size:512;not null"`
	ForgejoPRNumber      int     `gorm:"not null"`
	HeadRef              string  `gorm:"size:255;not null"`
	BaseRef              string  `gorm:"size:255;not null"`
	ExpectedHeadSHA      string  `gorm:"size:40;not null"`
	ExpectedBaseSHA      string  `gorm:"size:40;not null"`
	ExpectedLabels       string  `gorm:"type:text;not null"`
	PostLabels           string  `gorm:"type:text;not null"`
	PrincipalID          uint    `gorm:"not null"`
	AgentSessionID       *string `gorm:"type:char(36);index"`
	TeamIdentityID       string  `gorm:"size:255"`
	PolicyClass          string  `gorm:"size:255"`
	MembershipEpoch      int64
	AuthorityRev         string  `gorm:"size:255"`
	BoundaryProtocol     string  `gorm:"size:64"`
	BoundaryReceiptID    *string `gorm:"size:80;index:idx_pr_action_intent_boundary_receipt"`
	ProviderEffectStatus string  `gorm:"size:32"`
	ProviderOutcome      string  `gorm:"size:32"`
	MergeMethod          string  `gorm:"size:32"`
	// The exact one-shot unique index is owned and shape-validated by
	// MigrateDelegatedPRMergeV2. Keeping it out of AutoMigrate prevents a
	// same-name wrong artifact from being silently accepted or rewritten.
	DelegationID               *string    `gorm:"size:36"`
	DelegationRevision         string     `gorm:"size:36"`
	DelegationNotAfter         *time.Time `gorm:"index"`
	DelegationFactsDigest      string     `gorm:"size:71"`
	DelegationState            string     `gorm:"size:32"`
	DelegationConsumeReceiptID string     `gorm:"size:36;index"`
	DelegationConsumedAt       *time.Time
	RequestSource              string    `gorm:"size:32;not null;default:'ags_client'"`
	RequestActor               string    `gorm:"size:255"`
	RequestBindingRev          string    `gorm:"size:96"`
	ExpiresInSeconds           int64     `gorm:"not null;default:0"`
	ResultSHA                  string    `gorm:"size:40"`
	FailureCode                string    `gorm:"size:96"`
	FailureSummary             string    `gorm:"size:512"`
	ExpiresAt                  time.Time `gorm:"not null;index"`
	DispatchedAt               *time.Time
	AcceptedAt                 *time.Time
	FinishedAt                 *time.Time
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

func (PullRequestActionIntent) TableName() string { return "pull_request_action_intents" }
