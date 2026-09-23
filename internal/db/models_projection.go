package db

import "time"

// ProjectionEvent stores structured AGS -> external projection facts and failures.
// Rows are safe for operator APIs: raw tokens and authenticated remotes must be redacted before insert.
type ProjectionEvent struct {
	ID           uint       `gorm:"primaryKey;autoIncrement"`
	Provider     string     `gorm:"size:64;not null;index:idx_projection_events_provider_repo_ref"`
	Type         string     `gorm:"size:128;not null;index"`
	Status       string     `gorm:"size:32;not null;index"`
	Authority    string     `gorm:"size:64;not null;default:ags"`
	RepositoryID uint       `gorm:"index:idx_projection_events_provider_repo_ref"`
	Repository   Repository `gorm:"foreignKey:RepositoryID"`
	RepoFullName string     `gorm:"size:256;not null;index"`
	TargetRepo   string     `gorm:"size:256;index"`
	Ref          string     `gorm:"size:512;not null;index:idx_projection_events_provider_repo_ref"`
	Branch       string     `gorm:"size:512;index"`
	AGSSHA       string     `gorm:"column:ags_sha;size:64;index"`
	ExternalSHA  string     `gorm:"column:external_sha;size:64;index"`
	ErrorSummary LargeText
	OccurredAt   time.Time  `gorm:"not null;index"`
	ResolvedAt   *time.Time `gorm:"index"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ProjectionRefState stores the latest state for one provider/repo/ref projection edge.
type ProjectionRefState struct {
	ID             uint       `gorm:"primaryKey;autoIncrement"`
	Provider       string     `gorm:"size:64;not null;uniqueIndex:idx_projection_ref_state_provider_repo_ref"`
	RepositoryID   uint       `gorm:"uniqueIndex:idx_projection_ref_state_provider_repo_ref"`
	Repository     Repository `gorm:"foreignKey:RepositoryID"`
	RepoFullName   string     `gorm:"size:256;not null;index"`
	TargetRepo     string     `gorm:"size:256;index"`
	Ref            string     `gorm:"size:512;not null;uniqueIndex:idx_projection_ref_state_provider_repo_ref"`
	Branch         string     `gorm:"size:512;index"`
	Type           string     `gorm:"size:128;not null;index"`
	Status         string     `gorm:"size:32;not null;index"`
	Generation     uint       `gorm:"not null;default:1"`
	Authority      string     `gorm:"size:64;not null;default:ags"`
	AGSSHA         string     `gorm:"column:ags_sha;size:64;index"`
	ExternalSHA    string     `gorm:"column:external_sha;size:64;index"`
	ErrorSummary   LargeText
	FirstSeenAt    time.Time  `gorm:"not null;index"`
	LastSeenAt     time.Time  `gorm:"not null;index"`
	ResolvedAt     *time.Time `gorm:"index"`
	LastNotifiedAt *time.Time `gorm:"index"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
