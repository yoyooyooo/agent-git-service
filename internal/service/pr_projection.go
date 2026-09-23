package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ProjectionProviderForgejo = "forgejo"
	ProjectionProviderGitLab  = "gitlab"
	ProjectionProviderGitHub  = "github"
	ProjectionStateOpen       = "open"
	ProjectionStateClosed     = "closed"
	ProjectionStateMerged     = "merged"
)

// UpsertPullRequestProjection records an external projection for an AGS PR.
func (s *Service) UpsertPullRequestProjection(ctx context.Context, in db.PullRequestProjection) error {
	if in.PullRequestID == 0 || in.RepositoryID == 0 {
		return fmt.Errorf("projection requires pull request and repository IDs")
	}
	in.Provider = strings.TrimSpace(strings.ToLower(in.Provider))
	in.ExternalRepo = strings.TrimSpace(in.ExternalRepo)
	if in.Provider == "" || in.ExternalRepo == "" || in.ExternalNumber == 0 {
		return fmt.Errorf("projection requires provider, external repo, and external number")
	}
	if strings.TrimSpace(in.State) == "" {
		in.State = ProjectionStateOpen
	}
	return s.DBForCtx(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "pull_request_id"}, {Name: "provider"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"external_repo",
			"external_number",
			"external_url",
			"source_branch",
			"target_branch",
			"state",
			"last_synced_sha",
			"updated_at",
		}),
	}).Create(&in).Error
}

// ListPullRequestProjections returns authoritative external projection mappings for one AGS PR.
func (s *Service) ListPullRequestProjections(ctx context.Context, pullRequestID uint) ([]db.PullRequestProjection, error) {
	if pullRequestID == 0 {
		return nil, fmt.Errorf("projection list requires pull request ID")
	}
	var rows []db.PullRequestProjection
	if err := s.DBForCtx(ctx).Where("pull_request_id = ?", pullRequestID).Order("provider ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ListPullRequestProjectionsBatch returns authoritative external projection
// mappings keyed by AGS pull request ID. Missing IDs yield an empty slice.
func (s *Service) ListPullRequestProjectionsBatch(ctx context.Context, pullRequestIDs []uint) (map[uint][]db.PullRequestProjection, error) {
	out := make(map[uint][]db.PullRequestProjection, len(pullRequestIDs))
	ids := make([]uint, 0, len(pullRequestIDs))
	seen := make(map[uint]struct{}, len(pullRequestIDs))
	for _, id := range pullRequestIDs {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		out[id] = []db.PullRequestProjection{}
	}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []db.PullRequestProjection
	if err := s.DBForCtx(ctx).Where("pull_request_id IN ?", ids).Order("pull_request_id ASC, provider ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.PullRequestID] = append(out[row.PullRequestID], row)
	}
	return out, nil
}

// PullRequestProjectionJob returns the durable Forgejo projection state for one
// AGS PR. It is a presentation read only: creation remains AGS-authoritative and
// the asynchronous worker remains the sole owner of provider convergence.
func (s *Service) PullRequestProjectionJob(ctx context.Context, pullRequestID uint) (ProjectionJobStatus, bool, error) {
	if pullRequestID == 0 {
		return ProjectionJobStatus{}, false, fmt.Errorf("projection job requires pull request ID")
	}
	var job db.PullRequestProjectionJob
	err := s.DBForCtx(ctx).
		Preload("Attempts").
		Where("pull_request_id = ? AND provider = ?", pullRequestID, ProjectionProviderForgejo).
		Where(clause.Eq{Column: "trigger", Value: ForgejoProjectionTriggerPullRequest}).
		First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ProjectionJobStatus{}, false, nil
	}
	if err != nil {
		return ProjectionJobStatus{}, false, err
	}
	return projectionJobStatus(job), true, nil
}

// FindPullRequestByProjection resolves an external Forgejo/GitLab PR/MR back to the AGS PR.
func (s *Service) FindPullRequestByProjection(ctx context.Context, provider, externalRepo string, externalNumber int) (db.PullRequest, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	externalRepo = strings.TrimSpace(externalRepo)
	if provider == "" || externalRepo == "" || externalNumber == 0 {
		return db.PullRequest{}, fmt.Errorf("projection lookup requires provider, external repo, and external number")
	}
	var projection db.PullRequestProjection
	if err := s.DBForCtx(ctx).
		Preload("PullRequest").
		Preload("PullRequest.Repository").
		Preload("PullRequest.HeadRepository").
		Preload("PullRequest.Author").
		Where("provider = ? AND external_repo = ? AND external_number = ?", provider, externalRepo, externalNumber).
		First(&projection).Error; err != nil {
		return db.PullRequest{}, wrapErr(err)
	}
	return projection.PullRequest, nil
}

// MarkPullRequestClosedFromProjection records a Forgejo-authoritative unmerged close into the AGS PR fact.
func (s *Service) MarkPullRequestClosedFromProjection(ctx context.Context, provider, externalRepo string, externalNumber int, closedBy string) (db.PullRequest, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	externalRepo = strings.TrimSpace(externalRepo)
	if provider == "" || externalRepo == "" || externalNumber == 0 {
		return db.PullRequest{}, fmt.Errorf("projection lookup requires provider, external repo, and external number")
	}

	// Resolve, lock, re-read, and enqueue in one transaction. Both projection
	// terminal paths use projection -> PR ordering; this prevents a close from
	// racing a merge with an old PR snapshot or waiting in the opposite order.
	var lockedPR db.PullRequest
	var terminalMerged bool
	var transactionErr error
	for attempt := 0; attempt < 3; attempt++ {
		lockedPR = db.PullRequest{}
		terminalMerged = false
		transactionErr = s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
			var projection db.PullRequestProjection
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("provider = ? AND external_repo = ? AND external_number = ?", provider, externalRepo, externalNumber).
				First(&projection).Error; err != nil {
				return wrapErr(err)
			}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Preload("Repository").
				Preload("HeadRepository").
				Preload("Author").
				First(&lockedPR, projection.PullRequestID).Error; err != nil {
				return wrapErr(err)
			}
			if lockedPR.Merged {
				// A close webhook can be delivered after the provider's merge
				// webhook. The current merged fact is authoritative; do not add a
				// second closed-unmerged delivery row.
				terminalMerged = true
				return nil
			}

			observedAt := time.Now().UTC()
			if lockedPR.ClosedAt != nil && !lockedPR.ClosedAt.IsZero() {
				// Repeated close facts must reuse the first terminal timestamp so
				// the deterministic key has identical payload bytes.
				observedAt = lockedPR.ClosedAt.UTC()
			}
			if lockedPR.State != db.StateClosed || lockedPR.ClosedAt == nil || lockedPR.ClosedAt.IsZero() {
				result := tx.Model(&db.PullRequest{}).
					Where("id = ? AND merged = ?", lockedPR.ID, false).
					Updates(map[string]any{
						"state":      db.StateClosed,
						"closed_at":  &observedAt,
						"updated_at": observedAt,
					})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return fmt.Errorf("closed Forgejo projection terminal fact CAS lost")
				}
				lockedPR.State = db.StateClosed
				lockedPR.ClosedAt = &observedAt
				lockedPR.UpdatedAt = observedAt
			}
			if projection.State != ProjectionStateClosed {
				if err := tx.Model(&db.PullRequestProjection{}).
					Where("id = ? AND pull_request_id = ?", projection.ID, lockedPR.ID).
					Updates(map[string]any{
						"state":      ProjectionStateClosed,
						"updated_at": observedAt,
					}).Error; err != nil {
					return err
				}
			}
			// This also repairs a missing row when a repeated Forgejo close webhook
			// is the first event observed after an older AGS process crashed.
			return s.enqueueMulticaExternalPRTerminalDeliveryTx(ctx, tx, lockedPR, provider, externalRepo, externalNumber, ProjectionStateClosed, false, "", observedAt)
		})
		if transactionErr == nil || !isTransientDBTransactionErr(transactionErr) || attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return db.PullRequest{}, ctx.Err()
		case <-time.After(retryDelay(attempt)):
		}
	}
	if transactionErr != nil {
		return db.PullRequest{}, transactionErr
	}
	if terminalMerged || lockedPR.Merged {
		return lockedPR, nil
	}
	closed, err := s.GetPR(ctx, lockedPR.Repository.FullName, lockedPR.Number)
	if err != nil {
		return db.PullRequest{}, err
	}
	_ = strings.TrimSpace(closedBy)
	return closed, nil
}

// MarkPullRequestMergedFromProjection records a Forgejo-authoritative merge into the AGS PR fact.
func (s *Service) MarkPullRequestMergedFromProjection(ctx context.Context, provider, externalRepo string, externalNumber int, mergeSHA, mergedBy string) (db.PullRequest, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	externalRepo = strings.TrimSpace(externalRepo)
	if provider == "" || externalRepo == "" || externalNumber == 0 {
		return db.PullRequest{}, fmt.Errorf("projection lookup requires provider, external repo, and external number")
	}
	mergeSHA = strings.TrimSpace(mergeSHA)
	if !canonicalActionSHA(mergeSHA) {
		return db.PullRequest{}, fmt.Errorf("merged Forgejo projection requires a canonical full merge SHA")
	}

	// The PR row is the immutable terminal-fact mutex. Resolve the projection,
	// lock and re-read the current PR, then decide whether this is the first
	// merge or a same-SHA repair while still inside one transaction. The
	// merged=false predicate is a second CAS guard for dialects where row locks
	// are advisory or unavailable.
	var lockedPR db.PullRequest
	var transactionErr error
	for attempt := 0; attempt < 3; attempt++ {
		lockedPR = db.PullRequest{}
		transactionErr = s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
			var projection db.PullRequestProjection
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("provider = ? AND external_repo = ? AND external_number = ?", provider, externalRepo, externalNumber).
				First(&projection).Error; err != nil {
				return wrapErr(err)
			}
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Preload("Repository").
				Preload("HeadRepository").
				Preload("Author").
				First(&lockedPR, projection.PullRequestID).Error; err != nil {
				return wrapErr(err)
			}
			if lockedPR.Merged {
				if !strings.EqualFold(lockedPR.MergeCommitSHA, mergeSHA) {
					return fmt.Errorf("merged Forgejo projection conflicts with existing merge SHA; terminal fact is immutable")
				}
				// A repeated provider webhook may be the first opportunity to repair a
				// delivery row left behind by an older AGS process crashing. Reuse the
				// same transaction boundary without rewriting the terminal PR fact.
				observedAt := time.Now().UTC()
				if lockedPR.MergedAt != nil && !lockedPR.MergedAt.IsZero() {
					observedAt = lockedPR.MergedAt.UTC()
				}
				return s.enqueueMulticaExternalPRTerminalDeliveryTx(ctx, tx, lockedPR, provider, externalRepo, externalNumber, ProjectionStateMerged, true, mergeSHA, observedAt)
			}

			now := time.Now().UTC()
			result := tx.Model(&db.PullRequest{}).
				Where("id = ? AND merged = ?", lockedPR.ID, false).
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
				return fmt.Errorf("merged Forgejo projection terminal fact CAS lost")
			}
			if err := tx.Model(&db.PullRequestProjection{}).
				Where("pull_request_id = ?", lockedPR.ID).
				Updates(map[string]any{
					"state":           ProjectionStateClosed,
					"last_synced_sha": mergeSHA,
					"updated_at":      now,
				}).Error; err != nil {
				return err
			}
			return s.enqueueMulticaExternalPRTerminalDeliveryTx(ctx, tx, lockedPR, provider, externalRepo, externalNumber, ProjectionStateMerged, true, mergeSHA, now)
		})
		if transactionErr == nil || !isTransientDBTransactionErr(transactionErr) || attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return db.PullRequest{}, ctx.Err()
		case <-time.After(retryDelay(attempt)):
		}
	}
	if transactionErr != nil {
		return db.PullRequest{}, transactionErr
	}
	if lockedPR.Merged {
		// The locked row is the authoritative read for duplicate same-SHA
		// repairs; no second provider notification is emitted.
		if strings.EqualFold(lockedPR.MergeCommitSHA, mergeSHA) {
			return lockedPR, nil
		}
	}
	merged, err := s.GetPR(ctx, lockedPR.Repository.FullName, lockedPR.Number)
	if err != nil {
		return db.PullRequest{}, err
	}
	s.emitPullRequestMergedNotification(ctx, merged, "projection:"+provider, "")
	return merged, nil
}
