package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/providerlogbridge"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Forgejo keeps old tasks after a retry, but only the current job-to-task
// association is check evidence. Exercise that boundary through the real bridge
// and adapter, not a fixture backend that already filtered the task inventory.
func TestCIChecksForgejoRetriedTasksUseCurrentJobBinding(t *testing.T) {
	s, ctx, repo, pr, _, config := ciFixture(t)
	providerDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "provider.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := providerDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	for _, statement := range []string{
		`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
		`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
		`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text, status integer DEFAULT 0, started integer DEFAULT 0, stopped integer DEFAULT 0, attempt integer DEFAULT 0)`,
		`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text)`,
		`INSERT INTO repository VALUES (1, 'ci', 'project')`,
	} {
		if err := providerDB.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO action_run VALUES (17, 1, 9, 'refs/heads/feature', ?, 'push')`,
		`INSERT INTO action_task VALUES (40, 1, ?)`,
		`INSERT INTO action_task VALUES (41, 1, ?)`,
		`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha) VALUES (31, 17, 1, 41, 'unit', ?)`,
	} {
		if err := providerDB.Exec(statement, pr.HeadSHA).Error; err != nil {
			t.Fatal(err)
		}
	}
	handler, err := providerlogbridge.New(providerlogbridge.Config{
		DB: providerDB, ActionsLogDir: t.TempDir(), ServiceToken: "bridge-fixture-only", AllowedCIDRs: []string{"127.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(providerlogbridge.CaptureSocketPeer)
	router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", handler.ServeHTTP)
	bridge := httptest.NewServer(router)
	defer bridge.Close()

	currentStatus := "success"
	runStatus := ""
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token provider-fixture-only" || r.Method != http.MethodGet {
			t.Error("unexpected provider credential or method")
		}
		currentRunStatus := currentStatus
		if runStatus != "" {
			currentRunStatus = runStatus
		}
		run := map[string]any{
			"id": 17, "index_in_repo": 9, "workflow_id": "ci.yml", "prettyref": pr.HeadRef, "commit_sha": pr.HeadSHA,
			"event": "push", "status": currentRunStatus, "created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:01:00Z",
		}
		switch r.URL.Path {
		case "/api/v1/repos/ci/project/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
		case "/api/v1/repos/ci/project/actions/runs/17":
			_ = json.NewEncoder(w).Encode(run)
		case "/api/v1/repos/ci/project/actions/tasks":
			tasks := []any{}
			for _, id := range []int{41, 40} {
				status := "failure"
				if id == 41 {
					status = currentStatus
				}
				tasks = append(tasks, map[string]any{
					"id": id, "run_number": 9, "workflow_id": "ci.yml", "name": "unit", "head_sha": pr.HeadSHA,
					"status": status, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(tasks), "workflow_runs": tasks})
		default:
			t.Errorf("unexpected provider route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	selected := cibackend.BackendConfig{
		Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true,
		LogBridge: &cibackend.LogBridgeConfig{URL: bridge.URL, TokenFile: "fixture", AllowHTTP: true},
	}
	backend, err := cibackend.NewHTTP(selected, "provider-fixture-only", "bridge-fixture-only")
	if err != nil {
		t.Fatal(err)
	}
	config.Backends["builder"] = selected
	required := []string{"unit"}
	binding := config.Repositories[repo.FullName]
	binding.RequiredChecks = &required
	config.Repositories[repo.FullName] = binding
	s.CI, err = cibackend.WithBackends(config, map[string]cibackend.Backend{"builder": backend})
	if err != nil {
		t.Fatal(err)
	}

	for _, status := range []string{"success", "failure"} {
		currentStatus = status
		observed, err := s.ReadCIChecks(ctx, pr)
		if err != nil || len(observed.Checks) != 1 || observed.Checks[0].Conclusion != status || !observed.Checks[0].Required {
			t.Fatalf("PR checks must expose only the current task (%s): %+v %v", status, observed, err)
		}
		bySHA, err := s.ReadCIChecksForSHA(ctx, repo, pr.HeadSHA)
		if err != nil || len(bySHA.Checks) != 1 || bySHA.Checks[0].ID != observed.Checks[0].ID || bySHA.Checks[0].Conclusion != status {
			t.Fatalf("commit checks disagree with PR checks: %+v %v", bySHA, err)
		}
		if err := s.enforceConfiguredCIForMerge(ctx, pr); (err == nil) != (status == "success") {
			t.Fatalf("CI preflight for current task %s: %v", status, err)
		}
	}

	for _, tc := range []struct {
		name, runStatus, taskStatus, wantStatus, wantConclusion string
		unboundStatus                                           int
		wantRunCheck                                            bool
	}{
		{"waiting_attempt_after_completed_task", "waiting", "success", "queued", "", 5, true},
		{"blocked_attempt_after_completed_task", "blocked", "success", "queued", "", 7, true},
		{"running_after_completed_task", "running", "success", "in_progress", "", 5, true},
		{"waiting_after_failed_task", "waiting", "failure", "queued", "", 5, true},
		{"failed_run_after_successful_task", "failure", "success", "completed", "failure", 3, true},
		{"cancelled_run_after_successful_task", "cancelled", "success", "completed", "cancelled", 3, true},
		{"completed_success_needs_no_run_check", "success", "success", "", "", 4, false},
		{"failed_task_already_expresses_failure", "failure", "failure", "", "", 3, false},
		{"cancelled_task_already_expresses_cancellation", "cancelled", "cancelled", "", "", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			currentStatus, runStatus = tc.taskStatus, tc.runStatus
			t.Cleanup(func() {
				runStatus = ""
				if err := providerDB.Exec(`DELETE FROM action_run_job WHERE id=33`).Error; err != nil {
					t.Fatal(err)
				}
			})
			taskStatus := map[string]int{"success": 1, "failure": 2, "cancelled": 3}[tc.taskStatus]
			if err := providerDB.Exec(`UPDATE action_run_job SET status=?,started=10,stopped=20,attempt=1 WHERE id=31`, taskStatus).Error; err != nil {
				t.Fatal(err)
			}
			stopped := 0
			if tc.unboundStatus == 3 {
				stopped = 20
			}
			if err := providerDB.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha,status,started,stopped,attempt) VALUES (33,17,1,0,'waiting-stage',?,?,0,?,1)`, pr.HeadSHA, tc.unboundStatus, stopped).Error; err != nil {
				t.Fatal(err)
			}
			observed, err := s.ReadCIChecks(ctx, pr)
			if err != nil {
				t.Fatal(err)
			}
			var runCheck *CICheck
			var unit *CICheck
			for i := range observed.Checks {
				check := &observed.Checks[i]
				if check.ID == fmt.Sprint(check.RunID) {
					runCheck = check
				}
				if check.Name == "unit" {
					unit = check
				}
			}
			if unit == nil || unit.Status != "completed" || unit.Conclusion != tc.taskStatus || !unit.Required {
				t.Fatalf("real task evidence changed: %+v", observed.Checks)
			}
			if tc.wantRunCheck {
				if len(observed.Checks) != 2 || runCheck == nil || runCheck.Name != "ci.yml" || runCheck.Status != tc.wantStatus || runCheck.Conclusion != tc.wantConclusion || runCheck.Required {
					t.Fatalf("task checks hid or invented provider run state: %+v", observed.Checks)
				}
				jobs, err := s.CIJobs(ctx, repo.FullName, runCheck.RunID)
				if err != nil || len(jobs) != 1 || jobs[0].Job.Name != "unit" {
					t.Fatalf("run check invented a task: %+v %v", jobs, err)
				}
				if _, err := s.CILogs(ctx, repo.FullName, runCheck.RunID, true); !errors.Is(err, ErrNotFound) {
					t.Fatalf("run check acquired job-log identity: %v", err)
				}
			} else if len(observed.Checks) != 1 || runCheck != nil {
				t.Fatalf("represented provider outcome acquired a duplicate check: %+v", observed.Checks)
			}
			// Only unit is required here. Observing the workflow must not silently
			// add another required name or erase the real unit result.
			if err := s.enforceConfiguredCIForMerge(ctx, pr); (err == nil) != (tc.taskStatus == "success") {
				t.Fatalf("explicit required-name policy changed: %v", err)
			}
		})
	}

	// Two current jobs really sharing a name are still ambiguous. Filtering
	// historical tasks must not turn a conflicting current check into success.
	if err := providerDB.Exec(`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha) VALUES (32, 17, 1, 40, 'unit', ?)`, pr.HeadSHA).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadCIChecks(ctx, pr); !errors.Is(err, cibackend.ErrInvalid) {
		t.Fatalf("conflicting current names must remain invalid: %v", err)
	}
}
