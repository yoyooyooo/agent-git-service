package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
)

func nativeRunFixture(t *testing.T) (*Service, context.Context, db.User, db.Token) {
	t.Helper()
	database, e := db.Init("sqlite:" + filepath.Join(t.TempDir(), "client.db") + "?_foreign_keys=on")
	if e != nil {
		t.Fatal(e)
	}
	pool, e := database.DB()
	if e != nil {
		t.Fatal(e)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	actor := db.User{Login: "native-agent", Name: "Agent", Type: db.TypeUser, UserKind: db.UserKindAgent, Status: db.UserStatusActive}
	if e = database.Create(&actor).Error; e != nil {
		t.Fatal(e)
	}
	parent := db.Token{UserID: actor.ID, Value: "fixture-parent-token", Name: "fixture"}
	if e = database.Create(&parent).Error; e != nil {
		t.Fatal(e)
	}
	return &Service{DB: database, BaseURL: "https://primary.example.test"}, ContextWithUser(context.Background(), actor), actor, parent
}
func TestClientRunAssociationIsNotACommandPermissionGate(t *testing.T) {
	s, ctx, actor, parent := nativeRunFixture(t)
	first, e := s.StartClientRun(ctx, parent.Value, ClientRunInput{Context: ClientRunContext{Source: "runtime", Agent: "claimed-id", Task: "one", Run: "run-one"}})
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.StartClientRun(ctx, parent.Value, ClientRunInput{})
	if e != nil {
		t.Fatal(e)
	}
	if first.ID == second.ID || first.Token == second.Token || first.ActorID != actor.ID || first.AssociationStatus != "provisional" || second.AssociationStatus != "unlinked" {
		t.Fatalf("incorrect isolation or association: %+v %+v", first.ClientRunReceipt, second.ClientRunReceipt)
	}
	user, run, e := s.ResolveClientRun(ctx, first.Token)
	if e != nil || user.ID != actor.ID || run.ID != first.ID {
		t.Fatal(e)
	}
	if run.CredentialHash == first.Token || strings.Contains(run.ContextJSON, parent.Value) {
		t.Fatal("credential persisted in metadata")
	}
	validated, failure, e := s.ValidateAndResolveTokenDetailed(ctx, first.Token)
	if e != nil || failure != TokenValidationFailureNone || validated.ID != actor.ID {
		t.Fatal("replication identity differs", e)
	}
	if _, e = s.StartClientRun(ContextWithClientRun(ctx, run), first.Token, ClientRunInput{}); e == nil {
		t.Fatal("run token minted another run token")
	}
	if e = s.RevokeClientRun(ContextWithClientRun(ctx, run), second.ID); e == nil {
		t.Fatal("run token revoked another concurrent run")
	}
	if e = s.RevokeClientRun(ContextWithClientRun(ctx, run), first.ID); e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.ResolveClientRun(ctx, first.Token); e == nil {
		t.Fatal("revoked run still authenticated")
	}
	if _, _, e = s.ResolveClientRun(ctx, second.Token); e != nil {
		t.Fatal("revoking one run broke its sibling", e)
	}
}
func TestClientRunParentRevocationAndUserStateRemainAuthoritative(t *testing.T) {
	s, ctx, actor, parent := nativeRunFixture(t)
	expires := time.Now().UTC().Add(2 * time.Minute)
	if e := s.DB.Model(&parent).Update("expires_at", expires).Error; e != nil {
		t.Fatal(e)
	}
	issued, e := s.StartClientRun(ctx, parent.Value, ClientRunInput{TTLSeconds: 3600})
	if e != nil {
		t.Fatal(e)
	}
	if issued.ExpiresAt.After(expires) {
		t.Fatal("child outlived parent")
	}
	if e := s.DB.Model(&actor).Update("status", "suspended").Error; e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.ResolveClientRun(ctx, issued.Token); e == nil {
		t.Fatal("suspended actor allowed")
	}
	if e := s.DB.Model(&actor).Update("status", db.UserStatusActive).Error; e != nil {
		t.Fatal(e)
	}
	if e := s.DeleteTokenByID(ctx, actor.ID, parent.ID); e != nil {
		t.Fatal(e)
	}
	if _, _, e := s.ResolveClientRun(ctx, issued.Token); e == nil {
		t.Fatal("deleted parent allowed")
	}
	var violations []map[string]any
	if e := s.DB.Raw("PRAGMA foreign_key_check").Scan(&violations).Error; e != nil || len(violations) != 0 {
		t.Fatalf("orphan after parent revocation: %v %v", e, violations)
	}
}
func TestClientRunUnavailableMetadataDoesNotChangeActorOrGrantAccess(t *testing.T) {
	s, ctx, actor, parent := nativeRunFixture(t)
	issued, e := s.StartClientRun(ctx, parent.Value, ClientRunInput{Context: ClientRunContext{Task: "known-task"}, Source: &ClientRunSourceInput{Instance: "unavailable-source", Token: "synthetic-source-token"}})
	if e != nil || issued.ActorID != actor.ID || issued.AssociationStatus != "provisional" {
		t.Fatal("optional association blocked native identity", e)
	}
	other := db.User{Login: "other", Type: db.TypeUser}
	if e = s.DB.Create(&other).Error; e != nil {
		t.Fatal(e)
	}
	repo := db.Repository{OwnerID: other.ID, Name: "private", FullName: "other/private", Private: true, DefaultBranch: "main"}
	if e = s.DB.Create(&repo).Error; e != nil {
		t.Fatal(e)
	}
	user, run, e := s.ResolveClientRun(ctx, issued.Token)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetRepo(ContextWithClientRun(ContextWithUser(context.Background(), user), run), repo.FullName); e == nil {
		t.Fatal("run context granted repository access")
	}
	data, _ := json.Marshal(issued.ClientRunReceipt)
	if strings.Contains(string(data), parent.Value) || strings.Contains(string(data), "synthetic-source-token") {
		t.Fatal("source/native credential leaked")
	}
	if issued.ContextTrust != "caller_claimed_not_authority" {
		t.Fatal("unverified metadata labelled verified")
	}
}
