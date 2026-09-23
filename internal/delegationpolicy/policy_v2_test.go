package delegationpolicy_test

import (
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/delegationpolicy"
)

const (
	workspaceID   = "11111111-1111-4111-8111-111111111111"
	implementerID = "33333333-3333-4333-8333-333333333333"
)

func validV2Config() delegationpolicy.Config {
	return delegationpolicy.Config{
		Version: 2,
		Mappings: []delegationpolicy.AgentMapping{{
			ID:              "primary-a-agent-git-service-implementer-b-v2",
			Issuer:          "multica",
			WorkspaceID:     workspaceID,
			AgentID:         implementerID,
			Role:            "implementer-b",
			Target:          "primary-a",
			Repository:      "operator/agent-git-service",
			Principal:       "automation-principal",
			MaxCapabilities: []string{"repo:read", "repo:write", "pr:create"},
			MaxSessionTTL:   "15m",
			AllowMerge:      false,
			Status:          "active",
			PolicyVersion:   "2026-07-17.1",
		}},
	}
}

func TestV2ResolvesOnlyTheSignedImmutableAgentSelector(t *testing.T) {
	set, err := delegationpolicy.New(validV2Config())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resolved, err := set.Resolve(delegationpolicy.Selector{
		Issuer: "multica", WorkspaceID: workspaceID, AgentID: implementerID,
		TaskID: "task-1", Target: "primary-a", Repository: "operator/agent-git-service",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Mapping.AgentID != implementerID || resolved.Mapping.Role != "implementer-b" {
		t.Fatalf("resolved mapping = %#v", resolved.Mapping)
	}

	for _, selector := range []delegationpolicy.Selector{
		{Issuer: "multica", WorkspaceID: workspaceID, AgentID: "44444444-4444-4444-8444-444444444444", TaskID: "task-1", Target: "primary-a", Repository: "operator/agent-git-service"},
		{Issuer: "multica", WorkspaceID: workspaceID, AgentID: implementerID, TaskID: "task-1", Target: "primary-b", Repository: "operator/agent-git-service"},
		{Issuer: "multica", WorkspaceID: workspaceID, AgentID: implementerID, TaskID: "task-1", Target: "primary-a", Repository: "operator/other"},
	} {
		if _, err := set.Resolve(selector); err == nil {
			t.Fatalf("Resolve(%#v) unexpectedly succeeded", selector)
		}
	}
}

func TestV2FailsClosedForInvalidOrWidenedMappings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*delegationpolicy.Config)
		want   string
	}{
		{"unknown schema", func(c *delegationpolicy.Config) { c.Version = 99 }, "unsupported delegation policy version"},
		{"v1 v2 conflict", func(c *delegationpolicy.Config) {
			c.Policies = []delegationpolicy.Policy{{ID: "legacy"}}
		}, "cannot coexist"},
		{"unknown role", func(c *delegationpolicy.Config) { c.Mappings[0].Role = "maintainer" }, "unknown role"},
		{"malformed agent id", func(c *delegationpolicy.Config) { c.Mappings[0].AgentID = "example-implementer-b" }, "canonical UUID"},
		{"nil agent id", func(c *delegationpolicy.Config) { c.Mappings[0].AgentID = "00000000-0000-0000-0000-000000000000" }, "canonical UUID"},
		{"duplicate selector", func(c *delegationpolicy.Config) {
			duplicate := c.Mappings[0]
			duplicate.ID = "duplicate-selector"
			c.Mappings = append(c.Mappings, duplicate)
		}, "duplicate or ambiguous delegation selector"},
		{"merge widening", func(c *delegationpolicy.Config) { c.Mappings[0].AllowMerge = true }, "allow_merge must be false"},
		{"capability widening", func(c *delegationpolicy.Config) {
			c.Mappings[0].Role = "critic"
			c.Mappings[0].MaxCapabilities = []string{"repo:read", "repo:write"}
		}, "widens role"},
		{"ttl widening", func(c *delegationpolicy.Config) { c.Mappings[0].MaxSessionTTL = "31m" }, "widens role"},
		{"duplicate capability", func(c *delegationpolicy.Config) {
			c.Mappings[0].MaxCapabilities = append(c.Mappings[0].MaxCapabilities, "repo:read")
		}, "duplicate capability"},
		{"ci repair without observed task", func(c *delegationpolicy.Config) {
			c.Mappings[0].Role = "ci-repair"
			c.Mappings[0].MaxSessionTTL = "15m"
		}, "requires exact observed shared-failure task_ids"},
		{"non repair task constraint", func(c *delegationpolicy.Config) {
			c.Mappings[0].TaskIDs = []string{"8a7f9bba-838e-4f4b-ad7e-21e7e32b77dc"}
		}, "must not use task-scoped repair authority"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validV2Config()
			tt.mutate(&cfg)
			_, err := delegationpolicy.New(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestV2MigrationInventoryValidatesHistoricalRoleShape(t *testing.T) {
	roles := []struct {
		name         string
		capabilities []string
		allowed      bool
	}{
		{"implementer-a", []string{"repo:read", "repo:write", "pr:create"}, true},
		{"implementer-b", []string{"repo:read", "repo:write", "pr:create"}, true},
		{"critic", []string{"repo:read"}, true},
		{"critic", []string{"repo:read", "repo:write"}, false},
		{"coordinator", []string{"repo:read"}, true},
		{"coordinator", []string{"repo:write"}, false},
		{"planner", []string{"repo:read"}, true},
		{"planner", []string{"pr:create"}, false},
	}
	for _, role := range roles {
		t.Run(role.name+"/"+strings.Join(role.capabilities, "+"), func(t *testing.T) {
			cfg := validV2Config()
			cfg.Mappings[0].Role = role.name
			cfg.Mappings[0].MaxCapabilities = role.capabilities
			_, err := delegationpolicy.New(cfg)
			if role.allowed && err != nil {
				t.Fatalf("New: %v", err)
			}
			if !role.allowed && (err == nil || !strings.Contains(err.Error(), "widens role")) {
				t.Fatalf("error = %v, want role ceiling denial", err)
			}
		})
	}
}

func TestV2MigrationInventoryPreservesCIRepairTaskSelector(t *testing.T) {
	const observedTaskID = "8a7f9bba-838e-4f4b-ad7e-21e7e32b77dc"
	cfg := validV2Config()
	cfg.Mappings[0].AgentID = "66666666-6666-4666-8666-666666666661"
	cfg.Mappings[0].Role = "ci-repair"
	cfg.Mappings[0].TaskIDs = []string{observedTaskID}
	set, err := delegationpolicy.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	selector := delegationpolicy.Selector{
		Issuer: "multica", WorkspaceID: workspaceID, AgentID: cfg.Mappings[0].AgentID,
		TaskID: observedTaskID, Target: "primary-a", Repository: "operator/agent-git-service",
	}
	if _, err := set.Resolve(selector); err != nil {
		t.Fatalf("observed shared-failure task denied: %v", err)
	}
	selector.TaskID = "e8e187f3-5e62-4c92-9e04-0b22801d4619"
	if _, err := set.Resolve(selector); err == nil {
		t.Fatal("unlisted ci-repair task unexpectedly resolved")
	}
}
