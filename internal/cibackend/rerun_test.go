package cibackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestForgejoRerunDispatchesExactWorkflowAtOriginalSHA(t *testing.T) {
	for _, action := range []string{"rerun", "rerun-failed-jobs", "job-rerun"} {
		t.Run(action, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(withCurrentJobFixture(map[int64]string{41: "unit"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "token server-only", r.Header.Get("Authorization"))
				if r.Method == "POST" {
					writes++
					require.Equal(t, "/api/v1/repos/ci/project/actions/workflows/ci.yml/dispatches", r.URL.Path)
					require.Equal(t, "application/json", r.Header.Get("Content-Type"))
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, map[string]any{"ref": strings.Repeat("a", 40), "return_run_info": true}, body)
					w.WriteHeader(201)
					return
				}
				switch r.URL.Path {
				case "/api/v1/repos/ci/project/actions/runs/17":
					_ = json.NewEncoder(w).Encode(forgejoFixtureRun())
				case "/api/v1/repos/ci/project/actions/runs":
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{forgejoFixtureRun()}})
				case "/api/v1/repos/ci/project/actions/tasks":
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{"id": 41, "run_number": 9, "workflow_id": "ci.yml", "name": "unit", "head_branch": "feature", "head_sha": strings.Repeat("a", 40), "status": "failure", "url": "https://ci.example.test/ci/project/actions/runs/9", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}}})
				default:
					http.NotFound(w, r)
				}
			})))
			defer server.Close()
			b, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: server.URL, TokenFile: "fixture", AllowHTTP: true}}, "server-only", "bridge-only")
			require.NoError(t, err)
			id := "17"
			if action == "job-rerun" {
				id = "17:41"
			}
			require.NoError(t, b.Action(context.Background(), "ci/project", id, action))
			require.Equal(t, 1, writes)
		})
	}
}

func TestForgejoRerunRejectsActiveUnknownAndDoesNotRetry(t *testing.T) {
	for _, mode := range []string{"active", "untrusted", "fork", "unknown", "forbidden", "conflict", "inputs", "outage", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					writes++
					status := map[string]int{"forbidden": 403, "conflict": 409, "inputs": 400, "outage": 503, "redirect": 307}[mode]
					w.Header().Set("Location", "/replay")
					w.WriteHeader(status)
					return
				}
				if mode == "unknown" {
					http.NotFound(w, r)
					return
				}
				run := forgejoFixtureRun()
				if mode == "active" {
					run["status"] = "running"
				}
				if mode == "untrusted" {
					run["need_approval"] = true
				}
				if mode == "fork" {
					run["is_fork_pull_request"] = true
				}
				_ = json.NewEncoder(w).Encode(run)
			}))
			defer server.Close()
			b, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true}, "fixture")
			require.NoError(t, err)
			// Missing run and collection must not authorize a dispatch.
			if mode == "unknown" {
				err = b.Action(context.Background(), "ci/project", "17", "rerun")
				require.True(t, errors.Is(err, ErrNotFound))
				require.Zero(t, writes)
				return
			}
			err = b.Action(context.Background(), "ci/project", "17", "rerun")
			want := map[string]error{"active": ErrConflict, "untrusted": ErrForbidden, "fork": ErrForbidden, "inputs": ErrConflict, "forbidden": ErrForbidden, "conflict": ErrConflict, "outage": ErrUnavailable, "redirect": ErrUnavailable}[mode]
			require.ErrorIs(t, err, want)
			if mode == "active" || mode == "untrusted" || mode == "fork" {
				require.Zero(t, writes)
			} else {
				require.Equal(t, 1, writes)
			}
		})
	}
}

func TestGitHubActionsJobRerunUsesJobEndpoint(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/repos/ci/project/actions/jobs/41/rerun", r.URL.Path)
		require.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		w.WriteHeader(201)
	}))
	defer server.Close()
	b, err := NewHTTP(BackendConfig{Kind: "github-actions", URL: server.URL, TokenFile: "fixture", AllowHTTP: true}, "fixture")
	require.NoError(t, err)
	require.NoError(t, b.Action(context.Background(), "ci/project", "41", "job-rerun"))
	require.Equal(t, 1, calls)
}
