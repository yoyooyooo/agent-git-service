package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/githubappreceiver"
)

func webhookIntegrationConfig(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"app-id": []byte("123456"),
		"key":    pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		"secret": []byte("fixture-webhook-secret"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(directory, "integrations.yaml")
	body := fmt.Sprintf("github_app:\n  enabled: true\n  app_id_file: %s\n  private_key_file: %s\n  webhook_secret_file: %s\n", filepath.Join(directory, "app-id"), filepath.Join(directory, "key"), filepath.Join(directory, "secret"))
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewGitHubAppWebhookRoute(t *testing.T) {
	integrationPath := webhookIntegrationConfig(t)
	dataRoot := t.TempDir()
	cfg := config.Config{DBdsn: createBootstrapDSN(t, "github_app_webhook"), GitRepoDir: dataRoot, BaseURL: "http://primary.example.test", ListenMode: "production", Environment: "production", IntegrationsConfigFile: integrationPath}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	local := httptest.NewServer(srv.Handler())
	defer local.Close()

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previousLogger)
	body := []byte(`{"installation":{"id":42},"repository":{"id":7},"sensitive":"private-payload-marker"}`)
	request := func(method, path, id, secret string) (int, string) {
		t.Helper()
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req, err := http.NewRequest(method, local.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-GitHub-Delivery", id)
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("Content-Type", "application/json")
		response, err := local.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(result)
	}
	// A bad signature must never reach the spool, including for a new GUID.
	if code, response := request("POST", githubappreceiver.Path, "invalid-delivery", "wrong-secret"); code != 401 || !strings.Contains(response, "invalid signature") {
		t.Fatalf("invalid signature: HTTP %d", code)
	}
	spool := filepath.Join(dataRoot, ".github-app-deliveries")
	entries, err := os.ReadDir(spool)
	if err != nil || len(entries) != 0 {
		t.Fatal("rejected delivery reached spool")
	}
	if code, response := request("POST", githubappreceiver.Path, "valid-delivery", "fixture-webhook-secret"); code != 200 || !strings.Contains(response, "quarantined") {
		t.Fatalf("valid signature: HTTP %d", code)
	}
	if code, response := request("POST", githubappreceiver.Path, "valid-delivery", "fixture-webhook-secret"); code != 200 || !strings.Contains(response, "duplicate") {
		t.Fatalf("duplicate delivery: HTTP %d", code)
	}
	entries, err = os.ReadDir(spool)
	if err != nil || len(entries) != 1 {
		t.Fatal("accepted delivery not committed exactly once")
	}
	stored, err := os.ReadFile(filepath.Join(spool, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var envelope githubappreceiver.Envelope
	if json.Unmarshal(stored, &envelope) != nil || !bytes.Equal(envelope.Body, body) || envelope.Delivery.AppID != "123456" {
		t.Fatal("committed delivery lost original body or configured App ID")
	}
	if code, _ := request("POST", "/api/v3/integrations/github/events", "alias-delivery", "fixture-webhook-secret"); code == 200 {
		t.Fatal("legacy alias must not accept deliveries")
	}
	if code, _ := request("GET", githubappreceiver.Path, "get-delivery", "fixture-webhook-secret"); code == 200 {
		t.Fatal("GET must not accept deliveries")
	}
	if err := os.Rename(spool, spool+".unavailable"); err != nil {
		t.Fatal(err)
	}
	if code, _ := request("POST", githubappreceiver.Path, "unavailable-delivery", "fixture-webhook-secret"); code != 503 {
		t.Fatalf("unavailable spool: HTTP %d", code)
	}
	for _, sensitive := range []string{"private-payload-marker", "fixture-webhook-secret", "wrong-secret", "X-Hub-Signature-256"} {
		if strings.Contains(logs.String(), sensitive) {
			t.Fatal("webhook logs contain sensitive request material")
		}
	}
}

func TestGitHubAppWebhookOptInAndInvalidConfig(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "disabled.yaml")} {
		if path != "" {
			if err := os.WriteFile(path, []byte("github_app:\n  enabled: false\n  webhook_secret_file: /missing/secret\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		handler, err := initGitHubAppWebhook(config.Config{IntegrationsConfigFile: path})
		if err != nil || handler != nil {
			t.Fatal("disabled GitHub App created a webhook handler")
		}
	}
	path := webhookIntegrationConfig(t)
	if err := os.Chmod(filepath.Join(filepath.Dir(path), "secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := initGitHubAppWebhook(config.Config{IntegrationsConfigFile: path, GitRepoDir: t.TempDir()}); err == nil {
		t.Fatal("insecure webhook credential file accepted")
	}
}
