package db

import "time"

// OutboundDelivery records a durable attempt to deliver an AGS event to an
// external target. The idempotency key is a business-fact key, not a random
// request id: the same event fact delivered to the same target must reuse the
// same row.
type OutboundDelivery struct {
	ID             uint      `gorm:"primaryKey"`
	CreatedAt      time.Time `gorm:"index"`
	UpdatedAt      time.Time
	IdempotencyKey string `gorm:"size:512;uniqueIndex;not null"`

	EventType      string `gorm:"size:128;index;not null"`
	TargetName     string `gorm:"size:128;index;not null"`
	TargetType     string `gorm:"size:64;not null"`
	SubjectType    string `gorm:"size:128;index"`
	SubjectKey     string `gorm:"size:512;index"`
	PayloadVersion string `gorm:"size:64"`
	PayloadJSON    LargeText

	Status         string     `gorm:"size:32;index;not null"`
	AttemptCount   int        `gorm:"not null;default:0"`
	MaxAttempts    int        `gorm:"not null;default:7"`
	LeaseOwner     string     `gorm:"size:128;index"`
	LeaseExpiresAt *time.Time `gorm:"index"`
	NextAttemptAt  *time.Time `gorm:"index"`
	LastAttemptAt  *time.Time
	DeliveredAt    *time.Time

	LastHTTPStatus int
	LastErrorCode  string `gorm:"size:128"`
	LastError      LargeText

	RepoFullName   string `gorm:"size:256;index"`
	PRNumber       int    `gorm:"index"`
	ExternalRepo   string `gorm:"size:256"`
	ExternalNumber int
	MergeSHA       string `gorm:"size:64;index"`
}
