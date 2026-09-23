package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestReplicationRegistrationDeniesDelegatedAndRecycledRowObservation(t *testing.T) {
	svc, executor, repo := setupAccessGrantService(t)
	ctx := service.ContextWithUser(context.Background(), executor)
	before, err := svc.ObserveReplicationRegistration(ctx, "primary", repo.FullName)
	if err != nil {
		t.Fatal(err)
	}
	delegated := service.ContextWithDelegatedSession(ctx, db.DelegatedAgentSession{ID: "test-delegated-session"})
	if _, err := svc.RegisterReplicationRepository(delegated, "primary", repo.FullName, before.Request()); !errors.Is(err, service.ErrForbidden) {
		t.Fatal("delegated admin elevated", err)
	}
	if _, err := svc.ProvisionReplicationIdentity(delegated, "primary", repo.FullName, "repo"); !errors.Is(err, service.ErrForbidden) {
		t.Fatal("alternate provisioning elevated", err)
	}
	// A recycled numeric ID is insufficient: creation time must match too.
	if err := svc.DB.Model(&db.Repository{}).Where("id = ?", repo.ID).Update("created_at", before.CreatedAt.Add(time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterReplicationRepository(ctx, "primary", repo.FullName, before.Request()); !errors.Is(err, service.ErrReplicationRegistrationConflict) {
		t.Fatal("recycled-row fact accepted", err)
	}
	var current db.Repository
	if err := svc.DB.First(&current, repo.ID).Error; err != nil || current.GitStorageID != nil {
		t.Fatal("denial changed storage identity", err)
	}
}
