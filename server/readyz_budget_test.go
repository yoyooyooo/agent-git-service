package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestReadyzAuthorityRespectsCallerDeadline(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var deadlineRemaining time.Duration
	handler := readyzHandler(readyzConfig{MainDB: db, ProjectionAlertingRequired: true, ProjectionAlertingConfigured: true, AuthorityPolicyCheck: func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("readiness probe has no deadline")
		}
		deadlineRemaining = time.Until(deadline)
		<-ctx.Done()
		return ctx.Err()
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil).WithContext(ctx))
	if response.Code != http.StatusServiceUnavailable || deadlineRemaining > 25*time.Millisecond {
		t.Fatal("canceled authority probe must fail readiness within caller deadline")
	}
}

func TestReadyzCreatesProbeDeadline(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	handler := readyzHandler(readyzConfig{MainDB: db, ProjectionAlertingRequired: true, ProjectionAlertingConfigured: true, AuthorityPolicyCheck: func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Error("readiness budget missing")
		}
		return nil
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != 200 {
		t.Fatal("healthy authority must preserve readiness")
	}
}

// Ten mapped repositories take three waves with four workers. Even healthy
// 300ms provider reads need 4.5s for five sequential reads in each wave.
func TestReadyzAuthorityAllowsHealthyMultiRepositoryScan(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var calls atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Error("readiness must only read provider state")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		timer := time.NewTimer(300 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/permission"):
			_, _ = w.Write([]byte(`{"permission":"write"}`))
		case strings.Contains(r.URL.Path, "/branch_protections/"):
			_, _ = w.Write([]byte(`{"enable_push":true,"enable_force_push":false,"enable_push_whitelist":false,"enable_merge_whitelist":false}`))
		case strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(`[{"name":"ags/action-rebase"},{"name":"ags/status-rebasing"},{"name":"ags/status-projection-drift"},{"name":"ags/status-rebase-conflict"},{"name":"ags/status-blocked"},{"name":"ags/status-needs-rebase"}]`))
		case strings.HasSuffix(r.URL.Path, "/hooks"):
			_, _ = w.Write([]byte(`[{"id":1,"active":true,"events":["pull_request","issues","delete"],"config":{"url":"https://primary.example.test/hook"}}]`))
		default:
			_, _ = w.Write([]byte(`{"allow_rebase_update":false}`))
		}
	}))
	defer provider.Close()
	mappings := map[string]forgejointegration.RepoMapping{}
	for n := range 10 {
		repo := fmt.Sprintf("repo-%02d", n)
		mappings["fixture/"+repo] = forgejointegration.RepoMapping{Owner: "fixture", Repo: repo, BaseBranch: "main"}
	}
	integration := forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: provider.URL, WebhookSecret: "fixture-secret", AuthorityPolicyEnabled: true, WebhookURL: "https://primary.example.test/hook", IntegrationBot: "fixture-bot", RepoMap: mappings}, nil, nil)
	handler := readyzHandler(readyzConfig{MainDB: database, ProjectionAlertingRequired: true, ProjectionAlertingConfigured: true, AuthorityPolicyCheck: func(ctx context.Context) error {
		_, err := integration.VerifyRepositoryAuthorityReadiness(ctx)
		return err
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("healthy complete provider scan rejected: %d %s", response.Code, response.Body.String())
	}
	if calls.Load() != 50 || maximum.Load() > 4 {
		t.Fatalf("calls=%d maximum=%d; all reads must complete with the existing concurrency cap", calls.Load(), maximum.Load())
	}
}

func TestReadyzAuthorityDefaultBudgetExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, err := database.DB()
		if err != nil {
			t.Fatal(err)
		}
		defer sqlDB.Close()
		handler := readyzHandler(readyzConfig{MainDB: database, ProjectionAlertingRequired: true, ProjectionAlertingConfigured: true, AuthorityPolicyCheck: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}})
		start := time.Now()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
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
		if response.Code != http.StatusServiceUnavailable || body.Status != "not_ready" || body.Checks["forgejo_authority_policy"].Error != context.DeadlineExceeded.Error() {
			t.Fatalf("unresponsive provider must fail closed: %s", response.Body.String())
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("readiness exceeded bounded budget: %s", elapsed)
		}
	})
}
