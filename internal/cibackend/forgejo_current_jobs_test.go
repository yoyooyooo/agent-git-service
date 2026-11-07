package cibackend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/providerlogbridge"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestForgejoJobsKeepOnlyCurrentTasksThroughRealBridge(t *testing.T) {
	for _, mode := range []string{"valid", "skipped", "job-repository", "task-repository", "hidden-foreign-job", "missing-task", "missing-public-task", "no-bridge", "missing-endpoint"} {
		t.Run(mode, func(t *testing.T) {
			head := strings.Repeat("a", 40)
			database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "forgejo.db")), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := database.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			for _, statement := range []string{
				`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
				`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
				`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text, status integer DEFAULT 0, started integer DEFAULT 0, stopped integer DEFAULT 0)`,
				`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text)`,
				`INSERT INTO repository VALUES (1, 'ci', 'project'), (2, 'ci', 'other')`,
			} {
				require.NoError(t, database.Exec(statement).Error)
			}
			require.NoError(t, database.Exec(`INSERT INTO action_run VALUES (17, 1, 9, 'refs/heads/feature', ?, 'push')`, head).Error)
			require.NoError(t, database.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha) VALUES (31, 17, 1, 43, 'unit', ?)`, head).Error)
			for _, task := range []int{41, 42, 43} {
				require.NoError(t, database.Exec(`INSERT INTO action_task VALUES (?, 1, ?)`, task, head).Error)
			}
			switch mode {
			case "skipped":
				require.NoError(t, database.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha,status,started,stopped) VALUES (32,17,1,0,'conditional',?,4,0,0)`, head).Error)
			case "job-repository":
				require.NoError(t, database.Exec(`UPDATE action_run_job SET repo_id=2 WHERE id=31`).Error)
			case "task-repository":
				require.NoError(t, database.Exec(`UPDATE action_task SET repo_id=2 WHERE id=43`).Error)
			case "hidden-foreign-job":
				// Keep a valid current job beside the corrupt row. Filtering that row
				// out in SQL must not turn a partial inventory into successful evidence.
				require.NoError(t, database.Exec(`INSERT INTO action_task VALUES (44, 2, ?)`, head).Error)
				require.NoError(t, database.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha) VALUES (32, 17, 2, 44, 'foreign', ?)`, head).Error)
			case "missing-task":
				require.NoError(t, database.Exec(`UPDATE action_run_job SET task_id=999 WHERE id=31`).Error)
			}
			handler, err := providerlogbridge.New(providerlogbridge.Config{DB: database, ActionsLogDir: t.TempDir(), ServiceToken: "bridge-only", AllowedCIDRs: []string{"127.0.0.0/8"}})
			require.NoError(t, err)
			router := chi.NewRouter()
			router.Use(providerlogbridge.CaptureSocketPeer)
			if mode != "missing-endpoint" {
				router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", handler.ServeHTTP)
			}
			bridge := httptest.NewServer(router)
			defer bridge.Close()
			var taskReads atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "token provider-only", r.Header.Get("Authorization"))
				run := forgejoFixtureRun()
				run["status"], run["event"], run["prettyref"] = "success", "push", "feature"
				switch r.URL.Path {
				case "/api/v1/repos/ci/project/actions/runs/17":
					_ = json.NewEncoder(w).Encode(run)
				case "/api/v1/repos/ci/project/actions/runs":
					require.Equal(t, head, r.URL.Query().Get("head_sha"))
					require.Equal(t, "9", r.URL.Query().Get("run_number"))
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
				case "/api/v1/repos/ci/project/actions/tasks":
					taskReads.Add(1)
					tasks := []any{}
					for _, task := range []int{43, 42, 41} {
						if mode == "missing-public-task" && task == 43 {
							continue
						}
						status := "failure"
						if task == 43 {
							status = "success"
						}
						tasks = append(tasks, map[string]any{"id": task, "run_number": 9, "name": "unit", "workflow_id": "ci.yml", "head_sha": head, "status": status, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(tasks), "workflow_runs": tasks})
				default:
					t.Errorf("unexpected provider route: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer provider.Close()
			config := BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: bridge.URL, TokenFile: "fixture", AllowHTTP: true}}
			if mode == "no-bridge" {
				config.LogBridge = nil
			}
			backend, err := NewHTTP(config, "provider-only", "bridge-only")
			require.NoError(t, err)
			jobs, err := backend.Jobs(t.Context(), "ci/project", "17")
			if mode != "valid" && mode != "skipped" {
				expected := ErrUnavailable
				if mode == "no-bridge" {
					expected = ErrUnsupported
				} else if mode == "missing-endpoint" {
					expected = ErrNotFound
				}
				require.ErrorIs(t, err, expected)
				require.Empty(t, jobs.Items)
				require.False(t, jobs.Complete)
				if mode == "missing-public-task" {
					require.Equal(t, int32(1), taskReads.Load())
				} else {
					require.Zero(t, taskReads.Load(), "invalid current bindings must fail before reading historical tasks")
				}
				return
			}
			require.NoError(t, err)
			require.True(t, jobs.Complete)
			require.Equal(t, 1, jobs.Total)
			require.Len(t, jobs.Items, 1, "three attempts are one current job, not three checks")
			require.Equal(t, "17:43", jobs.Items[0].Key)
			require.Equal(t, "success", jobs.Items[0].Conclusion)
			for _, retired := range []int{41, 42} {
				_, err := backend.Job(t.Context(), "ci/project", "17:"+strconv.Itoa(retired))
				require.ErrorIs(t, err, ErrNotFound, "a historical attempt must not remain a current job")
			}
		})
	}
}
