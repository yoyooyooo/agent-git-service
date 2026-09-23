package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

// ForgejoRefChange describes a pushed ref transition for optional Forgejo integration.
type ForgejoRefChange struct {
	Ref     string
	Before  string
	After   string
	Created bool
	Deleted bool
	Forced  bool
}

// DispatchForgejoIntegration mirrors AGS post-push changes to Forgejo when configured.
func (s *Service) DispatchForgejoIntegration(ctx context.Context, repoFullName, repoPath string, changes []ForgejoRefChange) error {
	if s == nil || s.ForgejoIntegration == nil || len(changes) == 0 {
		return nil
	}
	beforeWrite, err := s.delegatedGitPushBeforeWrite(ctx, repoFullName)
	if err != nil {
		_ = s.RecordForgejoProjectionFailure(ctx, repoFullName, changes, err)
		return err
	}
	event := forgejointegration.PushEvent{
		RepoFullName: repoFullName,
		RepoPath:     repoPath,
		Changes:      make([]forgejointegration.RefChange, 0, len(changes)),
		BeforeWrite:  beforeWrite,
	}
	for _, change := range changes {
		event.Changes = append(event.Changes, forgejointegration.RefChange{
			Ref:     change.Ref,
			Before:  change.Before,
			After:   change.After,
			Created: change.Created,
			Deleted: change.Deleted,
			Forced:  change.Forced,
		})
	}
	result, err := s.ForgejoIntegration.HandlePushWithResult(ctx, event)
	if err != nil {
		_ = s.RecordForgejoProjectionFailure(ctx, repoFullName, changes, err)
		return err
	}
	s.ResolveForgejoProjectionSuccess(ctx, repoFullName, changes)
	if err := s.bindForgejoAutoPullRequests(ctx, result.AutoPullRequests); err != nil {
		return err
	}
	return nil
}

func (s *Service) delegatedGitPushBeforeWrite(ctx context.Context, repoFullName string) (func(context.Context) error, error) {
	if _, present := DelegatedSessionFromContext(ctx); !present {
		return nil, nil
	}
	var repo db.Repository
	if err := s.DBForCtx(ctx).Select("id", "full_name").Where("LOWER(full_name) = LOWER(?)", strings.TrimSpace(repoFullName)).First(&repo).Error; err != nil {
		return nil, err
	}
	return func(checkCtx context.Context) error {
		_, err := s.RevalidateDelegatedSession(checkCtx, repo.ID, "git.push", "repo:write", map[string]string{})
		return err
	}, nil
}

func (s *Service) bindForgejoAutoPullRequests(ctx context.Context, rows []forgejointegration.AutoPullRequestResult) error {
	if s == nil || len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		repoFullName := strings.TrimSpace(row.RepoFullName)
		head := strings.TrimSpace(row.Head)
		base := strings.TrimSpace(row.Base)
		if repoFullName == "" || head == "" || row.Number == 0 || row.ExternalRepo == "" {
			continue
		}
		repo, err := s.GetRepo(ctx, repoFullName)
		if err != nil {
			return fmt.Errorf("lookup repo for Forgejo auto PR binding: %w", err)
		}
		prs, err := s.ListOpenPRsByHead(ctx, repo.ID, head)
		if err != nil {
			return fmt.Errorf("lookup AGS PR for Forgejo auto PR binding: %w", err)
		}
		var matched db.PullRequest
		for _, pr := range prs {
			if strings.TrimSpace(pr.BaseRef) == base {
				matched = pr
				break
			}
		}
		if matched.ID == 0 {
			slog.InfoContext(ctx, "Forgejo auto PR has no AGS PR yet; merge webhook fallback will sync base if merged", "repo", repoFullName, "head", head, "base", base, "forgejo_pr", row.Number)
			continue
		}
		if err := s.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
			PullRequestID:  matched.ID,
			RepositoryID:   matched.RepositoryID,
			Provider:       ProjectionProviderForgejo,
			ExternalRepo:   row.ExternalRepo,
			ExternalNumber: row.Number,
			ExternalURL:    row.URL,
			SourceBranch:   head,
			TargetBranch:   base,
			State:          ProjectionStateOpen,
			LastSyncedSHA:  row.HeadSHA,
		}); err != nil {
			return fmt.Errorf("record Forgejo auto PR projection: %w", err)
		}
		slog.InfoContext(ctx, "bound Forgejo auto PR to existing AGS PR", "repo", repoFullName, "head", head, "base", base, "ags_pr", matched.Number, "forgejo_pr", row.Number)
	}
	return nil
}
