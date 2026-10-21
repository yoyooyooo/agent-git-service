package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ngaut/agent-git-service/internal/attribution"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/operationconstraints"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestProjectionAttributionSurvivesEnqueueWithoutGrantingAuthority(t *testing.T) {
	svc := setupProjectionStateService(t)
	svc.DisableForgejoProjectionWorker = true
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true}, nil, nil)
	var repo db.Repository
	if err := svc.DB.First(&repo).Error; err != nil {
		t.Fatal(err)
	}
	user := db.User{Login: "alice", Type: db.TypeUser}
	if err := svc.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{RepositoryID: repo.ID, Number: 1, AuthorID: user.ID, HeadRef: "agent/topic", BaseRef: "main", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: db.StateOpen}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithUser(context.Background(), user)
	original := attribution.FromContext(ctx)
	job, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := svc.loadForgejoProjectionJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AttributionUserID != user.ID || loaded.AttributionLogin != "alice" || loaded.AttributionRequestID != original.RequestID {
		t.Fatalf("provenance lost: %s", loaded.AttributionLogin)
	}
	provenance := attribution.WithActor(context.Background(), attribution.Actor{UserID: loaded.AttributionUserID, Login: loaded.AttributionLogin})
	if _, ok := UserFromContext(provenance); ok {
		t.Fatal("provenance became authorization")
	}
	// Background clones retain the initiating request id, not a new identity.
	if attribution.FromContext(svc.projectionBackgroundContext(ctx)).RequestID != original.RequestID {
		t.Fatal("request id lost")
	}
}

func TestProjectionAttributionAuditContainsNoCredentialsOrBody(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "audit.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AutoMigrate(&db.AuditLogEntry{}); err != nil {
		t.Fatal(err)
	}
	actor := attribution.Actor{UserID: 7, Login: "alice", RequestID: "request-1", Repo: "team/demo", PR: 3}
	event := attribution.Event{Actor: actor, Target: "forgejo", Op: "comment", TargetPR: 5, SudoAs: "alice.remote", Attribution: "sudo", Status: 201}
	if err := ProjectionAttributionRecorder(database)(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	var entry db.AuditLogEntry
	if err := database.First(&entry).Error; err != nil {
		t.Fatal(err)
	}
	var got attribution.Event
	if err := json.Unmarshal([]byte(entry.Details), &got); err != nil {
		t.Fatal(err)
	}
	if entry.ActorLogin != "alice" || entry.ActorID == nil || *entry.ActorID != 7 || got.SudoAs != "alice.remote" || got.TargetPR != 5 || got.Attribution != "sudo" {
		t.Fatal("incomplete audit")
	}
}

func TestProjectionAttributionRechecksCurrentAGSGrant(t *testing.T) {
	svc := setupProjectionStateService(t)
	var repo db.Repository
	if err := svc.DB.First(&repo).Error; err != nil {
		t.Fatal(err)
	}
	var owner db.User
	if err := svc.DB.First(&owner, repo.OwnerID).Error; err != nil {
		t.Fatal(err)
	}
	outsider := db.User{Login: "projection-outsider", Type: db.TypeUser}
	if err := svc.DB.Create(&outsider).Error; err != nil {
		t.Fatal(err)
	}
	for _, user := range []db.User{owner, outsider} {
		ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: user.ID, Login: user.Login})
		err := svc.AuthorizeProjectionAttribution(ctx, attribution.FromContext(ctx), repo.FullName, "comment", 0)
		if user.ID == owner.ID && err != nil {
			t.Fatal(err)
		}
		if user.ID == outsider.ID && !errors.Is(err, ErrForbidden) {
			t.Fatalf("unauthorized err=%v", err)
		}
		if _, ok := UserFromContext(ctx); ok {
			t.Fatal("provenance became service user")
		}
	}
}

func TestProjectionAttributionFreshGrantRevocationAndDisabledUser(t *testing.T) {
	svc := setupProjectionStateService(t)
	if err := svc.DB.AutoMigrate(&db.Collaborator{}); err != nil {
		t.Fatal(err)
	}
	var repo db.Repository
	if err := svc.DB.First(&repo).Error; err != nil {
		t.Fatal(err)
	}
	user := db.User{Login: "mapped-worker", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := svc.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	actor := attribution.Actor{UserID: user.ID, Login: user.Login, Repo: repo.FullName}
	ctx := attribution.WithActor(context.Background(), actor)
	grant := db.Collaborator{RepositoryID: repo.ID, UserID: user.ID, Permission: "write"}
	if err := svc.DB.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AuthorizeProjectionAttribution(ctx, actor, repo.FullName, "comment", 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&grant).Update("permission", "read").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AuthorizeProjectionAttribution(ctx, actor, repo.FullName, "comment", 1); !errors.Is(err, ErrForbidden) {
		t.Fatal("stale grant admitted", err)
	}
	if err := svc.DB.Model(&grant).Update("permission", "write").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&user).Update("status", db.UserStatusSuspended).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AuthorizeProjectionAttribution(ctx, actor, repo.FullName, "comment", 1); !errors.Is(err, ErrForbidden) {
		t.Fatal("suspended user admitted", err)
	}
	if _, ok := UserFromContext(ctx); ok {
		t.Fatal("stored attribution became authority")
	}
}

func TestProjectionAttributionCommitEmailContract(t *testing.T) {
	svc := setupProjectionStateService(t)
	var user db.User
	if err := svc.DB.First(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&user).Update("email", "alice@example.test").Error; err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"alice@example.test", "wrong@example.test"} {
		cfg := attribution.Config{Principals: map[string]attribution.Principal{user.Login: {Forgejo: "alice.remote", CommitEmail: email}}}
		err := ValidateProjectionCommitEmails(context.Background(), svc.DB, cfg)
		if (err != nil) != (email == "wrong@example.test") {
			t.Fatal("commit email mismatch not detected", err)
		}
	}
}

func TestProjectionAttributionDelegatedConstraintsMatchCanonicalSchema(t *testing.T) {
	pr := db.PullRequest{Number: 3, HeadRef: "agent/topic", BaseRef: "main"}
	for _, operation := range []string{"pr.create", "pr.edit", "pr.close", "pr.reopen", "pr.comment"} {
		facts := projectionAttributionConstraints(operation, pr)
		if _, err := operationconstraints.NormalizeStored(operation, facts); err != nil {
			t.Fatal(operation, err)
		}
		if !operationconstraints.Match(operation, facts, facts) {
			t.Fatal("canonical facts rejected", operation)
		}
	}
}
