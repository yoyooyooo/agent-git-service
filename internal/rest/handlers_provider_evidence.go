package rest

import (
	"errors"
	"net/http"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

// GetPullRequestProviderProjection reads one exact live provider PR projection
// through AGS. Ordinary clients never need provider credentials.
func (d *Deps) GetPullRequestProviderProjection(w http.ResponseWriter, r *http.Request) {
	pr, ok := d.providerEvidencePullRequest(w, r)
	if !ok {
		return
	}
	evidence, err := d.Svc.ReadProviderProjectionEvidence(r.Context(), pr)
	if err != nil {
		d.respondProviderEvidenceError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, evidence)
}

// GetPullRequestProviderCIRuns reads provider CI evidence bound to the exact
// AGS PR and head through the configured AGS provider adapter.
func (d *Deps) GetPullRequestProviderCIRuns(w http.ResponseWriter, r *http.Request) {
	pr, ok := d.providerEvidencePullRequest(w, r)
	if !ok {
		return
	}
	evidence, err := d.Svc.ReadProviderCIEvidence(r.Context(), pr)
	if err != nil {
		d.respondProviderEvidenceError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, evidence)
}

// GetPullRequestProviderCIRunLogs reads one exact provider CI run log through AGS.
func (d *Deps) GetPullRequestProviderCIRunLogs(w http.ResponseWriter, r *http.Request) {
	pr, ok := d.providerEvidencePullRequest(w, r)
	if !ok {
		return
	}
	runID, ok := mustIntParam(w, r, "run_id")
	if !ok {
		return
	}
	evidence, err := d.Svc.ReadProviderCIRunLogs(r.Context(), pr, int64(runID))
	if err != nil {
		d.respondProviderEvidenceError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, evidence)
}

func (d *Deps) providerEvidencePullRequest(w http.ResponseWriter, r *http.Request) (db.PullRequest, bool) {
	if viewer, ok := service.UserFromContext(r.Context()); !ok || viewer.ID == 0 {
		respond.Unauthorized(w, "Bad credentials")
		return db.PullRequest{}, false
	}
	number, ok := mustIntParam(w, r, "number")
	if !ok {
		return db.PullRequest{}, false
	}
	pr, err := d.Svc.GetPR(r.Context(), repoFullName(r), number)
	if err != nil {
		respond.ServiceErrorRequest(r, w, err)
		return db.PullRequest{}, false
	}
	if !d.revalidateDelegatedPRRead(w, r, pr) {
		return db.PullRequest{}, false
	}
	return pr, true
}

func (d *Deps) respondProviderEvidenceError(w http.ResponseWriter, r *http.Request, err error) {
	var readErr *service.ProviderEvidenceReadError
	if errors.As(err, &readErr) {
		status := http.StatusBadGateway
		if errors.Is(err, service.ErrProviderEvidenceUnavailable) && readErr.Receipt.ProviderAttempt == "not_attempted" {
			status = http.StatusConflict
		}
		respond.JSON(w, status, map[string]any{
			"message":             "Provider evidence is unavailable",
			"correlation_receipt": readErr.Receipt,
		})
		return
	}
	respond.ServiceErrorRequest(r, w, err)
}
