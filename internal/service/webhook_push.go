package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

const maxSyncPRHeadAttempts = 8

// Test-only hooks. Production keeps these nil.
var (
	testSyncPRHeadAfterTipRead              func()
	testEnqueueForgejoPullRequestProjection func(*Service, context.Context, db.PullRequest) (db.PullRequestProjectionJob, error)
)

// UpdateRepositoryPushedAt records the most recent successful push time.
func (s *Service) UpdateRepositoryPushedAt(ctx context.Context, repoID uint, pushedAt time.Time) error {
	return s.DBForCtx(ctx).
		Model(&db.Repository{}).
		Where("id = ?", repoID).
		Update("pushed_at", &pushedAt).Error
}

// ListOpenPRsByHead returns open pull requests whose head branch matches a pushed ref.
func (s *Service) ListOpenPRsByHead(ctx context.Context, headRepoID uint, headRef string) ([]db.PullRequest, error) {
	var prs []db.PullRequest
	err := preloadPRFull(s.DBForCtx(ctx)).
		Where("head_repository_id = ? AND head_ref = ? AND state = ? AND merged = ?", headRepoID, headRef, db.StateOpen, false).
		Find(&prs).Error
	return prs, err
}

// SyncOpenPRHeadsForBranch is the unified service seam after a refs/heads/*
// mutation that bypasses git-receive-pack. It refreshes every open PR whose
// head_ref equals the advanced branch and requeues durable Forgejo projection.
func (s *Service) SyncOpenPRHeadsForBranch(ctx context.Context, repoID uint, repoFullName, branch string) error {
	if s == nil {
		return nil
	}
	repoFullName = strings.TrimSpace(repoFullName)
	branch = strings.TrimSpace(strings.TrimPrefix(branch, "refs/heads/"))
	if repoID == 0 || repoFullName == "" || branch == "" {
		return nil
	}
	if s.Git == nil {
		err := fmt.Errorf("sync open PR heads: git store is not configured")
		return errors.Join(err, s.recordOpenPRHeadBranchSyncFailure(ctx, repoFullName, branch, err))
	}
	prs, err := s.ListOpenPRsByHead(ctx, repoID, branch)
	if err != nil {
		slog.WarnContext(ctx, "list open PRs after branch advance failed",
			"repo", repoFullName, "branch", branch, "error", err)
		return errors.Join(fmt.Errorf("list open PRs after branch advance: %w", err), s.recordOpenPRHeadBranchSyncFailure(ctx, repoFullName, branch, err))
	}
	var errs []error
	for _, pr := range prs {
		headRepo := repoFullName
		if name := strings.TrimSpace(pr.HeadRepository.FullName); name != "" {
			headRepo = name
		}
		if _, err := s.SyncPRHeadAfterPush(ctx, pr.ID, headRepo); err != nil {
			slog.WarnContext(ctx, "sync open PR head after branch advance failed",
				"repo", repoFullName, "branch", branch, "pr", pr.Number, "error", err)
			errs = append(errs, fmt.Errorf("PR #%d: %w", pr.Number, err))
		}
	}
	return errors.Join(errs...)
}

// SyncPRHeadAfterPush refreshes PR head metadata after a git push to its head
// branch and requeues Forgejo projection so last_synced_sha can converge.
// The UPDATE is CAS-protected against a stale tip overwriting a newer head.
func (s *Service) SyncPRHeadAfterPush(ctx context.Context, prID uint, headRepoFullName string) (db.PullRequest, error) {
	headRepoFullName = strings.TrimSpace(headRepoFullName)
	if s == nil {
		return db.PullRequest{}, fmt.Errorf("sync PR head: service is nil")
	}
	if prID == 0 {
		return db.PullRequest{}, fmt.Errorf("sync PR head: missing pull request id")
	}
	if headRepoFullName == "" {
		return db.PullRequest{}, fmt.Errorf("sync PR head: empty head repository")
	}
	if s.Git == nil {
		return db.PullRequest{}, fmt.Errorf("sync PR head: git store is not configured")
	}

	var last db.PullRequest
	for attempt := 0; attempt < maxSyncPRHeadAttempts; attempt++ {
		pr, err := s.GetPRByID(ctx, prID)
		if err != nil {
			return db.PullRequest{}, err
		}
		last = pr
		if pr.State != db.StateOpen || pr.Merged {
			return pr, nil
		}
		sha, err := s.Git.HeadSHA(ctx, headRepoFullName, pr.HeadRef)
		if err != nil {
			err = fmt.Errorf("read head SHA for %s %s: %w", headRepoFullName, pr.HeadRef, err)
			return pr, errors.Join(err, s.recordOpenPRHeadSyncDegraded(ctx, pr, err))
		}
		sha = strings.TrimSpace(sha)
		if sha == "" {
			err = fmt.Errorf("read head SHA for %s %s: empty", headRepoFullName, pr.HeadRef)
			return pr, errors.Join(err, s.recordOpenPRHeadSyncDegraded(ctx, pr, err))
		}
		if hook := testSyncPRHeadAfterTipRead; hook != nil {
			hook()
		}
		if strings.EqualFold(strings.TrimSpace(pr.HeadSHA), sha) {
			return s.completeOpenPRHeadSync(ctx, pr, headRepoFullName, false)
		}
		res := s.DBForCtx(ctx).Model(&db.PullRequest{}).
			Where("id = ? AND head_sha = ? AND state = ? AND merged = ?", pr.ID, pr.HeadSHA, db.StateOpen, false).
			Update("head_sha", sha)
		if res.Error != nil {
			return pr, res.Error
		}
		if res.RowsAffected == 0 {
			continue
		}
		pr.HeadSHA = sha
		return s.completeOpenPRHeadSync(ctx, pr, headRepoFullName, true)
	}
	err := fmt.Errorf("sync PR head: CAS retries exhausted for PR %d", prID)
	return last, errors.Join(err, s.recordOpenPRHeadSyncDegraded(ctx, last, err))
}

func (s *Service) completeOpenPRHeadSync(ctx context.Context, pr db.PullRequest, headRepoFullName string, wroteHead bool) (db.PullRequest, error) {
	if wroteHead {
		baseName := strings.TrimSpace(pr.Repository.FullName)
		if baseName == "" {
			baseName = headRepoFullName
		}
		if err := s.updatePRCommitDataFromLoaded(ctx, baseName, headRepoFullName, pr); err != nil {
			return pr, err
		}
	}
	updated, err := s.GetPRByID(ctx, pr.ID)
	if err != nil {
		return pr, err
	}
	if _, enqueueErr := enqueueForgejoProjectionAfterHeadSync(s, ctx, updated); enqueueErr != nil {
		if errors.Is(enqueueErr, ErrInvalidState) {
			// An action-rebase (or other non-generic) job already owns this PR.
			return updated, nil
		}
		slog.WarnContext(ctx, "enqueue Forgejo projection after head push failed",
			"repo", headRepoFullName, "pr", updated.Number, "head_sha", updated.HeadSHA, "error", enqueueErr)
		return updated, errors.Join(fmt.Errorf("enqueue Forgejo projection after head sync: %w", enqueueErr), s.recordOpenPRHeadSyncDegraded(ctx, updated, enqueueErr))
	}
	return updated, nil
}

func enqueueForgejoProjectionAfterHeadSync(s *Service, ctx context.Context, pr db.PullRequest) (db.PullRequestProjectionJob, error) {
	if hook := testEnqueueForgejoPullRequestProjection; hook != nil {
		return hook(s, ctx, pr)
	}
	return s.EnqueueForgejoPullRequestProjection(ctx, pr)
}

func (s *Service) recordOpenPRHeadSyncDegraded(ctx context.Context, pr db.PullRequest, cause error) error {
	if s == nil || pr.ID == 0 || cause == nil {
		return nil
	}
	repoName := strings.TrimSpace(pr.Repository.FullName)
	if repoName == "" {
		return nil
	}
	return s.recordPullRequestIntegrityFailure(ctx, repoName, pullRequestProjectionIntegrityRef(pr.Number), pr,
		forgejointegration.ProjectionFailureSHADrift, pr.HeadSHA, "",
		fmt.Sprintf("open PR head sync degraded: %v", cause), time.Now().UTC())
}

func (s *Service) recordOpenPRHeadBranchSyncFailure(ctx context.Context, repoFullName, branch string, cause error) error {
	if s == nil || strings.TrimSpace(repoFullName) == "" || cause == nil {
		return nil
	}
	return s.recordProjectionFailure(ctx, projectionFailureRecord{
		Provider:     ProjectionProviderForgejo,
		RepoFullName: repoFullName,
		Ref:          "refs/heads/" + strings.TrimSpace(branch),
		Branch:       strings.TrimSpace(branch),
		Type:         forgejointegration.ProjectionFailureUnknown,
		Authority:    ProjectionAuthorityAGS,
		ErrorSummary: fmt.Sprintf("refresh open PR heads after %s:%s advanced failed: %v", repoFullName, branch, cause),
		OccurredAt:   time.Now().UTC(),
	})
}
