package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	agsauth "github.com/ngaut/agent-git-service/auth"
	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/githubappmanifest"
)

type appRegistrationAuthenticator struct{}

func (appRegistrationAuthenticator) Authenticate(r *http.Request) (agsauth.Identity, bool, error) {
	user := r.Header.Get("X-Fixture-User")
	if user != "admin" && user != "reader" {
		return agsauth.Identity{}, false, nil
	}
	return agsauth.Identity{Provider: "fixture", Subject: user, Login: "fixture-" + user, SiteAdmin: user == "admin"}, true, nil
}
func TestNewGitHubAppRegistrationRouterAndReload(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	conversions := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app-manifests/fixture-code/conversions":
			if r.Method != "POST" {
				t.Error("wrong conversion method")
			}
			conversions++
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "slug": "fixture-app", "pem": private, "webhook_secret": "fixture-secret"})
		case "/app":
			if r.Header.Get("Authorization") == "" {
				t.Error("missing app authentication")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 123, "slug": "fixture-app", "html_url": "https://github.com/apps/fixture-app"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	integrationPath := filepath.Join(directory, "integrations.yaml")
	body := fmt.Sprintf("github_app:\n  enabled: false\n  registration_enabled: true\n  registration_state_file: %s\n  registration_redirect_url: https://example.ts.net%s\n  app_id_file: %s\n  private_key_file: %s\n  webhook_secret_file: %s\n  api_base_url: %s\n", filepath.Join(directory, "state"), githubappmanifest.CallbackPath, filepath.Join(directory, "app-id"), filepath.Join(directory, "key"), filepath.Join(directory, "hook"), provider.URL)
	if err := os.WriteFile(integrationPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DBdsn: createBootstrapDSN(t, "github_app_registration"), GitRepoDir: filepath.Join(directory, "repos"), BaseURL: "http://primary.example.test", ListenMode: "production", Environment: "production", IntegrationsConfigFile: integrationPath}
	srv, err := New(cfg, WithAuthenticator(appRegistrationAuthenticator{}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	request := func(method, path, user, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if user != "" {
			req.Header.Set("X-Fixture-User", user)
		}
		response := httptest.NewRecorder()
		srv.Handler().ServeHTTP(response, req)
		return response
	}
	if request("POST", githubappmanifest.StartPath, "", `{"name":"Fixture App"}`).Code != 401 {
		t.Fatal("anonymous registration accepted")
	}
	if request("POST", githubappmanifest.StartPath, "reader", `{"name":"Fixture App"}`).Code != 403 {
		t.Fatal("non-admin registration accepted")
	}
	if request("POST", githubappmanifest.StartPath, "admin", `{"name":"Fixture App"}`).Code != 201 {
		t.Fatal("admin registration route not mounted")
	}
	stateBody, err := os.ReadFile(filepath.Join(directory, "state"))
	if err != nil {
		t.Fatal(err)
	}
	var state githubappmanifest.State
	if json.Unmarshal(stateBody, &state) != nil {
		t.Fatal("invalid state file")
	}
	callback := request("GET", githubappmanifest.CallbackPath+"?code=fixture-code&state="+state.Value, "", "")
	if callback.Code != 200 || conversions != 1 {
		t.Fatal("server callback did not complete")
	}
	if request("GET", githubappmanifest.StatusPath, "reader", "").Code != 403 {
		t.Fatal("non-admin app probe accepted")
	}
	if request("GET", githubappmanifest.StatusPath, "admin", "").Code != 200 {
		t.Fatal("registered files not loaded by server probe")
	}
	// Reinitialize the composition from the committed files; no retained conversion.
	fresh, err := initGitHubAppRegistrar(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.App(httptest.NewRequest("GET", githubappmanifest.StatusPath, nil)); err != nil {
		t.Fatal("fresh registrar failed")
	}
	if request("GET", githubappmanifest.CallbackPath+"?code=fixture-code&state="+state.Value, "", "").Code != 400 || conversions != 1 {
		t.Fatal("replay exchanged again")
	}
	cfg.IntegrationsConfigFile = ""
	disabled, err := New(cfg, WithAuthenticator(appRegistrationAuthenticator{}))
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Shutdown(context.Background())
	response := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(response, httptest.NewRequest("GET", githubappmanifest.CallbackPath, nil))
	if response.Code != 404 {
		t.Fatal("registration routes must be opt-in")
	}
}
