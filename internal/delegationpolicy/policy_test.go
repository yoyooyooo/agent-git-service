package delegationpolicy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/delegationpolicy"
)

func validConfig() delegationpolicy.Config {
	return delegationpolicy.Config{
		Version: 1,
		Policies: []delegationpolicy.Policy{{
			ID:            "primary-a-multica-project-kit-v1",
			Issuer:        " multica ",
			WorkspaceID:   " 11111111-1111-4111-8111-111111111111 ",
			Target:        " primary-a ",
			Principal:     " automation-principal ",
			MaxSessionTTL: "30m",
			AllowMerge:    false,
			Status:        "active",
			PolicyVersion: "2026-07-14.1",
			Repositories: map[string]delegationpolicy.RepositoryPolicy{
				" operator / project-kit ": {MaxCapabilities: []string{"pr:create", "repo:read", "repo:write", "pr:create"}},
			},
		}},
	}
}

func TestNewNormalizesAndResolvesByTrustKeyAndRepository(t *testing.T) {
	set, err := delegationpolicy.New(validConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resolved, err := set.Resolve(delegationpolicy.Selector{
		Issuer: "multica", WorkspaceID: "11111111-1111-4111-8111-111111111111",
		Target: "primary-a", Repository: "operator/project-kit",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Policy.Principal != "automation-principal" || resolved.Repository != "operator/project-kit" {
		t.Fatalf("resolved = %#v", resolved)
	}
	if got, want := resolved.Policy.MaxSessionDuration(), 30*time.Minute; got != want {
		t.Fatalf("max session ttl = %s, want %s", got, want)
	}
	got := strings.Join(resolved.RepositoryPolicy.MaxCapabilities, ",")
	if got != "pr:create,repo:read,repo:write" {
		t.Fatalf("capabilities = %q", got)
	}
}

func TestNewAllowsEmptyOptionalConfig(t *testing.T) {
	set, err := delegationpolicy.New(delegationpolicy.Config{})
	if err != nil {
		t.Fatalf("New empty: %v", err)
	}
	if len(set.Policies()) != 0 {
		t.Fatalf("empty policies = %#v", set.Policies())
	}
}

func TestNewFailsClosedForInvalidPolicy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*delegationpolicy.Config)
		want   string
	}{
		{"duplicate trust key", func(c *delegationpolicy.Config) {
			c.Policies = append(c.Policies, c.Policies[0])
			c.Policies[1].ID = "duplicate-id"
		}, "duplicate delegation trust key"},
		{"merge enabled", func(c *delegationpolicy.Config) { c.Policies[0].AllowMerge = true }, "allow_merge must be false"},
		{"ttl too long", func(c *delegationpolicy.Config) { c.Policies[0].MaxSessionTTL = "3h" }, "must not exceed 2h"},
		{"unknown capability", func(c *delegationpolicy.Config) {
			c.Policies[0].Repositories = map[string]delegationpolicy.RepositoryPolicy{"operator/project-kit": {MaxCapabilities: []string{"repo:read", "repo:delete"}}}
		}, "unsupported capability"},
		{"hard forbidden capability", func(c *delegationpolicy.Config) {
			c.Policies[0].Repositories = map[string]delegationpolicy.RepositoryPolicy{"operator/project-kit": {MaxCapabilities: []string{"pr:merge"}}}
		}, "hard-forbidden capability"},
		{"missing principal", func(c *delegationpolicy.Config) { c.Policies[0].Principal = " " }, "principal is required"},
		{"bad repository", func(c *delegationpolicy.Config) {
			c.Policies[0].Repositories = map[string]delegationpolicy.RepositoryPolicy{"project-kit": {MaxCapabilities: []string{"repo:read"}}}
		}, "owner/name"},
		{"bad status", func(c *delegationpolicy.Config) { c.Policies[0].Status = "paused" }, "status must be active or disabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			_, err := delegationpolicy.New(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestResolveFailsClosedForUnknownTrustOrRepository(t *testing.T) {
	set, err := delegationpolicy.New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct{ issuer, workspace, target, repo string }{
		{"other", "11111111-1111-4111-8111-111111111111", "primary-a", "operator/project-kit"},
		{"multica", "unknown", "primary-a", "operator/project-kit"},
		{"multica", "11111111-1111-4111-8111-111111111111", "other", "operator/project-kit"},
		{"multica", "11111111-1111-4111-8111-111111111111", "primary-a", "operator/other"},
	} {
		if _, err := set.Resolve(delegationpolicy.Selector{
			Issuer: input.issuer, WorkspaceID: input.workspace, Target: input.target, Repository: input.repo,
		}); err == nil {
			t.Fatalf("Resolve(%#v) unexpectedly succeeded", input)
		}
	}
}
