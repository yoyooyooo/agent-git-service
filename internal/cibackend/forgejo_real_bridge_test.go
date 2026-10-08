package cibackend

import (
	"archive/zip"
	"bytes"
	"encoding/json"
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
		case "/api/v1/repos/ci/project/actions/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{
				"id": 41, "run_number": 9, "workflow_id": "ci.yml", "name": "test", "head_sha": head, "status": "failure",
				"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z",
			}}})
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
	body, err := backend.JobLogs(t.Context(), "ci/project", "17:41")
	if err != nil || string(body) != text {
		t.Fatalf("job log: %q %v", body, err)
	}
	body, err = backend.RunLogs(t.Context(), "ci/project", "17")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(archive.File) != 1 || archive.File[0].Name != "0_test.txt" {
		t.Fatalf("whole-job archive: %#v %v", archive, err)
	}
	reader, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	actual, err := io.ReadAll(reader)
	if err != nil || string(actual) != text {
		t.Fatalf("archived log: %q %v", actual, err)
	}
}
