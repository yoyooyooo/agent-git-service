package db

import "time"

const (
	MulticaLinkConfidenceAuthoritative = "authoritative"
	MulticaLinkConfidenceInferred      = "inferred"

	MulticaLinkSourceTaskToken           = "task_token"
	MulticaLinkSourceAccessGrantSnapshot = "access_grant_snapshot"
	MulticaLinkSourceMarker              = "marker"
)

// PullRequestMulticaLink stores the PR-create-time binding between an AGS PR
// and a Multica issue. Authoritative links may come from the historical
// Multica-signed task-token path or directly from the immutable execution-
// context snapshot bound to an Access Grant.
type PullRequestMulticaLink struct {
	ID                uint        `gorm:"primaryKey;autoIncrement"`
	PullRequestID     uint        `gorm:"not null;uniqueIndex:idx_pr_multica_link_primary;index:idx_pr_multica_link_issue"`
	PullRequest       PullRequest `gorm:"foreignKey:PullRequestID"`
	RepositoryID      uint        `gorm:"not null;index"`
	Repository        Repository  `gorm:"foreignKey:RepositoryID"`
	Workspace         string      `gorm:"size:255;not null;index:idx_pr_multica_link_issue,priority:1"`
	WorkspaceID       string      `gorm:"size:64;index"`
	IssueID           string      `gorm:"size:64;not null;index:idx_pr_multica_link_issue,priority:2"`
	IssueKey          string      `gorm:"size:64;not null"`
	IssueURL          string      `gorm:"size:1024"`
	TaskID            string      `gorm:"size:64;index"`
	AgentID           string      `gorm:"size:64;index"`
	RunID             string      `gorm:"size:128"`
	AccessGrantID     string      `gorm:"type:char(36);index"`
	SourceSnapshotID  string      `gorm:"type:char(36);index"`
	AssertionIssuer   string      `gorm:"size:255;uniqueIndex:idx_pr_multica_assertion,priority:1"`
	AssertionVersion  int
	AssertionPurpose  string  `gorm:"size:64;uniqueIndex:idx_pr_multica_assertion,priority:2"`
	AssertionAudience string  `gorm:"size:255"`
	AssertionJTI      *string `gorm:"size:255;uniqueIndex:idx_pr_multica_assertion,priority:3"`
	Confidence        string  `gorm:"size:32;not null;default:'inferred';index"`
	Source            string  `gorm:"size:64;not null"`
	CompletionIntent  bool    `gorm:"not null;default:true;index"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
