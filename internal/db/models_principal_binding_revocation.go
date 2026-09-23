package db

import "time"

// PrincipalBindingRevocation is an append-only tombstone for a revoked
// principal binding ID. It survives authority snapshot rollback and process
// restart so a revoked ID cannot issue or reactivate delegated Sessions.
type PrincipalBindingRevocation struct {
	BindingID        string    `gorm:"size:255;primaryKey" json:"binding_id"`
	BindingRevision  string    `gorm:"size:255;not null" json:"binding_revision"`
	IssuerInstanceID string    `gorm:"size:255;not null;index" json:"issuer_instance_id"`
	IssuerSubject    string    `gorm:"size:255;not null;index" json:"issuer_subject"`
	PrincipalUserID  uint      `gorm:"not null;index" json:"principal_user_id"`
	RevokedAt        time.Time `gorm:"not null;index" json:"revoked_at"`
	ObservedAt       time.Time `gorm:"not null" json:"observed_at"`
}

func (PrincipalBindingRevocation) TableName() string {
	return "principal_binding_revocations"
}
