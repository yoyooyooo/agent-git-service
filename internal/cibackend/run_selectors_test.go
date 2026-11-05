package cibackend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestForgejoRunSelectorsResolveOnlyTheBoundRepositoryIndex(t *testing.T) {
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "GET", r.Method)
		require.Equal(t, "/api/v1/repos/ci/project/actions/runs", r.URL.Path)
		require.Equal(t, "9", r.URL.Query().Get("run_number"))
		require.Equal(t, "2", r.URL.Query().Get("limit"))
		require.Equal(t, "1", r.URL.Query().Get("page"))
		require.Equal(t, "token provider-only", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{forgejoFixtureRun()}})
	}))
	defer provider.Close()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, WebURL: "https://web.example.test/forge", TokenFile: "fixture", AllowHTTP: true}, "provider-only", "")
	require.NoError(t, err)
	selectorBackend := backend.(RunSelectorBackend)
	for _, selector := range []string{"forgejo:9", provider.URL + "/ci/project/actions/runs/9", "https://web.example.test/forge/ci/project/actions/runs/9"} {
		run, err := selectorBackend.RunBySelector(t.Context(), "ci/project", selector)
		require.NoError(t, err)
		require.Equal(t, "17", run.Key, "provider global ID is not the repository run number")
		require.Equal(t, int64(9), run.Number)
	}
	require.Equal(t, 3, calls)
	for _, selector := range []string{
		"https://untrusted.example.test/forge/ci/project/actions/runs/9",
		"https://web.example.test/forge/other/project/actions/runs/9",
		"https://web.example.test:444/forge/ci/project/actions/runs/9",
		"http://web.example.test/forge/ci/project/actions/runs/9",
	} {
		_, err := selectorBackend.RunBySelector(t.Context(), "ci/project", selector)
		require.ErrorIs(t, err, ErrNotFound, selector)
	}
	for _, selector := range []string{
		"9", "forgejo:0", "forgejo:-9", "forgejo:+9", "forgejo:09", "forgejo:9223372036854775808", "forgejo:9\n", "github:9",
		"https://web.example.test/forge/ci/project/actions/runs/9/",
		"https://user:secret@web.example.test/forge/ci/project/actions/runs/9",
		"https://web.example.test/forge/ci/project/actions/runs/9?download=true",
		"https://web.example.test/forge/ci/project/actions/runs/9?",
		"https://web.example.test/forge/ci/project/actions/runs/9#job-0",
		"https://web.example.test/forge/ci/project/actions/runs/%39",
		strings.Repeat("a", 4097),
	} {
		_, err := selectorBackend.RunBySelector(t.Context(), "ci/project", selector)
		require.ErrorIs(t, err, ErrInvalidRunSelector, selector)
	}
	require.Equal(t, 3, calls, "unbound or malformed locators must not trigger provider requests")
}

func TestForgejoRunSelectorRequiresOneExactIndexResult(t *testing.T) {
	for _, mode := range []string{"missing", "wrong-number", "ambiguous", "denied", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				run := forgejoFixtureRun()
				switch mode {
				case "missing":
					fmt.Fprint(w, `{"total_count":0,"workflow_runs":[]}`)
				case "wrong-number":
					run["index_in_repo"] = 10
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
				case "ambiguous":
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "workflow_runs": []any{run, run}})
				case "denied":
					w.WriteHeader(403)
				case "unsupported":
					w.WriteHeader(501)
				}
			}))
			defer provider.Close()
			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}, "provider-only", "")
			require.NoError(t, err)
			_, err = backend.(RunSelectorBackend).RunBySelector(t.Context(), "ci/project", "forgejo:9")
			require.ErrorIs(t, err, map[string]error{"missing": ErrNotFound, "wrong-number": ErrInvalid, "ambiguous": ErrInvalid, "denied": ErrForbidden, "unsupported": ErrUnsupported}[mode])
			require.Equal(t, 1, calls, "a rejected exact lookup must not guess another ID or scan")
		})
	}
}

func TestForgejoWebURLIsExplicitAndDoesNotChangeResourceNamespace(t *testing.T) {
	config := Config{Backends: map[string]BackendConfig{"builder": {Kind: "forgejo", URL: "https://api.example.test", TokenFile: "fixture"}}, Repositories: map[string]Binding{"team/project": {Backend: "builder", Repository: "ci/project"}}}
	backend, err := NewHTTP(config.Backends["builder"], "provider-only", "")
	require.NoError(t, err)
	backends := map[string]Backend{"builder": backend}
	before, err := WithBackends(config, backends)
	require.NoError(t, err)
	original, err := before.Select("team/project")
	require.NoError(t, err)
	for _, value := range []string{"https://web.example.test", "https://web.example.test/forge/"} {
		b := config.Backends["builder"]
		b.WebURL = value
		config.Backends["builder"] = b
		after, err := WithBackends(config, backends)
		require.NoError(t, err)
		selection, err := after.Select("team/project")
		require.NoError(t, err)
		require.Equal(t, original.Namespace, selection.Namespace)
	}
	for _, value := range []string{"http://web.example.test", "https://user:secret@web.example.test", "https://web.example.test?x=y", "https://web.example.test/#secret", "https://web.example.test/%2e%2e", "https://web.example.test/../other", "file:///private/data"} {
		b := config.Backends["builder"]
		b.WebURL = value
		config.Backends["builder"] = b
		require.Error(t, Validate(config), value)
	}
}
