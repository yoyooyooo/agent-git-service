package service_test

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

// setupDelegatedSessionService supports durable historical lifecycle and
// authority-boundary readback tests. It does not configure the retired workload
// assertion exchange and cannot mint a Session.
func setupDelegatedSessionService(t *testing.T) (*service.Service, func()) {
	t.Helper()
	svc, cleanup := setupTestService(t)
	svc.DelegationPolicies = delegationSet(t, "active", "repo:read", "repo:write", "pr:create")
	seedDelegationPrincipalAndRepo(t, svc, db.UserStatusActive, "write")
	principal, err := svc.GetUser(context.Background(), "automation-principal")
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	svc.PrincipalSessions = principalSessionSet(t, principal.ID,
		sessionauthority.PrincipalBinding{ID: "binding-agent-1", IssuerInstanceID: "multica-mini", Subject: "agent-1", PrincipalID: principal.ID, Status: "active", BindingRevision: "binding-agent-1-v1"},
		sessionauthority.PrincipalBinding{ID: "binding-implementer-b", IssuerInstanceID: "multica-mini", Subject: "33333333-3333-4333-8333-333333333333", PrincipalID: principal.ID, Status: "active", BindingRevision: "binding-implementer-b-v1"},
	)
	return svc, cleanup
}

func issuedAccessGrantTransportContext(t *testing.T, svc *service.Service) (string, context.Context, db.DelegatedAgentSession) {
	t.Helper()
	issued, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	transport, err := svc.IssueAccessGrantTransportSession(context.Background(), issued.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	principal, session, err := svc.ResolveDelegatedSessionCredential(context.Background(), transport.SessionToken)
	if err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithDelegatedSession(service.ContextWithUser(context.Background(), principal), session)
	return transport.SessionToken, ctx, session
}

func principalSessionSet(t *testing.T, principalID uint, bindings ...sessionauthority.PrincipalBinding) *sessionauthority.Set {
	t.Helper()
	set, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision, LegacyCompatibilityMode: sessionauthority.LegacySubjectCompatibilityMode,
		TrustedIssuers: []sessionauthority.TrustedIssuer{
			{ID: "multica-mini", Issuer: "multica", KeyIDs: []string{"session-key"}, Status: "active", TrustRevision: "trust-v1"},
		},
		Bindings: bindings,
		Resources: []sessionauthority.ResourcePolicy{
			{ID: "repo", Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "repo-policy-v1"},
		},
	})
	if err != nil {
		t.Fatalf("sessionauthority.New principal=%d: %v", principalID, err)
	}
	return set
}
