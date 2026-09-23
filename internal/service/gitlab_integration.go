package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
)

// DispatchGitLabIntegration mirrors AGS post-push branch changes to GitLab when configured.
func (s *Service) DispatchGitLabIntegration(ctx context.Context, repoFullName, repoPath string, changes []ForgejoRefChange) error {
	if s == nil || s.GitLabIntegration == nil || len(changes) == 0 {
		return nil
	}
	beforeWrite, err := s.delegatedGitPushBeforeWrite(ctx, repoFullName)
	if err != nil {
		for _, change := range changes {
			_ = s.RecordGitLabProjectionFailure(ctx, repoFullName, change, err)
		}
		return err
	}
	var firstErr error
	for _, change := range changes {
		res, handled, err := s.GitLabIntegration.PushRef(ctx, gitlabintegration.PushRefRequest{
			RepoFullName: repoFullName,
			RepoPath:     repoPath,
			Ref:          change.Ref,
			SourceSHA:    change.After,
			Forced:       change.Forced,
			Deleted:      change.Deleted,
			BeforeWrite:  beforeWrite,
		})
		if err != nil {
			_ = s.RecordGitLabProjectionFailure(ctx, repoFullName, change, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if handled {
			_ = s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderGitLab, change.Ref, change.After, change.After, time.Now().UTC())
			ref := strings.TrimSpace(change.Ref)
			if !change.Deleted && change.After != projectionZeroSHA && strings.HasPrefix(ref, "refs/heads/") {
				// Same bookkeeping as Forgejo: after a confirmed same-ref PushRef,
				// open GitLab PR projections for that head must track the new SHA.
				s.advanceOpenPRProjectionAfterBranchPush(ctx, repoFullName, ProjectionProviderGitLab, strings.TrimPrefix(ref, "refs/heads/"), change.After)
			}
			slog.InfoContext(ctx, "gitlab branch projection pushed", "repo", repoFullName, "project", res.ProjectPath, "branch", res.Branch, "sha", res.PushedSHA, "deleted", res.Deleted)
		}
	}
	return firstErr
}

// RecordGitLabProjectionFailure stores a GitLab mirror failure in the shared projection state table.
func (s *Service) RecordGitLabProjectionFailure(ctx context.Context, repoFullName string, change ForgejoRefChange, err error) error {
	if s == nil || err == nil {
		return nil
	}
	classified := forgejointegration.ClassifyProjectionError(change.Ref, change.After, err)
	failureType := firstNonEmpty(classified.Type, forgejointegration.ProjectionFailureUnknown)
	summary := classified.ErrorSummary
	if errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		failureType = ProjectionFailureProviderAdmissionDenied
		summary = DelegatedSessionDenialReason(err)
	}
	failure := projectionFailureRecord{
		Provider:     ProjectionProviderGitLab,
		RepoFullName: repoFullName,
		Ref:          change.Ref,
		Branch:       branchNameFromRef(change.Ref),
		Type:         failureType,
		Authority:    ProjectionAuthorityAGS,
		AGSSHA:       change.After,
		ErrorSummary: summary,
		OccurredAt:   time.Now().UTC(),
	}
	failure.TargetRepo = gitLabProjectionTargetRepo(s.GitLabIntegration, repoFullName)
	if failure.ErrorSummary == "" {
		failure.ErrorSummary = err.Error()
	}
	return s.recordProjectionFailure(ctx, failure)
}

func gitLabProjectionTargetRepo(integration *gitlabintegration.Integration, repoFullName string) string {
	if integration == nil {
		return ""
	}
	project, ok := integration.ProjectPathForRepo(repoFullName)
	if !ok {
		return ""
	}
	return project
}

// recordGitLabShadowProjectionClosed upserts the GitLab PR projection as closed
// with the Forgejo-authoritative merged SHA. GitLab is never merge authority:
// even if GitLab already marked the MR merged by ancestry, AGS records closed.
func (s *Service) recordGitLabShadowProjectionClosed(ctx context.Context, pr db.PullRequest, result gitlabintegration.CloseShadowMergeRequestResult, mergedSHA string) error {
	if s == nil || pr.ID == 0 {
		return nil
	}
	existing, ok := s.projectionForPullRequest(ctx, pr.ID, ProjectionProviderGitLab)
	externalRepo := firstNonEmpty(result.ProjectPath, existing.ExternalRepo)
	externalNumber := result.IID
	if externalNumber == 0 {
		externalNumber = existing.ExternalNumber
	}
	if !ok && (strings.TrimSpace(externalRepo) == "" || externalNumber == 0) {
		return nil
	}
	repositoryID := pr.RepositoryID
	if repositoryID == 0 {
		repositoryID = existing.RepositoryID
	}
	return s.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID:  pr.ID,
		RepositoryID:   repositoryID,
		Provider:       ProjectionProviderGitLab,
		ExternalRepo:   externalRepo,
		ExternalNumber: externalNumber,
		ExternalURL:    firstNonEmpty(result.WebURL, existing.ExternalURL),
		SourceBranch:   firstNonEmpty(result.SourceBranch, existing.SourceBranch, pr.HeadRef),
		TargetBranch:   firstNonEmpty(result.TargetBranch, existing.TargetBranch, pr.BaseRef),
		State:          ProjectionStateClosed,
		LastSyncedSHA:  firstNonEmpty(mergedSHA, existing.LastSyncedSHA),
	})
}
