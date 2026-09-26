package db

import "time"

// ClientRunSession is one native actor's short-lived standard-client credential.
// It adds provenance, not permissions. The hashed bearer is never recoverable;
// the native parent token and user remain live authorization dependencies.
type ClientRunSession struct {
	ID                string `gorm:"primaryKey;size:36"`
	UserID            uint   `gorm:"not null;index"`
	User              User   `gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE"`
	ParentTokenID     *uint  `gorm:"index"`
	ParentToken       Token  `gorm:"foreignKey:ParentTokenID;constraint:OnDelete:SET NULL"`
	CredentialHash    string `gorm:"uniqueIndex;size:64;not null" json:"-"`
	ContextJSON       string `gorm:"type:text;not null"`
	AssociationStatus string `gorm:"size:24;not null"`
	SnapshotID        string `gorm:"size:36"`
	CreatedAt         time.Time
	ExpiresAt         time.Time `gorm:"index"`
	RevokedAt         *time.Time
}

// ClientRunLink persists only business provenance. It has no credential or
// permission role, and association-write failure cannot undo an accepted PR.
type ClientRunLink struct {
	ID            uint             `gorm:"primaryKey"`
	RunID         string           `gorm:"size:36;not null;uniqueIndex:idx_client_run_pr,priority:1"`
	Run           ClientRunSession `gorm:"foreignKey:RunID;constraint:OnDelete:CASCADE"`
	RepositoryID  uint             `gorm:"not null;index"`
	Repository    Repository       `gorm:"foreignKey:RepositoryID;constraint:OnDelete:CASCADE"`
	PullRequestID uint             `gorm:"not null;uniqueIndex:idx_client_run_pr,priority:2"`
	PullRequest   PullRequest      `gorm:"foreignKey:PullRequestID;constraint:OnDelete:CASCADE"`
	CreatedAt     time.Time
}
