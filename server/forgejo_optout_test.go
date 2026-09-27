package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ngaut/agent-git-service/config"
)

func TestDisabledForgejoCompositionDoesNotLoadSecretsOrSelectCI(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "integrations.yaml")
	// Native API-only startup remains viable after the operator disables an
	// integration and removes that integration's old private files.
	body := `forgejo:
  enabled: false
  token_file: /missing/forgejo.token
  webhook_secret_file: /missing/webhook.secret
  actions_log_bridge_url: https://logs.example.test
  actions_log_bridge_token_file: /missing/logs.token
  authority_policy:
    enabled: true
ci:
  default_backend: none
`
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{IntegrationsConfigFile: file, ForgejoIntegrationEnabled: true}
	// An explicit configuration file, including its disabled switch, owns this
	// setting; a leftover process environment must not re-enable Forgejo.
	integration, err := initForgejoIntegration(cfg)
	if err != nil || integration != nil {
		t.Fatal("disabled provider became a startup dependency", err)
	}
	ci, err := initCIBackends(cfg)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := ci.Select("team/project")
	if err != nil || selected.Name != "none" {
		t.Fatal("Forgejo changed independent CI selection", err)
	}
}

func TestEnabledForgejoFileRequiresUsableBaseAndToken(t *testing.T) {
	for _, options := range []string{
		"",
		"  base_url: https://forgejo.example.test\n",
		"  base_url: file:///tmp/forgejo\n  token: fixture-only\n",
	} {
		file := filepath.Join(t.TempDir(), "integrations.yaml")
		if err := os.WriteFile(file, []byte("forgejo:\n  enabled: true\n"+options), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := initForgejoIntegration(config.Config{IntegrationsConfigFile: file}); err == nil {
			t.Fatal("enabled incomplete file config was accepted")
		}
	}
	file := filepath.Join(t.TempDir(), "integrations.yaml")
	body := `forgejo:
  enabled: true
  base_url: https://forgejo.example.test
  token: fixture-only-not-for-network
  authority_policy:
    enabled: false
    operator_token_file: /missing/inactive-operator.token
`
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if integration, err := initForgejoIntegration(config.Config{IntegrationsConfigFile: file}); err != nil || integration == nil {
		t.Fatal("disabled authority child read a stale credential", err)
	}
}
