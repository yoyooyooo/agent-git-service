package db

import (
	"fmt"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type delegatedSessionBeforeActorProjection struct {
	ID                  string    `gorm:"type:char(36);primaryKey"`
	CredentialHash      string    `gorm:"size:64;not null;uniqueIndex"`
	CredentialPrefix    string    `gorm:"size:32;not null;index"`
	PrincipalUserID     uint      `gorm:"not null;index"`
	Issuer              string    `gorm:"size:255;not null;uniqueIndex:idx_delegated_session_assertion,priority:1"`
	AssertionVersion    int       `gorm:"not null"`
	AssertionPurpose    string    `gorm:"size:64;not null;uniqueIndex:idx_delegated_session_assertion,priority:2"`
	AssertionJTI        string    `gorm:"size:255;not null;uniqueIndex:idx_delegated_session_assertion,priority:3"`
	AssertionAudience   string    `gorm:"size:255;not null"`
	IssuerWorkspaceID   string    `gorm:"size:255;not null;index"`
	ExternalAgentID     string    `gorm:"size:255;not null;index"`
	ExternalAgentName   string    `gorm:"size:255;not null"`
	ExternalTaskID      string    `gorm:"size:255;not null;index"`
	TargetInstance      string    `gorm:"size:255;not null"`
	RepositoryID        uint      `gorm:"not null;index"`
	GrantedCapabilities []string  `gorm:"serializer:json;not null"`
	PolicyVersion       string    `gorm:"size:255;not null"`
	PolicySnapshotHash  string    `gorm:"size:64;not null"`
	CreatedAt           time.Time `gorm:"not null;index"`
	ExpiresAt           time.Time `gorm:"not null;index"`
}

func (delegatedSessionBeforeActorProjection) TableName() string { return "delegated_agent_sessions" }

func TestMigrateBackfillsDelegatedBySnapshotWithoutLosingSession(t *testing.T) {
	dsn := fmt.Sprintf("file:delegated-actor-migration-%d?mode=memory&cache=shared", time.Now().UnixNano())
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := gdb.AutoMigrate(&User{}, &AgentBinding{}, &Repository{}, &delegatedSessionBeforeActorProjection{}); err != nil {
		t.Fatal(err)
	}
	human := User{Login: "operator", Name: "Operator", Type: TypeUser, UserKind: UserKindHuman, Status: UserStatusActive}
	principal := User{Login: "automation-principal", Name: "Automation Principal", Type: TypeUser, UserKind: UserKindAgent, Status: UserStatusActive}
	if err := gdb.Create(&human).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&principal).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Create(&AgentBinding{HumanUserID: human.ID, AgentUserID: principal.ID}).Error; err != nil {
		t.Fatal(err)
	}
	repo := Repository{Name: "project-kit", FullName: "operator/project-kit", OwnerID: human.ID, DefaultBranch: "main"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	before := delegatedSessionBeforeActorProjection{
		ID: "session-before-actor", CredentialHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CredentialPrefix: "prefix",
		PrincipalUserID: principal.ID, Issuer: "multica", AssertionVersion: 1, AssertionPurpose: "ags_session_exchange",
		AssertionJTI: "migration-jti", AssertionAudience: "urn:ags:workload-session-exchange:v1", IssuerWorkspaceID: "workspace-1",
		ExternalAgentID: "agent-1", ExternalAgentName: "historical-agent-name", ExternalTaskID: "task-1", TargetInstance: "primary-a",
		RepositoryID: repo.ID, GrantedCapabilities: []string{"pr:create", "repo:read"}, PolicyVersion: "2026-07-14.1",
		PolicySnapshotHash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	}
	if err := gdb.Create(&before).Error; err != nil {
		t.Fatal(err)
	}
	unboundPrincipal := User{Login: "unbound-agent", Name: "Unbound Agent", Type: TypeUser, UserKind: UserKindAgent, Status: UserStatusActive}
	if err := gdb.Create(&unboundPrincipal).Error; err != nil {
		t.Fatal(err)
	}
	unbound := before
	unbound.ID = "session-before-actor-unbound"
	unbound.CredentialHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	unbound.CredentialPrefix = "unbound-prefix"
	unbound.PrincipalUserID = unboundPrincipal.ID
	unbound.AssertionJTI = "migration-unbound-jti"
	if err := gdb.Create(&unbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(gdb); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(gdb); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if !gdb.Migrator().HasColumn(&DelegatedAgentSession{}, "ExpiryAuditedAt") ||
		!gdb.Migrator().HasColumn(&DelegatedAgentSession{}, "BindingID") ||
		!gdb.Migrator().HasColumn(&DelegatedAgentSession{}, "OperationName") ||
		!gdb.Migrator().HasColumn(&DelegatedAgentSession{}, "ExternalSquadID") ||
		!gdb.Migrator().HasColumn(&DelegatedAgentSession{}, "AccessGrantID") ||
		!gdb.Migrator().HasColumn(&DelegatedAgentSession{}, "AccessGrantAuthorityRevision") ||
		!gdb.Migrator().HasColumn(&DelegatedAgentSession{}, "ActorUserID") ||
		!gdb.Migrator().HasColumn(&PullRequestMulticaLink{}, "AccessGrantID") ||
		!gdb.Migrator().HasColumn(&PullRequestMulticaLink{}, "SourceSnapshotID") {
		t.Fatal("delegated session lifecycle/principal-session columns were not migrated")
	}
	var after DelegatedAgentSession
	if err := gdb.First(&after, "id = ?", before.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ExternalAgentName != "historical-agent-name" || after.ExternalTaskID != "task-1" || after.ExternalSquadID != "" {
		t.Fatalf("session workload snapshot changed: %#v", after)
	}
	if after.BindingID != "" || after.OperationName != "" || after.ContractRevision != "" {
		t.Fatalf("legacy active Session was falsely upgraded into principal/session-v2 authority: %#v", after)
	}
	if after.PrincipalLogin != "automation-principal" {
		t.Fatalf("principal login snapshot not backfilled: %q", after.PrincipalLogin)
	}
	if after.DelegatedByUserID == nil || *after.DelegatedByUserID != human.ID || after.DelegatedByLogin != "operator" || after.DelegatedBySource != "migration_backfill" {
		t.Fatalf("delegator snapshot not backfilled: id=%v login=%q source=%q", after.DelegatedByUserID, after.DelegatedByLogin, after.DelegatedBySource)
	}
	var unboundAfter DelegatedAgentSession
	if err := gdb.First(&unboundAfter, "id = ?", unbound.ID).Error; err != nil {
		t.Fatal(err)
	}
	if unboundAfter.PrincipalLogin != "unbound-agent" || unboundAfter.DelegatedByUserID != nil || unboundAfter.DelegatedByLogin != "" || unboundAfter.DelegatedBySource != "principal_only" {
		t.Fatalf("unbound principal migration mismatch: %#v", unboundAfter)
	}
}
