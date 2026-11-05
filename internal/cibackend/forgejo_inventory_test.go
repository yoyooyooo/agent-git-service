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

func TestForgejoJobsStopWhenALowerRunNumberIsPassed(t *testing.T) {
	const total = 7422
	const target = int64(2990)
	pages := 0
	run := forgejoFixtureRun()
	run["index_in_repo"] = target
	provider := httptest.NewServer(withCurrentJobFixture(fixtureCurrentJobs(7422-8, 4, "job-", 4), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/runs/17"):
			_ = json.NewEncoder(w).Encode(run)
		case strings.HasSuffix(r.URL.Path, "/runs"):
			if r.URL.Query().Get("head_sha") != strings.Repeat("a", 40) || r.URL.Query().Get("run_number") != strconv.FormatInt(target, 10) {
				t.Errorf("run index query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			if r.URL.Query().Get("head_sha") != "" || r.URL.Query().Get("run_number") != "" {
				t.Errorf("task list must not pretend unknown filters work: %s", r.URL.RawQuery)
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			pages++
			items := make([]any, 0, 50)
			for slot := 0; slot < 50; slot++ {
				index := (page-1)*50 + slot
				id := total - index
				runNumber := int64(2992 - index/4)
				task := map[string]any{"id": id, "run_number": runNumber}
				if runNumber == target {
					task = map[string]any{"id": id, "run_number": target, "workflow_id": "ci.yml", "name": "job-" + strconv.Itoa(index-4), "head_sha": strings.Repeat("a", 40), "status": "success", "url": "https://ci.example.test/ci/project/actions/runs/2990", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}
				}
				items = append(items, task)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "workflow_runs": items})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})))
	defer provider.Close()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}}, "fixture-only", "bridge-only")
	if err != nil {
		t.Fatal(err)
	}
	found, err := backend.Jobs(context.Background(), "ci/project", "17")
	if err != nil || !found.Complete || len(found.Items) != 4 || pages != 2 {
		t.Fatalf("indexed run did not stop at the lower run number: pages=%d jobs=%+v error=%v", pages, found, err)
	}
}

func TestForgejoJobsKeepScanningUntilTheRunNumberIsPassed(t *testing.T) {
	const total = 1359
	pages := 0
	provider := httptest.NewServer(withCurrentJobFixture(map[int64]string{1: "late-job"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runs/17") {
			_ = json.NewEncoder(w).Encode(forgejoFixtureRun())
			return
		}
		if strings.HasSuffix(r.URL.Path, "/runs") {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{forgejoFixtureRun()}})
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
		for offset := 0; offset < 50 && (page-1)*50+offset < total; offset++ {
			id := total - ((page-1)*50 + offset)
			runNumber := int64(40 - page)
			value := map[string]any{"id": id, "run_number": runNumber}
			if id == 1 {
				value = map[string]any{"id": id, "run_number": 9, "workflow_id": "ci.yml", "name": "late-job", "head_sha": strings.Repeat("a", 40), "status": "success", "url": "https://ci.example.test/ci/project/actions/runs/9", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}
			}
			items = append(items, value)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "workflow_runs": items})
	})))
	defer provider.Close()
	config := BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}}
	backend, err := NewHTTP(config, "fixture-only", "bridge-only")
	if err != nil {
		t.Fatal(err)
	}
	found, err := backend.Jobs(context.Background(), "ci/project", "17")
	if err != nil || !found.Complete || len(found.Items) != 1 || found.Items[0].Name != "late-job" || pages != 28 {
		t.Fatalf("old run was dropped before the list ended: pages=%d jobs=%+v error=%v", pages, found, err)
	}
}

func TestForgejoJobsDoNotStopOnAPartialPageOfTheSameRun(t *testing.T) {
	pages := 0
	provider := httptest.NewServer(withCurrentJobFixture(fixtureCurrentJobs(500-40, 50, "job-", 40), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runs/17") {
			_ = json.NewEncoder(w).Encode(forgejoFixtureRun())
			return
		}
		if strings.HasSuffix(r.URL.Path, "/runs") {
			if r.URL.Query().Get("run_number") != "9" {
				t.Errorf("run index query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{forgejoFixtureRun()}})
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pages++
		items := make([]any, 50)
		for slot := 0; slot < 50; slot++ {
			index := (page-1)*50 + slot
			id := 500 - index
			runNumber := int64(20)
			if index >= 40 {
				runNumber = 9
			}
			if index >= 90 {
				runNumber = 8
			}
			task := map[string]any{"id": id, "run_number": runNumber}
			if runNumber == 9 {
				task = map[string]any{"id": id, "run_number": 9, "workflow_id": "ci.yml", "name": "job-" + strconv.Itoa(index), "head_sha": strings.Repeat("a", 40), "status": "success", "url": "https://ci.example.test/ci/project/actions/runs/9", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}
			}
			items[slot] = task
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 900, "workflow_runs": items})
	})))
	defer provider.Close()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}}, "fixture-only", "bridge-only")
	if err != nil {
		t.Fatal(err)
	}
	found, err := backend.Jobs(context.Background(), "ci/project", "17")
	if err != nil || !found.Complete || len(found.Items) != 50 || pages != 3 {
		t.Fatalf("same-run span was not closed on the first lower page: pages=%d jobs=%d error=%v", pages, len(found.Items), err)
	}
}

func TestForgejoJobInventoryRejectsBudgetAndUnstablePages(t *testing.T) {
	const total = 1359
	mode := "complete"
	provider := httptest.NewServer(withCurrentJobFixture(map[int64]string{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runs/17") {
			_ = json.NewEncoder(w).Encode(forgejoFixtureRun())
			return
		}
		if strings.HasSuffix(r.URL.Path, "/runs") {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{forgejoFixtureRun()}})
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		items := []any{}
		for offset := 0; offset < 50 && (page-1)*50+offset < total; offset++ {
			id := total - ((page-1)*50 + offset)
			value := map[string]any{"id": id, "run_number": 40}
			if mode == "duplicate" && page == 2 && offset == 0 {
				value["id"] = total
			}
			items = append(items, value)
		}
		count := total
		if mode == "moving" && page > 1 {
			count++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": count, "workflow_runs": items})
	})))
	defer provider.Close()
	config := BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}, TaskPageLimit: 20}
	limited, err := NewHTTP(config, "fixture-only", "bridge-only")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = limited.Jobs(context.Background(), "ci/project", "17"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("exhausted explicit budget became success", err)
	}
	config.TaskPageLimit = 0
	backend, err := NewHTTP(config, "fixture-only", "bridge-only")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"moving", "duplicate"} {
		mode = value
		if _, err = backend.Jobs(context.Background(), "ci/project", "17"); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unstable paged inventory accepted", value, err)
		}
	}
}

func TestForgejoJobsRejectAnUnindexedRunBeforeReadingTasks(t *testing.T) {
	tasks := 0
	provider := httptest.NewServer(withCurrentJobFixture(map[int64]string{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runs/17") {
			_ = json.NewEncoder(w).Encode(forgejoFixtureRun())
			return
		}
		if strings.HasSuffix(r.URL.Path, "/runs") {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "workflow_runs": []any{}})
			return
		}
		tasks++
		http.NotFound(w, r)
	})))
	defer provider.Close()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}}, "fixture-only", "bridge-only")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backend.Jobs(context.Background(), "ci/project", "17"); !errors.Is(err, ErrInvalid) || tasks != 0 {
		t.Fatal("unindexed run started a task scan", tasks, err)
	}
}
