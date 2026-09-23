package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"gorm.io/gorm"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
)

// GetRepoFlowEnvProjection handles GET /api/v3/repos/{owner}/{repo}/repo-flow/env/{env}/status.
func (d *Deps) GetRepoFlowEnvProjection(w http.ResponseWriter, r *http.Request) {
	res, err := d.Svc.GetRepoFlowEnvProjection(r.Context(), repoFullName(r), pathParam(r, "env"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respond.NotFound(w)
			return
		}
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, res)
}

// ListRepoFlowEnvEvidenceHistory handles GET /api/v3/repos/{owner}/{repo}/repo-flow/env/{env}/history.
func (d *Deps) ListRepoFlowEnvEvidenceHistory(w http.ResponseWriter, r *http.Request) {
	res, err := d.Svc.ListRepoFlowEvidenceHistory(r.Context(), repoFullName(r), pathParam(r, "env"), repoFlowLimitParam(r))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respond.NotFound(w)
			return
		}
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, res)
}

// RetryRepoFlowEnvProjection handles POST /api/v3/repos/{owner}/{repo}/repo-flow/env/{env}/retry.
func (d *Deps) RetryRepoFlowEnvProjection(w http.ResponseWriter, r *http.Request) {
	res, err := d.Svc.RetryRepoFlowEnvProjection(r.Context(), repoFullName(r), pathParam(r, "env"))
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, res)
}

// CreateRepoFlowEvidence handles POST /api/v3/repos/{owner}/{repo}/repo-flow/evidence.
func (d *Deps) CreateRepoFlowEvidence(w http.ResponseWriter, r *http.Request) {
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		respond.ValidationFailed(w, "invalid JSON body")
		return
	}
	res, err := d.Svc.RecordRepoFlowEvidence(r.Context(), repoFullName(r), payload)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respond.NotFound(w)
			return
		}
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusCreated, res)
}

// GetRepoFlowEvidenceStatus handles GET /api/v3/repos/{owner}/{repo}/repo-flow/evidence/status.
func (d *Deps) GetRepoFlowEvidenceStatus(w http.ResponseWriter, r *http.Request) {
	res, err := d.Svc.GetRepoFlowEvidenceStatus(r.Context(), repoFullName(r))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respond.NotFound(w)
			return
		}
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, res)
}

// ListRepoFlowEvidenceHistory handles GET /api/v3/repos/{owner}/{repo}/repo-flow/evidence/history.
func (d *Deps) ListRepoFlowEvidenceHistory(w http.ResponseWriter, r *http.Request) {
	res, err := d.Svc.ListRepoFlowEvidenceHistory(r.Context(), repoFullName(r), r.URL.Query().Get("env"), repoFlowLimitParam(r))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respond.NotFound(w)
			return
		}
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, res)
}

func repoFlowLimitParam(r *http.Request) int {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}
