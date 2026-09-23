package db

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

var ErrAuthorityBoundaryReceiptImmutable = errors.New("authority boundary receipt is immutable")

// AuthorityBoundaryReceipt is a narrow append-only, content-addressed ledger.
// Payload is canonical server-generated secret-safe JSON; transports never own
// or accept arbitrary receipt payloads.
type AuthorityBoundaryReceipt struct {
	ReceiptID       string    `gorm:"primaryKey;size:80" json:"receipt_id"`
	Kind            string    `gorm:"size:64;not null;index" json:"kind"`
	ParentDigest    string    `gorm:"size:64;index" json:"parent_digest,omitempty"`
	AuthorityEpoch  string    `gorm:"size:64;not null;index" json:"authority_epoch"`
	SnapshotDigest  string    `gorm:"size:64;not null;index" json:"snapshot_digest"`
	SourceRevision  string    `gorm:"size:64;not null;index" json:"source_revision"`
	ClaimLimit      string    `gorm:"size:512;not null" json:"claim_limit"`
	Payload         string    `gorm:"type:text;not null" json:"-"`
	CreatedByUserID uint      `gorm:"not null;index" json:"created_by_user_id"`
	CreatedAt       time.Time `gorm:"not null;index" json:"created_at"`
}

func (AuthorityBoundaryReceipt) TableName() string { return "authority_boundary_receipts" }

func (*AuthorityBoundaryReceipt) BeforeUpdate(*gorm.DB) error {
	return ErrAuthorityBoundaryReceiptImmutable
}

func (*AuthorityBoundaryReceipt) BeforeDelete(*gorm.DB) error {
	return ErrAuthorityBoundaryReceiptImmutable
}
