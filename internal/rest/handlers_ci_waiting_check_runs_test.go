package rest_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/providerlogbridge"
	"github.com/ngaut/agent-git-service/internal/testharness"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// A Forgejo job that is still waiting or blocked has task_id=0 and no public
// task. The inventory must accept that shape so check-runs can return queued
// with a null conclusion instead of HTTP 502.
func TestForgejoWaitingAndBlockedJobsReturnQueuedCheckRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		job    string
	}{
		{name: "waiting", status: 5, job: "CI Success"},
		{name: "blocked", status: 7, job: "critical-e2e"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testharness.New(t)
			repo, head := seedCheckRunsRepo(t, h, "queued-check-runs-"+tc.name)
			database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "forgejo.db")), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := database.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			for _, sql := range []string{
				`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
				`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
				`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text, status integer DEFAULT 0, started integer DEFAULT 0, stopped integer DEFAULT 0, attempt integer DEFAULT 0)`,
				`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text, log_filename text, log_in_storage boolean, log_expired boolean)`,
				`INSERT INTO repository VALUES (1, 'ci', 'project')`,
			} {
				require.NoError(t, database.Exec(sql).Error)
			}
			require.NoError(t, database.Exec(`INSERT INTO action_run VALUES (12451, 1, 2454, 'refs/pull/969/head', ?, 'pull_request')`, head).Error)
			require.NoError(t, database.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha,status,started,stopped,attempt) VALUES (24486,12451,1,0,?,?,?,0,0,0)`, tc.job, head, tc.status).Error)

			handler, err := providerlogbridge.New(providerlogbridge.Config{DB: database, ActionsLogDir: t.TempDir(), ServiceToken: "bridge-only", AllowedCIDRs: []string{"127.0.0.0/8"}})
			require.NoError(t, err)
			router := chi.NewRouter()
			router.Use(providerlogbridge.CaptureSocketPeer)
			router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", handler.ServeHTTP)
			bridge := httptest.NewServer(router)
			defer bridge.Close()

			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "token provider-only", r.Header.Get("Authorization"))
				now := "2026-11-16T07:16:46Z"
				run := map[string]any{
					"id": 12451, "index_in_repo": 2454, "workflow_id": "agent-kit-ci.yml",
					"title": "CI Success", "prettyref": "feature",
					"commit_sha": head, "event": "pull_request", "status": "waiting",
					"html_url": "https://ci.example.test/ci/project/actions/runs/2454",
					"created":  now, "updated": now,
				}
				switch r.URL.Path {
				case "/api/v1/repos/ci/project/actions/runs/12451":
					_ = json.NewEncoder(w).Encode(run)
				case "/api/v1/repos/ci/project/actions/runs":
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
				case "/api/v1/repos/ci/project/actions/tasks":
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []any{}})
				default:
					t.Errorf("unexpected provider route: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer provider.Close()

			config := cibackend.BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &cibackend.LogBridgeConfig{URL: bridge.URL, TokenFile: "fixture", AllowHTTP: true}}
			backend, err := cibackend.NewHTTP(config, "provider-only", "bridge-only")
			require.NoError(t, err)
			h.Svc.CI, err = cibackend.WithBackends(cibackend.Config{Backends: map[string]cibackend.BackendConfig{"builder": config}, Repositories: map[string]cibackend.Binding{repo.FullName: {Backend: "builder", Repository: "ci/project"}}}, map[string]cibackend.Backend{"builder": backend})
			require.NoError(t, err)

			jobs, err := backend.Jobs(t.Context(), "ci/project", "12451")
			require.NoError(t, err)
			require.True(t, jobs.Complete)
			require.Empty(t, jobs.Items)

			checks := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/commits/%s/check-runs", repo.FullName, head), nil)
			require.Equal(t, 200, checks.Code, checks.Body.String())
			body := testharness.DecodeJSON(t, checks)
			require.Equal(t, float64(1), body["total_count"])
			rows := body["check_runs"].([]any)
			require.Len(t, rows, 1)
			row := rows[0].(map[string]any)
			require.Equal(t, "queued", row["status"])
			require.Nil(t, row["conclusion"])
			require.Equal(t, head, row["head_sha"])
		})
	}
}
