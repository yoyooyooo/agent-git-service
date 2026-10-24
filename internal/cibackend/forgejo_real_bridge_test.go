package cibackend

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/providerlogbridge"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestForgejoSyncJobAndRunLogsThroughRealBridge(t *testing.T) {
	testForgejoDiagnosticBridge(t, false, false)
}

func TestForgejoDiagnosticGapsThroughRealBridge(t *testing.T) {
	testForgejoDiagnosticBridge(t, true, false)
}

func TestForgejoInvalidJobRepositoryFailsInventoryAndRunLogsThroughRealBridge(t *testing.T) {
	testForgejoDiagnosticBridge(t, true, true)
}

func testForgejoDiagnosticBridge(t *testing.T, gaps, corruptJobRepo bool) {
	t.Helper()
	head := strings.Repeat("a", 40)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "forgejo.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
		`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
		`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text)`,
		`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text, log_filename text, log_in_storage boolean, log_expired boolean)`,
		`INSERT INTO repository VALUES (1, 'ci', 'project')`,
		`INSERT INTO action_run VALUES (17, 1, 9, 'refs/pull/42/head', '` + head + `', 'pull_request_sync')`,
		`INSERT INTO action_run_job VALUES (31, 17, 1, 41, 'test', '` + head + `')`,
		`INSERT INTO action_task VALUES (41, 1, '` + head + `', 'job.log', true, false)`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	if gaps {
		// A retry leaves task 40 behind; only 41 and 42 still back current jobs.
		for _, sql := range []string{
			`INSERT INTO action_task VALUES (40, 1, '` + head + `', 'orphan.log', true, false)`,
			`INSERT INTO action_task VALUES (42, 1, '` + head + `', 'missing.log', true, false)`,
			`INSERT INTO action_run_job VALUES (32, 17, 1, 42, 'lint', '` + head + `')`,
		} {
			if err := db.Exec(sql).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if corruptJobRepo {
		if err := db.Exec(`UPDATE action_run_job SET repo_id=2 WHERE id=32`).Error; err != nil {
			t.Fatal(err)
		}
	}
	logs := t.TempDir()
	const text = "test failed: synchronized PR fixture\n"
	if err := os.WriteFile(filepath.Join(logs, "job.log"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := providerlogbridge.New(providerlogbridge.Config{
		DB: db, ActionsLogDir: logs, ServiceToken: "logs-only", AllowedCIDRs: []string{"127.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(providerlogbridge.CaptureSocketPeer)
	router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/tasks/{task}", handler.ServeHTTP)
	router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", handler.ServeHTTP)
	bridge := httptest.NewServer(router)
	defer bridge.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token provider-only" {
			t.Error("provider did not receive its own credential")
		}
		switch r.URL.Path {
		case "/api/v1/repos/ci/project/actions/runs/17":
			run := forgejoFixtureRun()
			run["prettyref"], run["event"], run["status"] = "#42", "pull_request", "failure"
			_ = json.NewEncoder(w).Encode(run)
		case "/api/v1/repos/ci/project/actions/runs":
			run := forgejoFixtureRun()
			run["prettyref"], run["event"], run["status"] = "#42", "pull_request", "failure"
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
		case "/api/v1/repos/ci/project/actions/tasks":
			tasks := []any{}
			ids := []int{41}
			if gaps {
				ids = []int{42, 41, 40}
			}
			for _, id := range ids {
				name := "test"
				if id == 42 {
					name = "lint"
				}
				tasks = append(tasks, map[string]any{
					"id": id, "run_number": 9, "workflow_id": "ci.yml", "name": name, "head_sha": head, "status": "failure",
					"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(tasks), "workflow_runs": tasks})
		case "/api/v1/repos/ci/project/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "head": map[string]any{"ref": "feature/source"}})
		default:
			t.Errorf("unexpected provider route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	backend, err := NewHTTP(BackendConfig{
		Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true,
		LogBridge: &LogBridgeConfig{URL: bridge.URL, TokenFile: "fixture", AllowHTTP: true},
	}, "provider-only", "logs-only")
	if err != nil {
		t.Fatal(err)
	}
	if corruptJobRepo {
		jobs, err := backend.Jobs(t.Context(), "ci/project", "17")
		if !errors.Is(err, ErrUnavailable) || jobs.Complete || jobs.Total != 0 || len(jobs.Items) != 0 {
			t.Errorf("invalid inventory must fail without partial jobs: %+v %v", jobs, err)
		}
		body, err := backend.RunLogs(t.Context(), "ci/project", "17")
		if !errors.Is(err, ErrUnavailable) || len(body) != 0 {
			t.Errorf("invalid inventory must fail without a partial ZIP: bytes=%d err=%v", len(body), err)
		}
		return
	}
	if gaps {
		jobs, err := backend.Jobs(t.Context(), "ci/project", "17")
		if err != nil || !jobs.Complete || len(jobs.Items) != 2 {
			t.Errorf("current jobs: %+v %v", jobs, err)
		}
		for _, job := range jobs.Items {
			if job.Key == "17:40" {
				t.Error("orphan task exposed as a job")
			}
		}
		if _, err := backend.Job(t.Context(), "ci/project", "17:40"); !errors.Is(err, ErrNotFound) {
			t.Errorf("retired job lookup: %v", err)
		}
	}
	body, err := backend.JobLogs(t.Context(), "ci/project", "17:41")
	if err != nil || string(body) != text {
		t.Fatalf("job log: %q %v", body, err)
	}
	body, err = backend.RunLogs(t.Context(), "ci/project", "17")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	expected := 1
	if gaps {
		expected = 2
	}
	if err != nil || len(archive.File) != expected {
		t.Fatalf("whole-job archive: %#v %v", archive, err)
	}
	var actualLog *zip.File
	for _, file := range archive.File {
		if strings.HasSuffix(file.Name, "_test.txt") {
			actualLog = file
		}
		if gaps && strings.HasSuffix(file.Name, "_lint.txt") {
			r, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(r)
			r.Close()
			if err != nil || !bytes.Contains(body, []byte("AGS_LOG_UNAVAILABLE")) || !bytes.Contains(body, []byte("17:42")) {
				t.Fatalf("missing log diagnosis: %s %v", body, err)
			}
		}
	}
	if actualLog == nil {
		t.Fatal("healthy job log lost")
	}
	reader, err := actualLog.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	actual, err := io.ReadAll(reader)
	if err != nil || string(actual) != text {
		t.Fatalf("archived log: %q %v", actual, err)
	}
}
