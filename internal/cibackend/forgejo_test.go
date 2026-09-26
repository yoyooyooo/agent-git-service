package cibackend

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These fields are from Forgejo's published ActionRun/ActionTask schemas, not
// GitHub's workflow_run/job shapes. In particular run 17 != task/job 41.
func forgejoFixtureRun() map[string]any {
	return map[string]any{"id": 17, "index_in_repo": 9, "workflow_id": "ci.yml", "title": "commit title", "prettyref": "feature", "commit_sha": strings.Repeat("a", 40), "event": "push", "status": "success", "created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:01:00Z", "html_url": "https://ci.example.test/ci/project/actions/runs/9"}
}
func TestForgejoNativeRunTaskAndExplicitLogBridge(t *testing.T) {
	corrupt := false
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/api/internal/provider-logs/") {
			if r.Header.Get("Authorization") != "Bearer bridge-only" {
				t.Error("wrong bridge credential")
			}
			if r.URL.Query().Get("head_sha") != strings.Repeat("a", 40) || r.URL.Query().Get("head_ref") != "feature" || r.URL.Query().Get("provider_pr") != "0" {
				t.Error("missing exact log binding")
			}
			sha := strings.Repeat("a", 40)
			if corrupt {
				sha = strings.Repeat("b", 40)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"schema": "ags.internal-provider-log.v1", "repo": "ci/project", "task_id": 41, "run_number": 9, "job_name": "test", "head_sha": sha, "provider_pr": 0, "provider_ref": "refs/heads/feature", "event": "push", "text": "2026-01-01T00:00:00Z fixture log\n"})
			return
		}
		if r.Header.Get("Authorization") != "token server-only" {
			t.Error("wrong provider credential")
		}
		switch r.URL.Path {
		case "/api/v1/repos/ci/project/actions/runs":
			if r.URL.Query().Get("limit") != "50" {
				t.Error("Forgejo uses bounded limit, not per_page")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{forgejoFixtureRun()}})
		case "/api/v1/repos/ci/project/actions/runs/17":
			_ = json.NewEncoder(w).Encode(forgejoFixtureRun())
		case "/api/v1/repos/ci/project/actions/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{"id": 41, "run_number": 9, "workflow_id": "ci.yml", "name": "test", "head_branch": "feature", "head_sha": strings.Repeat("a", 40), "status": "success", "url": "https://ci.example.test/ci/project/actions/runs/9", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}}})
		default:
			t.Errorf("guessed unsupported Forgejo route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	backend, e := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: server.URL, TokenFile: "other-fixture", AllowHTTP: true}}, "server-only", "bridge-only")
	if e != nil {
		t.Fatal(e)
	}
	runs, e := backend.Runs(context.Background(), "ci/project", Query{PerPage: 100})
	if e != nil || len(runs.Items) != 1 || runs.Items[0].Key != "17" {
		t.Fatal(runs, e)
	}
	jobs, e := backend.Jobs(context.Background(), "ci/project", "17")
	if e != nil || len(jobs.Items) != 1 || jobs.Items[0].Key != "17:41" || jobs.Items[0].Run != "17" {
		t.Fatal(jobs, e)
	}
	log, e := backend.JobLogs(context.Background(), "ci/project", "17:41")
	if e != nil || !bytes.Contains(log, []byte("fixture log")) {
		t.Fatal(e, string(log))
	}
	inventoryBefore := 0
	for _, call := range calls {
		if strings.HasSuffix(call, "/actions/tasks") {
			inventoryBefore++
		}
	}
	archive, e := backend.RunLogs(context.Background(), "ci/project", "17")
	if e != nil {
		t.Fatal(e)
	}
	z, e := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if e != nil || len(z.File) != 1 {
		t.Fatal(e)
	}
	r, e := z.File[0].Open()
	if e != nil {
		t.Fatal(e)
	}
	contents, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(contents, log) {
		t.Fatal("archive lost job log")
	}
	inventoryAfter := 0
	for _, call := range calls {
		if strings.HasSuffix(call, "/actions/tasks") {
			inventoryAfter++
		}
	}
	if inventoryAfter-inventoryBefore != 1 {
		t.Fatal("whole-run logs repeatedly enumerated repository task history")
	}
	before := len(calls)
	if e := backend.Action(context.Background(), "ci/project", "17", "rerun"); !errors.Is(e, ErrUnsupported) || len(calls) != before {
		t.Fatal("unsupported mutation performed I/O", e)
	}
	corrupt = true
	if _, e := backend.JobLogs(context.Background(), "ci/project", "17:41"); !errors.Is(e, ErrInvalid) {
		t.Fatal("mismatched log evidence accepted", e)
	}
}
func TestForgejoWithoutLogBridgeDoesNotGuessWebOrDatabaseFallback(t *testing.T) {
	backend, e := NewHTTP(BackendConfig{Kind: "forgejo", URL: "https://ci.example.test", TokenFile: "fixture"}, "fixture-only")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = backend.JobLogs(context.Background(), "ci/project", "17:41"); !errors.Is(e, ErrUnsupported) {
		t.Fatal(e)
	}
}
