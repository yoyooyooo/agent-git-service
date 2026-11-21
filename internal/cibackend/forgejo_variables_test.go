package cibackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type variableFixture struct {
	mu       sync.Mutex
	requests []string
	total    int
	cap      int
	override func(http.ResponseWriter, *http.Request) bool
}

func (f *variableFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.RequestURI())
	f.mu.Unlock()
	if r.Method != http.MethodGet || r.Header.Get("Authorization") != "token fixture-only" {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	if f.override != nil && f.override(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/repos/ci/project":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "full_name": "ci/project"})
	case "/api/v1/settings/api":
		_ = json.NewEncoder(w).Encode(map[string]any{"max_response_items": f.cap, "default_paging_num": 30})
	case "/api/v1/repos/ci/project/actions/variables":
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if page < 1 || limit < 1 {
			http.Error(w, "explicit pagination required", http.StatusBadRequest)
			return
		}
		limit = min(limit, f.cap)
		start := min((page-1)*limit, f.total)
		end := min(start+limit, f.total)
		items := make([]map[string]any, 0, end-start)
		for i := start; i < end; i++ {
			// Forgejo 12.0.4's repository list omits the stored data.
			items = append(items, map[string]any{"owner_id": 0, "repo_id": 42, "name": variableFixtureName(i), "data": ""})
		}
		w.Header().Set("X-Total-Count", strconv.Itoa(f.total))
		_ = json.NewEncoder(w).Encode(items)
	default:
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/ci/project/actions/variables/")
		index, err := strconv.Atoi(strings.TrimPrefix(name, "VAR_"))
		if err != nil || name != variableFixtureName(index) || index < 0 || index >= f.total {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(variableFixtureDetail(index))
	}
}

func variableFixtureName(index int) string { return fmt.Sprintf("VAR_%03d", index) }

func variableFixtureDetail(index int) map[string]any {
	value := fmt.Sprintf("fixture-%03d", index)
	if index == 2 {
		value = ""
	}
	return map[string]any{"owner_id": 0, "repo_id": 42, "name": variableFixtureName(index), "data": value}
}

func newVariableFixture(t *testing.T, total, cap int, override func(http.ResponseWriter, *http.Request) bool) (RepositoryVariableReader, *variableFixture) {
	t.Helper()
	fixture := &variableFixture{total: total, cap: cap, override: override}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true}, "fixture-only")
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := backend.(RepositoryVariableReader)
	if !ok {
		t.Fatal("Forgejo backend does not implement repository variable reads")
	}
	return reader, fixture
}

func (f *variableFixture) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func TestForgejoRepositoryVariablesHydrateOnlyTheRequestedPage(t *testing.T) {
	for _, tc := range []struct{ page, size, start, count int }{{1, 100, 0, 100}, {2, 75, 75, 75}, {3, 75, 150, 15}, {4, 75, 225, 0}} {
		t.Run(fmt.Sprintf("page%d-size%d", tc.page, tc.size), func(t *testing.T) {
			reader, fixture := newVariableFixture(t, 165, 50, nil)
			got, err := reader.RepositoryVariables(t.Context(), "ci/project", tc.page, tc.size)
			if err != nil {
				t.Fatal(err)
			}
			if got.Total != 165 || len(got.Items) != tc.count || got.Items == nil {
				t.Fatalf("wrong page: total=%d items=%d", got.Total, len(got.Items))
			}
			for i, v := range got.Items {
				want := variableFixtureDetail(tc.start + i)
				if v.Name != want["name"] || v.Value != want["data"] {
					t.Fatalf("detail was not preserved at index %d", i)
				}
			}
			details := 0
			for _, path := range fixture.paths() {
				if strings.Contains(path, "/variables/") {
					details++
				}
			}
			if details != tc.count {
				t.Fatalf("hydrated %d variables for a %d-item page", details, tc.count)
			}
		})
	}
}

func TestForgejoRepositoryVariableDistinguishesEmptyAndMissing(t *testing.T) {
	reader, _ := newVariableFixture(t, 3, 50, nil)
	value, err := reader.RepositoryVariable(t.Context(), "ci/project", "var_002")
	if err != nil || value.Name != "VAR_002" || value.Value != "" {
		t.Fatalf("defined empty variable did not survive: %v", err)
	}
	if _, err = reader.RepositoryVariable(t.Context(), "ci/project", "ABSENT"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected a proven missing repository variable, got %v", err)
	}
}

func TestForgejoRepositoryVariablesUseTheConfiguredPageLimitWithoutCaching(t *testing.T) {
	reader, fixture := newVariableFixture(t, 211, 37, nil)
	for request := 0; request < 2; request++ {
		got, err := reader.RepositoryVariables(t.Context(), "ci/project", 2, 100)
		if err != nil || got.Total != 211 || len(got.Items) != 100 {
			t.Fatalf("nondefault provider limit changed the outer page: %v", err)
		}
		for index, item := range got.Items {
			if item.Name != variableFixtureName(100+index) {
				t.Fatalf("wrong offset at index %d", index)
			}
		}
	}
	settings, pages := 0, 0
	for _, path := range fixture.paths() {
		if path == "/api/v1/settings/api" {
			settings++
		}
		if strings.Contains(path, "/variables?") {
			pages++
			if !strings.Contains(path, "limit=37") {
				t.Fatal("provider page limit was guessed")
			}
		}
	}
	if settings != 2 || pages != 8 {
		t.Fatalf("unexpected settings/page calls: %d/%d", settings, pages)
	}
}

func TestForgejoRepositoryVariablesRejectMissingOrInvalidSettings(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"max_response_items":0}`, `{"max_response_items":-1}`, `{"max_response_items":"50"}`} {
		t.Run(body, func(t *testing.T) {
			reader, fixture := newVariableFixture(t, 1, 50, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/v1/settings/api" {
					return false
				}
				_, _ = w.Write([]byte(body))
				return true
			})
			if _, err := reader.RepositoryVariables(t.Context(), "ci/project", 1, 100); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid settings accepted: %v", err)
			}
			for _, path := range fixture.paths() {
				if strings.Contains(path, "/variables") {
					t.Fatal("invalid settings fell back to a guessed page size")
				}
			}
		})
	}
}

func TestForgejoRepositoryVariableChecksBindingIdentityAndBodyLimit(t *testing.T) {
	for _, mode := range []string{"wrong-name", "wrong-id", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			reader, _ := newVariableFixture(t, 1, 50, func(w http.ResponseWriter, r *http.Request) bool {
				if mode == "oversized" && strings.Contains(r.URL.Path, "/variables/") {
					_, _ = w.Write([]byte(strings.Repeat("x", MaxBody+1)))
					return true
				}
				if r.URL.Path != "/api/v1/repos/ci/project" || mode == "oversized" {
					return false
				}
				metadata := map[string]any{"id": 42, "full_name": "ci/project"}
				if mode == "wrong-name" {
					metadata["full_name"] = "other/project"
				} else {
					metadata["id"] = 0
				}
				_ = json.NewEncoder(w).Encode(metadata)
				return true
			})
			_, err := reader.RepositoryVariable(t.Context(), "ci/project", "VAR_000")
			want := ErrInvalid
			if mode == "oversized" {
				want = ErrUnavailable
			}
			if !errors.Is(err, want) {
				t.Fatalf("unsafe binding or response was accepted: %v", err)
			}
		})
	}
}

func TestForgejoRepositoryVariableRejectsUnprovenAbsenceAndOutboundFailures(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		status     int
	}{
		{"missing-binding", "/api/v1/repos/ci/project", 404},
		{"binding-denied", "/api/v1/repos/ci/project", 403},
		{"credential-rejected", "/api/v1/repos/ci/project/actions/variables/VAR_000", 401},
		{"provider-denied", "/api/v1/repos/ci/project/actions/variables/VAR_000", 403},
		{"provider-outage", "/api/v1/repos/ci/project/actions/variables/VAR_000", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failures atomic.Int64
			reader, _ := newVariableFixture(t, 1, 50, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != tc.path {
					return false
				}
				failures.Add(1)
				http.Error(w, "private-provider-response", tc.status)
				return true
			})
			_, err := reader.RepositoryVariable(t.Context(), "ci/project", "VAR_000")
			if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
				t.Fatalf("outbound failure misclassified: %v", err)
			}
			if strings.Contains(err.Error(), "private-provider-response") || failures.Load() != 1 {
				t.Fatal("provider body escaped or the request was retried")
			}
		})
	}
}

func TestForgejoRepositoryVariableRejectsMalformedDetail(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing-data", `{"owner_id":0,"repo_id":42,"name":"VAR_000"}`},
		{"null-data", `{"owner_id":0,"repo_id":42,"name":"VAR_000","data":null}`},
		{"non-string-data", `{"owner_id":0,"repo_id":42,"name":"VAR_000","data":false}`},
		{"foreign-repo", `{"owner_id":0,"repo_id":43,"name":"VAR_000","data":"fixture"}`},
		{"owner-scope", `{"owner_id":42,"repo_id":42,"name":"VAR_000","data":"fixture"}`},
		{"missing-owner", `{"repo_id":42,"name":"VAR_000","data":"fixture"}`},
		{"other-name", `{"owner_id":0,"repo_id":42,"name":"VAR_001","data":"fixture"}`},
		{"trailing-json", `{"owner_id":0,"repo_id":42,"name":"VAR_000","data":"fixture"} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, _ := newVariableFixture(t, 1, 50, func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.Contains(r.URL.Path, "/variables/") {
					return false
				}
				_, _ = w.Write([]byte(tc.body))
				return true
			})
			if _, err := reader.RepositoryVariable(t.Context(), "ci/project", "VAR_000"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("malformed detail accepted: %v", err)
			}
		})
	}
}

func TestForgejoRepositoryVariablesRejectIncompleteInventory(t *testing.T) {
	for _, tc := range []struct{ name, count, body string }{
		{"missing-count", "", `[]`},
		{"bad-count", "unknown", `[]`},
		{"negative-count", "-1", `[]`},
		{"null-inventory", "0", `null`},
		{"truncated-page", "2", `[{"owner_id":0,"repo_id":42,"name":"VAR_000"}]`},
		{"duplicate", "2", `[{"owner_id":0,"repo_id":42,"name":"VAR_000"},{"owner_id":0,"repo_id":42,"name":"var_000"}]`},
		{"cross-repo", "1", `[{"owner_id":0,"repo_id":43,"name":"VAR_000"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, _ := newVariableFixture(t, 2, 50, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/v1/repos/ci/project/actions/variables" {
					return false
				}
				if tc.count != "" {
					w.Header().Set("X-Total-Count", tc.count)
				}
				_, _ = w.Write([]byte(tc.body))
				return true
			})
			got, err := reader.RepositoryVariables(t.Context(), "ci/project", 1, 100)
			if err == nil || len(got.Items) != 0 || got.Total != 0 {
				t.Fatal("incomplete inventory returned a partial success")
			}
		})
	}
}

func TestForgejoRepositoryVariablesRejectChangedTotalOrDeletedDetail(t *testing.T) {
	for _, kind := range []string{"total", "detail-deleted"} {
		t.Run(kind, func(t *testing.T) {
			reader, _ := newVariableFixture(t, 100, 50, func(w http.ResponseWriter, r *http.Request) bool {
				if kind == "total" && r.URL.Query().Get("page") == "2" {
					w.Header().Set("X-Total-Count", "101")
					_, _ = w.Write([]byte(`[]`))
					return true
				}
				if kind == "detail-deleted" && strings.Contains(r.URL.Path, "/variables/") {
					http.NotFound(w, r)
					return true
				}
				return false
			})
			got, err := reader.RepositoryVariables(t.Context(), "ci/project", 1, 100)
			if err == nil || errors.Is(err, ErrNotFound) || len(got.Items) > 0 {
				t.Fatalf("unstable inventory became a result: %v", err)
			}
		})
	}
}

func TestForgejoRepositoryVariablesBoundHydrationAndHonorCancellation(t *testing.T) {
	started := make(chan struct{}, 20)
	release := make(chan struct{})
	var active, peak atomic.Int64
	reader, _ := newVariableFixture(t, 20, 50, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/variables/") {
			return false
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return false
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := reader.RepositoryVariables(ctx, "ci/project", 1, 20)
		finished <- err
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			cancel()
			close(release)
			t.Fatal("hydration workers did not start")
		}
	}
	if peak.Load() > 4 {
		t.Error("hydration exceeded its concurrency bound")
	}
	cancel()
	close(release)
	select {
	case err := <-finished:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("cancellation was not an unavailable observation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hydration ignored request cancellation")
	}
}

func TestForgejoRepositoryVariablesRejectBadCoordinatesBeforeHTTP(t *testing.T) {
	reader, fixture := newVariableFixture(t, 0, 50, nil)
	for _, name := range []string{"", "../secret", "A/B", "%2f", "1BAD", "BAD\nNAME"} {
		if _, err := reader.RepositoryVariable(t.Context(), "ci/project", name); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe name accepted: %v", err)
		}
	}
	for _, page := range []struct{ page, size int }{{0, 100}, {-1, 100}, {1, 0}, {1, 101}, {int(^uint(0) >> 1), 100}} {
		if _, err := reader.RepositoryVariables(t.Context(), "ci/project", page.page, page.size); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid pagination accepted: %v", err)
		}
	}
	if len(fixture.paths()) != 0 {
		t.Fatal("invalid coordinates reached the provider")
	}
}

func TestForgejoRepositoryVariableRedirectsNeverReceiveCredentials(t *testing.T) {
	var sinkCalls atomic.Int64
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sinkCalls.Add(1) }))
	defer sink.Close()
	reader, _ := newVariableFixture(t, 1, 50, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.Contains(r.URL.Path, "/variables/") {
			return false
		}
		w.Header().Set("Location", sink.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return true
	})
	if _, err := reader.RepositoryVariable(t.Context(), "ci/project", "VAR_000"); !errors.Is(err, ErrUnavailable) || sinkCalls.Load() != 0 {
		t.Fatal("variable redirect escaped the provider boundary")
	}
}
