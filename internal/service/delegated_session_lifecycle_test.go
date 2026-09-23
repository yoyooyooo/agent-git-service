package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/operationconstraints"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness/testdb"
	"github.com/ngaut/agent-git-service/internal/workloadidentity"
	"gorm.io/gorm"
)

func TestDelegatedSessionLifecycleReadbackStatesAndClosedSchema(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	admin := createLifecycleAdmin(t, svc)
	principal, repository := lifecyclePrincipalAndRepository(t, svc)
	now := time.Now().UTC().Truncate(time.Second)
	revokedAt := now.Add(-2 * time.Minute)
	auditedAt := now.Add(-time.Minute)

	revokedAfterAuditAt := now.Add(-30 * time.Second)
	tests := []struct {
		name            string
		expiresAt       time.Time
		revokedAt       *time.Time
		expiryAuditedAt *time.Time
		wantState       string
	}{
		{name: "active", expiresAt: now.Add(time.Hour), wantState: "active"},
		{name: "revoked wins over elapsed ttl", expiresAt: now.Add(-time.Hour), revokedAt: &revokedAt, wantState: "revoked"},
		{name: "expired unaudited", expiresAt: now.Add(-time.Hour), wantState: "expired"},
		{name: "expired audited", expiresAt: now.Add(-time.Hour), expiryAuditedAt: &auditedAt, wantState: "expired"},
		{name: "operator revoke preserves prior expiry audit", expiresAt: now.Add(-time.Hour), revokedAt: &revokedAfterAuditAt, expiryAuditedAt: &auditedAt, wantState: "revoked"},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := createLifecycleSession(t, svc, principal, repository, index, tc.expiresAt, tc.revokedAt, tc.expiryAuditedAt)
			got, err := svc.GetDelegatedSessionLifecycle(service.ContextWithUser(context.Background(), admin), session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Schema != service.DelegatedSessionLifecycleSchema || got.SessionID != session.ID || got.State != tc.wantState {
				t.Fatalf("unexpected lifecycle identity/state: %+v", got)
			}
			if !got.CreatedAt.Equal(session.CreatedAt) || !got.ExpiresAt.Equal(session.ExpiresAt) {
				t.Fatalf("timestamps changed: got=%s/%s want=%s/%s", got.CreatedAt, got.ExpiresAt, session.CreatedAt, session.ExpiresAt)
			}
			if (got.RevokedAt == nil) != (tc.revokedAt == nil) || (got.ExpiryAuditedAt == nil) != (tc.expiryAuditedAt == nil) {
				t.Fatalf("optional lifecycle markers mismatch: %+v", got)
			}
			if got.Authority.Principal.ID != principal.ID || got.Authority.Principal.Login != principal.Login ||
				got.Authority.TeamIdentityID != "11111111-1111-4111-8111-111111111111" || got.Authority.PolicyClass != "multica.workspace.default.v1" || got.Authority.MembershipEpoch != 3 ||
				got.Authority.Target.Repository != repository.FullName || got.Authority.Resource.Service != "ags" || got.Authority.Operation.Name != "repo.read" || got.Authority.Operation.Constraints != nil ||
				got.Authority.AuthorizationBasis.TrustRevision != "trust-v1" || got.Authority.AuthorizationBasis.NativeGrantRevision != "grant-rev:05c236315cbdbe2cd9f8255a706e75ec5cd84d1fe2949f4e901b27e731801582" ||
				got.Authority.AuthorizationBasis.ResourcePolicyRevision != "resource-v1" || got.Authority.AuthorizationBasis.PolicyClassRevision != "policy-v1" {
				t.Fatalf("authority projection mismatch: %+v", got.Authority)
			}
			if got.Provenance.WorkspaceID != "22222222-2222-4222-8222-222222222222" || got.Provenance.AgentID != "33333333-3333-4333-8333-333333333333" ||
				got.Provenance.SquadID != "44444444-4444-4444-8444-444444444444" || got.Provenance.TaskID != "66666666-6666-4666-8666-666666666666" ||
				got.Provenance.RunID != "77777777-7777-4777-8777-777777777777" {
				t.Fatalf("provenance mismatch: %+v", got.Provenance)
			}
			if got.ClaimLimit != service.DelegatedSessionLifecycleClaimLimit {
				t.Fatalf("claim_limit=%q", got.ClaimLimit)
			}

			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatal(err)
			}
			wantTopLevel := []string{"authority", "claim_limit", "created_at", "expires_at", "provenance", "schema", "session_id", "state"}
			if tc.revokedAt != nil {
				wantTopLevel = append(wantTopLevel, "revoked_at")
			}
			if tc.expiryAuditedAt != nil {
				wantTopLevel = append(wantTopLevel, "expiry_audited_at")
			}
			sort.Strings(wantTopLevel)
			assertLifecycleJSONKeys(t, object, wantTopLevel)
			authority := object["authority"].(map[string]any)
			assertLifecycleJSONKeys(t, authority, []string{"authorization_basis", "contract_revision", "issuer_instance_id", "membership_epoch", "operation", "policy_class", "principal", "resource", "target", "team_binding_revision", "team_identity_id"})
			assertLifecycleJSONKeys(t, authority["principal"].(map[string]any), []string{"id", "login"})
			assertLifecycleJSONKeys(t, authority["target"].(map[string]any), []string{"instance", "repository"})
			assertLifecycleJSONKeys(t, authority["resource"].(map[string]any), []string{"repository", "service"})
			assertLifecycleJSONKeys(t, authority["operation"].(map[string]any), []string{"name"})
			assertLifecycleJSONKeys(t, authority["authorization_basis"].(map[string]any), []string{"native_grant_revision", "policy_class_revision", "requested_operation_scope", "resource_policy_revision", "trust_revision"})
			assertLifecycleJSONKeys(t, object["provenance"].(map[string]any), []string{"agent_id", "correlation_id", "issue_id", "issue_key", "run_id", "runtime_id", "schema", "squad_id", "task_id", "trigger_id", "workspace_id"})
			for _, forbidden := range []string{"credential", "assertion", "bearer", "token", "hash", "prefix", "fingerprint", "issuer_subject", "raw_audit"} {
				if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
					t.Fatalf("response contains forbidden field/value %q: %s", forbidden, encoded)
				}
			}
		})
	}
}

func TestDelegatedSessionLifecycleAuthorizationAndSecretRedaction(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	admin := createLifecycleAdmin(t, svc)
	nonAdmin := db.User{Login: "lifecycle-reader", Name: "lifecycle-reader", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := svc.DB.Create(&nonAdmin).Error; err != nil {
		t.Fatal(err)
	}
	principal, repository := lifecyclePrincipalAndRepository(t, svc)
	session := createLifecycleSession(t, svc, principal, repository, 20, time.Now().UTC().Add(time.Hour), nil, nil)
	if err := svc.DB.Model(&db.DelegatedAgentSession{}).Where("id = ?", session.ID).Updates(map[string]any{
		"team_identity_id": "ags_sess_should-not-leak", "external_agent_id": "mat_should-not-leak", "correlation_id": "eyJaaa.bbb.ccc",
		"issuer_subject": "assertion-subject-must-not-be-read",
	}).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := svc.GetDelegatedSessionLifecycle(context.Background(), session.ID); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("missing viewer error=%v", err)
	}
	if _, err := svc.GetDelegatedSessionLifecycle(service.ContextWithUser(context.Background(), nonAdmin), session.ID); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("non-admin error=%v", err)
	}
	delegatedAdmin := service.ContextWithDelegatedSession(service.ContextWithUser(context.Background(), admin), session)
	if _, err := svc.GetDelegatedSessionLifecycle(delegatedAdmin, session.ID); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("delegated credential reached operator surface: %v", err)
	}
	if _, err := svc.GetDelegatedSessionLifecycle(service.ContextWithUser(context.Background(), admin), uuid.NewString()); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown session error=%v", err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("site_admin", false).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetDelegatedSessionLifecycle(service.ContextWithUser(context.Background(), admin), session.ID); !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("stale context retained removed site-admin authority: %v", err)
	}
	if err := svc.DB.Model(&db.User{}).Where("id = ?", admin.ID).Update("site_admin", true).Error; err != nil {
		t.Fatal(err)
	}
	_, err := svc.GetDelegatedSessionLifecycle(service.ContextWithUser(context.Background(), admin), session.ID)
	if !errors.Is(err, service.ErrValidation) {
		t.Fatalf("noncanonical secret-shaped row error=%v", err)
	}
	for _, forbidden := range []string{"ags_sess_should-not-leak", "mat_should-not-leak", "eyJaaa.bbb.ccc", "assertion-subject-must-not-be-read", session.CredentialHash, session.CredentialPrefix} {
		if strings.Contains(fmt.Sprint(err), forbidden) {
			t.Fatalf("validation error leaked secret-shaped value %q: %v", forbidden, err)
		}
	}
}

func TestDelegatedSessionLifecycleRejectsNoncanonicalRowsAndUnsafeConstraints(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	testdb.DiscardOnReturn(t, svc.DB) // The maximum-ID probe permanently advances TiDB's allocator.
	admin := createLifecycleAdmin(t, svc)
	principal, repository := lifecyclePrincipalAndRepository(t, svc)
	now := time.Now().UTC().Truncate(time.Second)
	oversizePrincipal := db.User{ID: uint(operationconstraints.MaxJSONSafePositiveInteger + 1), Login: "lifecycle-oversize-principal", Name: "lifecycle-oversize-principal", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := svc.DB.Create(&oversizePrincipal).Error; err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		updates map[string]any
	}{
		{name: "non-team-v4 contract", updates: map[string]any{"contract_revision": "legacy-v2"}},
		{name: "missing required provenance", updates: map[string]any{"external_run_id": ""}},
		{name: "dangling principal", updates: map[string]any{"principal_user_id": uint(999999)}},
		{name: "dangling repository", updates: map[string]any{"repository_id": uint(999999)}},
		{name: "principal exceeds JSON safe integer", updates: map[string]any{"principal_user_id": oversizePrincipal.ID}},
		{name: "membership epoch exceeds JSON safe integer", updates: map[string]any{"membership_epoch": operationconstraints.MaxJSONSafePositiveInteger + 1}},
		{name: "unsafe operation constraint key"},
		{name: "invalid timestamp order", updates: map[string]any{"created_at": now.Add(time.Hour), "expires_at": now}},
		{name: "expiry marker before expiry", updates: map[string]any{"expires_at": now.Add(-time.Minute), "expiry_audited_at": now.Add(-2 * time.Minute)}},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := createLifecycleSession(t, svc, principal, repository, 60+index, now.Add(time.Hour), nil, nil)
			if tc.name == "unsafe operation constraint key" {
				session.OperationConstraints = map[string]string{"password": "plain-secret"}
				if err := svc.DB.Save(&session).Error; err != nil {
					t.Fatal(err)
				}
			} else if strings.HasPrefix(tc.name, "dangling ") {
				testdb.MutateWithoutForeignKeys(t, svc.DB, func(conn *gorm.DB) error {
					return conn.Model(&db.DelegatedAgentSession{}).Where("id = ?", session.ID).Updates(tc.updates).Error
				})
			} else if err := svc.DB.Model(&db.DelegatedAgentSession{}).Where("id = ?", session.ID).Updates(tc.updates).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := svc.GetDelegatedSessionLifecycle(service.ContextWithUser(context.Background(), admin), session.ID); !errors.Is(err, service.ErrValidation) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestDelegatedSessionLifecycleAcceptsJSONSafeIntegerMaximum(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	testdb.DiscardOnReturn(t, svc.DB)
	admin := createLifecycleAdmin(t, svc)
	principal, repository := lifecyclePrincipalAndRepository(t, svc)
	now := time.Now().UTC().Truncate(time.Second)

	maxPrincipal := db.User{ID: uint(operationconstraints.MaxJSONSafePositiveInteger), Login: "lifecycle-max-safe-principal", Name: "lifecycle-max-safe-principal", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := svc.DB.Create(&maxPrincipal).Error; err != nil {
		t.Fatal(err)
	}
	principalSession := createLifecycleSession(t, svc, maxPrincipal, repository, 89, now.Add(time.Hour), nil, nil)
	if _, err := svc.GetDelegatedSessionLifecycle(service.ContextWithUser(context.Background(), admin), principalSession.ID); err != nil {
		t.Fatalf("maximum safe principal rejected: %v", err)
	}

	epochSession := createLifecycleSession(t, svc, principal, repository, 90, now.Add(time.Hour), nil, nil)
	if err := svc.DB.Model(&db.DelegatedAgentSession{}).Where("id = ?", epochSession.ID).Update("membership_epoch", operationconstraints.MaxJSONSafePositiveInteger).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetDelegatedSessionLifecycle(service.ContextWithUser(context.Background(), admin), epochSession.ID); err != nil {
		t.Fatalf("maximum safe membership epoch rejected: %v", err)
	}
}

func TestDelegatedSessionLifecycleConcurrentRepeatedReadIsStable(t *testing.T) {
	svc, cleanup := setupDelegatedSessionService(t)
	defer cleanup()
	admin := createLifecycleAdmin(t, svc)
	principal, repository := lifecyclePrincipalAndRepository(t, svc)
	auditedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	session := createLifecycleSession(t, svc, principal, repository, 40, time.Now().UTC().Add(-time.Hour), nil, &auditedAt)
	ctx := service.ContextWithUser(context.Background(), admin)

	const readers = 12
	outputs := make(chan string, readers)
	errs := make(chan error, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := svc.GetDelegatedSessionLifecycle(ctx, session.ID)
			if err != nil {
				errs <- err
				return
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				errs <- err
				return
			}
			outputs <- string(encoded)
		}()
	}
	wg.Wait()
	close(outputs)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var first string
	count := 0
	for output := range outputs {
		if first == "" {
			first = output
		} else if output != first {
			t.Fatalf("concurrent lifecycle read drifted:\nfirst=%s\nother=%s", first, output)
		}
		count++
	}
	if count != readers {
		t.Fatalf("read count=%d", count)
	}
}

func TestDelegatedSessionLifecycleFrozenProducerFixture(t *testing.T) {
	createdAt := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 7, 28, 0, 1, 0, 0, time.UTC)
	auditedAt := time.Date(2026, 7, 28, 0, 2, 0, 0, time.UTC)
	readback := service.DelegatedSessionLifecycleReadback{
		Schema:    service.DelegatedSessionLifecycleSchema,
		SessionID: "11111111-1111-4111-8111-111111111111", State: "expired",
		CreatedAt: createdAt, ExpiresAt: expiresAt, ExpiryAuditedAt: &auditedAt,
		Authority: service.DelegatedSessionLifecycleAuthority{
			ContractRevision: "2026-07-24.team-authority-v4",
			Principal:        service.DelegatedSessionPrincipal{ID: 7, Login: "mini-team-principal"},
			TeamIdentityID:   "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", TeamBindingRevision: "2026-07-25.mini-team-binding-1",
			PolicyClass: "multica.workspace.default.v1", MembershipEpoch: 1, IssuerInstanceID: "multica-mini",
			Target:    service.DelegatedSessionTarget{Instance: "primary-a", Repository: "operator/project-kit"},
			Resource:  service.DelegatedSessionResource{Service: "ags", Repository: "operator/project-kit"},
			Operation: service.DelegatedSessionOperation{Name: "repo.read"},
			AuthorizationBasis: service.DelegatedSessionAuthorizationBasis{
				TrustRevision:           "2026-07-25.mini-team-authority-1",
				NativeGrantRevision:     "grant-rev:05c236315cbdbe2cd9f8255a706e75ec5cd84d1fe2949f4e901b27e731801582",
				ResourcePolicyRevision:  "2026-07-25.mini-resource-policy-1",
				PolicyClassRevision:     "2026-07-25.mini-default-dynamic-1",
				RequestedOperationScope: "repo.read:operator/project-kit",
			},
		},
		Provenance: service.DelegatedSessionLifecycleProvenance{
			Schema: "workload.context.v1", WorkspaceID: "11111111-1111-4111-8111-111111111111",
			AgentID: "b92d04cf-76fa-492c-afad-12d4b149743e", SquadID: "ba82b0c4-3488-47c0-be4a-39976d689b1a",
			IssueID: "22222222-2222-4222-8222-222222222222", IssueKey: "MINI-989",
			TaskID: "33333333-3333-4333-8333-333333333333", RunID: "44444444-4444-4444-8444-444444444444",
			CorrelationID: "55555555-5555-4555-8555-555555555555", TriggerID: "66666666-6666-4666-8666-666666666666",
			RuntimeID: "77777777-7777-4777-8777-777777777777",
		},
		ClaimLimit: service.DelegatedSessionLifecycleClaimLimit,
	}
	got, err := json.MarshalIndent(readback, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "delegated-session-lifecycle-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("producer fixture drifted:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func createLifecycleAdmin(t *testing.T, svc *service.Service) db.User {
	t.Helper()
	admin := db.User{Login: "lifecycle-admin", Name: "lifecycle-admin", Type: db.TypeUser, Status: db.UserStatusActive, SiteAdmin: true}
	if err := svc.DB.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	return admin
}

func lifecyclePrincipalAndRepository(t *testing.T, svc *service.Service) (db.User, db.Repository) {
	t.Helper()
	var principal db.User
	if err := svc.DB.Where("login = ?", "automation-principal").First(&principal).Error; err != nil {
		t.Fatal(err)
	}
	var repository db.Repository
	if err := svc.DB.Where("full_name = ?", "operator/project-kit").First(&repository).Error; err != nil {
		t.Fatal(err)
	}
	return principal, repository
}

func createLifecycleSession(t *testing.T, svc *service.Service, principal db.User, repository db.Repository, index int, expiresAt time.Time, revokedAt, expiryAuditedAt *time.Time) db.DelegatedAgentSession {
	t.Helper()
	createdAt := expiresAt.Add(-30 * time.Minute).UTC()
	if createdAt.After(time.Now().UTC().Add(-time.Minute)) {
		createdAt = time.Now().UTC().Add(-time.Minute)
	}
	session := db.DelegatedAgentSession{
		ID: uuid.NewString(), CredentialHash: fmt.Sprintf("%064d", index+1), CredentialPrefix: fmt.Sprintf("proof-%d", index),
		PrincipalUserID: principal.ID, PrincipalLogin: principal.Login,
		Issuer: "urn:multica:deployment:mini", AssertionVersion: 1, AssertionPurpose: "ags_session_exchange",
		AssertionJTI: fmt.Sprintf("lifecycle-jti-%d", index), AssertionAudience: "urn:ags:workload-session-exchange:v1", AssertionKeyID: "key-v1",
		ContractRevision: "2026-07-24.team-authority-v4", CredentialMode: "delegated_session",
		TeamIdentityID: "11111111-1111-4111-8111-111111111111", TeamBindingRevision: "team-binding-v1", PolicyClass: "multica.workspace.default.v1", MembershipEpoch: 3,
		IssuerInstanceID: "multica-mini", TrustRevision: "trust-v1", IssuerWorkspaceID: "22222222-2222-4222-8222-222222222222",
		ExternalAgentID: "33333333-3333-4333-8333-333333333333", ExternalSquadID: "44444444-4444-4444-8444-444444444444", ExternalIssueID: "55555555-5555-4555-8555-555555555555", ExternalIssueKey: "MINI-1",
		ExternalTaskID: "66666666-6666-4666-8666-666666666666", ExternalRunID: "77777777-7777-4777-8777-777777777777", CorrelationID: "88888888-8888-4888-8888-888888888888", ExternalTriggerID: "99999999-9999-4999-8999-999999999999", ExternalRuntimeID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		WorkloadContextSchema: workloadidentity.WorkloadContextSchema,
		TargetInstance:        "primary-a", ResourceService: "ags", RepositoryID: repository.ID, OperationName: "repo.read", OperationConstraints: map[string]string{},
		GrantedCapabilities: []string{"repo:read"}, PolicyVersion: "policy-v1", PolicySnapshotHash: strings.Repeat("a", 64),
		NativeGrantRevision: "grant-rev:05c236315cbdbe2cd9f8255a706e75ec5cd84d1fe2949f4e901b27e731801582", ResourcePolicyRevision: "resource-v1",
		CreatedAt: createdAt, ExpiresAt: expiresAt.UTC(), RevokedAt: revokedAt, ExpiryAuditedAt: expiryAuditedAt,
	}
	if err := svc.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	// Compare readback with the actual stored timestamp precision, not the
	// higher precision of the pre-insert Go clock value.
	if err := svc.DB.First(&session, "id = ?", session.ID).Error; err != nil {
		t.Fatal(err)
	}
	return session
}

func lifecycleJSONKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func assertLifecycleJSONKeys(t *testing.T, value map[string]any, want []string) {
	t.Helper()
	got := lifecycleJSONKeys(value)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("JSON keys=%v want=%v", got, want)
	}
}
