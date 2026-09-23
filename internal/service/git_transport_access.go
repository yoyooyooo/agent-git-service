package service

import (
	"context"
	"fmt"

	"github.com/ngaut/agent-git-service/internal/db"
)

// GitTransportAccessStage preserves the primary HTTP adapter's historical
// challenge/404/403 and audit distinctions without putting HTTP in service.
type GitTransportAccessStage string

const (
	GitAccessOperation      GitTransportAccessStage = "operation"
	GitAccessLookup         GitTransportAccessStage = "lookup"
	GitAccessAuthentication GitTransportAccessStage = "authentication"
	GitAccessDelegated      GitTransportAccessStage = "delegated"
	GitAccessPermission     GitTransportAccessStage = "permission"
)

// GitTransportAccessError must not be rendered verbatim to clients. Its Cause
// preserves errors.Is and the existing secret-safe delegated denial reason.
type GitTransportAccessError struct {
	Stage GitTransportAccessStage
	Cause error
}

func (e *GitTransportAccessError) Error() string {
	return fmt.Sprintf("git transport %s: %v", e.Stage, e.Cause)
}
func (e *GitTransportAccessError) Unwrap() error { return e.Cause }

func gitTransportPermission(gitService string) (RepoPermission, error) {
	switch gitService {
	case "git-upload-pack":
		return RepoPermissionRead, nil
	case "git-receive-pack":
		return RepoPermissionWrite, nil
	default:
		return RepoPermissionNone, ErrValidation
	}
}

// AuthorizeGitTransport resolves canonical repository identity and applies the
// SAME native/delegated authority as direct Git HTTP. The caller must first
// authenticate the original request through the existing token middleware.
// This method does not create/open repositories, refresh hooks, execute Git,
// authorize replication nodes, or mint an independently reusable capability.
// A future Edge prepare endpoint must separately authenticate its peer and
// preserve the original user's identity; a replication credential is not it.
func (s *Service) AuthorizeGitTransport(ctx context.Context, fullName, gitService string) (db.Repository, error) {
	required, err := gitTransportPermission(gitService)
	if err != nil {
		return db.Repository{}, &GitTransportAccessError{Stage: GitAccessOperation, Cause: err}
	}
	rep, err := s.LookupRepoIdentity(ctx, fullName)
	if err != nil {
		return rep, &GitTransportAccessError{Stage: GitAccessLookup, Cause: err}
	}
	allowAnonymousRead := !rep.Private && required.Effective() == RepoPermissionRead
	if _, authenticated := UserFromContext(ctx); !authenticated && !allowAnonymousRead {
		return rep, &GitTransportAccessError{Stage: GitAccessAuthentication, Cause: ErrUnauthorized}
	}
	if _, delegated := DelegatedSessionIDFromContext(ctx); delegated {
		if err := s.RevalidateDelegatedGitTransport(ctx, rep.ID, gitService); err != nil {
			return rep, &GitTransportAccessError{Stage: GitAccessDelegated, Cause: err}
		}
	}
	if err := s.RequireRepoPermission(ctx, rep.ID, required); err != nil {
		return rep, &GitTransportAccessError{Stage: GitAccessPermission, Cause: err}
	}
	return rep, nil
}

// RevalidateDelegatedGitTransport is the existing use-time delegated check.
// Primary handlers still call it immediately before CGI, AFTER storage/hook
// preparation. Edge reads waiting for a snapshot must similarly reauthorize
// immediately before serving, not rely on an earlier preparation decision.
func (s *Service) RevalidateDelegatedGitTransport(ctx context.Context, repositoryID uint, gitService string) error {
	operation, capability := "git.read", "repo:read"
	if gitService == "git-receive-pack" {
		operation, capability = "git.push", "repo:write"
	} else if gitService != "git-upload-pack" {
		return ErrDelegatedSessionOperationConstraintDenied
	}
	_, err := s.RevalidateDelegatedSession(ctx, repositoryID, operation, capability, map[string]string{})
	return err
}
