package rest

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

func (d *Deps) BeginGitHubAppManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	viewer, ok := service.UserFromContext(r.Context())
	if !ok || !viewer.SiteAdmin {
		respond.Error(w, http.StatusForbidden, "site administrator required")
		return
	}
	if d.GitHubAppRegistrar == nil {
		respond.NotFound(w)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		respond.Error(w, 400, "invalid app registration request")
		return
	}
	result, err := d.GitHubAppRegistrar.Begin(body.Name)
	if err != nil {
		respond.Error(w, 409, "registration not started; inspect private state before retrying")
		return
	}
	respond.JSON(w, 201, result)
}

func (d *Deps) GetRegisteredGitHubApp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	viewer, ok := service.UserFromContext(r.Context())
	if !ok || !viewer.SiteAdmin {
		respond.Error(w, http.StatusForbidden, "site administrator required")
		return
	}
	if d.GitHubAppRegistrar == nil {
		respond.NotFound(w)
		return
	}
	result, err := d.GitHubAppRegistrar.App(r)
	if err != nil {
		respond.Error(w, 503, "App credentials or provider observation unavailable")
		return
	}
	respond.JSON(w, 200, result)
}
