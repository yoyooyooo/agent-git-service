package rest

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

func (d *Deps) ListProviderPullRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	query := r.URL.Query()
	page, err := inventoryQueryInt(query, "page", 1)
	if err != nil {
		respond.ValidationFailed(w, "page must be an integer")
		return
	}
	perPage, err := inventoryQueryInt(query, "per_page", service.ProviderInventoryMaxPerPage)
	if err != nil {
		respond.ValidationFailed(w, "per_page must be an integer")
		return
	}
	state := query.Get("state")
	if state == "" {
		state = "open"
	}
	res, err := d.Svc.ListProviderPullRequests(r.Context(), repoFullName(r), state, page, perPage)
	if err != nil {
		providerInventoryError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if res.NextPage != nil {
		// Official gh requires absolute continuation URLs. Use the configured API
		// origin, never request Host or provider pagination URLs/credentials.
		next := url.Values{"state": {state}, "page": {strconv.Itoa(*res.NextPage)}, "per_page": {strconv.Itoa(perPage)}}
		w.Header().Set("Link", fmt.Sprintf("<%s%s?%s>; rel=\"next\"", d.Svc.APIBaseURL(), r.URL.EscapedPath(), next.Encode()))
	}
	respond.JSON(w, http.StatusOK, res)
}

func (d *Deps) GetProviderPullRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	number, err := strconv.Atoi(pathParam(r, "provider_number"))
	if err != nil {
		respond.ValidationFailed(w, "provider number must be a positive integer")
		return
	}
	res, err := d.Svc.ReadProviderPullRequest(r.Context(), repoFullName(r), number)
	if err != nil {
		providerInventoryError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond.JSON(w, http.StatusOK, res)
}

func (d *Deps) GetProviderGitRef(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	res, err := d.Svc.ReadProviderGitRef(r.Context(), repoFullName(r), r.URL.Query().Get("ref"))
	if err != nil {
		providerInventoryError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	respond.JSON(w, http.StatusOK, res)
}

func inventoryQueryInt(query url.Values, key string, fallback int) (int, error) {
	values, present := query[key]
	if !present {
		return fallback, nil
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("duplicate pagination parameter")
	}
	return strconv.Atoi(values[0])
}

func providerInventoryError(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, service.ErrProviderInventoryNotConfigured):
		respond.Error(w, http.StatusServiceUnavailable, "Provider inventory is not configured")
	case errors.Is(err, service.ErrProviderInventoryUnavailable):
		respond.Error(w, http.StatusBadGateway, "Provider inventory could not be observed")
	default:
		respond.ServiceErrorRequest(r, w, err)
	}
}
