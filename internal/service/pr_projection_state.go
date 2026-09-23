package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
)

// DispatchPullRequestClosedIntegrations closes external projections for an AGS PR closed without merge.
func (s *Service) DispatchPullRequestClosedIntegrations(ctx context.Context, pr db.PullRequest) error {
	if s == nil || pr.ID == 0 {
		return nil
	}
	repoFullName := pr.Repository.FullName
	if repoFullName == "" {
		repo, err := s.GetRepoByID(ctx, fmt.Sprint(pr.RepositoryID))
		if err != nil {
			return fmt.Errorf("lookup PR repo for projection close: %w", err)
		}
		repoFullName = repo.FullName
		pr.Repository = repo
	}

	var projections []db.PullRequestProjection
	if err := s.DBForCtx(ctx).Where("pull_request_id = ?", pr.ID).Find(&projections).Error; err != nil {
		return fmt.Errorf("load PR projections for close: %w", err)
	}
	closedProjectionIDs := make([]uint, 0, len(projections))
	openProjectionCount := 0
	failedProjectionCount := 0
	for _, projection := range projections {
		if projection.State == ProjectionStateClosed {
			continue
		}
		openProjectionCount++
		if s.closeExternalPullRequestProjection(ctx, repoFullName, pr, projection) {
			closedProjectionIDs = append(closedProjectionIDs, projection.ID)
		} else {
			failedProjectionCount++
		}
	}
	if len(closedProjectionIDs) > 0 {
		now := time.Now().UTC()
		if err := s.DBForCtx(ctx).Model(&db.PullRequestProjection{}).
			Where("id IN ?", closedProjectionIDs).
			Updates(map[string]any{
				"state":           ProjectionStateClosed,
				"last_synced_sha": firstNonEmpty(pr.HeadSHA, projectionSHA(projections)),
				"updated_at":      now,
			}).Error; err != nil {
			return fmt.Errorf("mark PR projections closed: %w", err)
		}
	}
	if failedProjectionCount == 0 {
		s.projectPullRequestClosedToMultica(ctx, pr)
	} else if openProjectionCount > 0 {
		slog.WarnContext(ctx, "skip Multica close projection because external PR projections remain open", "repo", repoFullName, "pr_number", pr.Number, "failed_projection_count", failedProjectionCount)
	}
	return nil
}

func (s *Service) closeExternalPullRequestProjection(ctx context.Context, repoFullName string, pr db.PullRequest, projection db.PullRequestProjection) bool {
	switch projection.Provider {
	case ProjectionProviderForgejo:
		if s.ForgejoIntegration == nil || projection.ExternalNumber == 0 {
			return false
		}
		if _, err := s.ForgejoIntegration.UpdatePullRequestState(ctx, repoFullName, projection.ExternalNumber, ProjectionStateClosed); err != nil {
			slog.WarnContext(ctx, "close Forgejo PR projection failed", "repo", repoFullName, "pr_number", pr.Number, "external_number", projection.ExternalNumber, "error", err)
			return false
		}
		return true
	case ProjectionProviderGitLab:
		if s.GitLabIntegration == nil {
			return false
		}
		_, handled, err := s.GitLabIntegration.CloseShadowMergeRequest(ctx, gitlabintegration.CloseShadowMergeRequestRequest{
			RepoFullName: repoFullName,
			HeadBranch:   firstNonEmpty(projection.SourceBranch, pr.HeadRef),
			BaseBranch:   firstNonEmpty(projection.TargetBranch, pr.BaseRef),
			PRNumber:     pr.Number,
			MergedSHA:    firstNonEmpty(pr.HeadSHA, projection.LastSyncedSHA),
			CloseReason:  fmt.Sprintf("Closed by AGS after AGS PR #%d closed without merge.", pr.Number),
		})
		if err != nil {
			slog.WarnContext(ctx, "close GitLab shadow MR projection failed", "repo", repoFullName, "pr_number", pr.Number, "external_number", projection.ExternalNumber, "error", err)
			return false
		}
		if !handled {
			slog.WarnContext(ctx, "GitLab shadow MR projection not found for close", "repo", repoFullName, "pr_number", pr.Number, "external_number", projection.ExternalNumber)
			return false
		}
		return true
	case ProjectionProviderGitHub:
		if s.GitHubIntegration == nil {
			return false
		}
		_, handled, err := s.GitHubIntegration.CloseShadowPullRequest(ctx, githubintegration.CloseShadowPullRequestRequest{
			RepoFullName: repoFullName,
			HeadBranch:   firstNonEmpty(projection.SourceBranch, pr.HeadRef),
			BaseBranch:   firstNonEmpty(projection.TargetBranch, pr.BaseRef),
			PRNumber:     pr.Number,
			MergedSHA:    firstNonEmpty(pr.HeadSHA, projection.LastSyncedSHA),
			CloseReason:  fmt.Sprintf("Closed by AGS after AGS PR #%d closed without merge.", pr.Number),
		})
		if err != nil {
			slog.WarnContext(ctx, "close GitHub shadow PR projection failed", "repo", repoFullName, "pr_number", pr.Number, "external_number", projection.ExternalNumber, "error", err)
			return false
		}
		if !handled {
			slog.WarnContext(ctx, "GitHub shadow PR projection not found for close", "repo", repoFullName, "pr_number", pr.Number, "external_number", projection.ExternalNumber)
			return false
		}
		return true
	}
	return false
}

func projectionSHA(projections []db.PullRequestProjection) string {
	for _, projection := range projections {
		if projection.LastSyncedSHA != "" {
			return projection.LastSyncedSHA
		}
	}
	return ""
}
