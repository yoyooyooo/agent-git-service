package cibackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestForgejoJobsWithVerifiedEmptyCurrentSetSkipTaskInventory(t *testing.T) {
	for _, status := range []string{"waiting", "blocked"} {
		t.Run(status, func(t *testing.T) {
			run := forgejoFixtureRun()
			run["status"] = status
			var taskRequests atomic.Int32
			provider := httptest.NewServer(withCurrentJobFixture(map[int64]string{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/repos/ci/project/actions/runs/17":
					_ = json.NewEncoder(w).Encode(run)
				case "/api/v1/repos/ci/project/actions/runs":
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
				case "/api/v1/repos/ci/project/actions/tasks":
					taskRequests.Add(1)
					items := make([]any, 50)
					for i := range items {
						items[i] = map[string]any{"id": 5000 - i, "run_number": 8}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 5000, "workflow_runs": items})
				default:
					t.Errorf("unexpected provider route: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})))
			defer provider.Close()

			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, TaskPageLimit: 1, LogBridge: &LogBridgeConfig{URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "bridge-only")
			if err != nil {
				t.Fatal(err)
			}
			found, err := backend.Jobs(context.Background(), "ci/project", "17")
			if err != nil || !found.Complete || found.Total != 0 || found.Items == nil || len(found.Items) != 0 || taskRequests.Load() != 0 {
				t.Fatalf("verified empty current tasks depended on repository history: requests=%d jobs=%+v error=%v", taskRequests.Load(), found, err)
			}
		})
	}
}

func TestForgejoJobsRejectUnverifiedEmptyCurrentSet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		status int
		err    error
	}{
		{name: "schema", change: func(body map[string]any) { body["schema"] = "unknown" }, err: ErrInvalid},
		{name: "repository", change: func(body map[string]any) { body["repo"] = "ci/other" }, err: ErrInvalid},
		{name: "native run", change: func(body map[string]any) { body["run"] = "18" }, err: ErrInvalid},
		{name: "run number", change: func(body map[string]any) { body["run_number"] = 10 }, err: ErrInvalid},
		{name: "commit", change: func(body map[string]any) { body["head_sha"] = strings.Repeat("b", 40) }, err: ErrInvalid},
		{name: "missing jobs", change: func(body map[string]any) { delete(body, "jobs") }, err: ErrInvalid},
		{name: "null jobs", change: func(body map[string]any) { body["jobs"] = nil }, err: ErrInvalid},
		{name: "unsupported", status: http.StatusNotImplemented, err: ErrUnsupported},
		{name: "not found", status: http.StatusNotFound, err: ErrNotFound},
		{name: "unavailable", status: http.StatusServiceUnavailable, err: ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var taskRequests atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/repos/ci/project/actions/runs/17":
					_ = json.NewEncoder(w).Encode(forgejoFixtureRun())
				case "/api/v1/repos/ci/project/actions/runs":
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{forgejoFixtureRun()}})
				case "/api/internal/provider-logs/repos/ci/project/runs/17/jobs":
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						return
					}
					body := map[string]any{"schema": "ags.internal-provider-jobs.v1", "repo": "ci/project", "run": "17", "run_number": 9, "head_sha": strings.Repeat("a", 40), "jobs": []any{}}
					tc.change(body)
					_ = json.NewEncoder(w).Encode(body)
				case "/api/v1/repos/ci/project/actions/tasks":
					taskRequests.Add(1)
					t.Error("unverified current tasks started a repository history scan")
					http.NotFound(w, r)
				default:
					t.Errorf("unexpected provider route: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer provider.Close()

			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "bridge-only")
			if err != nil {
				t.Fatal(err)
			}
			found, err := backend.Jobs(context.Background(), "ci/project", "17")
			if !errors.Is(err, tc.err) || found.Complete || taskRequests.Load() != 0 {
				t.Fatalf("unverified empty current tasks became an observation: requests=%d jobs=%+v error=%v, want %v", taskRequests.Load(), found, err, tc.err)
			}
		})
	}
}
