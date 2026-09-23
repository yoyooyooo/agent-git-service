package db

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

var errExecutionContextSnapshotImmutable = errors.New("execution context snapshots are immutable")

// ExecutionContextSnapshot is an immutable, credential-free observation pulled
// from a registered execution-context source. ContextJSON and SourceRefJSON are
// canonical JSON; no source bearer, credential hash, request header, runtime
// endpoint alias, or connector egress endpoint may be stored in this table.
type ExecutionContextSnapshot struct {
	ID                  string    `gorm:"type:char(36);primaryKey" json:"id"`
	Schema              string    `gorm:"size:64;not null" json:"schema"`
	SourceInstanceID    string    `gorm:"size:64;not null;index" json:"source_instance_id"`
	SourceRefJSON       string    `gorm:"type:text;not null" json:"-"`
	ContextSchema       string    `gorm:"size:64;not null" json:"context_schema"`
	ContextDigest       string    `gorm:"size:71;not null;index" json:"context_digest"`
	ContextJSON         string    `gorm:"type:text;not null" json:"-"`
	ExternalWorkspaceID string    `gorm:"size:255;not null;index" json:"external_workspace_id"`
	ExternalAgentID     string    `gorm:"size:255;not null;index" json:"external_agent_id"`
	ExternalTaskID      string    `gorm:"size:255;not null;index" json:"external_task_id"`
	ExternalRunID       string    `gorm:"size:255;not null" json:"external_run_id"`
	ExternalIssueID     string    `gorm:"size:255" json:"external_issue_id,omitempty"`
	ExternalRuntimeID   string    `gorm:"size:255" json:"external_runtime_id,omitempty"`
	SourceObservedAt    time.Time `gorm:"not null;index" json:"source_observed_at"`
	CreatedAt           time.Time `gorm:"not null;index" json:"created_at"`
}

func (ExecutionContextSnapshot) TableName() string { return "execution_context_snapshots" }

func (*ExecutionContextSnapshot) BeforeUpdate(*gorm.DB) error {
	return errExecutionContextSnapshotImmutable
}

func (*ExecutionContextSnapshot) BeforeDelete(*gorm.DB) error {
	return errExecutionContextSnapshotImmutable
}
