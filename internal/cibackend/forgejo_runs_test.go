package cibackend

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func forgejoRunInventoryFixture(total int) []map[string]any {
	items := make([]map[string]any, total)
	for i := range items {
		n := (i*37)%total + 1 // Deliberately unrelated to ID or creation order.
		run := forgejoFixtureRun()
		run["id"], run["index_in_repo"] = n+10000, n
		run["html_url"] = fmt.Sprintf("https://ci.example.test/ci/project/actions/runs/%d", n)
		run["prettyref"], run["workflow_id"], run["status"], run["event"] = "main", "ci.yml", "success", "push"
		if n%2 == 0 {
			run["prettyref"], run["workflow_id"], run["status"], run["event"] = "feature", "test.yml", "failure", "workflow_dispatch"
		}
		items[i] = run
	}
	return items
}

func forgejoRunInventoryServer(t *testing.T, items []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/ci/project/actions/runs" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		selected := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if q.Get("head_sha") != "" && q.Get("head_sha") != item["commit_sha"] {
				continue
			}
			matches := true
			for _, key := range []string{"event", "status"} {
				if values := q[key]; len(values) > 0 {
					matched := false
					for _, value := range values {
						matched = matched || value == item[key]
					}
					matches = matches && matched
				}
			}
			if q.Get("run_number") != "" && q.Get("run_number") != fmt.Sprint(item["index_in_repo"]) {
				continue
			}
			if matches {
				selected = append(selected, item)
			}
		}
		page, _ := strconv.Atoi(q.Get("page"))
		size, _ := strconv.Atoi(q.Get("limit"))
		require.Greater(t, page, 0)
		require.Greater(t, size, 0)
		require.LessOrEqual(t, size, 50, "provider page size is independent of the client page size")
		start := min((page-1)*size, len(selected))
		end := min(start+size, len(selected))
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(selected), "workflow_runs": selected[start:end]})
	}))
}

func TestForgejoRunsSortAndPaginateTheCompleteInventory(t *testing.T) {
	provider := forgejoRunInventoryServer(t, forgejoRunInventoryFixture(125))
	defer provider.Close()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}, "provider-only", "")
	require.NoError(t, err)
	seen := map[string]bool{}
	for page := 1; page <= 2; page++ {
		found, err := backend.Runs(t.Context(), "ci/project", Query{Page: page, PerPage: 100})
		require.NoError(t, err)
		require.Equal(t, 125, found.Total)
		require.Len(t, found.Items, map[int]int{1: 100, 2: 25}[page])
		require.Equal(t, page == 2, found.Complete)
		for i, run := range found.Items {
			require.Equal(t, int64(125-(page-1)*100-i), run.Number)
			require.False(t, seen[run.Key], "client pages must not overlap")
			seen[run.Key] = true
		}
	}
	require.Len(t, seen, 125)
	last, err := backend.Runs(t.Context(), "ci/project", Query{Page: 3, PerPage: 100})
	require.NoError(t, err)
	require.Empty(t, last.Items)
	require.Equal(t, 125, last.Total)
	require.True(t, last.Complete)
}

func TestForgejoRunsFilterBeforePaginationAndCountConclusions(t *testing.T) {
	provider := forgejoRunInventoryServer(t, forgejoRunInventoryFixture(125))
	defer provider.Close()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}, "provider-only", "")
	require.NoError(t, err)
	query := Query{Page: 2, PerPage: 10, Branch: "feature", Workflow: "test.yml", Status: "failure", Event: "workflow_dispatch", HeadSHA: strings.Repeat("a", 40)}
	found, err := backend.Runs(t.Context(), "ci/project", query)
	require.NoError(t, err)
	require.Equal(t, 62, found.Total)
	require.Len(t, found.Items, 10)
	require.False(t, found.Complete)
	for i, run := range found.Items {
		require.Equal(t, int64(104-2*i), run.Number)
		require.Equal(t, "completed", run.Status)
		require.Equal(t, "failure", run.Conclusion)
	}
	query.Status = "completed"
	found, err = backend.Runs(t.Context(), "ci/project", query)
	require.NoError(t, err)
	require.Equal(t, 62, found.Total)
	query.Branch = "missing"
	found, err = backend.Runs(t.Context(), "ci/project", query)
	require.NoError(t, err)
	require.Zero(t, found.Total)
	require.Empty(t, found.Items)
	require.True(t, found.Complete)
}

func TestForgejoRunsRejectIncompleteOrUnstableInventories(t *testing.T) {
	for _, mode := range []string{"duplicate", "number-collision", "changed-total", "short-page", "budget"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			items := forgejoRunInventoryFixture(125)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				start := (page - 1) * 50
				end := min(start+50, len(items))
				batch := append([]map[string]any{}, items[start:end]...)
				total := len(items)
				if page == 2 {
					switch mode {
					case "duplicate":
						batch[0] = items[0]
					case "number-collision":
						batch[0]["index_in_repo"] = items[0]["index_in_repo"]
					case "changed-total":
						total++
					case "short-page":
						batch = batch[:3]
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "workflow_runs": batch})
			}))
			defer provider.Close()
			limit := 100
			if mode == "budget" {
				limit = 1
			}
			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, TaskPageLimit: limit}, "provider-only", "")
			require.NoError(t, err)
			_, err = backend.Runs(t.Context(), "ci/project", Query{Page: 1, PerPage: 10})
			require.True(t, errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalid), "partial or ambiguous inventories cannot become pages: %v", err)
			require.LessOrEqual(t, calls, limit)
		})
	}
}

func TestForgejoIndexedRunPagesAndOldBridgeFallback(t *testing.T) {
	provider := forgejoRunInventoryServer(t, forgejoRunInventoryFixture(125))
	defer provider.Close()
	for _, mode := range []string{"valid", "wrong-repo", "wrong-schema", "wrong-filter", "duplicate", "unsorted", "short-page", "denied", "unavailable", "old-404", "old-501"} {
		t.Run(mode, func(t *testing.T) {
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/internal/provider-logs/repos/ci/project/runs", r.URL.Path)
				require.Equal(t, "Bearer bridge-only", r.Header.Get("Authorization"))
				require.Equal(t, "2", r.URL.Query().Get("per_page"))
				require.Equal(t, "failure", r.URL.Query().Get("status"))
				run1, run2 := forgejoFixtureRun(), forgejoFixtureRun()
				run1["status"], run2["status"] = "failure", "failure"
				run2["id"], run2["index_in_repo"] = 16, 8
				items := []any{run1, run2}
				schema, repo := "ags.internal-provider-runs.v1", "ci/project"
				switch mode {
				case "wrong-repo":
					repo = "ci/other"
				case "wrong-schema":
					schema = "unknown"
				case "wrong-filter":
					run2["status"] = "success"
				case "duplicate":
					run2["id"] = run1["id"]
				case "unsorted":
					items = []any{run2, run1}
				case "short-page":
					items = items[:1]
				case "denied":
					w.WriteHeader(403)
					return
				case "unavailable":
					w.WriteHeader(500)
					return
				case "old-404":
					w.WriteHeader(404)
					return
				case "old-501":
					w.WriteHeader(501)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"schema": schema, "repo": repo, "total_count": 3, "workflow_runs": items})
			}))
			defer bridge.Close()
			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, WebURL: "https://web.example.test", TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: bridge.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "bridge-only")
			require.NoError(t, err)
			found, err := backend.Runs(t.Context(), "ci/project", Query{Page: 1, PerPage: 2, Status: "failure"})
			switch mode {
			case "valid":
				require.NoError(t, err)
				require.Equal(t, 3, found.Total)
				require.False(t, found.Complete)
				require.Equal(t, []int64{9, 8}, []int64{found.Items[0].Number, found.Items[1].Number})
				require.Equal(t, "https://web.example.test/ci/project/actions/runs/9", found.Items[0].URL)
			case "old-404", "old-501":
				require.NoError(t, err)
				require.Equal(t, 62, found.Total)
				require.Equal(t, []int64{124, 122}, []int64{found.Items[0].Number, found.Items[1].Number})
			case "denied":
				require.ErrorIs(t, err, ErrForbidden, "provider denial must not become an API fallback")
			case "unavailable":
				require.ErrorIs(t, err, ErrUnavailable, "an outage must not become stale/partial evidence")
			default:
				require.ErrorIs(t, err, ErrInvalid, "invalid indexed evidence must not silently fall back")
			}
		})
	}
}
