package rest_test

import (
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestTokenAPI_CRUD(t *testing.T) {
	h := testharness.New(t)

	expiresAt := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	w := h.DoRESTJSON(t, "POST", "/api/ext/v1/user/tokens", map[string]any{
		"name":       "ci-token",
		"expires_at": expiresAt,
	})
	assertStatusCode(t, w, 201)
	created := testharness.DecodeJSON(t, w)
	assertFieldsPresent(t, created, map[string]string{
		"id":           "number",
		"name":         "string",
		"token":        "string",
		"created_at":   "string",
		"last_used_at": "string",
		"expires_at":   "string",
	})

	idFloat, ok := created["id"].(float64)
	if !ok || idFloat == 0 {
		t.Fatalf("expected numeric id in response, got %v", created["id"])
	}
	createdID := int(idFloat)

	w = h.DoREST(t, "GET", "/api/ext/v1/user/tokens", nil)
	assertStatusCode(t, w, 200)
	tokens := testharness.DecodeJSONArray(t, w)
	if _, found := findTokenByID(tokens, createdID); !found {
		t.Fatalf("expected token id %d in list", createdID)
	}

	w = h.DoRESTJSON(t, "DELETE", "/api/ext/v1/user/tokens", map[string]any{
		"id": createdID,
	})
	assertStatusCode(t, w, 204)

	w = h.DoREST(t, "GET", "/api/ext/v1/user/tokens", nil)
	assertStatusCode(t, w, 200)
	tokens = testharness.DecodeJSONArray(t, w)
	if _, found := findTokenByID(tokens, createdID); found {
		t.Fatalf("expected token id %d to be deleted", createdID)
	}
}

func findTokenByID(tokens []map[string]any, id int) (map[string]any, bool) {
	for _, tok := range tokens {
		if raw, ok := tok["id"].(float64); ok && int(raw) == id {
			return tok, true
		}
	}
	return nil, false
}

// A 11-15 incident: an agent deleted the shared ADMIN_TOKEN through the token
// API and every host was locked out until AGS restarted. Bootstrap tokens must
// be refused by value and by id and stay usable. The legacy /api/v3/user/tokens
// alias is routed to the same DeleteToken handler.
func TestTokenAPI_RefusesProtectedTokenDeletion(t *testing.T) {
	for _, prefix := range []string{"/api/ext/v1"} {
		for _, by := range []string{"token", "id", "token_id"} {
			t.Run(prefix+"/"+by, func(t *testing.T) {
				h := testharness.New(t)
				h.Svc.ProtectedTokenValues = []string{h.Token}

				w := h.DoREST(t, "GET", prefix+"/user/tokens", nil)
				assertStatusCode(t, w, 200)
				var protectedID int
				for _, tok := range testharness.DecodeJSONArray(t, w) {
					if raw, ok := tok["id"].(float64); ok {
						protectedID = int(raw)
					}
				}
				if protectedID == 0 {
					t.Fatal("expected the harness token in the list")
				}
				body := map[string]any{by: protectedID}
				if by == "token" {
					body = map[string]any{"token": h.Token}
				}
				w = h.DoRESTJSON(t, "DELETE", prefix+"/user/tokens", body)
				assertStatusCode(t, w, 403)
				resp := testharness.DecodeJSON(t, w)
				if resp["code"] != "protected_token" {
					t.Fatalf("expected code protected_token, got %v", resp)
				}

				// The token still authenticates and is still listed.
				w = h.DoREST(t, "GET", prefix+"/user/tokens", nil)
				assertStatusCode(t, w, 200)
				if _, found := findTokenByID(testharness.DecodeJSONArray(t, w), protectedID); !found {
					t.Fatalf("protected token %d was deleted", protectedID)
				}
			})
		}
	}
}

func TestTokenAPI_UnprotectedTokenStillDeletableWhenProtectionConfigured(t *testing.T) {
	h := testharness.New(t)
	h.Svc.ProtectedTokenValues = []string{h.Token}
	w := h.DoRESTJSON(t, "POST", "/api/ext/v1/user/tokens", map[string]any{"name": "scratch"})
	assertStatusCode(t, w, 201)
	created := testharness.DecodeJSON(t, w)
	value, _ := created["token"].(string)
	w = h.DoRESTJSON(t, "DELETE", "/api/ext/v1/user/tokens", map[string]any{"token": value})
	assertStatusCode(t, w, 204)
}
