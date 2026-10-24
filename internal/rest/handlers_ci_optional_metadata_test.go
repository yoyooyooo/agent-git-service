package rest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/providerlogbridge"
	"github.com/ngaut/agent-git-service/internal/testharness"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Exercise the actual Forgejo adapter and binding checks, not a StepBackend stub.
func TestForgejoOptionalStepsKeepJobsAndChecksAvailable(t *testing.T) {
	for _, tc := range []struct {
		name, ref, event string
		available        bool
	}{
		{"push", "refs/heads/main", "push", true},
		{"schedule", "refs/heads/main", "schedule", false},
		{"release", "refs/heads/main", "release", false},
		{"pull_request_target", "refs/heads/main", "pull_request_target", false},
		{"tag_push", "refs/tags/v1.0.0", "push", false},
		{"wrong_commit", "refs/heads/main", "push", false},
		{"wrong_repository", "refs/heads/main", "push", false},
		{"wrong_run", "refs/heads/main", "push", false},
		{"wrong_token", "refs/heads/main", "push", false},
		{"wrong_source", "refs/heads/main", "push", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testharness.New(t)
			repo, head := seedCheckRunsRepo(t, h, "optional-steps")
			database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "forgejo.db")), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := database.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			for _, sql := range []string{
				`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
				`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
				`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text)`,
				`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text, log_filename text, log_in_storage boolean, log_expired boolean)`,
				`CREATE TABLE action_task_step (id integer primary key, task_id integer, repo_id integer, name text, "index" integer, status integer, started integer, stopped integer)`,
				`INSERT INTO repository VALUES (1, 'ci', 'project')`,
			} {
				require.NoError(t, database.Exec(sql).Error)
			}
			require.NoError(t, database.Exec(`INSERT INTO action_run VALUES (17, 1, 9, ?, ?, ?)`, tc.ref, head, tc.event).Error)
			require.NoError(t, database.Exec(`INSERT INTO action_run_job VALUES (31, 17, 1, 41, 'unit', ?)`, head).Error)
			require.NoError(t, database.Exec(`INSERT INTO action_task VALUES (41, 1, ?, 'missing.log', false, true)`, head).Error)
			require.NoError(t, database.Exec(`INSERT INTO action_run_job VALUES (32, 17, 1, 40, 'lint', ?)`, head).Error)
			require.NoError(t, database.Exec(`INSERT INTO action_task VALUES (40, 1, ?, 'missing.log', false, true)`, head).Error)
			require.NoError(t, database.Exec(`INSERT INTO action_task_step VALUES (1,41,1,'private step',0,1,1767225600,1767225601)`).Error)
			switch tc.name {
			case "wrong_commit":
				require.NoError(t, database.Exec(`UPDATE action_task SET commit_sha=?`, strings.Repeat("b", 40)).Error)
			case "wrong_repository":
				require.NoError(t, database.Exec(`UPDATE repository SET lower_name='other'`).Error)
			case "wrong_run":
				require.NoError(t, database.Exec(`UPDATE action_run SET "index"=10`).Error)
			}
			allowed := []string{"127.0.0.0/8"}
			if tc.name == "wrong_source" {
				allowed = []string{"192.0.2.0/24"}
			}
			handler, err := providerlogbridge.New(providerlogbridge.Config{DB: database, ActionsLogDir: t.TempDir(), ServiceToken: "bridge-only", AllowedCIDRs: allowed})
			require.NoError(t, err)
			router := chi.NewRouter()
			router.Use(providerlogbridge.CaptureSocketPeer)
			router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/tasks/{task}", handler.ServeHTTP)
			router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", handler.ServeHTTP)
			bridge := httptest.NewServer(router)
			defer bridge.Close()
			config := cibackend.BackendConfig{Kind: "forgejo", TokenFile: "fixture", AllowHTTP: true, LogBridge: &cibackend.LogBridgeConfig{URL: bridge.URL, TokenFile: "fixture", AllowHTTP: true}}
			bridgeToken := "bridge-only"
			if tc.name == "wrong_token" {
				bridgeToken = "invalid-credential"
			}
			backend := bindOptionalForgejo(t, h, repo.FullName, head, tc.event, config, bridgeToken)
			runs, err := h.Svc.CIRuns(context.Background(), repo.FullName, cibackend.Query{})
			require.NoError(t, err)
			require.Len(t, runs.Runs, 1)
			run := runs.Runs[0]
			// The optional read still fails closed. No refused metadata is exposed.
			jobs, err := backend.Jobs(context.Background(), "ci/project", "17")
			if strings.HasPrefix(tc.name, "wrong_") {
				require.Error(t, err, "mandatory current-job binding must fail closed")
				response := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d/jobs", repo.FullName, run.ID), nil)
				require.NotEqual(t, 200, response.Code, response.Body.String())
				return
			}
			require.NoError(t, err)
			require.Len(t, jobs.Items, 2)
			steps, err := backend.(cibackend.StepBackend).JobSteps(context.Background(), "ci/project", run.Run, jobs.Items[0])
			if tc.available {
				require.NoError(t, err)
				require.Len(t, steps, 1)
			} else {
				require.Error(t, err)
				require.Empty(t, steps)
			}
			path := fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d/jobs", repo.FullName, run.ID)
			response := h.DoREST(t, "GET", path, nil)
			require.Equal(t, 200, response.Code, response.Body.String())
			rows := testharness.DecodeJSON(t, response)["jobs"].([]any)
			require.Len(t, rows, 2, "one failed step read must not truncate the collection")
			for i, raw := range rows {
				job := raw.(map[string]any)
				expected := 0
				if tc.available && i == 0 {
					expected = 1
				}
				require.Len(t, job["steps"], expected)
				if expected == 0 {
					require.NotEmpty(t, job["ags_steps_status"], "missing metadata needs an explicit diagnosis")
				}
				detail := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/jobs/%.0f", repo.FullName, job["id"]), nil)
				require.Equal(t, 200, detail.Code, detail.Body.String())
				require.Len(t, testharness.DecodeJSON(t, detail)["steps"], expected)
				check := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/check-runs/%.0f", repo.FullName, job["id"]), nil)
				require.Equal(t, 200, check.Code, check.Body.String())
			}
			checks := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/commits/%s/check-runs", repo.FullName, head), nil)
			require.Equal(t, 200, checks.Code, checks.Body.String())
			require.Len(t, testharness.DecodeJSON(t, checks)["check_runs"], 2)
		})
	}
}

func TestForgejoArtifactsWithoutLogBridgeFailExplicitly(t *testing.T) {
	h := testharness.New(t)
	repo, head := seedCheckRunsRepo(t, h, "optional-artifacts")
	bindOptionalForgejo(t, h, repo.FullName, head, "schedule", cibackend.BackendConfig{Kind: "forgejo", TokenFile: "fixture", AllowHTTP: true}, "")
	runs, err := h.Svc.CIRuns(context.Background(), repo.FullName, cibackend.Query{})
	require.NoError(t, err)
	require.Len(t, runs.Runs, 1)
	path := fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d/artifacts", repo.FullName, runs.Runs[0].ID)
	response := h.DoREST(t, "GET", path, nil)
	require.Equal(t, 501, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "does not support")
	repoResponse := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/artifacts", repo.FullName), nil)
	require.Equal(t, 501, repoResponse.Code, repoResponse.Body.String())
	unknown := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/runs/17/artifacts", repo.FullName), nil)
	require.Equal(t, 404, unknown.Code, "unsupported metadata must not bypass run identity")
}

// The provider exposes two jobs so a denied optional read cannot hide later jobs.
func bindOptionalForgejo(t *testing.T, h *testharness.Harness, repo, head, event string, config cibackend.BackendConfig, bridgeToken string) cibackend.Backend {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "token provider-only", r.Header.Get("Authorization"))
		now := "2026-01-01T00:00:00Z"
		run := map[string]any{"id": 17, "index_in_repo": 9, "workflow_id": "ci.yml", "prettyref": "main", "commit_sha": head, "event": event, "status": "success", "created": now, "updated": now}
		switch r.URL.Path {
		case "/api/v1/repos/ci/project/actions/runs/17":
			_ = json.NewEncoder(w).Encode(run)
		case "/api/v1/repos/ci/project/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
		case "/api/v1/repos/ci/project/actions/tasks":
			tasks := []any{}
			for i, id := range []int{41, 40} {
				tasks = append(tasks, map[string]any{"id": id, "run_number": 9, "name": []string{"unit", "lint"}[i], "workflow_id": "ci.yml", "head_sha": head, "status": "success", "created_at": now, "updated_at": now})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "workflow_runs": tasks})
		default:
			t.Errorf("unexpected provider route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	config.URL = provider.URL
	backend, err := cibackend.NewHTTP(config, "provider-only", bridgeToken)
	require.NoError(t, err)
	h.Svc.CI, err = cibackend.WithBackends(cibackend.Config{Backends: map[string]cibackend.BackendConfig{"builder": config}, Repositories: map[string]cibackend.Binding{repo: {Backend: "builder", Repository: "ci/project"}}}, map[string]cibackend.Backend{"builder": backend})
	require.NoError(t, err)
	return backend
}
