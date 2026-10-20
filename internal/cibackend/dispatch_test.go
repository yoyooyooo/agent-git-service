package cibackend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkflowDispatchUsesOnePinnedProviderWrite(t *testing.T) {
	for _, kind := range []string{"forgejo", "github-actions"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, "POST", r.Method)
				prefix, auth := "/api/v1", "token fixture-only"
				if kind == "github-actions" {
					prefix, auth = "", "Bearer fixture-only"
				}
				require.Equal(t, prefix+"/repos/ci/project/actions/workflows/ci.yml/dispatches", r.URL.Path)
				require.Equal(t, auth, r.Header.Get("Authorization"))
				var payload map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				require.Equal(t, strings.Repeat("a", 40), payload["ref"])
				inputs := payload["inputs"].(map[string]any)
				require.Equal(t, "test", inputs["target"])
				if kind == "forgejo" {
					require.Equal(t, "true", inputs["debug"])
				} else {
					require.Equal(t, true, inputs["debug"])
				}
				w.WriteHeader(204)
			}))
			defer provider.Close()
			backend, err := NewHTTP(BackendConfig{Kind: kind, URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}, "fixture-only")
			require.NoError(t, err)
			dispatch := backend.(WorkflowDispatcher)
			require.NoError(t, dispatch.DispatchWorkflow(context.Background(), "ci/project", "ci.yml", strings.Repeat("a", 40), map[string]any{"target": "test", "debug": true}))
			require.Equal(t, 1, calls)
			for _, workflow := range []string{"../ci.yml", "x/ci.yml", "ci.yml?x"} {
				require.Error(t, dispatch.DispatchWorkflow(context.Background(), "ci/project", workflow, strings.Repeat("a", 40), nil))
			}
			require.Error(t, dispatch.DispatchWorkflow(context.Background(), "ci/project", "ci.yml", "feature", nil))
			require.Error(t, dispatch.DispatchWorkflow(context.Background(), "ci/project", "ci.yml", strings.Repeat("a", 40), map[string]any{"nested": map[string]any{}}))
			require.Equal(t, 1, calls, "invalid input must not perform provider I/O")
		})
	}
}

func TestWorkflowDispatchPropagatesFailureWithoutReplay(t *testing.T) {
	for _, status := range []int{403, 404, 409, 422, 503, 307} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			writes := 0
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writes++
				w.Header().Set("Location", "/replay")
				w.WriteHeader(status)
			}))
			defer provider.Close()
			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}, "fixture-only")
			require.NoError(t, err)
			err = backend.(WorkflowDispatcher).DispatchWorkflow(context.Background(), "ci/project", "ci.yml", strings.Repeat("b", 40), nil)
			want := map[int]error{403: ErrForbidden, 404: ErrNotFound, 409: ErrConflict, 422: ErrConflict, 503: ErrUnavailable, 307: ErrUnavailable}[status]
			require.ErrorIs(t, err, want)
			require.Equal(t, 1, writes)
		})
	}
}
