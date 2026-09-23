package integrations

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadFileValidatesMulticaExternalPRDeliveryConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	secretPath := filepath.Join(dir, "multica-token")
	if err := os.WriteFile(secretPath, []byte("service-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid := `multica:
  enabled: true
  target_instance: mini-prod
  server_url: https://multica.example
  service_token_file: ` + secretPath + `
  external_pr_delivery:
    enabled: true
    timeout: 20s
`
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Multica.ExternalPRDelivery.Enabled || cfg.Multica.ExternalPRDelivery.Timeout != "20s" {
		t.Fatalf("typed delivery config=%#v", cfg.Multica.ExternalPRDelivery)
	}

	providerConflict := `multica:
  enabled: true
  target_instance: mini-prod
  server_url: https://multica.example
  external_pr_provider: gitlab
  service_token_file: ` + secretPath + `
  external_pr_delivery:
    enabled: true
`
	providerConflictPath := filepath.Join(dir, "provider-conflict.yaml")
	if err := os.WriteFile(providerConflictPath, []byte(providerConflict), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(providerConflictPath); err == nil || !strings.Contains(err.Error(), "external_pr_provider must be ags") {
		t.Fatalf("typed delivery accepted provider identity split: %v", err)
	}

	for _, invalid := range []string{
		"multica:\n  enabled: true\n  target_instance: mini-prod\n  external_pr_delivery:\n    enabled: true\n",
		"multica:\n  enabled: false\n  server_url: https://multica.example\n  service_token: service-secret\n  external_pr_delivery:\n    enabled: true\n",
		"multica:\n  enabled: true\n  server_url: https://multica.example\n  service_token_file: /missing/token\n  external_pr_delivery:\n    enabled: true\n    timeout: not-a-duration\n", "multica:\n  enabled: true\n  server_url: https://multica.example\n  service_token: service-secret\n  external_pr_delivery:\n    enabled: true\n",
	} {
		invalidPath := filepath.Join(dir, "invalid-"+strings.ReplaceAll(invalid[:5], "\n", "")+".yaml")
		if err := os.WriteFile(invalidPath, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(invalidPath); err == nil {
			t.Fatalf("expected invalid typed delivery config rejection for %q", invalid)
		}
	}
}

func TestLoadFileTypedServiceTokenRequiresOwnerOnlyRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only POSIX permission bits are not portable to Windows")
	}
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "service-token")
	configPath := filepath.Join(dir, "integrations.yaml")
	config := func(file string) string {
		return "multica:\n  enabled: true\n  target_instance: mini-prod\n  server_url: https://multica.example\n  service_token_file: " + file + "\n  external_pr_delivery:\n    enabled: true\n"
	}
	for _, mode := range []os.FileMode{0o400, 0o600} {
		_ = os.Chmod(secretPath, 0o600)
		if err := os.WriteFile(secretPath, []byte("typed-secret\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(config(secretPath)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(configPath); err != nil {
			t.Fatalf("owner-only mode %04o rejected: %v", mode, err)
		}
	}
	if err := os.Chmod(secretPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(configPath); err == nil {
		t.Fatal("group-readable service token file was accepted")
	}

	target := filepath.Join(dir, "service-token-link")
	if err := os.Symlink(secretPath, target); err != nil {
		t.Fatalf("create service token symlink: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(config(target)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(configPath); err == nil {
		t.Fatal("service token symlink was accepted")
	}

	if err := os.WriteFile(configPath, []byte("multica:\n  enabled: true\n  service_token_file: "+secretPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(configPath); err != nil {
		t.Fatalf("legacy non-typed service token file should retain compatibility: %v", err)
	}
}
