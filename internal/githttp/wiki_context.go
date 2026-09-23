package githttp

import (
	"errors"
	"log/slog"
	"net/http"

	applog "github.com/ngaut/agent-git-service/internal/logging"
	"github.com/ngaut/agent-git-service/internal/rest/respond"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/wikiv2"
)

// Synthetic Wiki repositories use the parent's current native permission, but
// have their own Git storage and receive-pack transaction. A main-repository
// delegated Session does not implicitly grant access to that sibling store.
func (h *Handler) resolveWikiRepoContext(w http.ResponseWriter, r *http.Request, action, gitService, requested string) (*repoContext, bool) {
	parentName, ok := wikiRepoParentName(requested)
	if !ok {
		respond.NotFound(w)
		return nil, false
	}
	if _, delegated := service.DelegatedSessionIDFromContext(r.Context()); delegated {
		respond.Error(w, http.StatusForbidden, "Delegated Wiki transport is not supported")
		return nil, false
	}
	parent, err := h.Svc.AuthorizeGitTransport(r.Context(), parentName, gitService)
	if err != nil {
		var denied *service.GitTransportAccessError
		if errors.As(err, &denied) && denied.Stage == service.GitAccessAuthentication {
			gitAuthChallenge(w)
			return nil, false
		}
		switch {
		case errors.Is(err, service.ErrNotFound):
			respond.NotFound(w)
		case errors.Is(err, service.ErrForbidden):
			respond.Error(w, http.StatusForbidden, "Wiki transport is not allowed")
		default:
			slog.ErrorContext(r.Context(), "githttp wiki authorization failed", "action", action, "repo", requested, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return nil, false
	}
	if !parent.HasWiki || parent.FullName+".wiki" != requested {
		respond.NotFound(w)
		return nil, false
	}
	applog.AddAttrs(r.Context(), slog.String("repo", parent.FullName), slog.String("git_repo", requested))
	if !h.store.Exists(r.Context(), requested) {
		if err := h.store.Init(r.Context(), requested, wikiv2.DefaultBranch, false); err != nil {
			slog.ErrorContext(r.Context(), "githttp ensure wiki repo failed", "action", action, "repo", requested, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return nil, false
		}
	}
	repoPath, err := h.store.GetRepoPath(r.Context(), requested)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return nil, false
	}
	root, err := h.store.RepoRoot(r.Context())
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return nil, false
	}
	return &repoContext{
		projectRoot: root, repoPath: repoPath, repoFullName: parent.FullName,
		gitRepoFullName: requested, repositoryID: parent.ID,
		defaultBranch: wikiv2.DefaultBranch, isWikiBacking: true,
	}, true
}
