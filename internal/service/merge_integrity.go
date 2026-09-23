package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

const MergeIntegritySupersededLabel = "ags/integrity-superseded"

func mergedPullRequestIntegrityRef(number int) string {
	return fmt.Sprintf("refs/pull/%d/merge-integrity", number)
}

func hasMergeIntegritySupersessionMarker(body string) bool {
	return strings.Contains(body, "[AGS-INTEGRITY-SUPERSEDED]") || strings.Contains(body, "Merge-Integrity-Superseded-By:")
}

func pullRequestHasAuditedSupersession(pr db.PullRequest) bool {
	hasLabel := false
	for _, label := range pr.Labels {
		if strings.EqualFold(strings.TrimSpace(label.Name), MergeIntegritySupersededLabel) {
			hasLabel = true
			break
		}
	}
	return hasLabel && hasMergeIntegritySupersessionMarker(string(pr.Body))
}

// scanMergedPullRequestIntegrity proves that every AGS pull request recorded as
// merged still contributes its merge commit to the current target branch. A
// non-fast-forward rewrite of the authority line therefore becomes a durable,
// alertable projection failure instead of silent history loss.
func (s *Service) scanMergedPullRequestIntegrity(ctx context.Context, repoFullName string) (int, error) {
	if s == nil || s.Git == nil {
		return 0, nil
	}
	repo, err := s.projectionRepository(ctx, repoFullName)
	if err != nil {
		return 0, err
	}
	var prs []db.PullRequest
	if err := s.DBForCtx(ctx).
		Preload("Labels").
		Where("pull_requests.repository_id = ? AND pull_requests.merged = ?", repo.ID, true).
		Where("pull_requests.merge_commit_sha <> ''").
		Order("pull_requests.number").
		Find(&prs).Error; err != nil {
		return 0, fmt.Errorf("list merged pull requests for integrity scan: %w", err)
	}

	baseSHAs := map[string]string{}
	baseErrors := map[string]error{}
	recorded := 0
	now := time.Now().UTC()
	for _, pr := range prs {
		ref := mergedPullRequestIntegrityRef(pr.Number)
		mergeSHA := strings.TrimSpace(pr.MergeCommitSHA)
		if pullRequestHasAuditedSupersession(pr) {
			if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, ref, "", mergeSHA, now); err != nil {
				return recorded, err
			}
			continue
		}

		base := strings.TrimSpace(pr.BaseRef)
		if base == "" {
			base = strings.TrimSpace(repo.DefaultBranch)
		}
		baseSHA, ok := baseSHAs[base]
		baseErr := baseErrors[base]
		if !ok && baseErr == nil {
			baseSHA, baseErr = s.Git.HeadSHA(ctx, repoFullName, base)
			if baseErr != nil {
				baseErrors[base] = baseErr
			} else {
				baseSHAs[base] = baseSHA
			}
		}
		if baseErr != nil {
			summary := fmt.Sprintf("AGS PR #%d is recorded merged at %s but base branch %s cannot be resolved: %v", pr.Number, mergeSHA, base, baseErr)
			if err := s.recordProjectionFailure(ctx, projectionFailureRecord{
				Provider:     ProjectionProviderForgejo,
				RepoFullName: repoFullName,
				Ref:          ref,
				Branch:       base,
				Type:         forgejointegration.ProjectionFailureMergedContentMissing,
				Authority:    ProjectionAuthorityAGS,
				ExternalSHA:  mergeSHA,
				ErrorSummary: summary,
				OccurredAt:   now,
			}); err != nil {
				return recorded, err
			}
			recorded++
			continue
		}
		ancestor, ancestorErr := s.Git.IsAncestor(ctx, repoFullName, mergeSHA, baseSHA)
		if ancestorErr == nil && ancestor {
			if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, ref, baseSHA, mergeSHA, now); err != nil {
				return recorded, err
			}
			continue
		}
		summary := fmt.Sprintf("AGS PR #%d is recorded merged at %s but merge commit %s is not reachable from %s (%s)", pr.Number, mergeSHA, mergeSHA, base, baseSHA)
		if ancestorErr != nil {
			summary = fmt.Sprintf("%s: %v", summary, ancestorErr)
		}
		if err := s.recordProjectionFailure(ctx, projectionFailureRecord{
			Provider:     ProjectionProviderForgejo,
			RepoFullName: repoFullName,
			Ref:          ref,
			Branch:       base,
			Type:         forgejointegration.ProjectionFailureMergedContentMissing,
			Authority:    ProjectionAuthorityAGS,
			AGSSHA:       baseSHA,
			ExternalSHA:  mergeSHA,
			ErrorSummary: summary,
			OccurredAt:   now,
		}); err != nil {
			return recorded, err
		}
		recorded++
	}
	return recorded, nil
}
