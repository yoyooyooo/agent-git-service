package githubappmanifest

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConversion() Conversion {
	conversion := Conversion{
		ID:            4242,
		Slug:          "ags-intent-hub",
		ClientID:      "Iv1.client-id-secret",
		ClientSecret:  "client-secret-value",
		WebhookSecret: "webhook-secret-value",
		PEM:           "fixture-private-key-material",
		Permissions:   map[string]string{"contents": "write"},
		Events:        []string{"push", "pull_request"},
	}
	conversion.Owner.Login = "yoyooyooo"
	conversion.HookAttributes.URL = "https://mini.example:8443/api/ext/v1/integrations/github/events"
	return conversion
}

func newFlow(t *testing.T, now time.Time) (*Flow, *FixtureExchanger, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	exchanger := &FixtureExchanger{Conversion: testConversion()}
	logs := &bytes.Buffer{}
	flow := &Flow{
		StatePath: filepath.Join(dir, "state.json"),
		Paths: Paths{
			AppID:         filepath.Join(dir, "github-app-id"),
			PrivateKey:    filepath.Join(dir, "github-app.pem"),
			WebhookSecret: filepath.Join(dir, "github-app-webhook.secret"),
		},
		Exchanger: exchanger,
		Now:       func() time.Time { return now },
		OwnerUID:  os.Getuid(),
		Log:       slog.New(slog.NewJSONHandler(logs, nil)),
	}
	return flow, exchanger, logs
}

func TestManifestCallbackWritesMode0600AndOmitsSecretsFromLogs(t *testing.T) {
	now := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	flow, exchanger, logs := newFlow(t, now)
	state, err := flow.Begin(CallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(flow.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode %o", info.Mode().Perm())
	}
	registration, err := flow.Complete("temporary-code", state.Value)
	if err != nil {
		t.Fatal(err)
	}
	if registration.AppID != "4242" || registration.OwnerLogin != "yoyooyooo" || registration.Slug != "ags-intent-hub" {
		t.Fatalf("registration = %+v", registration)
	}
	if registration.Events[0] != "push" || registration.Permissions["contents"] != "write" {
		t.Fatalf("non-secret record = %+v", registration)
	}
	if _, err := os.Lstat(flow.StatePath); !os.IsNotExist(err) {
		t.Fatal("state file was not removed")
	}
	for _, path := range []string{flow.Paths.AppID, flow.Paths.PrivateKey, flow.Paths.WebhookSecret} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o", path, info.Mode())
		}
	}
	appID, err := os.ReadFile(flow.Paths.AppID)
	if err != nil || string(appID) != "4242\n" {
		t.Fatalf("app id file %q %v", appID, err)
	}
	secret, err := os.ReadFile(flow.Paths.WebhookSecret)
	if err != nil || string(secret) != "webhook-secret-value\n" {
		t.Fatal("webhook secret file mismatch")
	}
	if len(exchanger.Calls) != 1 || exchanger.Calls[0] != "temporary-code" {
		t.Fatalf("exchange calls = %#v", exchanger.Calls)
	}
	logText := logs.String()
	for _, forbidden := range []string{"temporary-code", "client-secret-value", "webhook-secret-value", "fixture-private-key-material", "Iv1.client-id-secret"} {
		if strings.Contains(logText, forbidden) {
			t.Fatalf("log contains %q: %s", forbidden, logText)
		}
	}
	if !strings.Contains(logText, `"app_id":"4242"`) || !strings.Contains(logText, `"slug":"ags-intent-hub"`) {
		t.Fatalf("log missing non-secret record: %s", logText)
	}
}

func TestManifestCallbackRejectsExpiredStateAndReusedCode(t *testing.T) {
	start := time.Date(2026, 10, 12, 3, 0, 0, 0, time.UTC)
	current := start
	flow, _, _ := newFlow(t, start)
	flow.Now = func() time.Time { return current }
	state, err := flow.Begin(CallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	current = start.Add(Window + time.Second)
	if _, err := flow.Complete("temporary-code", state.Value); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiry, got %v", err)
	}
	current = start.Add(30 * time.Minute)
	if _, err := flow.Complete("temporary-code", "different-state"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected state mismatch, got %v", err)
	}
	if _, err := flow.Complete("temporary-code", state.Value); err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Complete("temporary-code", state.Value); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("expected reused code rejection, got %v", err)
	}
}

func TestManifestConversionFixtureKeepsClientSecretOutOfRegistration(t *testing.T) {
	raw := []byte(`{"id":7,"slug":"ags","client_id":"Iv1.cid","client_secret":"super-secret","webhook_secret":"hook-secret","pem":"fixture-key-body","owner":{"login":"yoyooyooo"},"permissions":{"metadata":"read"},"events":["meta"],"hook_attributes":{"url":"https://example.test/hook"}}`)
	conversion, err := DecodeConversionFixture(raw)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logs := &bytes.Buffer{}
	flow := &Flow{
		StatePath: filepath.Join(dir, "state.json"),
		Paths: Paths{
			AppID:         filepath.Join(dir, "id"),
			PrivateKey:    filepath.Join(dir, "key.pem"),
			WebhookSecret: filepath.Join(dir, "hook"),
		},
		Exchanger: &FixtureExchanger{Conversion: conversion},
		Now:       func() time.Time { return time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC) },
		OwnerUID:  os.Getuid(),
		Log:       slog.New(slog.NewJSONHandler(logs, nil)),
	}
	state, err := flow.Begin(CallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := flow.Complete("one-time", state.Value)
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.Join([]string{registration.AppID, registration.Slug, registration.OwnerLogin, registration.WebhookURL}, " ")
	if strings.Contains(encoded, "super-secret") || strings.Contains(logs.String(), "super-secret") || strings.Contains(logs.String(), "hook-secret") {
		t.Fatal("client or webhook secret leaked into the registration or log")
	}
	if _, err := os.Stat(filepath.Join(dir, "client_secret")); !os.IsNotExist(err) {
		t.Fatal("client secret was written")
	}
}
