package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ngaut/agent-git-service/internal/attribution"
	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

// projectNativePRComment is opt-in with attribution. Native comments remain
// authoritative; projection denial must not turn a committed comment into an
// ambiguous API failure. Only an existing exact PR binding is eligible.
func (s *Service) projectNativePRComment(ctx context.Context, comment db.IssueComment) {
	if s.ForgejoIntegration == nil || !s.ForgejoIntegration.AttributionEnabled() {
		return
	}
	actor, ok := UserFromContext(ctx)
	if !ok || actor.ID == 0 || actor.ID != comment.AuthorID {
		return
	}
	// Do not turn the attribution adapter into a second authorization path.
	if err := s.requireRepoPermission(ctx, comment.RepositoryID, RepoPermissionWrite); err != nil {
		return
	}
	var pr db.PullRequest
	err := s.DBForCtx(ctx).Where("repository_id = ? AND number = ?", comment.RepositoryID, comment.IssueNumber).First(&pr).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "load native PR for comment projection", "error", err)
		return
	}
	var projection db.PullRequestProjection
	if err := s.DBForCtx(ctx).Where("pull_request_id = ? AND provider = ?", pr.ID, ProjectionProviderForgejo).First(&projection).Error; err != nil {
		return
	}
	var repo db.Repository
	if s.DBForCtx(ctx).First(&repo, pr.RepositoryID).Error != nil {
		return
	}
	target, targetErr := s.ForgejoIntegration.ConfiguredTargetFullName(repo.FullName)
	if targetErr != nil || projection.ExternalRepo != target || projection.ExternalNumber <= 0 {
		return
	}
	ctx = attribution.WithCoordinates(ctx, repo.FullName, pr.Number)
	if _, delegated := DelegatedSessionIDFromContext(ctx); delegated {
		if _, err := s.RevalidateDelegatedSession(ctx, pr.RepositoryID, "pr.comment", "pr:comment", map[string]string{}); err != nil {
			return
		}
	}
	if err := s.ForgejoIntegration.CreatePullRequestComment(ctx, projection.ExternalRepo, projection.ExternalNumber, string(comment.Body)); err != nil {
		slog.WarnContext(ctx, "native PR comment projection failed", "repo", repo.FullName, "pr_number", pr.Number, "comment_id", comment.ID, "error", err)
	}
}
