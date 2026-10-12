package githubappmanifest

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestManifestFailedWriteKeepsExistingSetAndCanRetry(t *testing.T) {
	flow, exchanger, _ := newFlow(t, time.Date(2026, 10, 12, 5, 0, 0, 0, time.UTC))
	for _, path := range []string{flow.Paths.AppID, flow.Paths.PrivateKey, flow.Paths.WebhookSecret} {
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state, err := flow.Begin(CallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(flow.Paths.PrivateKey), "."+filepath.Base(flow.Paths.PrivateKey)+".tmp"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	flow.StageFailAfter = 1
	if _, err := flow.Complete("one-time-code", state.Value); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("expected staging failure, got %v", err)
	}
	for _, path := range []string{flow.Paths.AppID, flow.Paths.PrivateKey, flow.Paths.WebhookSecret} {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != "original" {
			t.Fatalf("%s changed to %q (%v)", path, body, err)
		}
	}
	calls := len(exchanger.Calls)
	flow.StageFailAfter = 0
	if _, err := flow.Complete("one-time-code", state.Value); err != nil {
		t.Fatal(err)
	}
	if len(exchanger.Calls) != calls {
		t.Fatalf("retry exchanged again: %#v", exchanger.Calls)
	}
	id, err := os.ReadFile(flow.Paths.AppID)
	if err != nil || string(id) != "4242\n" {
		t.Fatalf("app id = %q %v", id, err)
	}
	key, err := os.ReadFile(flow.Paths.PrivateKey)
	if err != nil || string(key) != "fixture-private-key-material" {
		t.Fatal("private key was not committed on retry")
	}
	hook, err := os.ReadFile(flow.Paths.WebhookSecret)
	if err != nil || string(hook) != "webhook-secret-value\n" {
		t.Fatal("webhook secret was not committed on retry")
	}
}

func TestManifestStateIsConsumedOnceConcurrently(t *testing.T) {
	flow, _, _ := newFlow(t, time.Date(2026, 10, 12, 5, 0, 0, 0, time.UTC))
	state, err := flow.Begin(CallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	gate := &gatedExchanger{entered: make(chan struct{}, 2)}
	flow.Exchanger = gate
	errs := make(chan error, 2)
	go func() {
		_, err := flow.Complete("code-a", state.Value)
		errs <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first exchange did not start")
	}
	go func() {
		_, err := flow.Complete("code-b", state.Value)
		errs <- err
	}()
	second := <-errs
	if second == nil || !strings.Contains(second.Error(), "already in use") {
		t.Fatalf("second completion while state is claimed: %v", second)
	}
	close(gate.release)
	first := <-errs
	if first != nil {
		t.Fatal(first)
	}
	id, err := os.ReadFile(flow.Paths.AppID)
	if err != nil || strings.TrimSpace(string(id)) != "4242" {
		t.Fatalf("app id = %q %v", id, err)
	}
}

// gatedExchanger blocks inside Convert until release is closed. entered is
// signaled once the call has passed state reservation.
type gatedExchanger struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (e *gatedExchanger) Convert(code string) (Conversion, error) {
	e.once.Do(func() {
		if e.release == nil {
			e.release = make(chan struct{})
		}
	})
	e.entered <- struct{}{}
	<-e.release
	return testConversion(), nil
}
