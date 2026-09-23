package integrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

const principalSessionYAML = `
team_authority:
  version: 1
  contract_revision: 2026-07-19.principal-session-v2
  legacy_compatibility_mode: legacy-subject-v2
  trusted_issuers:
    - id: multica-mini
      issuer: multica
      key_ids: [session-key]
      status: active
      trust_revision: multica-mini-keyring-v1
  bindings:
    - id: binding-mini-implementer-b
      issuer_instance_id: multica-mini
      subject: 33333333-3333-4333-8333-333333333333
      principal_id: 42
      status: active
      binding_revision: binding-mini-implementer-b-v1
  resources:
    - id: primary-a-agent-git-service
      target: primary-a
      service: ags
      repository: operator/agent-git-service
      status: active
      max_session_ttl: 30m
      policy_revision: primary-a-agent-git-service-v1
`

func TestLoadFileParsesPrincipalSessionAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	if err := os.WriteFile(path, []byte(principalSessionYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.PrincipalSessions.Version != sessionauthority.CurrentVersion || cfg.PrincipalSessions.ContractRevision != sessionauthority.ContractRevision {
		t.Fatalf("principal sessions = %#v", cfg.PrincipalSessions)
	}
	if len(cfg.PrincipalSessions.Bindings) != 1 || cfg.PrincipalSessions.Bindings[0].PrincipalID != 42 || cfg.PrincipalSessions.LegacyCompatibilityMode != sessionauthority.LegacySubjectCompatibilityMode {
		t.Fatalf("bindings = %#v", cfg.PrincipalSessions.Bindings)
	}
}

func TestLoadFileParsesMachineLevelResourceDefaultWithoutRepoInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	raw := `
team_authority:
  version: 2
  contract_revision: 2026-07-24.team-authority-v4
  trusted_issuers:
    - id: multica-mini
      issuer: multica
      key_ids: [session-key]
      status: active
      trust_revision: trust-v1
  team_bindings:
    - id: mini-team
      issuer_instance_id: multica-mini
      workspace_id: workspace-1
      team_identity_id: team-1
      policy_class: contributor
      principal_id: 42
      status: active
      binding_revision: team-v1
      epoch_floor: 1
  policy_classes:
    - id: contributor
      status: active
      policy_revision: class-v1
      operations: [repo.read]
  resource_defaults:
    - id: mini-default
      target: primary-a
      service: ags
      status: active
      max_session_ttl: 30m
      policy_revision: default-v1
  resources: []
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(cfg.PrincipalSessions.ResourceDefaults) != 1 || cfg.PrincipalSessions.ResourceDefaults[0].Target != "primary-a" || len(cfg.PrincipalSessions.Resources) != 0 {
		t.Fatalf("resource authority = %#v", cfg.PrincipalSessions)
	}
}

func TestLoadFileRejectsAuthorizationAndMutableIdentityFieldsInPrincipalBindings(t *testing.T) {
	for _, field := range []string{"role", "task_ids", "max_capabilities", "agent_name", "display_name", "workspace_id", "repository"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "integrations.yaml")
			raw := strings.Replace(principalSessionYAML, "      principal_id: 42\n", "      principal_id: 42\n      "+field+": attacker\n", 1)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFile(path)
			if err == nil || !strings.Contains(err.Error(), "unsupported field") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadFileKeepsLegacyDelegationAsNonExecutableMigrationInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	raw := principalSessionYAML + delegationPolicyV2YAML
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(cfg.Delegation.Mappings) != 1 || len(cfg.PrincipalSessions.Bindings) != 1 {
		t.Fatalf("config = %#v", cfg)
	}
}
