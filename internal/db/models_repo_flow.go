package db

import "time"

// RepoFlowEnvProjection records the latest AGS-owned deployment projection
// result for one repository environment ref, for example refs/heads/env/uat.
type RepoFlowEnvProjection struct {
	ID           uint       `gorm:"primaryKey;autoIncrement"`
	RepositoryID uint       `gorm:"not null;uniqueIndex:idx_repo_flow_env_projection_repo_env;index"`
	Repository   Repository `gorm:"foreignKey:RepositoryID"`
	RepoFullName string     `gorm:"size:512;not null;index"`
	Env          string     `gorm:"size:64;not null;uniqueIndex:idx_repo_flow_env_projection_repo_env"`
	SourceRef    string     `gorm:"size:255;not null"`
	SourceSHA    string     `gorm:"size:40;not null;index"`
	Provider     string     `gorm:"size:32;not null;default:'gitlab'"`
	ProjectPath  string     `gorm:"size:512"`
	TargetBranch string     `gorm:"size:255;index"`
	State        string     `gorm:"size:32;not null;index"` // pending, projected, failed, skipped
	Error        string     `gorm:"type:text"`
	TriggeredAt  time.Time  `gorm:"not null;index"`
	ProjectedAt  *time.Time `gorm:"index"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}
