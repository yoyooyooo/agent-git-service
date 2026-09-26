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

func fixtureRun() map[string]any {
	return map[string]any{"id": 17, "name": "test", "workflow_id": "ci.yml", "head_branch": "feature", "head_sha": strings.Repeat("a", 40), "event": "push", "status": "success", "run_number": 9, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z", "html_url": "https://ci.example.test/team/project/actions/runs/17"}
}
func TestExplicitSelectionAndCredentialIndependentResourceIdentity(t *testing.T) {
	r, e := New(Config{}, ".")
	if e != nil {
		t.Fatal(e)
	}
	s, e := r.Select("team/project")
	if e != nil || s.Name != "native" || s.Backend != nil {
		t.Fatalf("unexpected default %#v %v", s, e)
	}
	c := Config{Default: "none", Backends: map[string]BackendConfig{"builder": {Kind: "forgejo", URL: "https://ci.example.test", TokenFile: "not-read"}}, Repositories: map[string]Binding{"team/project": {Backend: "builder", Repository: "ci/project"}}}
	a, e := WithBackends(c, map[string]Backend{"builder": &HTTP{}})
	if e != nil {
		t.Fatal(e)
	}
	s, e = a.Select("team/project")
	if e != nil || s.Name != "builder" {
		t.Fatal(e, s)
	}
	empty, _ := a.Select("other/project")
	if empty.Name != "none" {
		t.Fatal("unexpected implicit binding")
	}
	old := s.Namespace
	b := c.Backends["builder"]
	b.TokenFile = "rotated-token"
	c.Backends["builder"] = b
	a, e = WithBackends(c, map[string]Backend{"builder": &HTTP{}})
	if e != nil {
		t.Fatal(e)
	}
	s, _ = a.Select("team/project")
	if s.Namespace != old {
		t.Fatal("token rotation changed identity")
	}
	b.URL = "https://other.example.test"
	c.Backends["builder"] = b
	a, e = WithBackends(c, map[string]Backend{"builder": &HTTP{}})
	if e != nil {
		t.Fatal(e)
	}
	s, _ = a.Select("team/project")
	if s.Namespace == old {
		t.Fatal("backend relocation reused identity")
	}
}
func TestForgejoAndGitHubActionsShareContractNotCredentialsOrRoutes(t *testing.T) {
	for _, kind := range []string{"forgejo", "github-actions"} {
		t.Run(kind, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				prefix := ""
				auth := "Bearer fixture-only"
				if kind == "forgejo" {
					prefix = "/api/v1"
					auth = "token fixture-only"
				}
				if r.URL.Path != prefix+"/repos/ci/project/actions/runs" || r.Header.Get("Authorization") != auth || r.URL.Query().Get("head_sha") != strings.Repeat("a", 40) {
					t.Errorf("wrong request %s", r.URL.Path)
				}
				run := fixtureRun()
				if kind == "github-actions" {
					run["status"] = "completed"
					run["conclusion"] = "success"
				}
				if kind == "forgejo" {
					run = forgejoFixtureRun()
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
			}))
			defer server.Close()
			b, e := NewHTTP(BackendConfig{Kind: kind, URL: server.URL, TokenFile: "fixture", AllowHTTP: true}, "fixture-only")
			if e != nil {
				t.Fatal(e)
			}
			result, e := b.Runs(context.Background(), "ci/project", Query{HeadSHA: strings.Repeat("a", 40), PerPage: 20})
			if e != nil {
				t.Fatal(e)
			}
			if requests != 1 || len(result.Items) != 1 || result.Items[0].Status != "completed" || result.Items[0].Conclusion != "success" || !result.Complete {
				t.Fatalf("bad normalized result %#v", result)
			}
		})
	}
}
func TestRedirectNeverLeaksTokenOrReplaysAction(t *testing.T) {
	var calls atomic.Int64
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); t.Error("credential reached redirect") }))
	defer sink.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", sink.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer provider.Close()
	b, e := NewHTTP(BackendConfig{Kind: "github-actions", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}, "not-for-sink")
	if e != nil {
		t.Fatal(e)
	}
	if e = b.Action(context.Background(), "ci/project", "17", "rerun"); e == nil {
		t.Fatal("unexpected write success")
	}
	if _, e = b.RunLogs(context.Background(), "ci/project", "17"); !errors.Is(e, ErrUnsupported) {
		t.Fatal(e)
	}
	if calls.Load() != 0 {
		t.Fatal("followed redirect")
	}
}
func TestUnknownStatusAndCrossRunJobsAreNotSuccess(t *testing.T) {
	mode := "status"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "status" {
			run := fixtureRun()
			run["status"] = "new-provider-state"
			_ = json.NewEncoder(w).Encode(run)
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []any{map[string]any{"id": 4, "run_id": 999, "name": "test", "status": "success"}}})
		}
	}))
	defer server.Close()
	b, e := NewHTTP(BackendConfig{Kind: "github-actions", URL: server.URL, TokenFile: "fixture", AllowHTTP: true}, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.Run(context.Background(), "ci/project", "17"); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
	mode = "jobs"
	if _, e = b.Jobs(context.Background(), "ci/project", "17"); !errors.Is(e, ErrInvalid) {
		t.Fatal(e)
	}
}
func TestConfigurationFailsBeforeUnsafeConnections(t *testing.T) {
	for _, b := range []BackendConfig{
		{Kind: "forgejo", URL: "http://ci.example.test", TokenFile: "x"},
		{Kind: "forgejo", URL: "https://secret@ci.example.test", TokenFile: "x"},
		{Kind: "forgejo", URL: "https://ci.example.test/?token=secret", TokenFile: "x"},
		{Kind: "invented", URL: "https://ci.example.test", TokenFile: "x"},
		{Kind: "forgejo", URL: "https://ci.example.test", TokenFile: "x", LogDownloadHosts: []string{"*.example.test"}},
	} {
		if _, e := NewHTTP(b, "fixture"); e == nil {
			t.Fatal("unsafe backend accepted")
		}
	}
	if _, _, e := normalized("completed", ""); !errors.Is(e, ErrInvalid) {
		t.Fatal("missing conclusion became success")
	}
}
