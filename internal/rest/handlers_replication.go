package rest

import (
	"errors"
	"net/http"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

// GetReplicationRegistration observes identity without allocating one. These
// native repo-admin endpoints are not node replication or transport endpoints.
func (d *Deps) GetReplicationRegistration(w http.ResponseWriter, r *http.Request) {
	d.replicationRegistration(w, r, false)
}
func (d *Deps) RegisterReplicationRepository(w http.ResponseWriter, r *http.Request) {
	d.replicationRegistration(w, r, true)
}
func (d *Deps) replicationRegistration(w http.ResponseWriter, r *http.Request, register bool) {
	w.Header().Set("Cache-Control", "no-store")
	if d.ReplicationAuthorityID == "" {
		respond.NotFound(w)
		return
	}
	if r.URL.RawQuery != "" || len(r.Header.Values("Authorization")) > 1 || r.Header.Get("Content-Encoding") != "" {
		respond.Error(w, http.StatusBadRequest, "Invalid registration envelope")
		return
	}
	var result edgeprotocol.Registration
	var err error
	if register {
		if r.Header.Get("Content-Type") != "application/json" {
			respond.Error(w, http.StatusUnsupportedMediaType, "Requires application/json")
			return
		}
		var expected edgeprotocol.RegisterRepository
		if edgeprotocol.DecodeControl(r.Body, &expected) != nil || expected.Validate() != nil {
			respond.Error(w, http.StatusBadRequest, "Requires an exact prior repository observation")
			return
		}
		result, err = d.Svc.RegisterReplicationRepository(r.Context(), d.ReplicationAuthorityID, repoFullName(r), expected)
	} else {
		result, err = d.Svc.ObserveReplicationRegistration(r.Context(), d.ReplicationAuthorityID, repoFullName(r))
	}
	if err != nil {
		switch {
		case errors.Is(err, service.ErrUnauthorized):
			respond.Error(w, http.StatusUnauthorized, "Requires authentication")
		case errors.Is(err, service.ErrForbidden):
			respond.Error(w, http.StatusForbidden, "Native repository administrator required")
		case errors.Is(err, service.ErrNotFound):
			respond.NotFound(w)
		case errors.Is(err, service.ErrReplicationRegistrationConflict):
			respond.Error(w, http.StatusConflict, "Repository observation changed; inspect again before registration")
		case errors.Is(err, service.ErrValidation):
			respond.Error(w, http.StatusBadRequest, "Invalid registration request")
		default:
			respond.Error(w, http.StatusInternalServerError, "Replication registration unavailable")
		}
		return
	}
	respond.JSON(w, http.StatusOK, result)
}
