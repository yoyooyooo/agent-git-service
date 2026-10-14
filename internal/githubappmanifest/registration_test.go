package githubappmanifest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistrationCallbackLoadsCredentialsAndAuthenticatesApp(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" {
			t.Errorf("unexpected provider path")
			w.WriteHeader(404)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Error("missing App JWT")
			w.WriteHeader(401)
			return
		}
		signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
		hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hash[:], signature) != nil {
			t.Error("invalid App JWT")
			w.WriteHeader(401)
			return
		}
		claimsBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		_ = json.Unmarshal(claimsBytes, &claims)
		if claims["iss"] != "123" {
			t.Error("wrong issuer")
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":123,"slug":"fixture-app","html_url":"https://github.com/apps/fixture-app","client_secret":"must-not-escape"}`))
	}))
	defer provider.Close()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	exchanger := &FixtureExchanger{Conversion: Conversion{ID: 123, Slug: "fixture-app", PEM: private, WebhookSecret: "fixture-hook-secret"}}
	flow := &Flow{StatePath: filepath.Join(directory, "state"), Paths: Paths{AppID: filepath.Join(directory, "id"), PrivateKey: filepath.Join(directory, "key"), WebhookSecret: filepath.Join(directory, "hook")}, Exchanger: exchanger}
	registrar, err := NewRegistrar(flow, "https://example.ts.net"+CallbackPath, provider.URL, provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	form, err := registrar.Begin("Fixture App")
	if err != nil {
		t.Fatal(err)
	}
	manifest := form["manifest"].(map[string]any)
	if manifest["hook_attributes"].(map[string]any)["active"] != false {
		t.Fatal("tailnet webhook must remain inactive")
	}
	state, err := flow.loadState()
	if err != nil {
		t.Fatal(err)
	}
	wrong := httptest.NewRecorder()
	registrar.Callback(wrong, httptest.NewRequest("GET", CallbackPath+"?code=fixture-code&state=wrong", nil))
	if wrong.Code != 400 || len(exchanger.Calls) != 0 {
		t.Fatal("wrong state reached conversion")
	}
	response := httptest.NewRecorder()
	registrar.Callback(response, httptest.NewRequest("GET", CallbackPath+"?code=fixture-code&state="+state.Value, nil))
	if response.Code != 200 || calls != 1 || len(exchanger.Calls) != 1 {
		t.Fatalf("callback failed: status=%d calls=%d", response.Code, calls)
	}
	for _, secret := range []string{private, "fixture-hook-secret", "must-not-escape", "fixture-code", state.Value} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("callback exposed secret material")
		}
	}
	for _, path := range []string{flow.Paths.AppID, flow.Paths.PrivateKey, flow.Paths.WebhookSecret} {
		info, _ := os.Stat(path)
		if info == nil || info.Mode().Perm() != 0600 {
			t.Fatal("credentials not owner-private")
		}
	}
	// A fresh Registrar reads the durable set, rather than retaining a conversion.
	fresh, err := NewRegistrar(&Flow{StatePath: flow.StatePath, Paths: flow.Paths}, registrar.RedirectURL, provider.URL, provider.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fresh.App(httptest.NewRequest("GET", StatusPath, nil)); err != nil {
		t.Fatal(err)
	}
	replay := httptest.NewRecorder()
	registrar.Callback(replay, httptest.NewRequest("GET", CallbackPath+"?code=fixture-code&state="+state.Value, nil))
	if replay.Code != 400 || len(exchanger.Calls) != 1 {
		t.Fatal("callback replay exchanged again")
	}
}

func TestRegistrarRejectsNonPrivateRedirects(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	flow := &Flow{StatePath: filepath.Join(directory, "state"), Paths: Paths{AppID: filepath.Join(directory, "id"), PrivateKey: filepath.Join(directory, "key"), WebhookSecret: filepath.Join(directory, "hook")}}
	if _, err := NewRegistrar(flow, "https://example.ts.net"+CallbackPath, "", nil); err != nil {
		t.Fatal("valid private flow rejected")
	}
	for _, redirect := range []string{"http://example.ts.net" + CallbackPath, "https://public.example.test" + CallbackPath, "https://example.ts.net" + CallbackPath + "?token=secret"} {
		if _, err := NewRegistrar(flow, redirect, "", nil); err == nil {
			t.Fatal("unsafe callback accepted")
		}
	}
}
