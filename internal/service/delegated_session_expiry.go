package service

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/ngaut/agent-git-service/internal/db"
)

const (
	defaultDelegatedSessionExpiryAuditInterval = time.Minute
	defaultDelegatedSessionExpiryAuditBatch    = 100
)

// AuditExpiredDelegatedSessions appends one canonical lifecycle audit for each
// newly observed expired session. The marker and audit entry commit together so
// repeated or concurrent sweeps remain idempotent. Revoked sessions retain their
// revoke terminal state and do not later emit an expiry event.
func (s *Service) AuditExpiredDelegatedSessions(ctx context.Context, now time.Time, limit int) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if limit <= 0 {
		limit = defaultDelegatedSessionExpiryAuditBatch
	}

	var candidates []db.DelegatedAgentSession
	if err := s.DBForCtx(ctx).
		Preload("PrincipalUser").
		Preload("Repository").
		Where("expires_at <= ? AND revoked_at IS NULL AND expiry_audited_at IS NULL", now).
		Order("expires_at ASC, id ASC").
		Limit(limit).
		Find(&candidates).Error; err != nil {
		return 0, err
	}

	audited := 0
	for i := range candidates {
		session := candidates[i]
		claimed := false
		err := s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
			result := tx.Model(&db.DelegatedAgentSession{}).
				Where("id = ? AND revoked_at IS NULL AND expiry_audited_at IS NULL", session.ID).
				Update("expiry_audited_at", now)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return nil
			}
			claimed = true
			session.ExpiryAuditedAt = &now
			auditCtx := ContextWithDB(ContextWithDelegatedSession(ctx, session), tx)
			return s.LogCurrentDelegatedSessionAudit(auditCtx, DelegatedSessionAuditEvent{
				Action: AuditActionDelegatedSessionExpiry, Operation: "session.expire", Outcome: "success", Reason: "ttl_elapsed",
			})
		})
		if err != nil {
			return audited, err
		}
		if claimed {
			audited++
		}
	}
	return audited, nil
}

// RunDelegatedSessionExpiryAuditor owns the bounded background sweep until ctx
// is cancelled. It performs one immediate sweep so expiry facts are reconciled
// after a restart without waiting for the first tick.
func (s *Service) RunDelegatedSessionExpiryAuditor(ctx context.Context, interval time.Duration, limit int) {
	if interval <= 0 {
		interval = defaultDelegatedSessionExpiryAuditInterval
	}
	if limit <= 0 {
		limit = defaultDelegatedSessionExpiryAuditBatch
	}
	sweep := func() {
		if _, err := s.AuditExpiredDelegatedSessions(ctx, time.Now().UTC(), limit); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "delegated session expiry audit sweep failed", "error", err)
		}
	}

	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
