package rest_test

import (
	"net/http"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

// Fixture values only. These tests never read host credentials or call a live AGS API.
const (
	fixtureResetProtectedAgentToken   = "fixture-admin-token-as-agent-cred-http"
	fixtureResetUnprotectedAgentToken = "fixture-agent-ordinary-token-http"
)

func TestResetAgentTokenEndpoint_RefusesProtectedLongLivedCredential(t *testing.T) {
	h := testharness.New(t)

	agent := db.User{Login: "reset-rest-protected-agent", Name: "reset-rest-protected-agent", Type: db.TypeUser, UserKind: db.UserKindAgent}
	if err := h.DB.Create(&agent).Error; err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := h.DB.Create(&db.AgentBinding{HumanUserID: h.User.ID, AgentUserID: agent.ID}).Error; err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if err := h.DB.Create(&db.Token{UserID: agent.ID, Name: "agent", Value: fixtureResetProtectedAgentToken}).Error; err != nil {
		t.Fatalf("create fixture token: %v", err)
	}
	h.Svc.ProtectedTokenValues = []string{fixtureResetProtectedAgentToken}

	w := h.DoRESTJSON(t, http.MethodPost, "/api/ext/v1/agent-bindings/"+agent.Login+"/reset-token", map[string]any{})
	assertStatusCode(t, w, http.StatusForbidden)
	resp := testharness.DecodeJSON(t, w)
	if resp["code"] != "protected_token" || resp["error"] != "protected_token" {
		t.Fatalf("expected protected_token code, got %v", resp)
	}

	w = h.DoRESTWithToken(t, http.MethodGet, "/api/v3/user", fixtureResetProtectedAgentToken)
	assertStatusCode(t, w, http.StatusOK)
	var count int64
	if err := h.DB.Model(&db.Token{}).Where("user_id = ? AND value = ?", agent.ID, fixtureResetProtectedAgentToken).Count(&count).Error; err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if count != 1 {
		t.Fatal("protected long-lived agent token was deleted")
	}
}

func TestResetAgentTokenEndpoint_StillResetsUnprotectedToken(t *testing.T) {
	h := testharness.New(t)

	agent := db.User{Login: "reset-rest-ok-agent", Name: "reset-rest-ok-agent", Type: db.TypeUser, UserKind: db.UserKindAgent}
	if err := h.DB.Create(&agent).Error; err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if err := h.DB.Create(&db.AgentBinding{HumanUserID: h.User.ID, AgentUserID: agent.ID}).Error; err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if err := h.DB.Create(&db.Token{UserID: agent.ID, Name: "agent", Value: fixtureResetUnprotectedAgentToken}).Error; err != nil {
		t.Fatalf("create fixture token: %v", err)
	}
	h.Svc.ProtectedTokenValues = []string{fixtureResetProtectedAgentToken}

	w := h.DoRESTJSON(t, http.MethodPost, "/api/ext/v1/agent-bindings/"+agent.Login+"/reset-token", map[string]any{})
	assertStatusCode(t, w, http.StatusOK)
	body := testharness.DecodeJSON(t, w)
	tokenPayload, ok := body["token"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested token payload, got %T", body["token"])
	}
	issued, _ := tokenPayload["token"].(string)
	if issued == "" || issued == fixtureResetUnprotectedAgentToken {
		t.Fatal("expected a newly issued fixture token")
	}

	w = h.DoRESTWithToken(t, http.MethodGet, "/api/v3/user", fixtureResetUnprotectedAgentToken)
	assertStatusCode(t, w, http.StatusUnauthorized)
	w = h.DoRESTWithToken(t, http.MethodGet, "/api/v3/user", issued)
	assertStatusCode(t, w, http.StatusOK)
}
