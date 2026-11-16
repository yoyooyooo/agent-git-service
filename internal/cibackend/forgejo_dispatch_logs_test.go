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

// These tests cover detached workflow_dispatch log binding on the current
// main structure. They reuse the real-bridge inventory and whole-job log
// fixture already used on main; they do not restore the deploy-line
// forgejo_step_logs_test.go file or its step-archive contract.
func TestForgejoDetachedDispatchLogsThroughRealBridge(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, test := range []struct {
		name, event, trigger, storedRef, wantEvent string
	}{
		{"legacy_empty_event", "", "workflow_dispatch", head, "workflow_dispatch"},
		{"explicit_dispatch_event", "workflow_dispatch", "", head, "workflow_dispatch"},
		{"commit_named_dispatch_branch", "", "workflow_dispatch", "refs/heads/" + head, "workflow_dispatch"},
		{"commit_named_push_branch", "push", "workflow_dispatch", "refs/heads/" + head, "push"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := forgejoFixtureRun()
			raw["prettyref"], raw["event"], raw["trigger_event"], raw["status"] = head, test.event, test.trigger, "failure"
			backend, database, text := detachedDispatchLogFixture(t, raw)
			if err := database.Exec("UPDATE action_run SET ref=?, event=? WHERE id=17", test.storedRef, test.wantEvent).Error; err != nil {
				t.Fatal(err)
			}
			run, err := backend.Run(t.Context(), "ci/project", "17")
			if err != nil {
				t.Fatal(err)
			}
			if run.Branch != head || run.HeadSHA != head || run.Event != test.wantEvent || len(run.PullRequests) != 0 {
				t.Errorf("run identity was not preserved: %+v", run)
			}
			jobs, err := backend.Jobs(t.Context(), "ci/project", "17")
			if err != nil || jobs.Total != 1 || len(jobs.Items) != 1 || jobs.Items[0].Key != "17:41" {
				t.Fatalf("only the real current task is a job: %+v %v", jobs, err)
			}
			body, err := backend.JobLogs(t.Context(), "ci/project", "17:41")
			if err != nil || string(body) != text {
				t.Fatalf("bound whole-job log: %q %v", body, err)
			}
			archive, err := backend.RunLogs(t.Context(), "ci/project", "17")
			if err != nil {
				t.Fatal(err)
			}
			if actual := readWholeJobArchive(t, archive, "_test.txt"); actual != text {
				t.Fatalf("run archive must keep the verified job log: %q", actual)
			}
		})
	}
}

func TestForgejoDetachedDispatchDoesNotInferMissingTrigger(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, trigger := range []string{"", "push", "unknown", " workflow_dispatch", "workflow_dispatch "} {
		t.Run("trigger="+trigger, func(t *testing.T) {
			raw := forgejoFixtureRun()
			raw["prettyref"], raw["event"], raw["trigger_event"] = head, "", trigger
			backend, database, _ := detachedDispatchLogFixture(t, raw)
			if err := database.Exec("UPDATE action_run SET ref=?, event='workflow_dispatch' WHERE id=17", head).Error; err != nil {
				t.Fatal(err)
			}
			run, err := backend.Run(t.Context(), "ci/project", "17")
			if err != nil || run.Event != "" {
				t.Fatalf("unknown trigger must not supply a dispatch event: %+v %v", run, err)
			}
			if _, err := backend.JobLogs(t.Context(), "ci/project", "17:41"); err == nil {
				t.Fatal("logs accepted without a canonical dispatch event")
			}
		})
	}
}

func TestForgejoDetachedDispatchAdapterRejectsForeignBindings(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, test := range []struct {
		name string
		edit func(*Run, map[string]any)
	}{
		{"multiple_pull_requests", func(r *Run, _ map[string]any) { r.PullRequests = []int{42, 43} }},
		{"push_event", func(r *Run, v map[string]any) { r.Event, v["event"] = "push", "push" }},
		{"different_branch", func(r *Run, _ map[string]any) { r.Branch = "feature" }},
		{"different_provider_ref", func(_ *Run, v map[string]any) { v["provider_ref"] = strings.Repeat("b", 40) }},
		{"foreign_provider_pr", func(_ *Run, v map[string]any) { v["provider_pr"] = 42 }},
		{"noncanonical_commit", func(r *Run, v map[string]any) {
			r.Branch, r.HeadSHA = strings.Repeat("A", 40), strings.Repeat("A", 40)
			v["provider_ref"], v["head_sha"] = r.HeadSHA, r.HeadSHA
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := Run{Key: "17", Number: 9, Branch: head, HeadSHA: head, Event: "workflow_dispatch"}
			observation := map[string]any{
				"schema": "ags.internal-provider-log.v1", "repo": "ci/project", "task_id": 41,
				"run_number": 9, "job_name": "test", "head_sha": head, "provider_pr": 0,
				"provider_ref": head, "event": "workflow_dispatch", "text": "fixture\n",
			}
			test.edit(&run, observation)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer logs-only" || r.URL.Path != "/api/internal/provider-logs/repos/ci/project/tasks/41" {
					t.Error("unexpected bridge request")
				}
				_ = json.NewEncoder(w).Encode(observation)
			}))
			defer server.Close()
			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true,
				LogBridge: &LogBridgeConfig{URL: server.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "logs-only")
			if err != nil {
				t.Fatal(err)
			}
			job := Job{Key: "17:41", Run: "17", Name: "test", HeadSHA: run.HeadSHA}
			if _, err := backend.(*Forgejo).boundJobResource(t.Context(), "ci/project", run, job, false); err == nil {
				t.Fatal("foreign detached binding accepted")
			}
		})
	}
}

func detachedDispatchLogFixture(t *testing.T, run map[string]any) (Backend, *gorm.DB, string) {
	t.Helper()
	head := strings.Repeat("a", 40)
	const text = "detached workflow dispatch fixture\n"
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "forgejo.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
		`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
		`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text, status integer DEFAULT 0, started integer DEFAULT 0, stopped integer DEFAULT 0)`,
		`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text, log_filename text, log_in_storage boolean, log_expired boolean)`,
		`INSERT INTO repository VALUES (1,'ci','project')`,
		`INSERT INTO action_run VALUES (17,1,9,'` + head + `','` + head + `','workflow_dispatch')`,
		`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha) VALUES (31,17,1,41,'test','` + head + `')`,
		`INSERT INTO action_run_job (id,run_id,repo_id,task_id,name,commit_sha,status,started,stopped) VALUES (32,17,1,0,'conditional','` + head + `',4,0,0)`,
		`INSERT INTO action_task VALUES (41,1,'` + head + `','job.log',true,false)`,
	} {
		if err := database.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	logs := t.TempDir()
	if err := os.WriteFile(filepath.Join(logs, "job.log"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := providerlogbridge.New(providerlogbridge.Config{DB: database, ActionsLogDir: logs, ServiceToken: "logs-only", AllowedCIDRs: []string{"127.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(providerlogbridge.CaptureSocketPeer)
	router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/tasks/{task}", handler.ServeHTTP)
	router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/runs/{run}/jobs", handler.ServeHTTP)
	bridge := httptest.NewServer(router)
	t.Cleanup(bridge.Close)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token provider-only" {
			t.Error("unexpected provider credential")
		}
		switch r.URL.Path {
		case "/api/v1/repos/ci/project/actions/runs/17":
			_ = json.NewEncoder(w).Encode(run)
		case "/api/v1/repos/ci/project/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
		case "/api/v1/repos/ci/project/actions/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{"id": 41, "run_number": 9, "workflow_id": "ci.yml", "name": "test", "head_sha": head, "status": "failure", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}}})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: bridge.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "logs-only")
	if err != nil {
		t.Fatal(err)
	}
	return backend, database, text
}

func readWholeJobArchive(t *testing.T, data []byte, suffix string) string {
	t.Helper()
	if err := ValidateLogArchive(data); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range archive.File {
		if !strings.HasSuffix(file.Name, suffix) {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	t.Fatalf("archive missing %s: %#v", suffix, archive.File)
	return ""
}
