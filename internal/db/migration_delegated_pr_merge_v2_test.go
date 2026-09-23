package db

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type preV2DelegatedSession struct {
	ID        string `gorm:"primaryKey"`
	ExpiresAt time.Time
}

func (preV2DelegatedSession) TableName() string { return "delegated_agent_sessions" }

type preV2MergeIntent struct {
	ID               string    `gorm:"primaryKey"`
	IdempotencyKey   string    `gorm:"not null"`
	Action           string    `gorm:"not null"`
	State            string    `gorm:"not null"`
	PullRequestID    uint      `gorm:"not null"`
	RepositoryID     uint      `gorm:"not null"`
	AGSPRNumber      int       `gorm:"column:ags_pr_number;not null"`
	Repository       string    `gorm:"not null"`
	ForgejoRepo      string    `gorm:"not null"`
	ForgejoPRNumber  int       `gorm:"not null"`
	HeadRef          string    `gorm:"not null"`
	BaseRef          string    `gorm:"not null"`
	ExpectedHeadSHA  string    `gorm:"not null"`
	ExpectedBaseSHA  string    `gorm:"not null"`
	ExpectedLabels   string    `gorm:"not null"`
	PostLabels       string    `gorm:"not null"`
	PrincipalID      uint      `gorm:"not null"`
	RequestSource    string    `gorm:"not null;default:'ags_client'"`
	ExpiresInSeconds int64     `gorm:"not null;default:0"`
	ExpiresAt        time.Time `gorm:"not null"`
}

func (preV2MergeIntent) TableName() string { return "pull_request_action_intents" }

func TestMigrateDelegatedPRMergeV2IsAdditiveRepeatableAndLeavesHistoryUnproven(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "delegated-merge-v2.db")), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&preV2DelegatedSession{}, &preV2MergeIntent{}); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&preV2DelegatedSession{ID: "historical-session", ExpiresAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []preV2MergeIntent{{ID: "old-1", IdempotencyKey: "old-1", Action: "pr.rebase", State: "completed"}, {ID: "old-2", IdempotencyKey: "old-2", Action: "pr.rebase", State: "completed"}} {
		if err := database.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for run := 1; run <= 2; run++ {
		if err := MigrateDelegatedPRMergeV2(database); err != nil {
			t.Fatalf("migration run %d: %v", run, err)
		}
	}
	for _, field := range []string{"MergeDelegationID", "MergeDelegationFactsDigest", "MergeDelegationNotAfter"} {
		if !database.Migrator().HasColumn(&DelegatedAgentSession{}, field) {
			t.Fatalf("missing Session field %s", field)
		}
	}
	for _, field := range []string{"DelegationID", "DelegationConsumeReceiptID", "ProviderOutcome", "MergeMethod"} {
		if !database.Migrator().HasColumn(&PullRequestActionIntent{}, field) {
			t.Fatalf("missing intent field %s", field)
		}
	}
	definitions, err := delegatedMergeIndexDefinitions(database)
	if err != nil {
		t.Fatal(err)
	}
	if found, correct := exactDelegatedMergeIndexDefinition(definitions); !found || !correct {
		t.Fatalf("missing exact one-shot index: %#v", definitions)
	}
	var sessions []struct {
		ID                      string
		MergeDelegationID       string
		MergeDelegationNotAfter *time.Time
	}
	if err := database.Table("delegated_agent_sessions").Select("id", "merge_delegation_id", "merge_delegation_not_after").Scan(&sessions).Error; err != nil || len(sessions) != 1 || sessions[0].MergeDelegationID != "" || sessions[0].MergeDelegationNotAfter != nil {
		t.Fatalf("historical session was inferred: rows=%#v err=%v", sessions, err)
	}
	var intents []struct {
		ID                         string
		DelegationID               *string
		ProviderOutcome            string
		DelegationConsumeReceiptID string
	}
	if err := database.Table("pull_request_action_intents").Select("id", "delegation_id", "provider_outcome", "delegation_consume_receipt_id").Scan(&intents).Error; err != nil || len(intents) != 2 {
		t.Fatalf("historical intents changed: rows=%#v err=%v", intents, err)
	}
	for _, intent := range intents {
		if intent.DelegationID != nil || intent.ProviderOutcome != "" || intent.DelegationConsumeReceiptID != "" {
			t.Fatalf("historical intent gained synthetic v2 evidence: %#v", intent)
		}
	}
}

func TestMigrateDelegatedPRMergeV2FailsClosedOnWrongIndexArtifacts(t *testing.T) {
	for _, tc := range []struct{ name, ddl string }{
		{name: "non unique", ddl: "CREATE INDEX idx_pr_action_intent_merge_delegation ON pull_request_action_intents (delegation_id)"},
		{name: "wrong column", ddl: "CREATE UNIQUE INDEX idx_pr_action_intent_merge_delegation ON pull_request_action_intents (state)"},
		{name: "partial predicate", ddl: "CREATE UNIQUE INDEX idx_pr_action_intent_merge_delegation ON pull_request_action_intents (delegation_id) WHERE delegation_id IS NOT NULL"},
		{name: "expression", ddl: "CREATE UNIQUE INDEX idx_pr_action_intent_merge_delegation ON pull_request_action_intents (substr(delegation_id, 1, 8))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "wrong-index.db")), &gorm.Config{Logger: gormlogger.Discard})
			if err != nil {
				t.Fatal(err)
			}
			if err := database.AutoMigrate(&preV2DelegatedSession{}, &preV2MergeIntent{}); err != nil {
				t.Fatal(err)
			}
			if err := database.Migrator().AddColumn(&PullRequestActionIntent{}, "DelegationID"); err != nil {
				t.Fatal(err)
			}
			if err := database.Exec(tc.ddl).Error; err != nil {
				t.Fatal(err)
			}
			err = MigrateDelegatedPRMergeV2(database)
			if err == nil || !strings.Contains(err.Error(), "migration blocked") || !strings.Contains(err.Error(), "AGS did not alter") {
				t.Fatalf("error=%v", err)
			}
			var sql string
			if scanErr := database.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", delegatedPRMergeIntentIndexName).Scan(&sql).Error; scanErr != nil {
				t.Fatal(scanErr)
			}
			if sql == "" {
				t.Fatal("migration removed the wrong artifact")
			}
		})
	}
}

func TestFullMigrateHistoricalReplayAndExactIndexDefinition(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "full-migrate.db")), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&preV2DelegatedSession{}, &preV2MergeIntent{}); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(&preV2MergeIntent{ID: "historical", IdempotencyKey: "historical", Action: "pr.rebase", State: "completed"}).Error; err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if err := Migrate(database); err != nil {
			t.Fatalf("full migration run %d: %v", run, err)
		}
	}
	definitions, err := delegatedMergeIndexDefinitions(database)
	if err != nil {
		t.Fatal(err)
	}
	if found, correct := exactDelegatedMergeIndexDefinition(definitions); !found || !correct {
		t.Fatalf("exact unique index not proven: %#v", definitions)
	}
	var historical struct{ DelegationID *string }
	if err := database.Table(delegatedPRMergeIntentTable).Select("delegation_id").Where("id = ?", "historical").Scan(&historical).Error; err != nil || historical.DelegationID != nil {
		t.Fatalf("history gained authority: %#v err=%v", historical, err)
	}
}

func TestFullMigrateFailsClosedWithoutChangingWrongIndexArtifact(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "full-wrong-index.db")), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(database); err != nil {
		t.Fatal(err)
	}
	if err := database.Exec("DROP INDEX " + delegatedPRMergeIntentIndexName).Error; err != nil {
		t.Fatal(err)
	}
	wrongDDL := "CREATE INDEX " + delegatedPRMergeIntentIndexName + " ON " + delegatedPRMergeIntentTable + " (state)"
	if err := database.Exec(wrongDDL).Error; err != nil {
		t.Fatal(err)
	}
	err = Migrate(database)
	if err == nil || !strings.Contains(err.Error(), "migration blocked") {
		t.Fatalf("full Migrate error=%v", err)
	}
	var sql string
	if scanErr := database.Raw("SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?", delegatedPRMergeIntentIndexName).Scan(&sql).Error; scanErr != nil {
		t.Fatal(scanErr)
	}
	if !strings.Contains(strings.ToLower(sql), " on "+delegatedPRMergeIntentTable+" (state)") {
		t.Fatalf("full Migrate altered wrong artifact: %q", sql)
	}
}

func TestDelegatedMergeIndexCatalogProofRejectsEveryUnprovenSafetyBit(t *testing.T) {
	exact := delegatedMergeIndexDefinition{Name: delegatedPRMergeIntentIndexName, Table: delegatedPRMergeIntentTable,
		Columns: []string{delegatedPRMergeIntentColumn}, Unique: true, Valid: true, Ready: true, Visible: true,
		NoPrefix: true, NoExpression: true, NoPredicate: true, Proven: true}
	if found, ok := exactDelegatedMergeIndexDefinition([]delegatedMergeIndexDefinition{exact}); !found || !ok {
		t.Fatal("exact native catalog definition rejected")
	}
	for name, mutate := range map[string]func(*delegatedMergeIndexDefinition){
		"wrong table identity": func(row *delegatedMergeIndexDefinition) { row.Table = "other_table" },
		"non unique":           func(row *delegatedMergeIndexDefinition) { row.Unique = false },
		"postgres invalid":     func(row *delegatedMergeIndexDefinition) { row.Valid = false },
		"postgres not ready":   func(row *delegatedMergeIndexDefinition) { row.Ready = false },
		"mysql invisible":      func(row *delegatedMergeIndexDefinition) { row.Visible = false },
		"mysql prefix":         func(row *delegatedMergeIndexDefinition) { row.NoPrefix = false },
		"expression":           func(row *delegatedMergeIndexDefinition) { row.NoExpression = false },
		"predicate partial":    func(row *delegatedMergeIndexDefinition) { row.NoPredicate = false },
		"metadata unproven":    func(row *delegatedMergeIndexDefinition) { row.Proven = false },
		"wrong ordered column": func(row *delegatedMergeIndexDefinition) { row.Columns = []string{"state"} },
	} {
		t.Run(name, func(t *testing.T) {
			wrong := exact
			wrong.Columns = append([]string(nil), exact.Columns...)
			mutate(&wrong)
			if found, ok := exactDelegatedMergeIndexDefinition([]delegatedMergeIndexDefinition{wrong}); !found || ok {
				t.Fatalf("unproven catalog artifact accepted: %#v", wrong)
			}
		})
	}
}
