package rest_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/operationconstraints"
	"github.com/ngaut/agent-git-service/internal/rest"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
	"github.com/ngaut/agent-git-service/internal/testharness/testdb"
	"github.com/ngaut/agent-git-service/internal/workloadidentity"
	"gorm.io/gorm"
)

func TestDelegatedSessionLifecycleOpenAPIClosedSchema(t *testing.T) {
	var spec map[string]any
	if err := json.Unmarshal(rest.OpenAPISpecBytes(), &spec); err != nil {
		t.Fatal(err)
	}
	paths := spec["paths"].(map[string]any)
	if _, ok := paths["/api/v3/agent-sessions/{session_id}/lifecycle"]; !ok {
		t.Fatalf("lifecycle path missing: %v", paths)
	}
	components := spec["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	lifecycle := schemas["DelegatedSessionLifecycleV1"].(map[string]any)
	if lifecycle["additionalProperties"] != false {
		t.Fatalf("lifecycle schema is not closed: %v", lifecycle)
	}
	required := map[string]bool{}
	for _, value := range lifecycle["required"].([]any) {
		required[value.(string)] = true
	}
	for _, key := range []string{"schema", "session_id", "state", "created_at", "expires_at", "authority", "provenance", "claim_limit"} {
		if !required[key] {
			t.Fatalf("required lifecycle key missing: %s", key)
		}
	}
	properties := lifecycle["properties"].(map[string]any)
	authority := properties["authority"].(map[string]any)
	authorityProperties := authority["properties"].(map[string]any)
	operation := authorityProperties["operation"].(map[string]any)
	principal := authorityProperties["principal"].(map[string]any)
	principalID := principal["properties"].(map[string]any)["id"].(map[string]any)
	membershipEpoch := authorityProperties["membership_epoch"].(map[string]any)
	for label, value := range map[string]any{"principal.id": principalID["maximum"], "membership_epoch": membershipEpoch["maximum"]} {
		if value != float64(operationconstraints.MaxJSONSafePositiveInteger) {
			t.Fatalf("%s maximum=%v", label, value)
		}
	}
	if _, ok := operation["properties"].(map[string]any)["constraints"]; ok {
		t.Fatal("lifecycle OpenAPI must not expose operation constraints")
	}
}

func TestDelegatedSessionLifecycleOperatorHTTPContract(t *testing.T) {
	h := testharness.New(t)
	repository := db.Repository{Name: "lifecycle", FullName: h.User.Login + "/lifecycle", OwnerID: h.User.ID, Owner: h.User, DefaultBranch: "main"}
	if err := h.DB.Create(&repository).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	auditedAt := now.Add(-time.Minute)
	session := db.DelegatedAgentSession{
		ID: uuid.NewString(), CredentialHash: strings.Repeat("b", 64), CredentialPrefix: "lifecycle-http",
		PrincipalUserID: h.User.ID, PrincipalLogin: h.User.Login,
		Issuer: "urn:multica:deployment:mini", AssertionVersion: 1, AssertionPurpose: "ags_session_exchange",
		AssertionJTI: "lifecycle-http-jti", AssertionAudience: "urn:ags:workload-session-exchange:v1",
		ContractRevision: "2026-07-24.team-authority-v4", CredentialMode: "delegated_session",
		TeamIdentityID: "11111111-1111-4111-8111-111111111111", TeamBindingRevision: "team-http-v1", PolicyClass: "multica.workspace.default.v1", MembershipEpoch: 2,
		IssuerInstanceID: "multica-mini", TrustRevision: "trust-http-v1", IssuerWorkspaceID: "22222222-2222-4222-8222-222222222222",
		ExternalAgentID: "33333333-3333-4333-8333-333333333333", ExternalTaskID: "66666666-6666-4666-8666-666666666666", ExternalRunID: "77777777-7777-4777-8777-777777777777", CorrelationID: "88888888-8888-4888-8888-888888888888",
		WorkloadContextSchema: workloadidentity.WorkloadContextSchema,
		TargetInstance:        "primary-a", ResourceService: "ags", RepositoryID: repository.ID, OperationName: "repo.read", OperationConstraints: map[string]string{},
		GrantedCapabilities: []string{"repo:read"}, PolicyVersion: "policy-http-v1", PolicySnapshotHash: strings.Repeat("c", 64),
		NativeGrantRevision: "grant-rev:05c236315cbdbe2cd9f8255a706e75ec5cd84d1fe2949f4e901b27e731801582", ResourcePolicyRevision: "resource-http-v1",
		CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), ExpiryAuditedAt: &auditedAt,
	}
	if err := h.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	path := "/api/v3/agent-sessions/" + session.ID + "/lifecycle"

	w := h.DoREST(t, http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("admin status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["schema"] != service.DelegatedSessionLifecycleSchema || body["session_id"] != session.ID || body["state"] != "expired" || body["expiry_audited_at"] == nil {
		t.Fatalf("unexpected lifecycle response: %v", body)
	}
	if _, ok := body["authority"].(map[string]any); !ok {
		t.Fatalf("authority is not an object: %T", body["authority"])
	}
	if _, ok := body["provenance"].(map[string]any); !ok {
		t.Fatalf("provenance is not an object: %T", body["provenance"])
	}
	lower := strings.ToLower(w.Body.String())
	for _, forbidden := range []string{"credential_hash", "credential_prefix", "assertion_jti", "issuer_subject", "session_token", "bearer", "fingerprint", "raw_audit"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("forbidden response field %q: %s", forbidden, w.Body.String())
		}
	}
	malformedCases := []struct {
		name      string
		field     string
		invalid   any
		valid     any
		forbidden []string
	}{
		{name: "missing provenance", field: "external_run_id", invalid: "", valid: session.ExternalRunID},
		{name: "dangling principal", field: "principal_user_id", invalid: uint(999999), valid: session.PrincipalUserID, forbidden: []string{"999999", h.User.Login}},
		{name: "dangling repository", field: "repository_id", invalid: uint(999999), valid: session.RepositoryID, forbidden: []string{"999999", repository.FullName}},
	}
	var canonicalValidationBody string
	for _, tc := range malformedCases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.HasPrefix(tc.name, "dangling ") {
				testdb.MutateWithoutForeignKeys(t, h.DB, func(conn *gorm.DB) error {
					return conn.Model(&db.DelegatedAgentSession{}).Where("id = ?", session.ID).Update(tc.field, tc.invalid).Error
				})
			} else if err := h.DB.Model(&db.DelegatedAgentSession{}).Where("id = ?", session.ID).Update(tc.field, tc.invalid).Error; err != nil {
				t.Fatal(err)
			}
			malformed := h.DoREST(t, http.MethodGet, path, nil)
			if malformed.Code != http.StatusUnprocessableEntity || !strings.Contains(malformed.Body.String(), "canonical team-v4 delegated Session lifecycle is unavailable") {
				t.Fatalf("malformed-row status=%d body=%s", malformed.Code, malformed.Body.String())
			}
			if canonicalValidationBody == "" {
				canonicalValidationBody = malformed.Body.String()
			} else if malformed.Body.String() != canonicalValidationBody {
				t.Fatalf("validation body drifted: got=%s want=%s", malformed.Body.String(), canonicalValidationBody)
			}
			for _, forbidden := range append([]string{session.ID}, tc.forbidden...) {
				if strings.Contains(malformed.Body.String(), forbidden) {
					t.Fatalf("malformed response leaked %q: %s", forbidden, malformed.Body.String())
				}
			}
			if err := h.DB.Model(&db.DelegatedAgentSession{}).Where("id = ?", session.ID).Update(tc.field, tc.valid).Error; err != nil {
				t.Fatal(err)
			}
		})
	}

	nonAdmin := db.User{Login: "lifecycle-http-reader", Name: "lifecycle-http-reader", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := h.DB.Create(&nonAdmin).Error; err != nil {
		t.Fatal(err)
	}
	nonAdminToken := "lifecycle-http-reader-token"
	if err := h.DB.Create(&db.Token{UserID: nonAdmin.ID, Value: nonAdminToken}).Error; err != nil {
		t.Fatal(err)
	}
	if denied := h.DoRESTWithToken(t, http.MethodGet, path, nonAdminToken); denied.Code != http.StatusForbidden {
		t.Fatalf("non-admin status=%d body=%s", denied.Code, denied.Body.String())
	}
	if missing := h.DoREST(t, http.MethodGet, "/api/v3/agent-sessions/"+uuid.NewString()+"/lifecycle", nil); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown status=%d body=%s", missing.Code, missing.Body.String())
	}
	if unauthorized := h.DoRESTNoAuth(t, http.MethodGet, path); unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
}
