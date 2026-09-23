package rest_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func configurePrincipalSessionAuthorityHarness(t *testing.T, h *testharness.Harness) {
	t.Helper()
	principal := db.User{Login: "principal-agent", Type: db.TypeUser, Status: db.UserStatusActive, UserKind: db.UserKindAgent}
	if err := h.DB.Create(&principal).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{Name: "project-kit", FullName: "operator/project-kit", OwnerID: h.User.ID, Owner: h.User, Private: true, DefaultBranch: "main"}
	if err := h.DB.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.DB.Create(&db.Collaborator{RepositoryID: repo.ID, UserID: principal.ID, Permission: "write"}).Error; err != nil {
		t.Fatal(err)
	}
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "multica-mini", Issuer: "multica", KeyIDs: []string{"session-key"}, Status: "active", TrustRevision: "trust-v1"}},
		Bindings: []sessionauthority.PrincipalBinding{{
			ID: "binding-agent", IssuerInstanceID: "multica-mini", Subject: "agent-1", PrincipalID: principal.ID,
			Status: "active", BindingRevision: "binding-v1",
		}},
		Resources: []sessionauthority.ResourcePolicy{{
			ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active",
			MaxSessionTTL: "30m", PolicyRevision: "repo-v1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Svc.PrincipalSessions = set
}

func configureCanonicalTeamAuthorityHarness(t *testing.T, h *testharness.Harness) {
	t.Helper()
	configurePrincipalSessionAuthorityHarness(t, h)
	var principal db.User
	if err := h.DB.Where("login = ?", "principal-agent").First(&principal).Error; err != nil {
		t.Fatal(err)
	}
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "multica-mini", Issuer: "multica", KeyIDs: []string{"session-key"}, Status: "active", TrustRevision: "trust-v1"}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: "contributor", Status: "active", PolicyRevision: "class-v1", Operations: []string{"repo.read"}}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team", IssuerInstanceID: "multica-mini", WorkspaceID: "workspace-1", TeamIdentityID: "team-1", PolicyClass: "contributor", PrincipalID: principal.ID, Status: "active", BindingRevision: "team-v1", EpochFloor: 1}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "repo-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Svc.PrincipalSessions = set
}

func principalSessionAuthorityPath(extra url.Values) string {
	query := url.Values{
		"issuer": {"multica"}, "issuer_instance_id": {"multica-mini"}, "key_id": {"session-key"}, "subject": {"agent-1"},
		"target": {"primary-a"}, "service": {"ags"}, "repository": {"operator/project-kit"}, "operation": {"pr.create"},
	}
	for key, values := range extra {
		query[key] = values
	}
	return "/api/v3/integrations/principal-sessions?" + query.Encode()
}

func TestPrincipalSessionAuthorityReadSurfaceVerifiesNativeGrant(t *testing.T) {
	h := testharness.New(t)
	configurePrincipalSessionAuthorityHarness(t, h)
	w := h.DoRESTWithToken(t, http.MethodGet, principalSessionAuthorityPath(nil), h.Token)
	assertStatusCode(t, w, http.StatusOK)
	body := testharness.DecodeJSON(t, w)
	if body["status"] != "pass" {
		t.Fatalf("body = %#v", body)
	}
	authority := body["authority"].(map[string]any)
	if authority["binding_id"] != "binding-agent" || authority["native_grant"] != "write" || authority["operation"] != "pr.create" {
		t.Fatalf("authority = %#v", authority)
	}
}

func TestTeamAuthorityEpochFloorRouteIsAuditedAndBoundToActiveCanonicalTeam(t *testing.T) {
	h := testharness.New(t)
	configureCanonicalTeamAuthorityHarness(t, h)
	w := h.DoRESTJSONWithToken(t, http.MethodPost, "/api/v3/integrations/principal-sessions/epoch-floor", h.Token, map[string]any{
		"issuer_instance_id": "multica-mini", "team_identity_id": "team-1", "policy_class": "contributor", "membership_epoch": 2,
	})
	assertStatusCode(t, w, http.StatusOK)
	var count int64
	if err := h.DB.Model(&db.AuditLogEntry{}).Where("action = ?", "team_authority.epoch_floor_advance").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("epoch advance audit count=%d err=%v", count, err)
	}
	w = h.DoRESTJSONWithToken(t, http.MethodPost, "/api/v3/integrations/principal-sessions/epoch-floor", h.Token, map[string]any{
		"issuer_instance_id": "multica-mini", "team_identity_id": "unknown", "policy_class": "contributor", "membership_epoch": 3,
	})
	assertStatusCode(t, w, http.StatusForbidden)
}

func TestCanonicalAuthorityReadSurfaceBindsWorkspace(t *testing.T) {
	h := testharness.New(t)
	configureCanonicalTeamAuthorityHarness(t, h)
	query := url.Values{
		"issuer": {"multica"}, "issuer_instance_id": {"multica-mini"}, "key_id": {"session-key"},
		"workspace_id": {"workspace-1"}, "team_identity_id": {"team-1"}, "policy_class": {"contributor"}, "membership_epoch": {"1"},
		"target": {"primary-a"}, "service": {"ags"}, "repository": {"operator/project-kit"}, "operation": {"repo.read"},
	}
	w := h.DoRESTWithToken(t, http.MethodGet, "/api/v3/integrations/principal-sessions?"+query.Encode(), h.Token)
	assertStatusCode(t, w, http.StatusOK)
	body := testharness.DecodeJSON(t, w)
	if body["status"] != "pass" {
		t.Fatalf("canonical authority=%#v", body)
	}
	assertExactReceiptJSONKeys(t, body, []string{"status", "authority", "claim_limit"})
	authority := body["authority"].(map[string]any)
	assertExactReceiptJSONKeys(t, authority, []string{
		"ok", "contract_revision", "issuer_instance_id", "trust_revision", "key_id",
		"workspace_id", "team_identity_id", "policy_class", "membership_epoch",
		"principal", "target", "service", "repository", "resource_policy_revision",
		"operation", "required_permission", "native_grant", "native_grant_revision", "checks", "claim_limit",
	})
	if _, present := authority["subject"]; present {
		t.Fatalf("canonical authority unexpectedly includes subject: %#v", authority)
	}
	if _, present := authority["binding_id"]; present {
		t.Fatalf("canonical authority unexpectedly includes legacy binding: %#v", authority)
	}
	assertExactReceiptJSONKeys(t, authority["principal"].(map[string]any), []string{"id", "login", "user_kind", "status", "exists", "effective_permission"})

	query.Set("workspace_id", "other-workspace")
	w = h.DoRESTWithToken(t, http.MethodGet, "/api/v3/integrations/principal-sessions?"+query.Encode(), h.Token)
	assertStatusCode(t, w, http.StatusOK)
	if body := testharness.DecodeJSON(t, w); body["status"] != "fail" {
		t.Fatalf("wrong workspace authority=%#v", body)
	}
}

func TestCanonicalAuthorityReadSurfaceDoesNotEchoUnresolvedQueryValues(t *testing.T) {
	h := testharness.New(t)
	configureCanonicalTeamAuthorityHarness(t, h)
	for field, value := range map[string]string{
		"workspace_id": "other-workspace", "team_identity_id": "other-team", "policy_class": "other-class",
		"repository": "operator/other", "operation": "unknown.operation",
	} {
		t.Run(field, func(t *testing.T) {
			query := canonicalAuthorityQuery()
			query.Set(field, value)
			w := h.DoRESTWithToken(t, http.MethodGet, "/api/v3/integrations/principal-sessions?"+query.Encode(), h.Token)
			assertStatusCode(t, w, http.StatusOK)
			if body := w.Body.String(); strings.Contains(body, value) {
				t.Fatalf("unresolved authority query value leaked: %s", body)
			}
		})
	}
}

func TestCanonicalAuthorityReadSurfaceRejectsSecretShapedQueryValuesWithoutEchoing(t *testing.T) {
	h := testharness.New(t)
	configureCanonicalTeamAuthorityHarness(t, h)
	for field, value := range map[string]string{
		"operation":        strings.Join([]string{"mat", "_query_sentinel"}, ""),
		"repository":       strings.Join([]string{"ags", "_sess_query_sentinel"}, ""),
		"workspace_id":     strings.Join([]string{"ey", "JhbGciOiJIUzI1NiJ9", ".eyJzdWIiOiJ4In0", ".signature"}, ""),
		"team_identity_id": strings.Join([]string{"-----BE", "GIN PRIVATE KEY-----"}, ""),
	} {
		t.Run(field, func(t *testing.T) {
			query := canonicalAuthorityQuery()
			query.Set(field, value)
			w := h.DoRESTWithToken(t, http.MethodGet, "/api/v3/integrations/principal-sessions?"+query.Encode(), h.Token)
			assertStatusCode(t, w, http.StatusUnprocessableEntity)
			if body := w.Body.String(); strings.Contains(body, value) {
				t.Fatalf("secret-shaped authority query leaked: %s", body)
			}
		})
	}
}

func canonicalAuthorityQuery() url.Values {
	return url.Values{
		"issuer": {"multica"}, "issuer_instance_id": {"multica-mini"}, "key_id": {"session-key"},
		"workspace_id": {"workspace-1"}, "team_identity_id": {"team-1"}, "policy_class": {"contributor"}, "membership_epoch": {"1"},
		"target": {"primary-a"}, "service": {"ags"}, "repository": {"operator/project-kit"}, "operation": {"repo.read"},
	}
}

func TestPrincipalSessionAuthorityReadSurfaceRejectsProvenanceSelectors(t *testing.T) {
	h := testharness.New(t)
	configurePrincipalSessionAuthorityHarness(t, h)
	for _, field := range []string{"role", "task_id", "agent_id", "display_name", "principal"} {
		w := h.DoRESTWithToken(t, http.MethodGet, principalSessionAuthorityPath(url.Values{field: {"attacker"}}), h.Token)
		assertStatusCode(t, w, http.StatusUnprocessableEntity)
	}
}
