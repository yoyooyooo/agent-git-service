package db

import "time"

// PullRequestProjection records external PR/MR projections for an AGS pull request.
// AGS remains the authoritative data source; Forgejo/GitLab rows are projections.
type PullRequestProjection struct {
	ID             uint        `gorm:"primaryKey;autoIncrement"`
	PullRequestID  uint        `gorm:"not null;uniqueIndex:idx_pr_projection_pr_provider;index"`
	PullRequest    PullRequest `gorm:"foreignKey:PullRequestID"`
	RepositoryID   uint        `gorm:"not null;index"`
	Repository     Repository  `gorm:"foreignKey:RepositoryID"`
	Provider       string      `gorm:"size:32;not null;uniqueIndex:idx_pr_projection_pr_provider;uniqueIndex:idx_pr_projection_external"`
	ExternalRepo   string      `gorm:"size:512;not null;uniqueIndex:idx_pr_projection_external"`
	ExternalNumber int         `gorm:"not null;uniqueIndex:idx_pr_projection_external"`
	ExternalURL    string      `gorm:"size:2048"`
	SourceBranch   string      `gorm:"size:255;index"`
	TargetBranch   string      `gorm:"size:255;index"`
	State          string      `gorm:"size:32;not null;default:'open';index"`
	LastSyncedSHA  string      `gorm:"size:40"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
