package rest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
	"github.com/stretchr/testify/require"
)

func TestExternalWorkflowDispatchPinsHeadAndChecksWriteAccess(t *testing.T) {
	h := testharness.New(t)
	repo, head := seedCheckRunsRepo(t, h, "workflow-dispatch")
	writes := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes++
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/api/v1/repos/ci/project/actions/workflows/ci.yml/dispatches", r.URL.Path)
		require.Equal(t, "token provider-only", r.Header.Get("Authorization"))
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, head, payload["ref"])
		require.Equal(t, map[string]any{"target": "test"}, payload["inputs"])
		w.WriteHeader(204)
	}))
	defer provider.Close()
	config := cibackend.BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}
	backend, err := cibackend.NewHTTP(config, "provider-only")
	require.NoError(t, err)
	h.Svc.CI, err = cibackend.WithBackends(cibackend.Config{Backends: map[string]cibackend.BackendConfig{"builder": config}, Repositories: map[string]cibackend.Binding{repo.FullName: {Backend: "builder", Repository: "ci/project"}}}, map[string]cibackend.Backend{"builder": backend})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v3/repos/%s/actions/workflows/ci.yml/dispatches", repo.FullName)
	payload := map[string]any{"ref": repo.DefaultBranch, "inputs": map[string]any{"target": "test"}}
	response := h.DoRESTJSON(t, "POST", path, payload)
	require.Equal(t, 204, response.Code, response.Body.String())
	require.Equal(t, 1, writes)

	var rows []db.CIResource
	// Catalogue discovery is not needed for filename dispatch, and no run is fabricated.
	require.NoError(t, h.DB.Where("repository_id = ?", repo.ID).Find(&rows).Error)
	require.Empty(t, rows)
	var runs int64
	require.NoError(t, h.DB.Model(&db.WorkflowRun{}).Where("repository_id = ?", repo.ID).Count(&runs).Error)
	require.Zero(t, runs)

	// Numeric IDs belong to a repository and a backend namespace, not Forgejo.
	selection, err := h.Svc.CISelection(repo.FullName)
	require.NoError(t, err)
	mapped := db.CIResource{RepositoryID: repo.ID, Namespace: selection.Namespace, ExternalRepository: "ci/project", Kind: "workflow", ExternalID: "ci.yml"}
	require.NoError(t, h.DB.Create(&mapped).Error)
	numericPath := fmt.Sprintf("/api/v3/repos/%s/actions/workflows/%d/dispatches", repo.FullName, service.CIExternalIDBase+uint64(mapped.ID))
	pinned := h.DoRESTJSON(t, "POST", numericPath, map[string]any{"ref": head, "inputs": map[string]any{"target": "test"}})
	require.Equal(t, 204, pinned.Code, pinned.Body.String())
	require.Equal(t, 2, writes)
	require.NoError(t, h.DB.Model(&mapped).Update("namespace", "retired-backend").Error)
	retired := h.DoRESTJSON(t, "POST", numericPath, payload)
	require.Equal(t, 404, retired.Code, retired.Body.String())

	invalid := h.DoRESTJSON(t, "POST", path, map[string]any{"ref": "missing-branch"})
	require.Equal(t, 422, invalid.Code, invalid.Body.String())
	missing := h.DoRESTJSON(t, "POST", fmt.Sprintf("/api/v3/repos/%s/actions/workflows/%d/dispatches", repo.FullName, service.CIExternalIDBase+999999), payload)
	require.Equal(t, 404, missing.Code, missing.Body.String())
	reader := db.User{Login: "dispatch-reader", Type: db.TypeUser}
	require.NoError(t, h.DB.Create(&reader).Error)
	require.NoError(t, h.DB.Create(&db.Collaborator{RepositoryID: repo.ID, UserID: reader.ID, Permission: "read"}).Error)
	err = h.Svc.CIDispatchWorkflow(service.ContextWithUser(context.Background(), reader), repo.FullName, "ci.yml", repo.DefaultBranch, nil)
	require.ErrorIs(t, err, service.ErrForbidden)
	require.Equal(t, 2, writes, "denied and invalid requests must not call provider")
}
