package rest_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestAuthorityBoundaryReceiptRoutesAcceptOnlyExpectedEpochAndReadBackReceiptID(t *testing.T) {
	h := testharness.New(t)
	configurePrincipalSessionAuthorityHarness(t, h)
	h.Svc.SourceRevision = strings.Repeat("c", 40)
	if err := h.DB.Model(&db.User{}).Where("id = ?", h.User.ID).Update("site_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	epoch, err := h.Svc.CurrentAuthorityEpoch()
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v3/integrations/authority-boundary-receipts/legacy-capture"
	w := h.DoRESTJSONWithToken(t, http.MethodPost, path, h.Token, map[string]any{"expected_authority_epoch": epoch})
	assertStatusCode(t, w, http.StatusCreated)
	created := testharness.DecodeJSON(t, w)
	assertExactReceiptJSONKeys(t, created, []string{"receipt_id", "kind", "authority_epoch", "snapshot_digest", "source_revision", "claim_limit", "payload", "created_by_user_id", "created_at"})
	payload, ok := created["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload=%#v", created["payload"])
	}
	assertExactReceiptJSONKeys(t, payload, []string{"schema", "version", "contract_revision", "legacy_compatibility_mode", "trusted_issuers", "bindings", "team_bindings", "policy_classes", "resource_defaults", "resources", "principals"})
	if created["kind"] != service.AuthorityBoundaryReceiptLegacyCaptureKind || payload["schema"] != "ags.authority-boundary.legacy-capture.v2" {
		t.Fatalf("current legacy capture version drifted: kind=%#v schema=%#v", created["kind"], payload["schema"])
	}
	if defaults, ok := payload["resource_defaults"].([]any); !ok || len(defaults) != 0 {
		t.Fatalf("resource_defaults=%#v", payload["resource_defaults"])
	}
	issuers, _ := payload["trusted_issuers"].([]any)
	if len(issuers) != 1 {
		t.Fatalf("trusted_issuers=%#v", payload["trusted_issuers"])
	}
	assertExactReceiptJSONKeys(t, issuers[0].(map[string]any), []string{"id", "issuer", "status", "trust_revision"})
	bindings, _ := payload["bindings"].([]any)
	if len(bindings) != 1 {
		t.Fatalf("bindings=%#v", payload["bindings"])
	}
	assertExactReceiptJSONKeys(t, bindings[0].(map[string]any), []string{"id", "issuer_instance_id", "subject", "principal_id", "status", "binding_revision"})
	policyClasses, policyClassesOK := payload["policy_classes"].([]any)
	if payload["team_bindings"] != nil || !policyClassesOK || len(policyClasses) != 0 {
		t.Fatalf("legacy subject null/empty contract drifted: team_bindings=%#v policy_classes=%#v", payload["team_bindings"], payload["policy_classes"])
	}
	resources, _ := payload["resources"].([]any)
	if len(resources) != 1 {
		t.Fatalf("resources=%#v", payload["resources"])
	}
	assertExactReceiptJSONKeys(t, resources[0].(map[string]any), []string{"id", "target", "service", "repository", "status", "max_session_ttl", "policy_revision"})
	principals, _ := payload["principals"].([]any)
	if len(principals) == 0 {
		t.Fatalf("principals=%#v", payload["principals"])
	}
	for _, principal := range principals {
		assertExactReceiptJSONKeys(t, principal.(map[string]any), []string{"id", "login", "status", "site_admin"})
	}
	receiptID, _ := created["receipt_id"].(string)
	digest, _ := created["snapshot_digest"].(string)
	if !strings.HasPrefix(receiptID, "abr_") || len(digest) != 64 {
		t.Fatalf("receipt=%#v", created)
	}
	w = h.DoRESTWithToken(t, http.MethodGet, "/api/v3/integrations/authority-boundary-receipts/"+receiptID, h.Token)
	assertStatusCode(t, w, http.StatusOK)
	readback := testharness.DecodeJSON(t, w)
	if readback["snapshot_digest"] != digest || readback["receipt_id"] != receiptID {
		t.Fatalf("readback=%#v created=%#v", readback, created)
	}
	// Snapshot digests are payload coordinates, not globally unique receipt
	// identities across accepted source revisions. Public GET is receipt-ID only.
	w = h.DoRESTWithToken(t, http.MethodGet, "/api/v3/integrations/authority-boundary-receipts/"+digest, h.Token)
	assertStatusCode(t, w, http.StatusNotFound)

	w = h.DoRESTJSONWithToken(t, http.MethodPost, path, h.Token, map[string]any{
		"expected_authority_epoch": epoch,
		"payload":                  map[string]any{"forbidden": true},
	})
	assertStatusCode(t, w, http.StatusUnprocessableEntity)
}

func assertExactReceiptJSONKeys(t *testing.T, value map[string]any, expected []string) {
	t.Helper()
	if len(value) != len(expected) {
		t.Fatalf("JSON keys=%v, want exactly %v", mapKeys(value), expected)
	}
	for _, key := range expected {
		if _, ok := value[key]; !ok {
			t.Fatalf("JSON keys=%v missing %q", mapKeys(value), key)
		}
	}
}

func mapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}
