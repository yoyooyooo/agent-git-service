package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ngaut/agent-git-service/internal/attribution"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
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
