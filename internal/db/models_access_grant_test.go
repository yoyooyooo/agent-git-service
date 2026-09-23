package db_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestAccessGrantAuthorityFactsAreImmutableAndRowsCannotBeDeleted(t *testing.T) {
	svc, cleanup := testharness.NewService(t, testharness.ServiceConfig{})
	defer cleanup()
	now := time.Now().UTC()
	grant := db.AccessGrant{
		ID: "11111111-1111-4111-8111-111111111111", CredentialHash: strings.Repeat("a", 64), CredentialPrefix: "ags_grant_fixture",
		ActorUserID: 1, ExecutorUserID: 2, SnapshotID: "22222222-2222-4222-8222-222222222222",
		SourceInstanceID: "multica-mini", ExternalWorkspaceID: "workspace", ExternalAgentID: "agent", ExternalTaskID: "task", ExternalRunID: "run",
		RepositoryID: 1, RepositoryFullName: "example-owner/demo", TargetInstance: "primary-a",
		AgentSelectorOutcome: "default", DefaultPolicyClass: "default", PolicyClassOutcome: "default",
		EffectivePolicyClasses: []string{"default"}, RequestedOperations: []string{}, EffectiveOperations: []string{"repo.read"}, Warnings: []string{},
		DefaultBindingRevision: "binding-v1", ResourcePolicyRevision: "resource-v1", AuthorityRevision: "sha256:" + strings.Repeat("b", 64),
		CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	}
	if err := svc.DB.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.AccessGrant{}).Where("id = ?", grant.ID).Update("actor_user_id", 9).Error; err == nil {
		t.Fatal("immutable actor authority was updated")
	}
	used := now.Add(time.Minute)
	if err := svc.DB.Model(&db.AccessGrant{}).Where("id = ?", grant.ID).Update("last_used_at", used).Error; err != nil {
		t.Fatalf("mutable lifecycle update failed: %v", err)
	}
	if err := svc.DB.Delete(&grant).Error; err == nil {
		t.Fatal("access grant deletion was allowed")
	}
}

func TestAccessGrantInvocationFactsAreImmutableAndRowsCannotBeDeleted(t *testing.T) {
	svc, cleanup := testharness.NewService(t, testharness.ServiceConfig{})
	defer cleanup()
	now := time.Now().UTC()
	effectKey := "sha256:" + strings.Repeat("c", 64)
	invocation := db.AccessGrantInvocation{
		ID: "33333333-3333-4333-8333-333333333333", EffectKey: &effectKey,
		GrantID: "11111111-1111-4111-8111-111111111111", ActorUserID: 1, ExecutorUserID: 2,
		RepositoryID: 1, Repository: "example-owner/demo", Operation: "pr.merge", ConstraintsJSON: `{}`,
		AuthorityRevision: "sha256:" + strings.Repeat("d", 64), State: "planned", AuthorizationOutcome: "allowed",
		ProviderAttempt: "not_attempted", ProviderOutcome: "not_attempted", CreatedAt: now, UpdatedAt: now,
	}
	if err := svc.DB.Create(&invocation).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.AccessGrantInvocation{}).Where("id = ?", invocation.ID).Update("repository", "other/repo").Error; err == nil {
		t.Fatal("immutable invocation facts were updated")
	}
	if err := svc.DB.Model(&db.AccessGrantInvocation{}).Where("id = ?", invocation.ID).Updates(map[string]any{
		"state": "completed", "provider_outcome": "not_applicable", "updated_at": now.Add(time.Minute),
	}).Error; err != nil {
		t.Fatalf("mutable invocation lifecycle update failed: %v", err)
	}
	if err := svc.DB.Delete(&invocation).Error; err == nil {
		t.Fatal("access grant invocation deletion was allowed")
	}
}
