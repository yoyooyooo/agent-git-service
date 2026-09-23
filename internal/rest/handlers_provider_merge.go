package rest

import (
	"net/http"
	"strings"

	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

// MergePRViaProvider handles the Human AGS single-frontdoor provider effect.
// Provider coordinates and credentials are deployment-owned and are therefore
// intentionally absent from the request schema.
func (d *Deps) MergePRViaProvider(w http.ResponseWriter, r *http.Request) {
	number, ok := mustIntParam(w, r, "number")
	if !ok {
		return
	}
	var body struct {
		ExpectedHeadSHA string `json:"expected_head_sha"`
		MergeMethod     string `json:"merge_method"`
	}
	if err := decodeBodyStrict(r, &body); err != nil {
		respond.ValidationFailed(w, "invalid body")
		return
	}
	body.ExpectedHeadSHA = strings.ToLower(strings.TrimSpace(body.ExpectedHeadSHA))
	body.MergeMethod = strings.ToLower(strings.TrimSpace(body.MergeMethod))
	result, err := d.Svc.ExecuteHumanProviderMerge(r.Context(), repoFullName(r), number, service.HumanProviderMergeInput{
		ExpectedHeadSHA: body.ExpectedHeadSHA,
		MergeMethod:     body.MergeMethod,
	})
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, result)
}

// ObservePRViaProvider is a read-only merge status surface. It never POSTs a provider merge.
func (d *Deps) ObservePRViaProvider(w http.ResponseWriter, r *http.Request) {
	number, ok := mustIntParam(w, r, "number")
	if !ok {
		return
	}
	expected := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("expected_head_sha")))
	result, err := d.Svc.ObserveHumanProviderMerge(r.Context(), repoFullName(r), number, expected)
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return
	}
	respond.JSON(w, http.StatusOK, result)
}
