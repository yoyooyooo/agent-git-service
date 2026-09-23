package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

func canonicalProviderMergeResult(result forgejointegration.PullRequestMergeResult, mergeErr error, expectedHeadSHA string) (string, bool) {
	if mergeErr != nil || !result.Snapshot.Merged || !exactGitSHA(result.Snapshot.HeadSHA, expectedHeadSHA) ||
		(result.ProviderOutcome != "confirmed" && result.ProviderOutcome != "reconciled_after_error") {
		return "", false
	}
	return canonicalProviderMergeSnapshotSHA(result.Snapshot, expectedHeadSHA)
}

func canonicalProviderMergeSnapshotSHA(snapshot forgejointegration.PullRequestSnapshot, expectedHeadSHA string) (string, bool) {
	if canonicalActionSHA(snapshot.MergeCommitSHA) {
		return snapshot.MergeCommitSHA, true
	}
	if snapshot.BaseSHA == expectedHeadSHA && canonicalActionSHA(snapshot.BaseSHA) {
		return snapshot.BaseSHA, true
	}
	return "", false
}

type providerMergeSides struct {
	BaseRef      string
	Head         string
	AGSBase      string
	ProviderBase string
}

func (s *Service) loadProviderMergeSides(ctx context.Context, repository, baseRef, head string) (providerMergeSides, error) {
	sides := providerMergeSides{BaseRef: strings.TrimSpace(baseRef), Head: strings.ToLower(strings.TrimSpace(head))}
	agsBase, err := s.Git.HeadSHA(ctx, repository, sides.BaseRef)
	if err != nil || !canonicalActionSHA(agsBase) {
		return providerMergeSides{}, fmt.Errorf("%w: current AGS base is unavailable", ErrConflict)
	}
	sides.AGSBase = strings.ToLower(strings.TrimSpace(agsBase))
	repoPath, err := s.Git.GetRepoPath(ctx, repository)
	if err != nil {
		return providerMergeSides{}, fmt.Errorf("%w: AGS repo path is unavailable", ErrConflict)
	}
	providerBase, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, repository, repoPath, sides.BaseRef)
	if err != nil || !checked || !canonicalActionSHA(providerBase) {
		return providerMergeSides{}, fmt.Errorf("%w: current Forgejo base is unavailable", ErrConflict)
	}
	sides.ProviderBase = strings.ToLower(strings.TrimSpace(providerBase))
	return sides, nil
}

func (s *Service) providerMergePreflight(ctx context.Context, repository, externalRepository string, forgejoPRNumber int, expectedHeadSHA, expectedBaseRef, agsBaseSHA, providerBaseSHA string) (forgejointegration.PullRequestSnapshot, error) {
	const attempts = 12
	const delay = 100 * time.Millisecond
	var snapshot forgejointegration.PullRequestSnapshot
	for attempt := 0; attempt < attempts; attempt++ {
		observed, found, err := s.ForgejoIntegration.ExactPullRequestSnapshot(ctx, repository, externalRepository, forgejoPRNumber)
		if err != nil || !found {
			return forgejointegration.PullRequestSnapshot{}, fmt.Errorf("%w: exact Forgejo pull request is unavailable", ErrConflict)
		}
		snapshot = observed
		if snapshot.State != "open" || snapshot.Merged {
			return snapshot, fmt.Errorf("%w: Forgejo pull request is not open and mergeable", ErrInvalidState)
		}
		if !exactGitSHA(snapshot.HeadSHA, expectedHeadSHA) || snapshot.BaseRef != expectedBaseRef || !exactGitSHA(snapshot.BaseSHA, providerBaseSHA) {
			return snapshot, fmt.Errorf("%w: Forgejo pull request head/base drift ags_base=%s provider_ref=%s pr_base=%s pr_head=%s expected_head=%s",
				ErrConflict, strings.TrimSpace(agsBaseSHA), strings.TrimSpace(providerBaseSHA), strings.TrimSpace(snapshot.BaseSHA), strings.TrimSpace(snapshot.HeadSHA), strings.TrimSpace(expectedHeadSHA))
		}
		if snapshot.Mergeable {
			return snapshot, nil
		}
		if attempt+1 < attempts {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return snapshot, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return snapshot, fmt.Errorf("%w: Forgejo pull request is not open and mergeable", ErrInvalidState)
}
