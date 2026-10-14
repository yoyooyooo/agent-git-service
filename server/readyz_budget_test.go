package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

// Exercise production mux composition, not only a standalone readiness handler.
func newAuthorityReadinessServer(t *testing.T, providerURL string) *Server {
	t.Helper()
	root := t.TempDir()
	file := filepath.Join(root, "integrations.yaml")
	body := fmt.Sprintf(`forgejo:
  enabled: true
  base_url: %q
  token: fixture-admin
  webhook_secret: fixture-webhook
  authority_policy:
    enabled: true
    operator_token: fixture-admin
    webhook_url: https://primary.example.test/hook
    integration_bot: fixture-admin
  repos:
    fixture/repo:
      owner: fixture
      repo: repo
      base_branch: main
`, providerURL)
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := New(config.Config{
		DBdsn:                          "file:" + filepath.Join(root, "primary.db"),
		GitRepoDir:                     filepath.Join(root, "repos"),
		BaseURL:                        "http://primary.example.test",
		ListenMode:                     "production",
		Environment:                    "production",
		IntegrationsConfigFile:         file,
		AllowMissingProjectionAlerting: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return srv
}

func assertAuthorityReadinessWarning(t *testing.T, srv *Server) {
	t.Helper()
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	warning := body.Checks["forgejo_authority_policy"]
	if response.Code != http.StatusOK || body.Status != "ready" || warning.Status != "degraded" || !strings.Contains(warning.Error, "diagnostic_only") {
		t.Fatalf("authority diagnostics must not reject readiness: %d %s", response.Code, response.Body.String())
	}
}

func TestReadyzAuthorityTimeoutReturns200(t *testing.T) {
	entered := make(chan struct{}, 1)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer provider.Close()
	srv := newAuthorityReadinessServer(t, provider.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := srv.deps.SvcDeps.ForgejoIntegration.VerifyRepositoryAuthorityReadiness(ctx)
		done <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("diagnostic never reached the provider")
	}
	// A diagnostic is hung at the provider, but readiness must finish before it.
	assertAuthorityReadinessWarning(t, srv)
	if ctx.Err() != nil {
		t.Fatal("readiness waited for the external diagnostic deadline")
	}
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a diagnostic timeout, got %v", err)
	}
	assertAuthorityReadinessWarning(t, srv)
	if calls.Load() != 1 {
		t.Fatalf("readiness contacted Forgejo: %d calls", calls.Load())
	}
}

// This client deliberately has no authority verifier capability.
type noAuthorityVerifierClient struct{}

func (noAuthorityVerifierClient) EnsureRepository(context.Context, string, string, bool) error {
	return nil
}
func (noAuthorityVerifierClient) EnsurePullRequest(context.Context, forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (noAuthorityVerifierClient) UpdatePullRequestState(context.Context, string, string, int, string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}

func TestReadyzAuthorityVerifierMissingReturns200(t *testing.T) {
	srv := newAuthorityReadinessServer(t, "http://forgejo.example.test")
	srv.deps.SvcDeps.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, WebhookSecret: "fixture-webhook", AuthorityPolicyEnabled: true,
		WebhookURL: "https://primary.example.test/hook", IntegrationBot: "fixture-admin",
		RepoMap: map[string]forgejointegration.RepoMapping{"fixture/repo": {Owner: "fixture", Repo: "repo", BaseBranch: "main"}},
	}, noAuthorityVerifierClient{}, nil)
	if err := srv.deps.SvcDeps.ForgejoIntegration.AuthorityPolicyConfigured(); err == nil || !strings.Contains(err.Error(), "verifier is not configured") {
		t.Fatalf("expected a missing verifier, got %v", err)
	}
	assertAuthorityReadinessWarning(t, srv)
}

func TestReadyzAuthorityPermissionFailureReturns200WithWarning(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "fixture permission denied", http.StatusForbidden)
	}))
	defer provider.Close()
	srv := newAuthorityReadinessServer(t, provider.URL)
	_, err := srv.deps.SvcDeps.ForgejoIntegration.VerifyRepositoryAuthorityReadiness(context.Background())
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("expected provider permission failure, got %v", err)
	}
	assertAuthorityReadinessWarning(t, srv)
	if calls.Load() != 1 {
		t.Fatalf("readiness repeated the failed permission check: %d calls", calls.Load())
	}
}
