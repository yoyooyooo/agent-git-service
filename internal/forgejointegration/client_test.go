package forgejointegration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPClientMergePullRequestBindsExpectedHeadAndMethod(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/example-team/app-fixture/pulls/2/merge" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token token" {
			t.Fatalf("authorization=%q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode merge body: %v", err)
		}
		if body["Do"] != "fast-forward-only" || body["head_commit_id"] != strings.Repeat("a", 40) || body["delete_branch_after_merge"] != true {
			t.Fatalf("merge body=%#v", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	if err := client.MergePullRequest(t.Context(), "example-team", "app-fixture", 2, PullRequestMergeRequest{Method: "fast-forward-only", ExpectedHead: strings.Repeat("a", 40), DeleteBranch: true}); err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
}

func TestNewHTTPClientAllowsLargeRepositoryPages(t *testing.T) {
	client := NewHTTPClient("http://forgejo.local", "token")
	if client.client.Timeout < 60*time.Second {
		t.Fatalf("HTTP timeout=%s, want at least 60s for variably slow paginated large-repository reads", client.client.Timeout)
	}
}

func TestHTTPClientEnsureRepositoryCreatesMissingRepo(t *testing.T) {
	var calls []string
	var repoLookups int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/example-owner/demo" {
			repoLookups++
			if repoLookups == 1 {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"full_name":"example-owner/demo","name":"demo"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/orgs/example-owner" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/user" {
			_, _ = w.Write([]byte(`{"login":"example-owner"}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/user/repos" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["name"] != "demo" || body["private"] != true || body["auto_init"] != false {
				t.Fatalf("unexpected body: %#v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"full_name":"example-owner/demo","name":"demo"}`))
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	if err := client.EnsureRepository(t.Context(), "example-owner", "demo", true); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	wantCalls := []string{
		"GET /api/v1/repos/example-owner/demo",
		"GET /api/v1/orgs/example-owner",
		"GET /api/v1/user",
		"POST /api/v1/user/repos",
		"GET /api/v1/repos/example-owner/demo",
	}
	if !equalStrings(calls, wantCalls) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestHTTPClientEnsureRepositoryCreatesMissingOrgRepo(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/example-team/imd" {
			if len(calls) == 1 {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"full_name":"example-team/imd","name":"imd"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/orgs/example-team" {
			_, _ = w.Write([]byte(`{"username":"example-team"}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/orgs/example-team/repos" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["name"] != "imd" || body["private"] != true || body["auto_init"] != false {
				t.Fatalf("unexpected body: %#v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"full_name":"example-team/imd","name":"imd"}`))
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	if err := client.EnsureRepository(t.Context(), "example-team", "imd", true); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	wantCalls := []string{
		"GET /api/v1/repos/example-team/imd",
		"GET /api/v1/orgs/example-team",
		"POST /api/v1/orgs/example-team/repos",
		"GET /api/v1/repos/example-team/imd",
	}
	if !equalStrings(calls, wantCalls) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestHTTPClientEnsureRepositoryRejectsTokenUserFallbackForOrgRepo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/example-team/imd" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/orgs/example-team" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/user" {
			_, _ = w.Write([]byte(`{"login":"ags-forgejo-bot"}`))
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	if err := client.EnsureRepository(t.Context(), "example-team", "imd", true); err == nil {
		t.Fatal("expected owner mismatch error")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHTTPClientListPullRequestsPaginatesPastProviderDefaultLimit(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/example-owner/demo/pulls" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		pages = append(pages, r.URL.Query().Get("page"))
		page := r.URL.Query().Get("page")
		rows := make([]map[string]any, 0, 50)
		if page == "1" {
			for i := 1; i <= 50; i++ {
				rows = append(rows, map[string]any{
					"number": i, "state": "closed", "merged": true,
					"body": "[AGS-INTEGRITY-SUPERSEDED]", "labels": []map[string]any{{"name": "ags/integrity-superseded"}},
					"merge_commit_sha": "merge", "head": map[string]any{"ref": "agent/test", "sha": "head"},
					"base": map[string]any{"ref": "main"},
				})
			}
		} else if page == "2" {
			rows = append(rows, map[string]any{
				"number": 51, "state": "open", "merged": false,
				"head": map[string]any{"ref": "agent/last", "sha": "last"},
				"base": map[string]any{"ref": "main"},
			})
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	rows, err := client.ListPullRequests(t.Context(), "example-owner", "demo", "all")
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(rows) != 51 || rows[50].Number != 51 || rows[50].HeadRef != "agent/last" {
		t.Fatalf("unexpected rows: count=%d last=%#v", len(rows), rows[len(rows)-1])
	}
	if rows[0].Body != "[AGS-INTEGRITY-SUPERSEDED]" || !equalStrings(rows[0].Labels, []string{"ags/integrity-superseded"}) {
		t.Fatalf("integrity disposition fields missing: %#v", rows[0])
	}
	if !equalStrings(pages, []string{"1", "2"}) {
		t.Fatalf("pages=%v", pages)
	}
}

func TestHTTPClientListWorkflowRunsPageReportsCompleteness(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/example-owner/demo/actions/tasks" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count":   101,
			"workflow_runs": []map[string]any{{"id": 101, "head_branch": "#16", "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	runs, hasMore, err := client.ListWorkflowRunsPage(t.Context(), "example-owner", "demo", 2, 50)
	if err != nil || len(runs) != 1 || runs[0].ID != 101 || !hasMore {
		t.Fatalf("runs=%#v hasMore=%t err=%v", runs, hasMore, err)
	}
	if !equalStrings(queries, []string{"page=2&limit=50"}) {
		t.Fatalf("queries=%v", queries)
	}
}

func TestHTTPClientGetPullRequestReadsExactNumber(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/repos/example-owner/demo/pulls/42" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 42, "state": "closed", "merged": true,
			"merge_commit_sha": "merge42", "head": map[string]any{"ref": "agent/exact", "sha": "head42"},
			"base": map[string]any{"ref": "main", "sha": "base42"},
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	row, found, err := client.GetPullRequest(t.Context(), "example-owner", "demo", 42)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if !found || row.Number != 42 || row.State != "closed" || !row.Merged || row.HeadSHA != "head42" || row.BaseSHA != "base42" {
		t.Fatalf("unexpected exact PR: found=%t row=%#v", found, row)
	}
}

func TestHTTPClientGetPullRequestReturnsNotFound(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	row, found, err := client.GetPullRequest(t.Context(), "example-owner", "demo", 404)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if found || row.Number != 0 {
		t.Fatalf("expected exact PR to be absent: found=%t row=%#v", found, row)
	}
}

func TestHTTPClientEnsurePullRequestSkipsExistingPR(t *testing.T) {
	var postCalled bool
	var patchCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":   7,
				"html_url": "http://forgejo.local/example-owner/demo/pulls/7",
				"title":    "title",
				"body":     "body",
				"head":     map[string]any{"ref": "agent/demo", "sha": "abc123"},
				"base":     map[string]any{"ref": "main"},
			}})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			postCalled = true
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls/7" {
			patchCalled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	pr, err := client.EnsurePullRequest(t.Context(), PullRequestRequest{Owner: "example-owner", Repo: "demo", Head: "agent/demo", Base: "main", Title: "title", Body: "body"})
	if err != nil {
		t.Fatalf("EnsurePullRequest: %v", err)
	}
	if pr.Number != 7 || pr.URL != "http://forgejo.local/example-owner/demo/pulls/7" || pr.HeadSHA != "abc123" {
		t.Fatalf("pr=%#v", pr)
	}
	if postCalled {
		t.Fatal("expected existing PR to skip POST")
	}
	if patchCalled {
		t.Fatal("expected existing PR with matching metadata to skip PATCH")
	}
}

func TestHTTPClientEnsurePullRequestReadsLargeOpenPRList(t *testing.T) {
	var postCalled bool
	var patchBody map[string]any
	largeBody := strings.Repeat("x", 90*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"number":   6,
					"html_url": "http://forgejo.local/example-owner/demo/pulls/6",
					"title":    "large unrelated PR",
					"body":     largeBody,
					"head":     map[string]any{"ref": "agent/other", "sha": "def456"},
					"base":     map[string]any{"ref": "main"},
				},
				{
					"number":   7,
					"html_url": "http://forgejo.local/example-owner/demo/pulls/7",
					"title":    "stale title",
					"body":     "stale body",
					"head":     map[string]any{"ref": "agent/demo", "sha": "abc123"},
					"base":     map[string]any{"ref": "main"},
				},
			})
			return
		}
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls/7" {
			if err := json.NewDecoder(r.Body).Decode(&patchBody); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   7,
				"html_url": "http://forgejo.local/example-owner/demo/pulls/7",
				"head":     map[string]any{"sha": "abc123"},
			})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			postCalled = true
			w.WriteHeader(http.StatusConflict)
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	pr, err := client.EnsurePullRequest(t.Context(), PullRequestRequest{Owner: "example-owner", Repo: "demo", Head: "agent/demo", Base: "main", Title: "fresh title", Body: "fresh body"})
	if err != nil {
		t.Fatalf("EnsurePullRequest: %v", err)
	}
	if pr.Number != 7 || pr.HeadSHA != "abc123" {
		t.Fatalf("pr=%#v", pr)
	}
	if postCalled {
		t.Fatal("expected large existing PR list to be decoded before POST")
	}
	if patchBody["title"] != "fresh title" || patchBody["body"] != "fresh body" {
		t.Fatalf("patchBody=%#v", patchBody)
	}
}

func TestHTTPClientEnsurePullRequestRefetchesAfterDuplicateConflict(t *testing.T) {
	getCalls := 0
	postCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			getCalls++
			if getCalls == 1 {
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":   7,
				"html_url": "http://forgejo.local/example-owner/demo/pulls/7",
				"title":    "title",
				"body":     "body",
				"head":     map[string]any{"ref": "agent/demo", "sha": "abc123"},
				"base":     map[string]any{"ref": "main"},
			}})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			postCalls++
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": "pull request already exists for these targets",
				"url":     "http://forgejo.local/api/swagger",
			})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	pr, err := client.EnsurePullRequest(t.Context(), PullRequestRequest{Owner: "example-owner", Repo: "demo", Head: "agent/demo", Base: "main", Title: "title", Body: "body"})
	if err != nil {
		t.Fatalf("EnsurePullRequest: %v", err)
	}
	if pr.Number != 7 || pr.URL != "http://forgejo.local/example-owner/demo/pulls/7" || pr.HeadSHA != "abc123" {
		t.Fatalf("pr=%#v", pr)
	}
	if postCalls != 1 || getCalls < 2 {
		t.Fatalf("postCalls=%d getCalls=%d", postCalls, getCalls)
	}
}

func TestHTTPClientEnsurePullRequestCanSkipExistingPRMetadataReconcile(t *testing.T) {
	var patchCalled bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":   7,
				"html_url": "http://forgejo.local/example-owner/demo/pulls/7",
				"title":    "AGS title",
				"body":     "AGS body",
				"head":     map[string]any{"ref": "agent/demo", "sha": "abc123"},
				"base":     map[string]any{"ref": "main"},
			}})
			return
		}
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls/7" {
			patchCalled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			t.Fatal("expected existing PR to skip POST")
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	pr, err := client.EnsurePullRequest(t.Context(), PullRequestRequest{Owner: "example-owner", Repo: "demo", Head: "agent/demo", Base: "main", Title: "default title", Body: "default body", SkipMetadataReconcile: true})
	if err != nil {
		t.Fatalf("EnsurePullRequest: %v", err)
	}
	if pr.Number != 7 || pr.HeadSHA != "abc123" {
		t.Fatalf("pr=%#v", pr)
	}
	if patchCalled {
		t.Fatal("expected existing PR metadata to be preserved")
	}
}

func TestHTTPClientEnsurePullRequestClearsExistingPRBody(t *testing.T) {
	var patchBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":   7,
				"html_url": "http://forgejo.local/example-owner/demo/pulls/7",
				"title":    "title",
				"body":     "stale projected body",
				"head":     map[string]any{"ref": "agent/demo", "sha": "abc123"},
				"base":     map[string]any{"ref": "main"},
			}})
			return
		}
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls/7" {
			if err := json.NewDecoder(r.Body).Decode(&patchBody); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   7,
				"html_url": "http://forgejo.local/example-owner/demo/pulls/7",
				"head":     map[string]any{"sha": "abc123"},
			})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			t.Fatal("expected existing PR to skip POST")
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	pr, err := client.EnsurePullRequest(t.Context(), PullRequestRequest{Owner: "example-owner", Repo: "demo", Head: "agent/demo", Base: "main", Title: "title", Body: ""})
	if err != nil {
		t.Fatalf("EnsurePullRequest: %v", err)
	}
	if pr.Number != 7 || pr.HeadSHA != "abc123" {
		t.Fatalf("pr=%#v", pr)
	}
	if body, ok := patchBody["body"]; !ok || body != "" {
		t.Fatalf("patch body=%#v, want explicit empty body", patchBody)
	}
	if _, ok := patchBody["title"]; ok {
		t.Fatalf("unchanged title should not be patched: %#v", patchBody)
	}
}

func TestHTTPClientEnsurePullRequestPatchesExistingPRMetadata(t *testing.T) {
	var patchBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":   7,
				"html_url": "http://forgejo.local/example-owner/demo/pulls/7",
				"title":    "agent/demo -> main",
				"body":     "Created by AGS Forgejo integration for `example-owner/demo`.",
				"head":     map[string]any{"ref": "agent/demo", "sha": "abc123"},
				"base":     map[string]any{"ref": "main"},
			}})
			return
		}
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls/7" {
			if err := json.NewDecoder(r.Body).Decode(&patchBody); err != nil {
				t.Fatalf("decode patch body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   7,
				"html_url": "http://forgejo.local/example-owner/demo/pulls/7",
			})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/example-owner/demo/pulls" {
			t.Fatal("expected existing PR to skip POST")
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	pr, err := client.EnsurePullRequest(t.Context(), PullRequestRequest{
		Owner: "example-owner",
		Repo:  "demo",
		Head:  "agent/demo",
		Base:  "main",
		Title: "HUM-60: add smoke marker",
		Body:  "Created from AGS PR #21.\n\nMultica: HUM-60\n\nMultica issue: https://multica.ai/workspace-alpha/issues/HUM-60",
	})
	if err != nil {
		t.Fatalf("EnsurePullRequest: %v", err)
	}
	if pr.Number != 7 || pr.URL != "http://forgejo.local/example-owner/demo/pulls/7" || pr.HeadSHA != "abc123" {
		t.Fatalf("pr=%#v", pr)
	}
	if patchBody["title"] != "HUM-60: add smoke marker" {
		t.Fatalf("patch title=%#v", patchBody["title"])
	}
	if patchBody["body"] != "Created from AGS PR #21.\n\nMultica: HUM-60\n\nMultica issue: https://multica.ai/workspace-alpha/issues/HUM-60" {
		t.Fatalf("patch body=%#v", patchBody["body"])
	}
}
