package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

// Fixture values only. These tests never read host credentials or call a live AGS API.
const (
	fixtureProtectedAgentToken   = "fixture-admin-token-as-agent-cred"
	fixtureUnprotectedAgentToken = "fixture-agent-ordinary-token"
	fixtureSiblingAgentToken     = "fixture-agent-sibling-token"
)

func seedBoundAgentWithToken(t *testing.T, svc *service.Service, humanLogin, agentLogin, tokenName, tokenValue string) (db.User, db.User, db.Token) {
	t.Helper()
	human := db.User{Login: humanLogin, Name: humanLogin, Type: db.TypeUser, UserKind: db.UserKindHuman}
	if err := svc.DB.Create(&human).Error; err != nil {
		t.Fatalf("create human: %v", err)
	}
	agent := db.User{Login: agentLogin, Name: agentLogin, Type: db.TypeUser, UserKind: db.UserKindAgent}
	if err := svc.DB.Create(&agent).Error; err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := svc.DB.Create(&db.AgentBinding{HumanUserID: human.ID, AgentUserID: agent.ID}).Error; err != nil {
		t.Fatalf("create binding: %v", err)
	}
	tok := db.Token{UserID: agent.ID, Name: tokenName, Value: tokenValue}
	if err := svc.DB.Create(&tok).Error; err != nil {
		t.Fatalf("create fixture token: %v", err)
	}
	return human, agent, tok
}

func countAgentTokens(t *testing.T, svc *service.Service, agentID, tokenID uint) int64 {
	t.Helper()
	var count int64
	if err := svc.DB.Model(&db.Token{}).Where("user_id = ? AND id = ?", agentID, tokenID).Count(&count).Error; err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	return count
}

func TestResetAgentToken_RefusesProtectedLongLivedCredential(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	human, agent, tok := seedBoundAgentWithToken(t, svc, "reset-human-protected", "reset-agent-protected", "agent", fixtureProtectedAgentToken)
	svc.ProtectedTokenValues = []string{"", fixtureProtectedAgentToken}

	_, err := svc.ResetAgentToken(context.Background(), human.ID, agent.Login)
	if !errors.Is(err, service.ErrProtectedToken) || !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("ResetAgentToken error = %v, want ErrProtectedToken (ErrForbidden)", err)
	}
	if countAgentTokens(t, svc, agent.ID, tok.ID) != 1 {
		t.Fatal("protected long-lived agent token was deleted")
	}
	var issued int64
	if err := svc.DB.Model(&db.Token{}).Where("user_id = ? AND name = ?", agent.ID, "reset").Count(&issued).Error; err != nil {
		t.Fatalf("count issued tokens: %v", err)
	}
	if issued != 0 {
		t.Fatalf("issued %d reset tokens, want 0", issued)
	}
}

func TestResetAgentToken_RefusesWhenAnyExistingTokenIsProtected(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	human, agent, protected := seedBoundAgentWithToken(t, svc, "reset-human-mixed", "reset-agent-mixed", "agent", fixtureProtectedAgentToken)
	sibling := db.Token{UserID: agent.ID, Name: "sibling", Value: fixtureSiblingAgentToken}
	if err := svc.DB.Create(&sibling).Error; err != nil {
		t.Fatalf("create sibling token: %v", err)
	}
	svc.ProtectedTokenValues = []string{fixtureProtectedAgentToken}

	_, err := svc.ResetAgentToken(context.Background(), human.ID, agent.Login)
	if !errors.Is(err, service.ErrProtectedToken) {
		t.Fatalf("ResetAgentToken error = %v, want ErrProtectedToken", err)
	}
	if countAgentTokens(t, svc, agent.ID, protected.ID) != 1 || countAgentTokens(t, svc, agent.ID, sibling.ID) != 1 {
		t.Fatal("mixed reset deleted tokens; the whole reset must be refused")
	}
}

func TestResetAgentToken_StillResetsUnprotectedTokens(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	human, agent, tok := seedBoundAgentWithToken(t, svc, "reset-human-ok", "reset-agent-ok", "agent", fixtureUnprotectedAgentToken)
	svc.ProtectedTokenValues = []string{fixtureProtectedAgentToken}

	issued, err := svc.ResetAgentToken(context.Background(), human.ID, agent.Login)
	if err != nil {
		t.Fatalf("ResetAgentToken: %v", err)
	}
	if issued.ID == 0 || issued.UserID != agent.ID || issued.Name != "reset" {
		t.Fatalf("unexpected issued token id=%d user=%d name=%q", issued.ID, issued.UserID, issued.Name)
	}
	if issued.Value == "" || issued.Value == tok.Value {
		t.Fatal("expected a new fixture token distinct from the previous one")
	}
	if countAgentTokens(t, svc, agent.ID, tok.ID) != 0 {
		t.Fatal("unprotected previous token should have been deleted")
	}
	if countAgentTokens(t, svc, agent.ID, issued.ID) != 1 {
		t.Fatal("expected the newly issued token to remain")
	}
}

func TestResetAgentToken_UnboundAgentIsNotProtected(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	human := db.User{Login: "reset-human-unbound", Type: db.TypeUser, UserKind: db.UserKindHuman}
	if err := svc.DB.Create(&human).Error; err != nil {
		t.Fatalf("create human: %v", err)
	}
	agent := db.User{Login: "reset-agent-unbound", Type: db.TypeUser, UserKind: db.UserKindAgent}
	if err := svc.DB.Create(&agent).Error; err != nil {
		t.Fatalf("create agent: %v", err)
	}
	tok := db.Token{UserID: agent.ID, Name: "agent", Value: fixtureProtectedAgentToken}
	if err := svc.DB.Create(&tok).Error; err != nil {
		t.Fatalf("create fixture token: %v", err)
	}
	svc.ProtectedTokenValues = []string{fixtureProtectedAgentToken}

	_, err := svc.ResetAgentToken(context.Background(), human.ID, agent.Login)
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("ResetAgentToken error = %v, want ErrNotFound for missing binding", err)
	}
	if countAgentTokens(t, svc, agent.ID, tok.ID) != 1 {
		t.Fatal("unbound reset changed agent tokens")
	}
}
