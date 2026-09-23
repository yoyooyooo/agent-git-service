package sessionauthority_test

import (
	"testing"

	"github.com/ngaut/agent-git-service/internal/delegationpolicy"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

func TestPlanLegacyDelegationTreatsV2RoleAndTasksAsMigrationOnly(t *testing.T) {
	legacy, err := delegationpolicy.New(delegationpolicy.Config{
		Version: 2,
		Mappings: []delegationpolicy.AgentMapping{
			{
				ID: "mini-ci-repair-v2", Issuer: "multica", WorkspaceID: "11111111-1111-4111-8111-111111111111",
				AgentID: "66666666-6666-4666-8666-666666666661", Role: "ci-repair",
				TaskIDs: []string{"bd9b1046-25fd-4a0e-a1f1-fb9d3d01349c"}, Target: "primary-a",
				Repository: "operator/agent-git-service", Principal: "automation-principal",
				MaxCapabilities: []string{"repo:read", "repo:write", "pr:create"}, MaxSessionTTL: "15m",
				Status: "active", PolicyVersion: "2026-07-17.1",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	plan := sessionauthority.PlanLegacyDelegation(legacy.Config())
	if len(plan.Candidates) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	candidate := plan.Candidates[0]
	if candidate.SourceSchemaVersion != 2 || candidate.Subject != "66666666-6666-4666-8666-666666666661" || candidate.PrincipalLogin != "automation-principal" {
		t.Fatalf("candidate = %#v", candidate)
	}
	if candidate.Executable {
		t.Fatal("legacy mapping must not be executable authority")
	}
	if len(candidate.IgnoredAuthorizationFields) != 4 || candidate.IgnoredAuthorizationFields[0] != "max_capabilities" || candidate.IgnoredAuthorizationFields[3] != "task_ids" {
		t.Fatalf("ignored fields = %#v", candidate.IgnoredAuthorizationFields)
	}
	if len(candidate.Blockers) == 0 {
		t.Fatal("migration must require explicit issuer instance, principal ID, and resource-policy decisions")
	}
}

func TestPlanLegacyDelegationCannotInferSubjectFromV1WorkspacePolicy(t *testing.T) {
	legacy, err := delegationpolicy.New(delegationpolicy.Config{
		Version: 1,
		Policies: []delegationpolicy.Policy{
			{
				ID: "workspace-v1", Issuer: "multica", WorkspaceID: "workspace-1", Target: "primary-a",
				Principal: "automation-principal", Repositories: map[string]delegationpolicy.RepositoryPolicy{
					"operator/project-kit": {MaxCapabilities: []string{"repo:read"}},
				},
				MaxSessionTTL: "30m", Status: "active", PolicyVersion: "legacy-v1",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := sessionauthority.PlanLegacyDelegation(legacy.Config())
	if len(plan.Candidates) != 1 || plan.Candidates[0].Subject != "" || plan.Candidates[0].Executable {
		t.Fatalf("plan = %#v", plan)
	}
}
