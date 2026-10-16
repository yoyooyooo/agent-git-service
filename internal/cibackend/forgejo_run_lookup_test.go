package cibackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// Older Forgejo versions list ActionRuns but do not expose a single-run GET.
// Lookup must retain the provider database ID, not the repository run number.
func TestForgejoRunLookupWithoutSingleRunEndpoint(t *testing.T) {
	for _, target := range []string{"17", "9", "999"} {
		t.Run(target, func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/repos/ci/project/actions/runs" {
					pages++
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{forgejoFixtureRun()}})
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true}, "fixture-only")
			if err != nil {
				t.Fatal(err)
			}
			run, err := backend.Run(context.Background(), "ci/project", target)
			if target == "17" {
				if err != nil || run.Key != "17" || run.Number != 9 {
					t.Fatalf("run=%+v err=%v", run, err)
				}
				if err = backend.Action(context.Background(), "ci/project", run.Key, "cancel"); !errors.Is(err, ErrUnsupported) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign or run-number identity accepted: %+v %v", run, err)
			}
			if pages != 1 {
				t.Fatalf("list fallback pages=%d", pages)
			}
		})
	}
}

func TestForgejoRunLookupBoundsAndValidatesInventory(t *testing.T) {
	for _, mode := range []string{"next-page", "budget", "overlap", "changed-total", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/repos/ci/project/actions/runs" {
					http.NotFound(w, r)
					return
				}
				pages++
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				total := 51
				items := []any{}
				if page == 1 {
					for i := 100; i < 150; i++ {
						item := forgejoFixtureRun()
						item["id"] = i
						items = append(items, item)
					}
				} else {
					item := forgejoFixtureRun()
					switch mode {
					case "overlap":
						item["id"] = 100
					case "changed-total":
						total = 52
					case "incomplete":
						item["id"] = 200
						total = 52
					}
					items = append(items, item)
				}
				if mode == "incomplete" {
					total = 52
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "workflow_runs": items})
			}))
			defer server.Close()
			limit := 2
			if mode == "budget" {
				limit = 1
			}
			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true, TaskPageLimit: limit}, "fixture-only")
			if err != nil {
				t.Fatal(err)
			}
			run, err := backend.Run(context.Background(), "ci/project", "17")
			if mode == "next-page" {
				if err != nil || run.Key != "17" || pages != 2 {
					t.Fatalf("run=%+v err=%v pages=%d", run, err, pages)
				}
			} else if !errors.Is(err, ErrUnavailable) || pages > limit {
				t.Fatalf("incomplete observation became not-found/success: err=%v pages=%d", err, pages)
			}
		})
	}
}

func TestForgejoRunLookupDoesNotFallbackOnProviderFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) }))
	defer server.Close()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true}, "fixture-only")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backend.Run(context.Background(), "ci/project", "17"); !errors.Is(err, ErrUnavailable) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
