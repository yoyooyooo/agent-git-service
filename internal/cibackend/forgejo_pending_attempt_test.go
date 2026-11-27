package cibackend

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/providerlogbridge"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestForgejoPreparedUnboundAttemptCannotBorrowHistoricalTask(t *testing.T) {
	for _, attempt := range []int{0, 1, 2} {
		for _, status := range []string{"waiting", "blocked"} {
			t.Run(fmt.Sprintf("attempt_%d_%s", attempt, status), func(t *testing.T) {
				head := strings.Repeat("a", 40)
				database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "provider.db")), &gorm.Config{})
				if err != nil {
					t.Fatal(err)
				}
				pool, err := database.DB()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = pool.Close() })
				for _, statement := range []string{
					`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
					`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
					`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text, status integer, started integer, stopped integer, attempt integer)`,
					`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text, log_filename text, log_in_storage boolean, log_expired boolean)`,
					`INSERT INTO repository VALUES (1, 'ci', 'project')`,
					`INSERT INTO action_run VALUES (17, 1, 9, 'refs/heads/main', '` + head + `', 'push')`,
					`INSERT INTO action_task VALUES (41, 1, '` + head + `', 'previous-attempt.log', true, false)`,
				} {
					if err := database.Exec(statement).Error; err != nil {
						t.Fatal(err)
					}
				}
				jobStatus := 5
				if status == "blocked" {
					jobStatus = 7
				}
				if err := database.Exec(`INSERT INTO action_run_job VALUES (31,17,1,0,'unit',?,?,0,0,?)`, head, jobStatus, attempt).Error; err != nil {
					t.Fatal(err)
				}
				handler, err := providerlogbridge.New(providerlogbridge.Config{
					DB: database, ActionsLogDir: t.TempDir(), ServiceToken: "bridge-fixture", AllowedCIDRs: []string{"127.0.0.0/8"},
				})
				if err != nil {
					t.Fatal(err)
				}
				var logRequests, taskRequests atomic.Int32
				router := chi.NewRouter()
				router.Use(providerlogbridge.CaptureSocketPeer)
				router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", handler.ServeHTTP)
				router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/tasks/{task}", func(w http.ResponseWriter, r *http.Request) {
					logRequests.Add(1)
					handler.ServeHTTP(w, r)
				})
				bridge := httptest.NewServer(router)
				defer bridge.Close()
				run := forgejoFixtureRun()
				run["status"] = status
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/repos/ci/project/actions/runs/17":
						_ = json.NewEncoder(w).Encode(run)
					case "/api/v1/repos/ci/project/actions/runs":
						_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
					case "/api/v1/repos/ci/project/actions/tasks":
						taskRequests.Add(1)
						t.Error("an unbound attempt must not scan historical tasks")
						http.NotFound(w, r)
					default:
						t.Errorf("unexpected provider route: %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer provider.Close()
				backend, err := NewHTTP(BackendConfig{
					Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, TaskPageLimit: 1,
					LogBridge: &LogBridgeConfig{URL: bridge.URL, TokenFile: "fixture", AllowHTTP: true},
				}, "provider-fixture", "bridge-fixture")
				if err != nil {
					t.Fatal(err)
				}
				jobs, err := backend.Jobs(t.Context(), "ci/project", "17")
				if err != nil || !jobs.Complete || jobs.Items == nil || len(jobs.Items) != 0 || jobs.Total != 0 {
					t.Fatalf("prepared attempt must have a verified empty task set: %+v %v", jobs, err)
				}
				if _, err := backend.JobLogs(t.Context(), "ci/project", "17:41"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("historical task gained log identity: %v", err)
				}
				body, err := backend.RunLogs(t.Context(), "ci/project", "17")
				if err != nil {
					t.Fatal(err)
				}
				archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
				if err != nil || len(archive.File) != 0 || logRequests.Load() != 0 || taskRequests.Load() != 0 {
					t.Fatalf("unbound attempt read or invented logs: log=%d tasks=%d archive=%+v err=%v", logRequests.Load(), taskRequests.Load(), archive, err)
				}
			})
		}
	}
}
