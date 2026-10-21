package service

import (
	"context"
	"fmt"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
)

// Keep exact rebase effects on their existing action-bound authority path;
// generic creation is durable and reloads the authoritative AGS fact on retry.
func (s *Service) ensureGitHubShadowPullRequest(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest) (githubintegration.ShadowPullRequestResult, bool, error) {
	if s == nil || s.GitHubIntegration == nil {
		return githubintegration.ShadowPullRequestResult{}, false, nil
	}
	if _, bound := forgejoActionBindingFromContext(ctx); bound {
		return s.ensureGitHubShadowPullRequestOnce(ctx, repoFullName, repoPath, headSHA, pr)
	}
	if sessionID, delegated := DelegatedSessionIDFromContext(ctx); delegated {
		// Preserve the historical fail-closed unbound rebase path and admission
		// checks before any durable retry is admitted.
		var session db.DelegatedAgentSession
		if err := s.DBForCtx(ctx).Select("id", "operation_name").First(&session, "id = ?", sessionID).Error; err != nil {
			return githubintegration.ShadowPullRequestResult{}, false, err
		}
		if session.OperationName == "pr.rebase" {
			return githubintegration.ShadowPullRequestResult{}, false, fmt.Errorf("GitHub rebase projection has no exact action binding: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
		}
		if _, err := s.RevalidateDelegatedSession(ctx, pr.RepositoryID, "pr.create", "pr:create", map[string]string{"head_ref": pr.HeadRef, "base_ref": pr.BaseRef}); err != nil {
			return githubintegration.ShadowPullRequestResult{}, false, err
		}
	}
	if pr.AgentSessionID != nil {
		delegatedCtx, err := s.ContextForDelegatedSessionID(ctx, *pr.AgentSessionID)
		if err != nil {
			return githubintegration.ShadowPullRequestResult{}, false, err
		}
		ctx = delegatedCtx
	}
	current, err := s.projectionPullRequestFact(ctx, pr.ID)
	if err != nil {
		return githubintegration.ShadowPullRequestResult{}, false, err
	}
	if current.State != db.StateOpen || current.Merged || !s.GitHubIntegration.ShadowPullRequestEnabled(repoFullName, current.HeadRef) {
		return githubintegration.ShadowPullRequestResult{}, false, nil
	}
	job, err := s.enqueueGitHubProjection(ctx, current, githubProjectionEnsure)
	if err != nil {
		return githubintegration.ShadowPullRequestResult{}, false, err
	}
	err = s.runGitHubProjectionAttempt(ctx, job, repoPath)
	s.scheduleGitHubProjection(ctx, job.ID)
	if err != nil {
		return githubintegration.ShadowPullRequestResult{}, false, err
	}
	row, ok, err := s.gitHubProjection(ctx, pr.ID)
	return githubintegration.ShadowPullRequestResult{TargetRepo: row.ExternalRepo, Number: row.ExternalNumber, WebURL: row.ExternalURL, SourceBranch: row.SourceBranch, TargetBranch: row.TargetBranch, State: row.State}, ok, err
}

func (s *Service) closeGitHubProjectionDurably(ctx context.Context, pr db.PullRequest) (githubintegration.CloseShadowPullRequestResult, bool, error) {
	if s == nil || s.GitHubIntegration == nil {
		return githubintegration.CloseShadowPullRequestResult{}, false, fmt.Errorf("GitHub integration unavailable")
	}
	current, err := s.projectionPullRequestFact(ctx, pr.ID)
	if err != nil {
		return githubintegration.CloseShadowPullRequestResult{}, false, err
	}
	if current.State == db.StateOpen && !current.Merged {
		return githubintegration.CloseShadowPullRequestResult{}, false, fmt.Errorf("refuse close for open AGS PR: %w", ErrInvalidState)
	}
	job, err := s.enqueueGitHubProjection(ctx, current, githubProjectionClose)
	if err != nil {
		return githubintegration.CloseShadowPullRequestResult{}, false, err
	}
	err = s.runGitHubProjectionAttempt(ctx, job, "")
	s.scheduleGitHubProjection(ctx, job.ID)
	if err != nil {
		return githubintegration.CloseShadowPullRequestResult{}, false, err
	}
	row, ok, err := s.gitHubProjection(ctx, pr.ID)
	return githubintegration.CloseShadowPullRequestResult{TargetRepo: row.ExternalRepo, Number: row.ExternalNumber, WebURL: row.ExternalURL, SourceBranch: row.SourceBranch, TargetBranch: row.TargetBranch}, ok, err
}
