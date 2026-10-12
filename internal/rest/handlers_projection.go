package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"gorm.io/gorm"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
)

// GetProjectionStatus handles GET /api/v3/repos/{owner}/{repo}/projection/status.
func (d *Deps) GetProjectionStatus(w http.ResponseWriter, r *http.Request) {
	res, err := d.Svc.GetProjectionStatus(r.Context(), repoFullName(r))
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

// RetryProjection handles POST /api/v3/repos/{owner}/{repo}/projection/forgejo/pulls/{number}/retry.
func (d *Deps) RetryProjection(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(pathParam(r, "number"))
	if err != nil || number <= 0 {
		respond.ValidationFailed(w, "invalid pull request number")
		return
	}
	res, err := d.Svc.RetryForgejoPullRequestProjection(r.Context(), repoFullName(r), number)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respond.NotFound(w)
			return
		}
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusAccepted, res)
}

// RepairProjectionAuthority handles POST /api/v3/repos/{owner}/{repo}/projection/forgejo/pulls/{number}/repair.
// The JSON body field confirm must be the current full AGS head SHA.
func (d *Deps) RepairProjectionAuthority(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(pathParam(r, "number"))
	if err != nil || number <= 0 {
		respond.ValidationFailed(w, "invalid pull request number")
		return
	}
	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		respond.ValidationFailed(w, "confirm must be the full current AGS head SHA")
		return
	}
	res, err := d.Svc.RepairForgejoPullRequestProjectionAuthority(r.Context(), repoFullName(r), number, body.Confirm)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			respond.NotFound(w)
			return
		}
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusAccepted, res)
}
