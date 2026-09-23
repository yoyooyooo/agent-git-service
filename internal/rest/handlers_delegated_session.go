package rest

import (
	"net/http"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
)

func (d *Deps) GetDelegatedAgentSessionLifecycle(w http.ResponseWriter, r *http.Request) {
	readback, err := d.Svc.GetDelegatedSessionLifecycle(r.Context(), pathParam(r, "session_id"))
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, readback)
}
