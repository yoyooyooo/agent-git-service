package service_test

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/delegationpolicy"
	"github.com/ngaut/agent-git-service/internal/service"
)

func delegationSet(t *testing.T, status string, capabilities ...string) *delegationpolicy.Set {
	t.Helper()
	set, err := delegationpolicy.New(delegationpolicy.Config{
		Version: 1,
		Policies: []delegationpolicy.Policy{{
			ID:            "primary-a-multica-agent-kit-v1",
			Issuer:        "multica",
			WorkspaceID:   "11111111-1111-4111-8111-111111111111",
			Target:        "primary-a",
			Principal:     "automation-principal",
			MaxSessionTTL: "30m",
			Status:        status,
			PolicyVersion: "2026-07-14.1",
			Repositories: map[string]delegationpolicy.RepositoryPolicy{
				"operator/project-kit": {MaxCapabilities: capabilities},
			},
		}},
	})
	if err != nil {
		t.Fatalf("delegationpolicy.New: %v", err)
	}
	return set
}

func seedDelegationPrincipalAndRepo(t *testing.T, svc *service.Service, principalStatus, permission string) {
	t.Helper()
	owner := db.User{Login: "operator", Name: "Operator", Type: db.TypeUser, Status: db.UserStatusActive, UserKind: db.UserKindHuman}
	principal := db.User{Login: "automation-principal", Name: "Mini Workspace Principal", Type: db.TypeUser, Status: principalStatus, UserKind: db.UserKindAgent}
	for _, user := range []*db.User{&owner, &principal} {
		if err := svc.DB.Create(user).Error; err != nil {
			t.Fatalf("create %s: %v", user.Login, err)
		}
	}
	repo := db.Repository{Name: "project-kit", FullName: "operator/project-kit", OwnerID: owner.ID, Owner: owner, Private: true, DefaultBranch: "main"}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if permission != "none" {
		if err := svc.DB.Create(&db.Collaborator{RepositoryID: repo.ID, UserID: principal.ID, Permission: permission}).Error; err != nil {
			t.Fatalf("create collaborator: %v", err)
		}
	}
	if err := svc.DB.Create(&db.AgentBinding{HumanUserID: owner.ID, AgentUserID: principal.ID}).Error; err != nil {
		t.Fatalf("create agent binding: %v", err)
	}
}

func policyQuery() service.DelegationPolicyQuery {
	return service.DelegationPolicyQuery{
		Issuer:      "multica",
		WorkspaceID: "11111111-1111-4111-8111-111111111111",
		Target:      "primary-a",
		Repository:  "operator/project-kit",
	}
}

func TestExplainDelegationPolicyVerifiesPrincipalRepositoryAndGrant(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	svc.DelegationPolicies = delegationSet(t, "active", "repo:read", "repo:write", "pr:create")
	seedDelegationPrincipalAndRepo(t, svc, db.UserStatusActive, "write")

	report, err := svc.ExplainDelegationPolicy(context.Background(), policyQuery())
	if err != nil {
		t.Fatalf("ExplainDelegationPolicy: %v", err)
	}
	if !report.OK || report.Principal.Login != "automation-principal" || report.Principal.EffectivePermission != "write" {
		t.Fatalf("report = %#v", report)
	}
	if report.PolicyVersion != "2026-07-14.1" || report.ClaimLimit != "legacy delegation inventory verified; it cannot authorize principal/session-v2 exchange" {
		t.Fatalf("report contract = %#v", report)
	}
	requiredChecks := map[string]bool{"policy_match": false, "policy_active": false, "principal_exists": false, "principal_active": false, "repository_exists": false, "principal_repo_permission": false}
	for _, check := range report.Checks {
		if _, ok := requiredChecks[check.ID]; ok && check.Status == "pass" {
			requiredChecks[check.ID] = true
		}
	}
	for id, passed := range requiredChecks {
		if !passed {
			t.Fatalf("required check %s missing/pass=false: %#v", id, report.Checks)
		}
	}
}

func TestExplainDelegationPolicyFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		principal  string
		permission string
		query      service.DelegationPolicyQuery
		wantCheck  string
	}{
		{"disabled policy", "disabled", db.UserStatusActive, "write", policyQuery(), "policy_active"},
		{"inactive principal", "active", db.UserStatusSuspended, "write", policyQuery(), "principal_active"},
		{"grant insufficient", "active", db.UserStatusActive, "read", policyQuery(), "principal_repo_permission"},
		{"unknown repository", "active", db.UserStatusActive, "write", service.DelegationPolicyQuery{Issuer: "multica", WorkspaceID: "11111111-1111-4111-8111-111111111111", Target: "primary-a", Repository: "operator/other"}, "policy_match"},
		{"unknown workspace", "active", db.UserStatusActive, "write", service.DelegationPolicyQuery{Issuer: "multica", WorkspaceID: "other", Target: "primary-a", Repository: "operator/project-kit"}, "policy_match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, cleanup := setupTestService(t)
			defer cleanup()
			svc.DelegationPolicies = delegationSet(t, tt.status, "repo:read", "repo:write", "pr:create")
			seedDelegationPrincipalAndRepo(t, svc, tt.principal, tt.permission)
			report, err := svc.ExplainDelegationPolicy(context.Background(), tt.query)
			if err != nil {
				t.Fatalf("ExplainDelegationPolicy: %v", err)
			}
			if report.OK {
				t.Fatalf("report unexpectedly OK: %#v", report)
			}
			found := false
			for _, check := range report.Checks {
				if check.ID == tt.wantCheck && check.Status == "fail" {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing failed check %q in %#v", tt.wantCheck, report.Checks)
			}
		})
	}
}
