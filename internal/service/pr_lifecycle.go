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
	// Strict equality binds the signed event to this exact readback. Future,
	// older and ambiguous revisions cannot be promoted using the AGS clock.
	if !eventAt.Equal(liveAt) {
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
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&binding, projection.ID).Error; err != nil {
			return err
		}
		var current db.PullRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, pr.ID).Error; err != nil {
			return err
		}
		if current.Merged || current.State != db.StateOpen || current.LifecycleGeneration != pr.LifecycleGeneration || binding.ExternalRepo != projection.ExternalRepo || binding.ExternalNumber != projection.ExternalNumber {
			return ErrStaleProjectionClose
		}
		if current.ForgejoLifecycleAt != nil && at.Before(*current.ForgejoLifecycleAt) {
			return fmt.Errorf("Forgejo reopen readback lifecycle regressed")
		}
		if err := tx.Model(&current).Updates(map[string]any{"forgejo_reopen_pending": false, "forgejo_lifecycle_at": at}).Error; err != nil {
			return err
		}
		return tx.Model(&binding).Updates(map[string]any{"state": ProjectionStateOpen}).Error
	})
}

// updatePRLifecycleTx is shared by REST and GraphQL. Record reopen intent in
// the native fact transaction, before any provider I/O (including async work).
// Partial updates never save an old merged or generation snapshot back over a
// terminal fact committed while the caller was preparing metadata.
func (s *Service) updatePRLifecycleTx(tx *gorm.DB, id uint, state *string, updates map[string]any) (db.PullRequest, string, error) {
	// Match callback lock order even when native close enqueues via projection.
	if state != nil {
		var bindings []db.PullRequestProjection
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("pull_request_id = ?", id).Order("id").Find(&bindings).Error; err != nil {
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
	return current, previous, nil
}
