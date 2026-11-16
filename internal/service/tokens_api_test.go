package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestCreateUserToken_Success(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	u := db.User{Login: "token-user", Type: db.TypeUser}
	if err := svc.DB.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	exp := time.Now().Add(24 * time.Hour)
	tok, err := svc.CreateUserToken(context.Background(), u.ID, "ci-token", &exp)
	if err != nil {
		t.Fatalf("CreateUserToken: %v", err)
	}
	if tok.ID == 0 {
		t.Fatal("expected token ID to be set")
	}
	if tok.Value == "" {
		t.Fatal("expected token value to be set")
	}
	if tok.Name != "ci-token" {
		t.Fatalf("expected token name 'ci-token', got %q", tok.Name)
	}
	if tok.ExpiresAt == nil || !tok.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("expected expires_at in the future, got %v", tok.ExpiresAt)
	}

	var persisted db.Token
	if err := svc.DB.First(&persisted, "id = ?", tok.ID).Error; err != nil {
		t.Fatalf("load token: %v", err)
	}
	if persisted.Name != "ci-token" {
		t.Fatalf("expected persisted name 'ci-token', got %q", persisted.Name)
	}
}

func TestCreateUserToken_InvalidName(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	u := db.User{Login: "token-user", Type: db.TypeUser}
	if err := svc.DB.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	_, err := svc.CreateUserToken(context.Background(), u.ID, "   ", nil)
	if !errors.Is(err, service.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestCreateUserToken_Expired(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	u := db.User{Login: "token-user", Type: db.TypeUser}
	if err := svc.DB.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	exp := time.Now().Add(-1 * time.Hour)
	_, err := svc.CreateUserToken(context.Background(), u.ID, "ci-token", &exp)
	if !errors.Is(err, service.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestListTokens_Scoped(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	u1 := db.User{Login: "token-user-1", Type: db.TypeUser}
	u2 := db.User{Login: "token-user-2", Type: db.TypeUser}
	if err := svc.DB.Create(&u1).Error; err != nil {
		t.Fatalf("create user1: %v", err)
	}
	if err := svc.DB.Create(&u2).Error; err != nil {
		t.Fatalf("create user2: %v", err)
	}
	if err := svc.DB.Create(&db.Token{UserID: u1.ID, Value: "tok-1"}).Error; err != nil {
		t.Fatalf("create token1: %v", err)
	}
	if err := svc.DB.Create(&db.Token{UserID: u2.ID, Value: "tok-2"}).Error; err != nil {
		t.Fatalf("create token2: %v", err)
	}

	tokens, err := svc.ListTokens(context.Background(), u1.ID)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}
	if tokens[0].Value != "tok-1" {
		t.Fatalf("expected token value 'tok-1', got %q", tokens[0].Value)
	}
}

func TestDeleteToken_Scoped(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	u1 := db.User{Login: "token-user-1", Type: db.TypeUser}
	u2 := db.User{Login: "token-user-2", Type: db.TypeUser}
	if err := svc.DB.Create(&u1).Error; err != nil {
		t.Fatalf("create user1: %v", err)
	}
	if err := svc.DB.Create(&u2).Error; err != nil {
		t.Fatalf("create user2: %v", err)
	}

	tok := db.Token{UserID: u2.ID, Value: "tok-2"}
	if err := svc.DB.Create(&tok).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}

	err := svc.DeleteTokenByID(context.Background(), u1.ID, tok.ID)
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("expected ErrNotFound deleting other user's token, got %v", err)
	}

	err = svc.DeleteTokenByValue(context.Background(), u1.ID, tok.Value)
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("expected ErrNotFound deleting other user's token by value, got %v", err)
	}
}

func TestDeleteToken_RefusesProtectedValues(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	u := db.User{Login: "admin-user", Type: db.TypeUser}
	if err := svc.DB.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	admin := db.Token{UserID: u.ID, Value: "shared-admin-token"}
	if err := svc.DB.Create(&admin).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}
	other := db.Token{UserID: u.ID, Name: "other", Value: "other-token"}
	if err := svc.DB.Create(&other).Error; err != nil {
		t.Fatalf("create token: %v", err)
	}
	svc.ProtectedTokenValues = []string{"", "shared-admin-token"}

	for name, del := range map[string]func() error{
		"value": func() error { return svc.DeleteTokenByValue(context.Background(), u.ID, " shared-admin-token ") },
		"id":    func() error { return svc.DeleteTokenByID(context.Background(), u.ID, admin.ID) },
	} {
		err := del()
		if !errors.Is(err, service.ErrProtectedToken) || !errors.Is(err, service.ErrForbidden) {
			t.Fatalf("%s: expected ErrProtectedToken (ErrForbidden), got %v", name, err)
		}
	}
	var count int64
	svc.DB.Model(&db.Token{}).Where("id = ?", admin.ID).Count(&count)
	if count != 1 {
		t.Fatal("protected token was deleted")
	}

	if err := svc.DeleteTokenByID(context.Background(), u.ID, other.ID); err != nil {
		t.Fatalf("unprotected delete: %v", err)
	}
	// Unknown ids still report not found rather than protected.
	if err := svc.DeleteTokenByID(context.Background(), u.ID, other.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for deleted token, got %v", err)
	}
}
