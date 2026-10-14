package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/testharness/testdb"
)

func TestProjectionObservedBaseCASOnTiDB(t *testing.T) {
	gdb, cleanup := testdb.Open(t, testdb.Options{Prefix: "projection_base_cas"})
	defer cleanup()
	if err := gdb.AutoMigrate(&db.User{}, &db.Repository{}, &db.Label{}, &db.PullRequest{}, &db.PullRequestProjection{}); err != nil {
		t.Fatal(err)
	}
	owner := db.User{Login: "example-owner", Type: "User"}
	if err := gdb.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{OwnerID: owner.ID, Name: "demo", FullName: "example-owner/demo", DefaultBranch: "main"}
	if err := gdb.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{Number: 1, RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: owner.ID,
		Title: "observed base", State: db.StateOpen, HeadRef: "agent/stack", HeadSHA: strings.Repeat("a", 40), BaseRef: "main"}
	if err := gdb.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	projection := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
		ExternalRepo: "forgejo/demo", ExternalNumber: 42, SourceBranch: pr.HeadRef, TargetBranch: "agent/old-base",
		State: ProjectionStateOpen, LastSyncedSHA: pr.HeadSHA}
	if err := gdb.Create(&projection).Error; err != nil {
		t.Fatal(err)
	}
	provider := forgejointegration.PullRequestSnapshot{Number: 42, State: db.StateOpen, HeadRef: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef}
	svc := &Service{DB: gdb}
	after, err := svc.reconcileObservedForgejoProjectionBase(context.Background(), pr, projection, provider, "forgejo/demo")
	if err != nil || after.TargetBranch != "main" {
		t.Fatalf("TiDB observed base update failed: base=%s err=%v", after.TargetBranch, err)
	}
	var stored db.PullRequestProjection
	if err := gdb.First(&stored, projection.ID).Error; err != nil || stored.TargetBranch != "main" || stored.LastSyncedSHA != projection.LastSyncedSHA {
		t.Fatalf("unexpected TiDB persisted projection: base=%s err=%v", stored.TargetBranch, err)
	}
	// The old mapping snapshot is no longer valid even if its provider read was
	// correct. Replaying it must not report another successful CAS update.
	if _, err := svc.reconcileObservedForgejoProjectionBase(context.Background(), pr, projection, provider, "forgejo/demo"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("TiDB accepted stale mapping snapshot: %v", err)
	}
}
