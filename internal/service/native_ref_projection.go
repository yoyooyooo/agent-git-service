package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/gitstore"
)

// ProjectNativeRefChange completes the optional projection side of an already
// authorized, committed native API write. Provider failures remain durable drift;
// they cannot roll back the native fact or turn its response into a failed write.
// Git receive-pack owns its existing post-push dispatch and does not call this.
func (s *Service) ProjectNativeRefChange(ctx context.Context, repoFullName string, change ForgejoRefChange) error {
	if s == nil || s.Git == nil || (s.ForgejoIntegration == nil && s.GitLabIntegration == nil && s.GitHubIntegration == nil) {
		return nil
	}
	if !strings.HasPrefix(change.Ref, gitstore.RefsHeadsPrefix) || change.Ref == gitstore.RefsHeadsPrefix {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.projectionBackgroundContext(ctx), 30*time.Second)
	defer cancel()
	if change.Deleted {
		// LookupRef collapses Git command failures into ErrRefNotFound. A
		// provider deletion requires a successful read proving actual absence.
		refs, err := s.Git.ListRefsWithPrefix(ctx, repoFullName, change.Ref)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if ref.Ref == change.Ref {
				return fmt.Errorf("native ref %s was recreated; skip obsolete projection deletion", change.Ref)
			}
		}
		change.After = strings.Repeat("0", 40)
	} else {
		current, err := s.Git.LookupRef(ctx, repoFullName, change.Ref)
		if err != nil {
			return fmt.Errorf("read committed native ref for projection: %w", err)
		}
		change.After = current
	}
	repoPath, err := s.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		return err
	}
	changes := []ForgejoRefChange{change}
	return errors.Join(
		s.DispatchForgejoIntegration(ctx, repoFullName, repoPath, changes),
		s.DispatchGitLabIntegration(ctx, repoFullName, repoPath, changes),
		s.DispatchGitHubIntegration(ctx, repoFullName, repoPath, changes),
	)
}
