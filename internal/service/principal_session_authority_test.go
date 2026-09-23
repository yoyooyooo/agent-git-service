package service_test

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

func TestExplainPrincipalSessionAuthorityVerifiesBindingPrincipalNativeGrantAndOperation(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	seedDelegationPrincipalAndRepo(t, svc, db.UserStatusActive, "write")
	principal, err := svc.GetUser(context.Background(), "automation-principal")
	if err != nil {
		t.Fatal(err)
	}
	svc.PrincipalSessions = principalSessionSet(t, principal.ID, sessionauthority.PrincipalBinding{
		ID: "binding-agent-1", IssuerInstanceID: "multica-mini", Subject: "agent-1", PrincipalID: principal.ID,
		Status: "active", BindingRevision: "binding-agent-1-v1",
	})

	report, err := svc.ExplainPrincipalSessionAuthority(context.Background(), service.PrincipalSessionAuthorityQuery{
		Issuer: "multica", IssuerInstanceID: "multica-mini", AssertionKeyID: "session-key", Subject: "agent-1",
		Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Operation: "pr.create",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Principal.ID != principal.ID || report.BindingID != "binding-agent-1" || report.NativeGrant != "write" {
		t.Fatalf("report = %#v", report)
	}
	if report.ContractRevision != sessionauthority.ContractRevision || report.Operation != "pr.create" || report.RequiredPermission != "write" {
		t.Fatalf("report = %#v", report)
	}
}

func TestExplainPrincipalSessionAuthorityReportsNativeGrantDenial(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	seedDelegationPrincipalAndRepo(t, svc, db.UserStatusActive, "read")
	principal, err := svc.GetUser(context.Background(), "automation-principal")
	if err != nil {
		t.Fatal(err)
	}
	svc.PrincipalSessions = principalSessionSet(t, principal.ID, sessionauthority.PrincipalBinding{
		ID: "binding-agent-1", IssuerInstanceID: "multica-mini", Subject: "agent-1", PrincipalID: principal.ID,
		Status: "active", BindingRevision: "binding-agent-1-v1",
	})
	report, err := svc.ExplainPrincipalSessionAuthority(context.Background(), service.PrincipalSessionAuthorityQuery{
		Issuer: "multica", IssuerInstanceID: "multica-mini", AssertionKeyID: "session-key", Subject: "agent-1",
		Target: "primary-a", Service: "ags", Repository: "operator/project-kit", Operation: "pr.create",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.NativeGrant != "read" {
		t.Fatalf("report = %#v", report)
	}
}
