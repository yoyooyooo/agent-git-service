package db

import (
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestMigrateAddsAuthorityBoundaryReceiptLedgerAdditivelyAndIsRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority-boundary-receipt.db")
	gdb, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if sqlDB, err := gdb.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	if err := gdb.AutoMigrate(&User{}); err != nil {
		t.Fatal(err)
	}
	legacy := User{Login: "preserved", Type: TypeUser, Status: UserStatusActive}
	if err := gdb.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if err := Migrate(gdb); err != nil {
			t.Fatalf("migration run %d: %v", run, err)
		}
	}
	for _, column := range []string{"ReceiptID", "Kind", "ParentDigest", "AuthorityEpoch", "SnapshotDigest", "SourceRevision", "ClaimLimit", "Payload", "CreatedByUserID", "CreatedAt"} {
		if !gdb.Migrator().HasColumn(&AuthorityBoundaryReceipt{}, column) {
			t.Fatalf("receipt column %s missing", column)
		}
	}
	if !gdb.Migrator().HasColumn(&PullRequestActionIntent{}, "AgentSessionID") || !gdb.Migrator().HasColumn(&PullRequestActionIntent{}, "ExpiresInSeconds") ||
		!gdb.Migrator().HasColumn(&PullRequestProjectionJob{}, "AgentSessionID") ||
		!gdb.Migrator().HasColumn(&PullRequestProjectionJob{}, "ActionIntentID") ||
		!gdb.Migrator().HasIndex(&PullRequestProjectionJob{}, "idx_pr_projection_job_action_intent") {
		t.Fatal("delegated effect intent/job Session or exact action-intent provenance authority is missing")
	}
	var readback User
	if err := gdb.First(&readback, legacy.ID).Error; err != nil || readback.Login != legacy.Login {
		t.Fatalf("legacy row not preserved: row=%#v err=%v", readback, err)
	}
}
