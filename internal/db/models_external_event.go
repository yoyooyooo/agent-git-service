package db

import "time"

// ExternalEvent stores facts accepted from systems outside AGS. The row is the
// durable source for downstream projections such as repo incident issues and
// notification deliveries.
type ExternalEvent struct {
	ID           uint       `gorm:"primaryKey;autoIncrement"`
	EventKey     string     `gorm:"size:512;not null;uniqueIndex"`
	Source       string     `gorm:"size:64;not null;index:idx_external_events_source_type"`
	Type         string     `gorm:"size:128;not null;index:idx_external_events_source_type"`
	RepositoryID uint       `gorm:"index:idx_external_events_repo_aggregate"`
	Repository   Repository `gorm:"foreignKey:RepositoryID"`
	AggregateKey string     `gorm:"size:512;not null;index:idx_external_events_repo_aggregate"`
	Payload      LargeText
	OccurredAt   time.Time `gorm:"index"`
	AcceptedAt   time.Time `gorm:"index"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RepoIncident is the repo-scoped, agent-consumable projection of external
// events. For Multica failures it points to the generated repo issue that
// carries the rolling 30-day digest.
type RepoIncident struct {
	ID                   uint       `gorm:"primaryKey;autoIncrement"`
	Source               string     `gorm:"size:64;not null;uniqueIndex:idx_repo_incident_source_aggregate"`
	Type                 string     `gorm:"size:128;not null;index"`
	AggregateKey         string     `gorm:"size:512;not null;uniqueIndex:idx_repo_incident_source_aggregate"`
	RepositoryID         uint       `gorm:"not null;index"`
	Repository           Repository `gorm:"foreignKey:RepositoryID"`
	IssueID              uint       `gorm:"not null;index"`
	Issue                Issue      `gorm:"foreignKey:IssueID"`
	WindowDays           int        `gorm:"not null;default:30"`
	EventCount30d        int        `gorm:"not null;default:0"`
	TotalEventCount      int        `gorm:"not null;default:0"`
	LastEventKey         string     `gorm:"size:512"`
	LastEventAt          *time.Time `gorm:"index"`
	LastNotifiedEventKey string     `gorm:"size:512"`
	LastNotifiedAt       *time.Time `gorm:"index"`
	Status               string     `gorm:"size:32;not null;default:open;index"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
}
