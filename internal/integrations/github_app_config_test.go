package integrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const githubAppPEM = "fixture-private-key-material"

func writeGitHubAppFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func githubAppYAML(dir string, enabled bool) string {
	flag := "false"
	if enabled {
		flag = "true"
	}
	return "github_app:\n" +
		"  enabled: " + flag + "\n" +
		"  app_id_file: " + filepath.Join(dir, "github-app-id") + "\n" +
		"  private_key_file: " + filepath.Join(dir, "github-app.pem") + "\n" +
		"  webhook_secret_file: " + filepath.Join(dir, "github-app-webhook.secret") + "\n"
}

func writeGitHubAppFiles(t *testing.T, dir string) {
	t.Helper()
	writeGitHubAppFile(t, filepath.Join(dir, "github-app-id"), "123456\n", 0o600)
	writeGitHubAppFile(t, filepath.Join(dir, "github-app.pem"), githubAppPEM, 0o600)
	writeGitHubAppFile(t, filepath.Join(dir, "github-app-webhook.secret"), "webhook-secret-value\n", 0o600)
}

func loadGitHubApp(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadFile(path)
}

func TestLoadFileGitHubAppDisabledDoesNotReadSecretFiles(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadGitHubApp(t, githubAppYAML(dir, false))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubApp.Enabled {
		t.Fatal("github_app.enabled defaulted on")
	}
	if cfg.GitHubApp.AppID != "" || cfg.GitHubApp.WebhookSecret != "" {
		t.Fatalf("disabled config loaded secret material: %+v", cfg.GitHubApp)
	}
	if cfg.GitHubApp.APIBaseURL != DefaultGitHubAppAPIBaseURL {
		t.Fatalf("api base = %q", cfg.GitHubApp.APIBaseURL)
	}
	if cfg.GitHubApp.WebhookPath != DefaultGitHubAppWebhookPath {
		t.Fatalf("webhook path = %q", cfg.GitHubApp.WebhookPath)
	}
	if _, err := os.Stat(filepath.Join(dir, "github-app-id")); !os.IsNotExist(err) {
		t.Fatal("disabled load touched or required a secret file")
	}
}

func TestLoadFileGitHubAppEnabledRequiresModeOwnerAndRegularFile(t *testing.T) {
	dir := t.TempDir()
	writeGitHubAppFiles(t, dir)
	cfg, err := loadGitHubApp(t, githubAppYAML(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubApp.AppID != "123456" {
		t.Fatalf("app id = %q", cfg.GitHubApp.AppID)
	}
	if cfg.GitHubApp.WebhookSecret != "webhook-secret-value" {
		t.Fatal("webhook secret was not loaded from the file")
	}

	if err := os.Chmod(filepath.Join(dir, "github-app-webhook.secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGitHubApp(t, githubAppYAML(dir, true)); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("expected mode 0600 rejection, got %v", err)
	}

	if err := os.Chmod(filepath.Join(dir, "github-app-webhook.secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "github-app.pem"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGitHubApp(t, githubAppYAML(dir, true)); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("expected 0400 rejection, got %v", err)
	}
}

func TestLoadFileGitHubAppRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	writeGitHubAppFiles(t, dir)
	target := filepath.Join(dir, "github-app-webhook.secret")
	link := filepath.Join(dir, "webhook-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(githubAppYAML(dir, true), target, link, 1)
	if _, err := loadGitHubApp(t, body); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestLoadFileGitHubAppRejectsInlineSecrets(t *testing.T) {
	for _, key := range []string{"app_id", "private_key", "webhook_secret", "client_secret", "pem"} {
		body := "github_app:\n  enabled: false\n  " + key + ": present\n"
		if _, err := loadGitHubApp(t, body); err == nil || !strings.Contains(err.Error(), "inline material is rejected") {
			t.Fatalf("%s: expected inline rejection, got %v", key, err)
		}
	}
}

func TestLoadFileGitHubAppEnabledMissingFileIsNotReady(t *testing.T) {
	dir := t.TempDir()
	writeGitHubAppFiles(t, dir)
	if err := os.Remove(filepath.Join(dir, "github-app-id")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGitHubApp(t, githubAppYAML(dir, true)); err == nil || !strings.Contains(err.Error(), "app_id_file") {
		t.Fatalf("expected missing app id file, got %v", err)
	}
}

func TestLoadFileGitHubAppDisabledIgnoresMissingFiles(t *testing.T) {
	if _, err := loadGitHubApp(t, "github_app:\n  enabled: false\n"); err != nil {
		t.Fatal(err)
	}
}
