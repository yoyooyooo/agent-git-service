package cibackend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForgejoPRLogsCarryTheExactProviderSourceBranch(t *testing.T) {
	head := strings.Repeat("a", 40)
	providerPRReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/ci/project/actions/runs/17":
			run := forgejoFixtureRun()
			run["prettyref"] = "#42"
			run["event"] = "pull_request"
			_ = json.NewEncoder(w).Encode(run)
		case "/api/v1/repos/ci/project/actions/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{"id": 41, "run_number": 9, "workflow_id": "ci.yml", "name": "test", "head_sha": head, "status": "success", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}}})
		case "/api/v1/repos/ci/project/pulls/42":
			providerPRReads++
			if r.Header.Get("Authorization") != "token provider-only" {
				t.Error("wrong provider identity")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "head": map[string]any{"ref": "feature/real-source"}, "base": map[string]any{"repo": map[string]any{"full_name": "ci/project"}}})
		case "/api/internal/provider-logs/repos/ci/project/tasks/41":
			if r.Header.Get("Authorization") != "Bearer logs-only" || r.URL.Query().Get("head_ref") != "feature/real-source" || r.URL.Query().Get("provider_pr") != "42" || r.URL.Query().Get("head_sha") != head {
				t.Error("missing exact provider branch/ref identity")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"schema": "ags.internal-provider-log.v1", "repo": "ci/project", "task_id": 41, "run_number": 9, "job_name": "test", "head_sha": head, "provider_pr": 42, "provider_ref": "refs/pull/42/head", "event": "pull_request", "text": "fixture\n"})
		default:
			t.Errorf("guessed log context endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: server.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "logs-only")
	if err != nil {
		t.Fatal(err)
	}
	log, err := backend.JobLogs(context.Background(), "ci/project", "17:41")
	if err != nil || string(log) != "fixture\n" || providerPRReads != 1 {
		t.Fatalf("provider source binding missing: %q %v reads=%d", log, err, providerPRReads)
	}
}
