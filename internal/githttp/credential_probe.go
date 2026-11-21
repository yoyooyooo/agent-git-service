package githttp

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	applog "github.com/ngaut/agent-git-service/internal/logging"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
)

// CredentialProbe answers credential helpers at the repository URL using the
// same read authority as upload-pack, without preparing storage or running Git.
// Callers must apply the same optional authentication middleware as Git HTTP.
func (h *Handler) CredentialProbe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	requested := pathParam(r, "owner") + "/" + strings.TrimSuffix(pathParam(r, "repo"), ".git")
	applog.AddAttrs(r.Context(), slog.String("repo", requested))
	rep, err := h.Svc.AuthorizeGitTransport(r.Context(), requested, "git-upload-pack")
	if err != nil {
		var denied *service.GitTransportAccessError
		if errors.As(err, &denied) {
			switch denied.Stage {
			case service.GitAccessAuthentication:
				gitAuthChallenge(w)
				return
			case service.GitAccessDelegated:
				h.logDelegatedGitOperation(r.Context(), "git-upload-pack", "denied", service.DelegatedSessionDenialReason(err))
				respond.Error(w, http.StatusForbidden, "Delegated session operation does not allow this Git transport")
				return
			}
		}
		switch {
		case errors.Is(err, service.ErrForbidden):
			respond.Error(w, http.StatusForbidden, "Delegated session capability does not allow this Git operation")
		case errors.Is(err, service.ErrNotFound):
			respond.NotFound(w)
		default:
			slog.ErrorContext(r.Context(), "githttp credential probe authorization failed", "repo", requested, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	applog.GitHTTPTraceFromContext(r.Context()).Authorized(rep.FullName)
	applog.AddAttrs(r.Context(), slog.String("repo", rep.FullName))
	w.WriteHeader(http.StatusOK)
}
