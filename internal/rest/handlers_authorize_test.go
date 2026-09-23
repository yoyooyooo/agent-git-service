package rest_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func configureDurableAuthorizationAuthority(t *testing.T, h *testharness.Harness, repository, policyRevision string) {
	t.Helper()
	if err := h.DB.Model(&h.User).Update("user_kind", "agent").Error; err != nil {
		t.Fatalf("mark durable authorization fixture as agent: %v", err)
	}
	h.User.UserKind = "agent"
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{
			ID: "test-issuer", Issuer: "test", KeyIDs: []string{"test-key"}, Status: "active", TrustRevision: "test-trust-v1",
		}},
		Bindings: []sessionauthority.PrincipalBinding{{
			ID: "test-binding", IssuerInstanceID: "test-issuer", Subject: "durable-test", PrincipalID: h.User.ID,
			Status: "active", BindingRevision: "test-binding-v1",
		}},
		Resources: []sessionauthority.ResourcePolicy{{
			ID: "durable-test-resource", Target: "test", Service: "ags", Repository: repository,
			Status: "active", MaxSessionTTL: "30m", PolicyRevision: policyRevision,
		}},
	})
	if err != nil {
		t.Fatalf("sessionauthority.New: %v", err)
	}
	h.Svc.PrincipalSessions = set
}

func TestAuthorizeOperation_ReadOperation_Permitted(t *testing.T) {
	h := testharness.New(t)
	repo, err := h.Svc.CreateRepo(context.Background(), service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-repo", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	configureDurableAuthorizationAuthority(t, h, repo.FullName, "authz-policy-v1")

	w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", map[string]any{
		"contract_revision": "2026-07-19.principal-session-v2",
		"resource":          map[string]string{"service": "ags", "repository": repo.FullName},
		"operation": map[string]any{"name": "ci.read", "constraints": map[string]any{
			"pull_request_number": 1, "forgejo_pull_request_number": 2, "head_sha": strings.Repeat("a", 40),
		}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	result := testharness.DecodeJSON(t, w)
	if authorized, _ := result["authorized"].(bool); !authorized {
		t.Fatalf("expected authorized=true, got %v", result)
	}
	if rev, _ := result["contract_revision"].(string); rev != sessionauthority.ContractRevision {
		t.Fatalf("contract_revision mismatch: %s", rev)
	}
	basis, _ := result["authorization_basis"].(map[string]any)
	if basis == nil || basis["native_grant_revision"] == nil || basis["native_grant_revision"] == "" {
		t.Fatalf("missing authorization basis: %#v", basis)
	}
	if basis["resource_policy_revision"] != "authz-policy-v1" {
		t.Fatalf("resource_policy_revision = %v", basis["resource_policy_revision"])
	}
	operation, _ := result["operation"].(map[string]any)
	constraints, _ := operation["constraints"].(map[string]any)
	if operation["name"] != "ci.read" || constraints["pull_request_number"] != float64(1) || constraints["forgejo_pull_request_number"] != float64(2) || constraints["head_sha"] != strings.Repeat("a", 40) {
		t.Fatalf("operation scope mismatch: %#v", operation)
	}
	body := w.Body.String()
	for _, leaked := range []string{h.Token, "principal=", "repository=", "permission="} {
		if strings.Contains(body, leaked) {
			t.Fatalf("authorization receipt leaked grant or credential material %q: %s", leaked, body)
		}
	}
}

func TestAuthorizeOperation_ReadDeniedForNonMember(t *testing.T) {
	h := testharness.New(t)
	repo, err := h.Svc.CreateRepo(context.Background(), service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-restricted", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	configureDurableAuthorizationAuthority(t, h, repo.FullName, "restricted-policy-v1")
	_, outsiderToken := seedHarnessUser(t, h, "authz-outsider", false)

	w := h.DoRESTJSONWithToken(t, "POST", "/api/v3/operations/authorize", outsiderToken, map[string]any{
		"contract_revision": sessionauthority.ContractRevision,
		"resource":          map[string]string{"service": "ags", "repository": repo.FullName},
		"operation":         map[string]any{"name": "ci.read", "constraints": durableReadConstraints("ci.read")},
	})
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("expected 403 or 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_WriteOperationRequiresWritePermission(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repo, err := h.Svc.CreateRepo(ctx, service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-write-repo", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	configureDurableAuthorizationAuthority(t, h, repo.FullName, "write-policy-v1")
	reader, readerToken := seedHarnessUser(t, h, "authz-reader", false)
	if err := h.Svc.AddCollaborator(ctx, repo.ID, reader.ID, "read"); err != nil {
		t.Fatalf("AddCollaborator(read): %v", err)
	}
	request := map[string]any{
		"contract_revision": sessionauthority.ContractRevision,
		"resource":          map[string]string{"service": "ags", "repository": repo.FullName},
		"operation":         map[string]any{"name": "pr.merge", "constraints": durableMergeConstraints()},
	}

	w := h.DoRESTJSONWithToken(t, "POST", "/api/v3/operations/authorize", readerToken, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for pr.merge with read-only native grant, got %d: %s", w.Code, w.Body.String())
	}
	w = h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", request)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for pr.merge with owner permission, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_WritePermissionDoesNotGrantAdminOperation(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repo, err := h.Svc.CreateRepo(ctx, service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-admin-repo", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	configureDurableAuthorizationAuthority(t, h, repo.FullName, "admin-policy-v1")
	writer, writerToken := seedHarnessUser(t, h, "authz-writer", false)
	if err := h.Svc.AddCollaborator(ctx, repo.ID, writer.ID, "write"); err != nil {
		t.Fatalf("AddCollaborator(write): %v", err)
	}
	request := map[string]any{
		"contract_revision": sessionauthority.ContractRevision,
		"resource":          map[string]string{"service": "ags", "repository": repo.FullName},
		"operation": map[string]any{"name": "repo.admin", "constraints": map[string]any{
			"target_repository": repo.FullName,
			"base_ref":          "main",
			"source_base_sha":   strings.Repeat("a", 40),
			"source_ref_digest": strings.Repeat("b", 64),
			"action":            "forgejo_onboard",
		}},
	}

	w := h.DoRESTJSONWithToken(t, "POST", "/api/v3/operations/authorize", writerToken, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for repo.admin with write-only native grant, got %d: %s", w.Code, w.Body.String())
	}
	w = h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", request)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for repo.admin with owner permission, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_UnsupportedContractRevision(t *testing.T) {
	h := testharness.New(t)
	w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", map[string]any{
		"contract_revision": "wrong-revision",
		"resource":          map[string]string{"service": "ags", "repository": h.User.Login + "/some-repo"},
		"operation":         map[string]any{"name": "ci.read", "constraints": durableReadConstraints("ci.read")},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for invalid contract_revision, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_MalformedContractRevision(t *testing.T) {
	h := testharness.New(t)
	w := h.DoREST(t, "POST", "/api/v3/operations/authorize", strings.NewReader(`{
		"contract_revision": 20260719,
		"resource": {"service": "ags", "repository": "owner/repo"},
		"operation": {"name": "ci.read"}
	}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for malformed contract_revision, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_RejectsTrailingJSON(t *testing.T) {
	h := testharness.New(t)
	w := h.DoREST(t, "POST", "/api/v3/operations/authorize", strings.NewReader(`{
		"contract_revision": "2026-07-19.principal-session-v2",
		"resource": {"service": "ags", "repository": "owner/repo"},
		"operation": {"name": "ci.read"}
	}{}`))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for trailing JSON, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_RejectsUnsafeOrNonScalarConstraints(t *testing.T) {
	h := testharness.New(t)
	repo, err := h.Svc.CreateRepo(context.Background(), service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-constraints", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	configureDurableAuthorizationAuthority(t, h, repo.FullName, "constraints-policy-v1")

	for name, constraints := range map[string]map[string]any{
		"credential-shaped key": {"credential_hint": "must-not-echo"},
		"nested value":          {"pull_request": map[string]any{"number": 1}},
		"empty string":          {"head_sha": "  "},
	} {
		t.Run(name, func(t *testing.T) {
			w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", map[string]any{
				"contract_revision": sessionauthority.ContractRevision,
				"resource":          map[string]string{"service": "ags", "repository": repo.FullName},
				"operation":         map[string]any{"name": "ci.read", "constraints": constraints},
			})
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "must-not-echo") {
				t.Fatalf("response echoed rejected constraint: %s", w.Body.String())
			}
		})
	}
}

func TestAuthorizeOperation_PRMergeRequiresExactConstraints(t *testing.T) {
	h := testharness.New(t)
	repo, err := h.Svc.CreateRepo(context.Background(), service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-merge-constraints", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	configureDurableAuthorizationAuthority(t, h, repo.FullName, "merge-constraints-policy-v1")

	valid := durableMergeConstraints()
	cases := map[string]map[string]any{
		"empty":                    {},
		"missing merge method":     withoutConstraint(valid, "merge_method"),
		"legacy exact head":        {"pull_request_number": 1, "forgejo_pull_request_number": 2, "exact_head": strings.Repeat("a", 40)},
		"non-canonical head":       {"pull_request_number": 1, "forgejo_pull_request_number": 2, "merge_method": "rebase", "expected_head_sha": strings.Repeat("A", 40)},
		"unsupported merge method": {"pull_request_number": 1, "forgejo_pull_request_number": 2, "merge_method": "octopus", "expected_head_sha": strings.Repeat("a", 40)},
		"string PR number":         {"pull_request_number": "1", "forgejo_pull_request_number": 2, "merge_method": "rebase", "expected_head_sha": strings.Repeat("a", 40)},
	}
	for name, constraints := range cases {
		t.Run(name, func(t *testing.T) {
			w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", authorizeOperationRequest(repo.FullName, "pr.merge", constraints))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
			}
		})
	}

	w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", authorizeOperationRequest(repo.FullName, "pr.merge", valid))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for exact pr.merge constraints, got %d: %s", w.Code, w.Body.String())
	}
	operation := testharness.DecodeJSON(t, w)["operation"].(map[string]any)
	constraints := operation["constraints"].(map[string]any)
	if constraints["pull_request_number"] != float64(1) || constraints["forgejo_pull_request_number"] != float64(2) || constraints["merge_method"] != "rebase" || constraints["expected_head_sha"] != strings.Repeat("a", 40) {
		t.Fatalf("operation scope mismatch: %#v", operation)
	}
}

func TestAuthorizeOperation_WrongService(t *testing.T) {
	h := testharness.New(t)
	w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", map[string]any{
		"contract_revision": sessionauthority.ContractRevision,
		"resource":          map[string]string{"service": "gitlab", "repository": h.User.Login + "/some-repo"},
		"operation":         map[string]any{"name": "ci.read", "constraints": durableReadConstraints("ci.read")},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for non-ags service, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_InvalidRepositoryResource(t *testing.T) {
	h := testharness.New(t)
	for name, repository := range map[string]string{
		"missing": "", "owner-only": "owner", "too-many-segments": "owner/repo/extra",
	} {
		t.Run(name, func(t *testing.T) {
			w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", map[string]any{
				"contract_revision": sessionauthority.ContractRevision,
				"resource":          map[string]string{"service": "ags", "repository": repository},
				"operation":         map[string]any{"name": "ci.read", "constraints": durableReadConstraints("ci.read")},
			})
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422 for repository %q, got %d: %s", repository, w.Code, w.Body.String())
			}
		})
	}
}

func TestAuthorizeOperation_Unauthenticated(t *testing.T) {
	h := testharness.New(t)
	w := h.DoRESTNoAuth(t, "POST", "/api/v3/operations/authorize")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_TargetDefaultDerivesRepoButNativeGrantStillDecides(t *testing.T) {
	h := testharness.New(t)
	if err := h.DB.Model(&h.User).Update("user_kind", "agent").Error; err != nil {
		t.Fatalf("mark durable authorization fixture as agent: %v", err)
	}
	h.User.UserKind = "agent"
	repo, err := h.Svc.CreateRepo(context.Background(), service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-derived-resource", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "test-issuer", Issuer: "test", KeyIDs: []string{"test-key"}, Status: "active", TrustRevision: "test-trust-v1"}},
		Bindings:       []sessionauthority.PrincipalBinding{{ID: "test-binding", IssuerInstanceID: "test-issuer", Subject: "durable-test", PrincipalID: h.User.ID, Status: "active", BindingRevision: "test-binding-v1"}},
		ResourceDefaults: []sessionauthority.ResourceDefault{{
			ID: "test-machine-default", Target: "test", Service: "ags", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "test-machine-default-v1",
		}},
	})
	if err != nil {
		t.Fatalf("sessionauthority.New: %v", err)
	}
	h.Svc.PrincipalSessions = set
	request := map[string]any{
		"contract_revision": sessionauthority.ContractRevision,
		"resource":          map[string]string{"service": "ags", "repository": repo.FullName},
		"operation":         map[string]any{"name": "ci.read", "constraints": durableReadConstraints("ci.read")},
	}

	w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", request)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for native owner grant through target default, got %d: %s", w.Code, w.Body.String())
	}
	basis := testharness.DecodeJSON(t, w)["authorization_basis"].(map[string]any)
	if basis["resource_policy_revision"] != "test-machine-default-v1" {
		t.Fatalf("resource policy revision = %v", basis["resource_policy_revision"])
	}

	_, outsiderToken := seedHarnessUser(t, h, "authz-derived-outsider", false)
	w = h.DoRESTJSONWithToken(t, "POST", "/api/v3/operations/authorize", outsiderToken, request)
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("expected native-grant denial for outsider, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_HumanUsesNativePermissionWithoutPrincipalSessionPolicy(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repo, err := h.Svc.CreateRepo(ctx, service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-native-human", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	request := authorizeOperationRequest(repo.FullName, "pr.merge", durableMergeConstraints())

	w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", request)
	if w.Code != http.StatusOK {
		t.Fatalf("expected native owner permission to authorize without PrincipalSessions, got %d: %s", w.Code, w.Body.String())
	}
	basis := testharness.DecodeJSON(t, w)["authorization_basis"].(map[string]any)
	if basis["resource_policy_revision"] != "native-human-repository-permission-v1" || basis["policy_class"] != nil {
		t.Fatalf("unexpected native human authorization basis: %#v", basis)
	}

	maintainer, maintainerToken := seedHarnessUser(t, h, "authz-human-maintainer", false)
	if err := h.Svc.AddCollaborator(ctx, repo.ID, maintainer.ID, "write"); err != nil {
		t.Fatalf("AddCollaborator(write): %v", err)
	}
	w = h.DoRESTJSONWithToken(t, "POST", "/api/v3/operations/authorize", maintainerToken, request)
	if w.Code != http.StatusOK {
		t.Fatalf("expected native maintainer permission to authorize without PrincipalSessions, got %d: %s", w.Code, w.Body.String())
	}

	reader, readerToken := seedHarnessUser(t, h, "authz-human-reader-no-merge", false)
	if err := h.Svc.AddCollaborator(ctx, repo.ID, reader.ID, "read"); err != nil {
		t.Fatalf("AddCollaborator(read): %v", err)
	}
	w = h.DoRESTJSONWithToken(t, "POST", "/api/v3/operations/authorize", readerToken, request)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected native read permission to deny pr.merge, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_DurableAgentRequiresConfiguredResourcePolicy(t *testing.T) {
	h := testharness.New(t)
	if err := h.DB.Model(&h.User).Update("user_kind", "agent").Error; err != nil {
		t.Fatalf("mark durable authorization fixture as agent: %v", err)
	}
	h.User.UserKind = "agent"
	repo, err := h.Svc.CreateRepo(context.Background(), service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-unconfigured-agent", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", map[string]any{
		"contract_revision": sessionauthority.ContractRevision,
		"resource":          map[string]string{"service": "ags", "repository": repo.FullName},
		"operation":         map[string]any{"name": "ci.read", "constraints": durableReadConstraints("ci.read")},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected durable agent 403 without resource policy, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_UnknownRepository(t *testing.T) {
	h := testharness.New(t)
	w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", map[string]any{
		"contract_revision": sessionauthority.ContractRevision,
		"resource":          map[string]string{"service": "ags", "repository": "nobody/missing-repo"},
		"operation":         map[string]any{"name": "ci.read", "constraints": durableReadConstraints("ci.read")},
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown repository, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeOperation_ReadPermissionForAllReadOperations(t *testing.T) {
	h := testharness.New(t)
	repository, err := h.Svc.CreateRepo(context.Background(), service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-all-read", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	configureDurableAuthorizationAuthority(t, h, repository.FullName, "all-read-policy-v1")
	for _, op := range []string{"repo.read", "pr.read", "ci.read", "review.read", "git.read"} {
		t.Run(op, func(t *testing.T) {
			w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", map[string]any{
				"contract_revision": sessionauthority.ContractRevision,
				"resource":          map[string]string{"service": "ags", "repository": repository.FullName},
				"operation":         map[string]any{"name": op, "constraints": durableReadConstraints(op)},
			})
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 for %s, got %d: %s", op, w.Code, w.Body.String())
			}
		})
	}
}

func TestAuthorizeOperation_RejectsSecretShapedRequestValuesWithoutEchoing(t *testing.T) {
	h := testharness.New(t)
	repo, err := h.Svc.CreateRepo(context.Background(), service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-secret-values", AutoInit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	configureDurableAuthorizationAuthority(t, h, repo.FullName, "secret-values-policy-v1")

	cases := []struct {
		name   string
		secret string
		body   func(string) map[string]any
	}{
		{"mat operation", strings.Join([]string{"mat", "_durable_sentinel"}, ""), func(secret string) map[string]any {
			return authorizeOperationRequest(repo.FullName, secret, nil)
		}},
		{"session operation", strings.Join([]string{"ags", "_sess_durable_sentinel"}, ""), func(secret string) map[string]any {
			return authorizeOperationRequest(repo.FullName, secret, nil)
		}},
		{"jwt operation", strings.Join([]string{"ey", "JhbGciOiJIUzI1NiJ9", ".eyJzdWIiOiJ4In0", ".signature"}, ""), func(secret string) map[string]any {
			return authorizeOperationRequest(repo.FullName, secret, nil)
		}},
		{"private key operation", strings.Join([]string{"-----BE", "GIN PRIVATE KEY-----"}, ""), func(secret string) map[string]any {
			return authorizeOperationRequest(repo.FullName, secret, nil)
		}},
		{"constraint", strings.Join([]string{"mat", "_constraint_sentinel"}, ""), func(secret string) map[string]any {
			return authorizeOperationRequest(repo.FullName, "ci.read", map[string]any{"pull_request_number": 1, "forgejo_pull_request_number": 2, "head_sha": secret})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", tc.body(tc.secret))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), tc.secret) {
				t.Fatalf("response leaked rejected request value: %s", w.Body.String())
			}
		})
	}
}

func durableMergeConstraints() map[string]any {
	return map[string]any{
		"pull_request_number":         1,
		"forgejo_pull_request_number": 2,
		"merge_method":                "rebase",
		"expected_head_sha":           strings.Repeat("a", 40),
	}
}

func withoutConstraint(input map[string]any, key string) map[string]any {
	out := make(map[string]any, len(input)-1)
	for candidate, value := range input {
		if candidate != key {
			out[candidate] = value
		}
	}
	return out
}

func durableReadConstraints(operation string) map[string]any {
	switch operation {
	case "pr.read":
		return map[string]any{"pull_request_number": 1}
	case "review.read":
		return map[string]any{"pull_request_number": 1, "forgejo_pull_request_number": 2}
	case "ci.read":
		return map[string]any{"pull_request_number": 1, "forgejo_pull_request_number": 2, "head_sha": strings.Repeat("a", 40)}
	default:
		return map[string]any{}
	}
}

func authorizeOperationRequest(repository, operation string, constraints map[string]any) map[string]any {
	return map[string]any{
		"contract_revision": sessionauthority.ContractRevision,
		"resource":          map[string]string{"service": "ags", "repository": repository},
		"operation":         map[string]any{"name": operation, "constraints": constraints},
	}
}

func TestAuthorizeOperation_UnsupportedOperation(t *testing.T) {
	h := testharness.New(t)
	repo, err := h.Svc.CreateRepo(context.Background(), service.CreateRepoInput{
		OwnerLogin: h.User.Login, Name: "authz-unsupported", AutoInit: true,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	configureDurableAuthorizationAuthority(t, h, repo.FullName, "unsupported-policy-v1")
	w := h.DoRESTJSON(t, "POST", "/api/v3/operations/authorize", map[string]any{
		"contract_revision": sessionauthority.ContractRevision,
		"resource":          map[string]string{"service": "ags", "repository": repo.FullName},
		"operation":         map[string]any{"name": "unknown.op"},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for unsupported operation, got %d: %s", w.Code, w.Body.String())
	}
}
