package rest_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
	"github.com/stretchr/testify/require"
)

func TestForgejoCollectionOnlyRunDetailsJobsLogsAndRerun(t *testing.T) {
	h := testharness.New(t)
	repo, head := seedCheckRunsRepo(t, h, "collection-only-ci")
	writes := 0
	runStatus := "failure"
	writeStatus := 201
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			require.Equal(t, "/api/v1/repos/ci/project/actions/workflows/ci.yml/dispatches", r.URL.Path)
			require.Equal(t, "token provider-only", r.Header.Get("Authorization"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, head, body["ref"])
			w.WriteHeader(writeStatus)
			return
		}
		now := "2026-01-01T00:00:00Z"
		switch r.URL.Path {
		case "/api/v1/repos/ci/project/actions/runs":
			require.Equal(t, "token provider-only", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{
				"id": 17, "index_in_repo": 9, "workflow_id": "ci.yml", "prettyref": "feature", "commit_sha": head, "event": "push", "status": runStatus, "created": now, "updated": now, "html_url": "https://ci.example.test/ci/project/actions/runs/9",
			}}})
		case "/api/v1/repos/ci/project/actions/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{map[string]any{
				"id": 41, "run_number": 9, "name": "check-linux", "workflow_id": "ci.yml", "head_sha": head, "status": "failure", "created_at": now, "updated_at": now, "url": "https://ci.example.test/ci/project/actions/runs/9",
			}}})
		case "/api/internal/provider-logs/repos/ci/project/tasks/41":
			require.Equal(t, "Bearer bridge-only", r.Header.Get("Authorization"))
			require.Equal(t, head, r.URL.Query().Get("head_sha"))
			require.Equal(t, "feature", r.URL.Query().Get("head_ref"))
			_ = json.NewEncoder(w).Encode(map[string]any{"schema": "ags.internal-provider-log.v1", "repo": "ci/project", "task_id": 41, "run_number": 9, "job_name": "check-linux", "head_sha": head, "provider_pr": 0, "provider_ref": "refs/heads/feature", "event": "push", "text": "2026-01-01T00:00:00Z check-linux failure fixture\n"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	config := cibackend.BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &cibackend.LogBridgeConfig{URL: provider.URL, TokenFile: "bridge-fixture", AllowHTTP: true}}
	backend, err := cibackend.NewHTTP(config, "provider-only", "bridge-only")
	require.NoError(t, err)
	h.Svc.CI, err = cibackend.WithBackends(cibackend.Config{Backends: map[string]cibackend.BackendConfig{"builder": config}, Repositories: map[string]cibackend.Binding{repo.FullName: {Backend: "builder", Repository: "ci/project"}}}, map[string]cibackend.Backend{"builder": backend})
	require.NoError(t, err)
	runs, err := h.Svc.CIRuns(context.Background(), repo.FullName, cibackend.Query{})
	require.NoError(t, err)
	require.Len(t, runs.Runs, 1)
	path := fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d", repo.FullName, runs.Runs[0].ID)
	detail := h.DoREST(t, "GET", path, nil)
	require.Equal(t, 200, detail.Code, detail.Body.String())
	require.Equal(t, head, testharness.DecodeJSON(t, detail)["head_sha"])
	jobs := h.DoREST(t, "GET", path+"/jobs", nil)
	require.Equal(t, 200, jobs.Code, jobs.Body.String())
	rows := testharness.DecodeJSON(t, jobs)["jobs"].([]any)
	require.Len(t, rows, 1)
	job := rows[0].(map[string]any)
	require.Equal(t, "check-linux", job["name"])
	logs := h.DoREST(t, "GET", path+"/logs", nil)
	require.Equal(t, 200, logs.Code, logs.Body.String())
	archive, err := zip.NewReader(bytes.NewReader(logs.Body.Bytes()), int64(logs.Body.Len()))
	require.NoError(t, err)
	require.Len(t, archive.File, 1)
	require.Equal(t, "0_check-linux.txt", archive.File[0].Name)
	file, err := archive.File[0].Open()
	require.NoError(t, err)
	text, err := io.ReadAll(file)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.Contains(t, string(text), "check-linux failure fixture")
	jobLogs := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/jobs/%.0f/logs", repo.FullName, job["id"]), nil)
	require.Equal(t, 200, jobLogs.Code, jobLogs.Body.String())
	require.Equal(t, string(text), jobLogs.Body.String())
	for _, action := range []string{"rerun", "rerun-failed-jobs"} {
		res := h.DoREST(t, "POST", path+"/"+action, nil)
		require.Equal(t, 201, res.Code, res.Body.String())
		require.Equal(t, "workflow", res.Header().Get("X-AGS-CI-Rerun-Scope"))
	}
	jobRerun := h.DoREST(t, "POST", fmt.Sprintf("/api/v3/repos/%s/actions/jobs/%.0f/rerun", repo.FullName, job["id"]), nil)
	require.Equal(t, 201, jobRerun.Code, jobRerun.Body.String())
	require.Equal(t, 3, writes, "one dispatch per rerun")
	cancel := h.DoREST(t, "POST", path+"/cancel", nil)
	require.Equal(t, 501, cancel.Code)
	require.True(t, strings.Contains(cancel.Body.String(), "does not support"))
	require.Equal(t, 3, writes, "cancel must not dispatch")
	runStatus = "running"
	active := h.DoREST(t, "POST", path+"/rerun", nil)
	require.Equal(t, 409, active.Code, active.Body.String())
	require.Equal(t, 3, writes)
	runStatus = "failure"
	writeStatus = 403
	denied := h.DoREST(t, "POST", path+"/rerun", nil)
	require.Equal(t, 403, denied.Code, denied.Body.String())
	require.Equal(t, 4, writes)
	for _, suffix := range []string{"rerun", "rerun-failed-jobs"} {
		missing := h.DoREST(t, "POST", fmt.Sprintf("/api/v3/repos/%s/actions/runs/%d/%s", repo.FullName, service.CIExternalIDBase+999999, suffix), nil)
		require.Equal(t, 404, missing.Code, missing.Body.String())
	}
	missingJob := h.DoREST(t, "POST", fmt.Sprintf("/api/v3/repos/%s/actions/jobs/%d/rerun", repo.FullName, service.CIExternalIDBase+999999), nil)
	require.Equal(t, 404, missingJob.Code, missingJob.Body.String())
	require.Equal(t, 4, writes)
	foreign := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/runs/17", repo.FullName), nil)
	require.Equal(t, 404, foreign.Code, "provider IDs cannot bypass AGS mappings")
	unknownArtifacts := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/actions/runs/17/artifacts", repo.FullName), nil)
	require.Equal(t, 404, unknownArtifacts.Code, "unknown runs cannot fabricate successful artifact collections")
	validArtifacts := h.DoREST(t, "GET", path+"/artifacts", nil)
	require.Equal(t, 200, validArtifacts.Code, validArtifacts.Body.String())
	require.Equal(t, float64(0), testharness.DecodeJSON(t, validArtifacts)["total_count"])
}
