package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrStaleProjectionClose is a successful no-op at the webhook boundary. It is
// separate from provider/readback errors, which must remain retryable failures.
var ErrStaleProjectionClose = errors.New("stale projection close lifecycle")

const forgejoProjectionRetiredClosed = "projection job retired because the authoritative AGS pull request is closed"

func providerLifecycleTime(raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.IsZero() {
		return time.Time{}, fmt.Errorf("Forgejo lifecycle requires a valid updated_at")
	}
	// SQL timestamp precision is microseconds on the supported database dialects.
	// Do not manufacture order from precision the durable watermark cannot retain.
	return t.UTC().Truncate(time.Microsecond), nil
}

// readProjectionClose captures the generation BEFORE provider I/O. The close
// transaction must re-read it under the PR lock; a readback before reopen can
// never authorize a write after reopen. Direct legacy callers without an event
// revision retain repair semantics only until the first native reopen.
func (s *Service) readProjectionClose(ctx context.Context, provider, externalRepo string, number int, revisions []string) (db.PullRequest, *time.Time, error) {
	pr, err := s.FindPullRequestByProjection(ctx, provider, externalRepo, number)
	if err != nil {
		return pr, nil, err
	}
	if pr.Merged {
		return pr, nil, nil
	}
	if len(revisions) == 0 && pr.LifecycleGeneration == 0 && pr.State == db.StateClosed {
		return pr, nil, nil
	}
	if pr.ForgejoReopenPending {
		return pr, nil, fmt.Errorf("Forgejo reopen lifecycle confirmation pending")
	}
	if len(revisions) != 1 {
		return pr, nil, fmt.Errorf("projection close requires provider lifecycle revision")
	}
	eventAt, err := providerLifecycleTime(revisions[0])
	if err != nil {
		return pr, nil, err
	}
	if pr.ForgejoLifecycleAt != nil && !eventAt.After(*pr.ForgejoLifecycleAt) {
		// An equal close can repair a missing delivery; an equal conflicting state
		// cannot beat open. The same comparison is repeated under the write lock.
		if eventAt.Before(*pr.ForgejoLifecycleAt) || pr.State != db.StateClosed {
			return pr, nil, ErrStaleProjectionClose
		}
	}
	if s.ForgejoIntegration == nil {
		return pr, nil, fmt.Errorf("Forgejo close readback unavailable")
	}
	snapshot, found, err := s.ForgejoIntegration.ExactPullRequestSnapshot(ctx, pr.Repository.FullName, externalRepo, number)
	if err != nil {
		return pr, nil, err
	}
	if !found {
		return pr, nil, fmt.Errorf("Forgejo close readback not found")
	}
	if snapshot.Number != number || snapshot.HeadRef != pr.HeadRef || snapshot.BaseRef != pr.BaseRef {
		return pr, nil, fmt.Errorf("Forgejo close readback identity mismatch")
	}
	if snapshot.Merged || snapshot.State == db.StateOpen {
		return pr, nil, ErrStaleProjectionClose
	}
	if snapshot.State != db.StateClosed {
		return pr, nil, fmt.Errorf("Forgejo close readback state is invalid")
	}
	liveAt, err := providerLifecycleTime(snapshot.UpdatedAt)
	if err != nil {
		return pr, nil, err
	}
	// Metadata may advance updated_at after the signed close was emitted.
	// The event must beat the reopen watermark and cannot be ahead of live
	// provider state; a later closed readback does not invalidate that close.
	if eventAt.After(liveAt) {
		return pr, nil, ErrStaleProjectionClose
	}
	return pr, &eventAt, nil
}

// confirmForgejoReopen completes the durable pending intent only for the same
// generation, exact mapping, and open provider readback. Network I/O has already
// completed; all writes below share the terminal path's projection -> PR order.
func (s *Service) confirmForgejoReopen(ctx context.Context, pr db.PullRequest, projection db.PullRequestProjection, snapshot forgejointegration.PullRequestSnapshot) error {
	at, err := providerLifecycleTime(snapshot.UpdatedAt)
	if err != nil {
		return err
	}
	return s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		var binding db.PullRequestProjection
		if err := lockPRProjectionSet(tx, pr.ID); err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&binding, projection.ID).Error; err != nil {
			return err
		}
		var current db.PullRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, pr.ID).Error; err != nil {
			return err
		}
		if err := validateForgejoProjectionFact(current, pr, binding, projection.ExternalRepo, projection.ExternalNumber); err != nil {
			return err
		}
		return confirmForgejoReopenTx(tx, current, pr, binding, snapshot, at)
	})
}

// A completed intent cannot authorize another reopen or advance its watermark.
// The caller holds the complete projection set and PR locks through provider
// write admission and consumption; generation alone does not fence completion.
func validateForgejoReopenIntent(current, observed db.PullRequest) error {
	if !current.ForgejoReopenPending || !observed.ForgejoReopenPending ||
		(current.ForgejoLifecycleAt == nil) != (observed.ForgejoLifecycleAt == nil) ||
		(current.ForgejoLifecycleAt != nil && !current.ForgejoLifecycleAt.Equal(*observed.ForgejoLifecycleAt)) {
		return ErrStaleProjectionClose
	}
	return nil
}

func confirmForgejoReopenTx(tx *gorm.DB, current, observed db.PullRequest, binding db.PullRequestProjection, snapshot forgejointegration.PullRequestSnapshot, at time.Time) error {
	if err := validateForgejoReopenIntent(current, observed); err != nil {
		return err
	}
	if snapshot.Number != binding.ExternalNumber || snapshot.State != db.StateOpen || snapshot.Merged || snapshot.HeadRef != current.HeadRef || snapshot.BaseRef != current.BaseRef {
		return fmt.Errorf("Forgejo reopen open readback mismatch")
	}
	if binding.PullRequestID != observed.ID || binding.Provider != ProjectionProviderForgejo {
		return ErrStaleProjectionClose
	}
	if current.ForgejoLifecycleAt != nil && at.Before(*current.ForgejoLifecycleAt) {
		return fmt.Errorf("Forgejo reopen readback lifecycle regressed")
	}
	if err := tx.Model(&current).Updates(map[string]any{"forgejo_reopen_pending": false, "forgejo_lifecycle_at": at}).Error; err != nil {
		return err
	}
	return tx.Model(&binding).Updates(map[string]any{"state": ProjectionStateOpen}).Error
}

// updatePRLifecycleTx is shared by REST and GraphQL. Record reopen intent in
// the native fact transaction, before any provider I/O (including async work).
// Partial updates never save an old merged or generation snapshot back over a
// terminal fact committed while the caller was preparing metadata.
func (s *Service) updatePRLifecycleTx(tx *gorm.DB, id uint, state *string, updates map[string]any) (db.PullRequest, string, error) {
	// Match callback lock order even when native close enqueues via projection.
	if state != nil {
		if err := lockPRProjectionSet(tx, id); err != nil {
			return db.PullRequest{}, "", err
		}
	}
	var current db.PullRequest
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, id).Error; err != nil {
		return current, "", err
	}
	previous := current.State
	if state != nil {
		if *state == db.StateOpen && current.Merged {
			return current, previous, fmt.Errorf("%w: a merged pull request cannot be reopened", ErrInvalidState)
		}
		updates["state"] = *state
		if *state == db.StateOpen {
			updates["closed_at"] = nil
			if previous == db.StateClosed {
				updates["lifecycle_generation"] = current.LifecycleGeneration + 1
				updates["forgejo_reopen_pending"] = true
			}
		} else if *state == db.StateClosed {
			if current.ClosedAt == nil || current.ClosedAt.IsZero() {
				now := time.Now().UTC()
				updates["closed_at"] = &now
			} else {
				updates["closed_at"] = current.ClosedAt
			}
		}
	}
	if len(updates) > 0 {
		if err := tx.Model(&current).Updates(updates).Error; err != nil {
			return current, previous, err
		}
	}
	if err := tx.First(&current, id).Error; err != nil {
		return current, previous, err
	}
	if current.ForgejoReopenPending && current.State == db.StateOpen && !current.Merged && s.ForgejoIntegration != nil {
		// Queue in the same transaction as intent. A crash before synchronous
		// dispatch, or a failed confirmation GET, leaves restartable work.
		if err := s.queueForgejoReopenTx(tx, current); err != nil {
			return current, previous, err
		}
	}
	return current, previous, nil
}

// Every lifecycle writer locks the complete projection set by primary key,
// then the native PR. Locking only the callback's provider creates a cycle
// when merge subsequently updates the other providers.
func lockPRProjectionSet(tx *gorm.DB, id uint) error {
	if tx.Dialector.Name() == "sqlite" {
		// SQLite drops FOR UPDATE, including in WAL mode. Promote this transaction
		// to a writer BEFORE reading intent: even a no-op UPDATE reserves the
		// database writer across connections/processes until commit or rollback.
		// Contention (or an older read snapshot) must fail here, before provider
		// side effects. Use raw SQL to leave values and updated_at untouched.
		if err := tx.Exec("UPDATE pull_request_projections SET id = id WHERE pull_request_id = ?", id).Error; err != nil {
			return err
		}
	}
	var bindings []db.PullRequestProjection
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("pull_request_id = ?", id).Order("id").Find(&bindings).Error
}

func validateForgejoProjectionFact(current, observed db.PullRequest, binding db.PullRequestProjection, externalRepo string, number int) error {
	if current.Merged || current.State != db.StateOpen || current.LifecycleGeneration != observed.LifecycleGeneration ||
		current.HeadRef != observed.HeadRef || current.BaseRef != observed.BaseRef || current.HeadSHA != observed.HeadSHA {
		return ErrStaleProjectionClose
	}
	if binding.ID != 0 && (binding.PullRequestID != current.ID || binding.Provider != ProjectionProviderForgejo || binding.ExternalRepo != externalRepo || binding.ExternalNumber != number) {
		return ErrStaleProjectionClose
	}
	return nil
}

// queueForgejoReopenTx revives completed generic work or a job retired solely
// because native state was closed. Active claims keep their lease; unrelated
// terminal failures and action authority retain their explicit repair path.
func (s *Service) queueForgejoReopenTx(tx *gorm.DB, pr db.PullRequest) error {
	now := time.Now().UTC()
	job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerPullRequest, RepoFullName: pr.Repository.FullName, AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA, Phase: ForgejoProjectionPhaseQueued, NextRunAt: &now}
	if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "pull_request_id"}, {Name: "provider"}}, DoNothing: true}).Create(&job).Error; err != nil {
		return err
	}
	return tx.Model(&db.PullRequestProjectionJob{}).
		Where("pull_request_id = ? AND provider = ? AND `trigger` = ? AND (phase = ? OR (phase = ? AND last_error_type = ? AND last_error = ?))", pr.ID, ProjectionProviderForgejo, ForgejoProjectionTriggerPullRequest, ForgejoProjectionPhaseProjected, ForgejoProjectionPhaseFailedTerminal, forgejointegration.ProjectionFailurePullRequestStateDrift, forgejoProjectionRetiredClosed).
		Updates(map[string]any{"phase": ForgejoProjectionPhaseQueued, "attempt": gorm.Expr("attempt + 1"), "head_sha": pr.HeadSHA, "head_ref": pr.HeadRef, "base_ref": pr.BaseRef, "next_run_at": &now, "finished_at": nil, "last_error": "", "last_error_type": ""}).Error
}
