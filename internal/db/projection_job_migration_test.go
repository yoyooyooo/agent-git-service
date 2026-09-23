package db

import (
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type legacyPullRequestProjectionJob struct {
	ID            uint   `gorm:"primaryKey;autoIncrement"`
	PullRequestID uint   `gorm:"not null;uniqueIndex:idx_pr_projection_job_pr_provider"`
	RepositoryID  uint   `gorm:"not null"`
	Provider      string `gorm:"size:32;not null;uniqueIndex:idx_pr_projection_job_pr_provider"`
	RepoFullName  string `gorm:"size:512;not null"`
	AGSPRNumber   int    `gorm:"column:ags_pr_number;not null"`
	Phase         string `gorm:"size:32;not null"`
}

func (legacyPullRequestProjectionJob) TableName() string { return "pull_request_projection_jobs" }

func TestMigrateAddsSafeDefaultsForLegacyProjectionJobs(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open legacy DB: %v", err)
	}
	if err := database.AutoMigrate(&legacyPullRequestProjectionJob{}); err != nil {
		t.Fatalf("create legacy projection job schema: %v", err)
	}
	legacy := legacyPullRequestProjectionJob{PullRequestID: 7, RepositoryID: 3, Provider: "forgejo", RepoFullName: "example-owner/demo", AGSPRNumber: 9, Phase: "projected"}
	if err := database.Create(&legacy).Error; err != nil {
		t.Fatalf("create legacy projection job: %v", err)
	}
	if err := Migrate(database); err != nil {
		t.Fatalf("migrate legacy DB: %v", err)
	}
	var migrated PullRequestProjectionJob
	if err := database.First(&migrated, legacy.ID).Error; err != nil {
		t.Fatalf("load migrated projection job: %v", err)
	}
	if migrated.Trigger != "pull_request_projection" || migrated.ActionGeneration != 0 || migrated.ActionIntentID != nil || migrated.DesiredAGSHeadSHA != "" || migrated.SuccessCommentedAt != nil {
		t.Fatalf("unsafe legacy projection job defaults: %#v", migrated)
	}
	if !database.Migrator().HasTable(&PullRequestProjectionJobAttempt{}) {
		t.Fatal("expected pull_request_projection_job_attempts after migrate")
	}
}
