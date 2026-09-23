package integrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const delegationPolicyYAML = `
delegation:
  version: 1
  policies:
    - id: primary-a-multica-agent-kit-v1
      issuer: multica
      workspace_id: 11111111-1111-4111-8111-111111111111
      target: mini
      principal: automation-principal
      repositories:
        operator/project-kit:
          max_capabilities:
            - repo:read
            - repo:write
            - pr:read
            - pr:create
      max_session_ttl: 30m
      allow_merge: false
      status: active
      policy_version: 2026-07-14.1
`

const delegationPolicyV2YAML = `
delegation:
  version: 2
  mappings:
    - id: primary-a-agent-git-service-implementer-b-v2
      issuer: multica
      workspace_id: 11111111-1111-4111-8111-111111111111
      agent_id: 33333333-3333-4333-8333-333333333333
      role: implementer-b
      target: mini
      repository: operator/agent-git-service
      principal: automation-principal
      max_capabilities:
        - repo:read
        - repo:write
        - pr:create
      max_session_ttl: 15m
      allow_merge: false
      status: active
      policy_version: 2026-07-17.1
`

func TestLoadFileParsesDelegationPolicies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	if err := os.WriteFile(path, []byte(delegationPolicyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(cfg.Delegation.Policies) != 1 {
		t.Fatalf("policies = %#v", cfg.Delegation.Policies)
	}
	policy := cfg.Delegation.Policies[0]
	if policy.Principal != "automation-principal" || policy.Repositories["operator/project-kit"].MaxCapabilities[0] != "pr:create" {
		t.Fatalf("normalized policy = %#v", policy)
	}
}

func TestLoadFileParsesImmutableAgentIDDelegationMappings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	if err := os.WriteFile(path, []byte(delegationPolicyV2YAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Delegation.Version != 2 || len(cfg.Delegation.Mappings) != 1 || len(cfg.Delegation.Policies) != 0 {
		t.Fatalf("delegation config = %#v", cfg.Delegation)
	}
	mapping := cfg.Delegation.Mappings[0]
	if mapping.AgentID != "33333333-3333-4333-8333-333333333333" || mapping.Role != "implementer-b" || mapping.Repository != "operator/agent-git-service" {
		t.Fatalf("mapping = %#v", mapping)
	}
}

func TestLoadFileAllowsUnconfiguredDelegation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	if err := os.WriteFile(path, []byte("delegation: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Delegation.Version != 0 || len(cfg.Delegation.Policies) != 0 || len(cfg.Delegation.Mappings) != 0 {
		t.Fatalf("delegation config = %#v", cfg.Delegation)
	}
}

func TestLoadFileRejectsUnknownOrConflictingDelegationGenerations(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"explicit unknown version zero", "delegation:\n  version: 0\n", "unsupported delegation policy version 0"},
		{"v1 with mappings", strings.Replace(delegationPolicyV2YAML, "version: 2", "version: 1", 1), "cannot coexist"},
		{"v1 with empty mappings", "delegation:\n  version: 1\n  mappings: []\n", "cannot coexist"},
		{"v2 with policies", strings.Replace(delegationPolicyYAML, "version: 1", "version: 2", 1), "cannot coexist"},
		{"v2 with empty policies", "delegation:\n  version: 2\n  policies: []\n", "cannot coexist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "integrations.yaml")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFile(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadFileRejectsSecretsInsideDelegationPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	raw := strings.Replace(delegationPolicyYAML, "      principal: automation-principal\n", "      principal: automation-principal\n      session_token: ags_sess_forbidden\n", 1)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "delegation policy contains forbidden sensitive field") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadFileRejectsIdentitySelectorFieldsInsideDelegationPolicy(t *testing.T) {
	for _, field := range []string{"profile", "login", "display_name", "workflow_role"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "integrations.yaml")
			raw := strings.Replace(delegationPolicyYAML, "      principal: automation-principal\n", "      principal: automation-principal\n      "+field+": attacker\n", 1)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFile(path)
			if err == nil || !strings.Contains(err.Error(), "contains unsupported field") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadFileRejectsMutableIdentitySelectorsInV2Mappings(t *testing.T) {
	for _, field := range []string{"agent_name", "prompt_role", "label", "runtime_host", "login"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "integrations.yaml")
			raw := strings.Replace(delegationPolicyV2YAML, "      role: implementer-b\n", "      role: implementer-b\n      "+field+": attacker\n", 1)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFile(path)
			if err == nil || !strings.Contains(err.Error(), "contains unsupported field") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadFileRejectsDuplicateDelegationTrustKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	second := strings.TrimPrefix(delegationPolicyYAML, "\ndelegation:\n  version: 1\n  policies:\n")
	second = strings.Replace(second, "primary-a-multica-agent-kit-v1", "primary-a-multica-agent-kit-v1-duplicate", 1)
	duplicate := strings.Replace(delegationPolicyYAML, "      policy_version: 2026-07-14.1\n", "      policy_version: 2026-07-14.1\n"+second, 1)
	if err := os.WriteFile(path, []byte(duplicate), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate delegation trust key") {
		t.Fatalf("error = %v", err)
	}
}
