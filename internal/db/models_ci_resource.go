package db

// CIResource is a small durable namespace mapping, not a CI execution record or
// log cache. Provider integers must never be reinterpreted after a backend switch.
// Rows are removed with their owning AGS repository.
type CIResource struct {
	ID                 uint       `gorm:"primaryKey"`
	RepositoryID       uint       `gorm:"not null;uniqueIndex:idx_ci_resource_key,priority:1"`
	Repository         Repository `gorm:"foreignKey:RepositoryID;constraint:OnDelete:CASCADE"`
	Namespace          string     `gorm:"size:64;not null;uniqueIndex:idx_ci_resource_key,priority:2"`
	ExternalRepository string     `gorm:"size:255;not null;uniqueIndex:idx_ci_resource_key,priority:3"`
	Kind               string     `gorm:"size:16;not null;uniqueIndex:idx_ci_resource_key,priority:4"`
	ExternalID         string     `gorm:"size:128;not null;uniqueIndex:idx_ci_resource_key,priority:5"`
	ParentID           string     `gorm:"size:128"`
}
