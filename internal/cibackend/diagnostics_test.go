package cibackend

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestForgejoLogFailuresRemainTypedAndRunArchivesDiagnosable(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
		label  string
	}{{404, ErrNotFound, "not-found"}, {409, ErrConflict, "conflict"}, {501, ErrUnsupported, "unsupported"}, {403, ErrForbidden, "forbidden"}, {401, ErrForbidden, "forbidden"}, {503, ErrUnavailable, "unavailable"}} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			server := httptest.NewServer(withCurrentJobFixture(map[int64]string{41: "test"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/tasks/41"):
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte("private provider error body and credential"))
				case strings.HasSuffix(r.URL.Path, "/runs/17"):
					json.NewEncoder(w).Encode(forgejoFixtureRun())
				case strings.HasSuffix(r.URL.Path, "/runs"):
					json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{forgejoFixtureRun()}})
				case strings.HasSuffix(r.URL.Path, "/tasks"):
					json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{"id": 41, "run_number": 9, "workflow_id": "ci.yml", "name": "test", "head_sha": strings.Repeat("a", 40), "status": "success", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}}})
				default:
					http.NotFound(w, r)
				}
			})))
			defer server.Close()
			backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: server.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "bridge-only")
			require.NoError(t, err)
			_, err = backend.JobLogs(t.Context(), "ci/project", "17:41")
			require.True(t, errors.Is(err, tc.want), "%v", err)
			data, err := backend.RunLogs(t.Context(), "ci/project", "17")
			require.NoError(t, err)
			require.NoError(t, ValidateLogArchive(data))
			archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			require.NoError(t, err)
			require.Len(t, archive.File, 1)
			file, err := archive.File[0].Open()
			require.NoError(t, err)
			text, err := io.ReadAll(file)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			require.Contains(t, string(text), "AGS_LOG_UNAVAILABLE")
			require.Contains(t, string(text), "status="+tc.label)
			require.NotContains(t, string(text), "private provider")
			require.NotContains(t, string(text), "credential")
		})
	}
}

func TestForgejoStepsWithoutBridgeAreExplicitlyUnsupported(t *testing.T) {
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: "https://ci.example.test", TokenFile: "fixture"}, "provider-only")
	require.NoError(t, err)
	_, err = backend.(StepBackend).JobSteps(t.Context(), "ci/project", Run{}, Job{})
	require.ErrorIs(t, err, ErrUnsupported)
}
