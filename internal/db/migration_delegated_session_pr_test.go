package db

import (
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type pullRequestBeforeDelegatedSession struct {
	ID               uint `gorm:"primaryKey;autoIncrement"`
	RepositoryID     uint `gorm:"not null;index"`
	HeadRepositoryID uint `gorm:"not null;index"`
	Number           int  `gorm:"not null"`
	Title            string
	AuthorID         uint
	HeadRef          string
	BaseRef          string
}

func (pullRequestBeforeDelegatedSession) TableName() string { return "pull_requests" }

func TestMigrateAddsDelegatedSessionPRProvenanceWithoutLosingExistingPR(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "delegated-session-pr-upgrade.db")
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if sqlDB, err := gdb.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}

	if err := gdb.AutoMigrate(&User{}, &Repository{}, &pullRequestBeforeDelegatedSession{}); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	owner := User{Login: "owner", Type: TypeUser, Status: UserStatusActive}
	if err := gdb.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	repo := Repository{Name: "repo", FullName: "owner/repo", OwnerID: owner.ID, DefaultBranch: "main"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	legacy := pullRequestBeforeDelegatedSession{
		RepositoryID: repo.ID, HeadRepositoryID: repo.ID, Number: 1, Title: "existing", AuthorID: owner.ID,
		HeadRef: "agent/existing", BaseRef: "main",
	}
	if err := gdb.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}

	if err := Migrate(gdb); err != nil {
		t.Fatalf("upgrade migration: %v", err)
	}
	if !gdb.Migrator().HasTable(&DelegatedAgentSession{}) || !gdb.Migrator().HasColumn(&DelegatedAgentSession{}, "TeamBindingRevision") || !gdb.Migrator().HasTable(&TeamAuthorityEpoch{}) || !gdb.Migrator().HasColumn(&PullRequest{}, "AgentSessionID") {
		t.Fatal("delegated session team authority or PR provenance schema is missing after migration")
	}
	var upgraded PullRequest
	if err := gdb.First(&upgraded, legacy.ID).Error; err != nil {
		t.Fatalf("load existing PR after migration: %v", err)
	}
	if upgraded.Title != legacy.Title || upgraded.AgentSessionID != nil {
		t.Fatalf("existing PR changed during migration: %#v", upgraded)
	}
}
