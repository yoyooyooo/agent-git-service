package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	providerMergeOutcomeSourceSuccess = "source_success"
	providerMergeProjectionPRPending  = "provider_pr_pending"
	providerMergeAckMergedBy          = "fast-forward-ack"
)

func (s *Service) fastForwardAckEligible(repository, mergeMethod, providerBase, expectedHead string) bool {
	return s != nil && s.ForgejoIntegration != nil &&
		strings.EqualFold(strings.TrimSpace(mergeMethod), "fast-forward-only") &&
		s.ForgejoIntegration.FastForwardAckEnabled(repository) &&
		exactGitSHA(providerBase, expectedHead)
}

func (s *Service) acknowledgeFastForwardMerge(ctx context.Context, receipt HumanProviderMergeReceipt, pr db.PullRequest, projection ProviderEvidenceBinding, sides providerMergeSides) (HumanProviderMergeReceipt, error) {
	if !exactGitSHA(sides.ProviderBase, sides.Head) {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: Forgejo target ref is not the expected head", ErrConflict)
	}
	// Hold the PR row lock while checking both Git refs and writing the terminal
	// fact. Source pushes are fenced by Git's source-ref verification; projection
	// updates and PR closure are fenced by the open/head database condition.
	if pr.HeadRepositoryID != 0 && pr.HeadRepositoryID != pr.RepositoryID {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: atomic cross-repository acknowledgement is unavailable", ErrConflict)
	}
	err := s.markAGSPRMergedKeepProviderOpen(ctx, pr.ID, sides.Head, providerMergeAckMergedBy, func() error {
		newBase := sides.AGSBase
		if !exactGitSHA(sides.AGSBase, sides.Head) {
			if ok, err := s.Git.IsAncestor(ctx, receipt.Repository, sides.AGSBase, sides.Head); err != nil {
				return err
			} else if ok {
				newBase = sides.Head
			} else if containsHead, err := s.Git.IsAncestor(ctx, receipt.Repository, sides.Head, sides.AGSBase); err != nil {
				return err
			} else if !containsHead {
				return fmt.Errorf("%w: AGS and Forgejo target refs have diverged", ErrConflict)
			}
		}
		if err := s.Git.UpdateRefCASWithHead(ctx, receipt.Repository, "refs/heads/"+pr.HeadRef, sides.Head, "refs/heads/"+sides.BaseRef, newBase, sides.AGSBase); err != nil {
			return fmt.Errorf("%w: AGS head/base transaction failed: %v", ErrConflict, err)
		}
		return nil
	})
	if err != nil {
		return HumanProviderMergeReceipt{}, err
	}
	receipt.ProviderMerged = false
	receipt.ProviderMergeSHA = sides.Head
	receipt.ProjectionStatus = providerMergeProjectionPRPending
	receipt.Outcome = providerMergeOutcomeSourceSuccess
	receipt.AGSBaseSHA = sides.AGSBase
	receipt.ProviderBaseSHA = sides.ProviderBase
	_ = projection
	return receipt, nil
}

func (s *Service) markAGSPRMergedKeepProviderOpen(ctx context.Context, pullRequestID uint, mergeSHA, mergedBy string, publish func() error) error {
	mergeSHA = strings.ToLower(strings.TrimSpace(mergeSHA))
	if pullRequestID == 0 || !canonicalActionSHA(mergeSHA) {
		return fmt.Errorf("%w: merged AGS PR requires a canonical SHA", ErrValidation)
	}
	now := time.Now().UTC()
	return s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		var locked db.PullRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, pullRequestID).Error; err != nil {
			return err
		}
		if locked.Merged {
			if !exactGitSHA(locked.HeadSHA, mergeSHA) || !exactGitSHA(locked.MergeCommitSHA, mergeSHA) {
				return fmt.Errorf("%w: terminal AGS merge SHA conflicts with expected head", ErrConflict)
			}
			return nil
		}
		if locked.State != db.StateOpen || !exactGitSHA(locked.HeadSHA, mergeSHA) {
			return fmt.Errorf("%w: AGS pull request is no longer open at expected head", ErrConflict)
		}
		if err := publish(); err != nil {
			return err
		}
		result := tx.Model(&db.PullRequest{}).
			Where("id = ? AND merged = ? AND state = ? AND head_sha = ?", pullRequestID, false, db.StateOpen, mergeSHA).
			Updates(map[string]any{
				"state":            db.StateClosed,
				"merged":           true,
				"merged_at":        &now,
				"closed_at":        &now,
				"merge_commit_sha": mergeSHA,
				"merged_by_login":  strings.TrimSpace(mergedBy),
				"updated_at":       now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("%w: AGS merge terminal fact CAS lost", ErrConflict)
		}
		return tx.Model(&db.PullRequestProjection{}).
			Where("pull_request_id = ? AND provider = ?", pullRequestID, ProjectionProviderForgejo).
			Updates(map[string]any{
				"last_synced_sha": mergeSHA,
				"updated_at":      now,
			}).Error
	})
}

// ObserveHumanProviderMerge is a read-only status surface. It never POSTs a provider merge.
func (s *Service) ObserveHumanProviderMerge(ctx context.Context, repository string, prNumber int, expectedHead string) (HumanProviderMergeReceipt, error) {
	if s == nil || s.ForgejoIntegration == nil || s.Git == nil {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: provider merge executor is unavailable", ErrInvalidState)
	}
	repository = strings.TrimSpace(repository)
	expectedHead = strings.ToLower(strings.TrimSpace(expectedHead))
	if repository == "" || prNumber <= 0 || !canonicalActionSHA(expectedHead) {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: repository, pull request, and expected head are required", ErrValidation)
	}
	actor, err := s.GetCurrentUser(ctx)
	if err != nil {
		return HumanProviderMergeReceipt{}, err
	}
	if actor.UserKind != db.UserKindHuman {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: Human provider merge observe requires a Human AGS identity", ErrForbidden)
	}
	pr, err := s.GetPR(ctx, repository, prNumber)
	if err != nil {
		return HumanProviderMergeReceipt{}, err
	}
	projection, err := s.forgejoEvidenceBinding(ctx, pr)
	if err != nil {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: authoritative provider projection is unavailable", ErrConflict)
	}
	receipt := HumanProviderMergeReceipt{
		Schema: "ags.provider-merge-receipt.v1", Repository: repository, AGSPRNumber: prNumber,
		ActorID: actor.ID, ActorLogin: actor.Login, Provider: ProjectionProviderForgejo,
		ProviderRepo: projection.ExternalRepo, ProviderPRNumber: projection.ExternalNumber,
		ExpectedHeadSHA: expectedHead,
	}
	if pr.Merged && exactGitSHA(pr.MergeCommitSHA, expectedHead) {
		receipt.ProviderMergeSHA = strings.ToLower(pr.MergeCommitSHA)
		receipt.ProjectionStatus = "pending"
		if pr.MergedByLogin == providerMergeAckMergedBy {
			receipt.Outcome = providerMergeOutcomeSourceSuccess
			receipt.ProjectionStatus = providerMergeProjectionPRPending
		} else {
			receipt.ProviderMerged = true
			receipt.Outcome = "provider_success"
		}
		return receipt, nil
	}
	snapshot, found, snapErr := s.ForgejoIntegration.ExactPullRequestSnapshot(ctx, repository, projection.ExternalRepo, projection.ExternalNumber)
	if snapErr == nil && found && snapshot.Merged {
		if mergeSHA, canonical := canonicalProviderMergeSnapshotSHA(snapshot, expectedHead); canonical {
			receipt.ProviderMerged = true
			receipt.ProviderMergeSHA = strings.ToLower(mergeSHA)
			receipt.ProjectionStatus = "pending"
			receipt.Outcome = "provider_success"
			return receipt, nil
		}
	}
	receipt.Outcome = "recovery_needed"
	return receipt, nil
}
