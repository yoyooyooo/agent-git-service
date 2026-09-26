package rest

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

func requestBearer(r *http.Request) string {
	if _, password, ok := r.BasicAuth(); ok {
		return password
	}
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) == 2 && (strings.EqualFold(parts[0], "bearer") || strings.EqualFold(parts[0], "token")) {
		return strings.TrimSpace(parts[1])
	}
	return ""
}
func (d *Deps) StartClientRun(w http.ResponseWriter, r *http.Request) {
	var input service.ClientRunInput
	if e := decodeBodyStrict(r, &input); e != nil {
		respond.ValidationFailed(w, "invalid run context or lifetime")
		return
	}
	issued, e := d.Svc.StartClientRun(r.Context(), requestBearer(r), input)
	if e != nil {
		respond.ServiceErrorRequest(r, w, e)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond.JSON(w, 201, issued)
}
func (d *Deps) GetClientRun(w http.ResponseWriter, r *http.Request) {
	value, e := d.Svc.ClientRunStatus(r.Context(), chi.URLParam(r, "run_id"))
	if e != nil {
		respond.ServiceErrorRequest(r, w, e)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond.JSON(w, 200, value)
}
func (d *Deps) RevokeClientRun(w http.ResponseWriter, r *http.Request) {
	if e := d.Svc.RevokeClientRun(r.Context(), chi.URLParam(r, "run_id")); e != nil {
		respond.ServiceErrorRequest(r, w, e)
		return
	}
	respond.NoContent(w)
}
func (d *Deps) GetClientRunLinks(w http.ResponseWriter, r *http.Request) {
	number, ok := mustIntParam(w, r, "number")
	if !ok {
		return
	}
	value, e := d.Svc.ClientRunLinks(r.Context(), repoFullName(r), number)
	if e != nil {
		respond.ServiceErrorRequest(r, w, e)
		return
	}
	respond.JSON(w, 200, value)
}
func (d *Deps) GetCIBackend(w http.ResponseWriter, r *http.Request) {
	repo := d.mustGetRepo(w, r)
	if repo == nil {
		return
	}
	selected, e := d.Svc.CISelection(repo.FullName)
	if e != nil {
		ciHTTPError(w, e)
		return
	}
	var required any
	if selected.Binding.RequiredChecks != nil {
		required = *selected.Binding.RequiredChecks
	}
	respond.JSON(w, 200, map[string]any{"schema": "ags.ci-backend.v1", "repository": repo.FullName, "backend": selected.Name, "kind": selected.Kind, "external_repository": selected.Binding.Repository, "required_checks": required, "git_hosting_is_ci": false, "merge_authority_is_ci": false})
}
