package integrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForgejoDisabledHasNoActiveSubconfiguration(t *testing.T) {
	for _, switchLine := range []string{"", "  enabled: false\n"} {
		t.Run(strings.TrimSpace(switchLine), func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "integrations.yaml")
			// Valid YAML for a disabled integration, even though enabled-mode settings
			// are incomplete/stale and its retired credential files no longer exist.
			body := "forgejo:\n" + switchLine + `  push_timeout: not-a-duration
  token_file: /missing/forgejo.token
  webhook_secret_file: /missing/webhook.token
  actions_log_bridge_url: https://logs.example.test
  actions_log_bridge_token_file: /missing/logs.token
  authority_policy:
    enabled: true
    action_principal_bindings:
      invalid/actor: 0
ci:
  default_backend: none
`
			if err := os.WriteFile(file, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadFile(file)
			if err != nil {
				t.Fatal("disabled integration read/validated active dependencies", err)
			}
			if cfg.Forgejo.Enabled || cfg.Forgejo.ActionsLogBridgeToken != "" || cfg.CI.Default != "none" {
				t.Fatal("disabled projection changed CI or loaded a secret")
			}
			if cfg.Forgejo.ToForgejoIntegrationConfig().Enabled {
				t.Fatal("conversion enabled a disabled integration")
			}
		})
	}
}

func TestForgejoEnabledStillValidatesItsDependencies(t *testing.T) {
	for _, body := range []string{
		"  push_timeout: not-a-duration\n",
		"  authority_policy:\n    enabled: true\n",
		"  actions_log_bridge_url: https://logs.example.test\n",
		"  actions_log_bridge_url: https://logs.example.test\n  actions_log_bridge_token_file: /missing/logs.token\n",
	} {
		file := filepath.Join(t.TempDir(), "integrations.yaml")
		if err := os.WriteFile(file, []byte("forgejo:\n  enabled: true\n"+body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(file); err == nil {
			t.Fatal("enabled integration accepted an invalid dependency")
		}
	}
}

func TestDisabledForgejoDoesNotDisableIndependentCIPolicyValidation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "integrations.yaml")
	body := "forgejo:\n  enabled: false\nci:\n  default_backend: missing-backend\n"
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(file); err == nil {
		t.Fatal("disabled Forgejo suppressed unrelated CI validation")
	}
}
