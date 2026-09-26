package cibackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestForgejoLargeTaskInventoryIsCompleteOrExplicitlyUnavailable(t *testing.T) {
	const total = 1359
	mode := "complete"
	pages := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runs/17") {
			_ = json.NewEncoder(w).Encode(forgejoFixtureRun())
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/tasks") {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pages++
		items := []any{}
		for id := (page-1)*50 + 1; id <= page*50 && id <= total; id++ {
			value := map[string]any{"id": id, "run_number": 999}
			if id == total {
				value = map[string]any{"id": id, "run_number": 9, "workflow_id": "ci.yml", "name": "late-job", "head_sha": strings.Repeat("a", 40), "status": "success", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}
			}
			if mode == "duplicate" && page == 2 && id == 51 {
				value["id"] = 1
			}
			items = append(items, value)
		}
		count := total
		if mode == "moving" && page > 1 {
			count++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": count, "workflow_runs": items})
	}))
	defer provider.Close()
	config := BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}
	backend, err := NewHTTP(config, "fixture-only")
	if err != nil {
		t.Fatal(err)
	}
	found, err := backend.Jobs(context.Background(), "ci/project", "17")
	if err != nil || !found.Complete || len(found.Items) != 1 || found.Items[0].Name != "late-job" || pages != 28 {
		t.Fatalf("large current inventory not completed: pages=%d jobs=%+v error=%v", pages, found, err)
	}
	config.TaskPageLimit = 20
	limited, err := NewHTTP(config, "fixture-only")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = limited.Jobs(context.Background(), "ci/project", "17"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("exhausted explicit budget became success", err)
	}
	for _, value := range []string{"moving", "duplicate"} {
		mode = value
		if _, err = backend.Jobs(context.Background(), "ci/project", "17"); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unstable paged inventory accepted", value, err)
		}
	}
}
