package db

import (
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type preBoundaryPullRequestActionIntent struct {
	ID               string  `gorm:"primaryKey;size:64"`
	IdempotencyKey   string  `gorm:"size:255;not null;uniqueIndex:idx_pr_action_intent_idempotency"`
	Action           string  `gorm:"size:64;not null;index"`
	State            string  `gorm:"size:32;not null;index"`
	PullRequestID    uint    `gorm:"not null;index"`
	RepositoryID     uint    `gorm:"not null;index"`
	AGSPRNumber      int     `gorm:"column:ags_pr_number;not null"`
	Repository       string  `gorm:"size:512;not null"`
	ForgejoRepo      string  `gorm:"size:512;not null"`
	ForgejoPRNumber  int     `gorm:"not null"`
	HeadRef          string  `gorm:"size:255;not null"`
	BaseRef          string  `gorm:"size:255;not null"`
	ExpectedHeadSHA  string  `gorm:"size:40;not null"`
	ExpectedBaseSHA  string  `gorm:"size:40;not null"`
	ExpectedLabels   string  `gorm:"type:text;not null"`
	PostLabels       string  `gorm:"type:text;not null"`
	PrincipalID      uint    `gorm:"not null"`
	AgentSessionID   *string `gorm:"type:char(36);index"`
	TeamIdentityID   string  `gorm:"size:255"`
	PolicyClass      string  `gorm:"size:255"`
	MembershipEpoch  int64
	AuthorityRev     string    `gorm:"size:255"`
	ExpiresInSeconds int64     `gorm:"not null;default:0"`
	ResultSHA        string    `gorm:"size:40"`
	FailureCode      string    `gorm:"size:96"`
	FailureSummary   string    `gorm:"size:512"`
	ExpiresAt        time.Time `gorm:"not null;index"`
	DispatchedAt     *time.Time
	AcceptedAt       *time.Time
	FinishedAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (preBoundaryPullRequestActionIntent) TableName() string { return "pull_request_action_intents" }

func TestMigratePullRequestActionBoundaryReceiptIsAdditiveRepeatableAndDoesNotBackfill(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy-action.db")), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&preBoundaryPullRequestActionIntent{}); err != nil {
		t.Fatal(err)
	}
	sessionID := "11111111-1111-4111-8111-111111111111"
	rows := []preBoundaryPullRequestActionIntent{
		{ID: "legacy-active", IdempotencyKey: "legacy-active", Action: "pr.rebase", State: "dispatching", PullRequestID: 1, RepositoryID: 2, AGSPRNumber: 3, Repository: "owner/repo", ForgejoRepo: "forgejo/repo", ForgejoPRNumber: 4, HeadRef: "agent/a", BaseRef: "main", ExpectedHeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpectedBaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ExpectedLabels: "[]", PostLabels: "[]", PrincipalID: 5, AgentSessionID: &sessionID, AuthorityRev: "preserved-active", ExpiresAt: time.Now().UTC().Add(time.Hour)},
		{ID: "legacy-terminal", IdempotencyKey: "legacy-terminal", Action: "pr.rebase", State: "completed", PullRequestID: 6, RepositoryID: 7, AGSPRNumber: 8, Repository: "owner/other", ForgejoRepo: "forgejo/other", ForgejoPRNumber: 9, HeadRef: "agent/b", BaseRef: "main", ExpectedHeadSHA: "cccccccccccccccccccccccccccccccccccccccc", ExpectedBaseSHA: "dddddddddddddddddddddddddddddddddddddddd", ExpectedLabels: "[]", PostLabels: "[]", PrincipalID: 10, AuthorityRev: "preserved-terminal", ExpiresAt: time.Now().UTC().Add(time.Hour)},
	}
	if err := gdb.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if err := MigratePullRequestActionBoundaryReceipt(gdb); err != nil {
			t.Fatalf("migration run %d: %v", run, err)
		}
	}
	for _, column := range []string{"BoundaryProtocol", "BoundaryReceiptID", "ProviderEffectStatus"} {
		if !gdb.Migrator().HasColumn(&PullRequestActionIntent{}, column) {
			t.Fatalf("missing additive column %s", column)
		}
	}
	if !gdb.Migrator().HasIndex(&PullRequestActionIntent{}, "idx_pr_action_intent_boundary_receipt") {
		t.Fatal("missing Boundary Receipt locator index")
	}
	var migrated []PullRequestActionIntent
	if err := gdb.Order("id ASC").Find(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if len(migrated) != 2 {
		t.Fatalf("rows=%d", len(migrated))
	}
	for _, row := range migrated {
		if row.BoundaryProtocol != "" || row.BoundaryReceiptID != nil || row.ProviderEffectStatus != "" {
			t.Fatalf("historical row was inferred/backfilled: %#v", row)
		}
		if row.ID == "legacy-active" && row.AuthorityRev != "preserved-active" {
			t.Fatalf("active row changed: %#v", row)
		}
		if row.ID == "legacy-terminal" && row.AuthorityRev != "preserved-terminal" {
			t.Fatalf("terminal row changed: %#v", row)
		}
	}
}
