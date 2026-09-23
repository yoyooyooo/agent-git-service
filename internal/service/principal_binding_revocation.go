package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

// ReconcilePrincipalBindingRevocations persists every revoked binding in the
// current authority snapshot before the server accepts Session traffic. The
// tombstone is append-only: later snapshots cannot reactivate or reuse the ID.
func (s *Service) ReconcilePrincipalBindingRevocations(ctx context.Context) error {
	if s == nil || s.PrincipalSessions == nil {
		return nil
	}
	bindings := s.PrincipalSessions.Config().Bindings
	if len(bindings) == 0 {
		return nil
	}
	database := s.DBForCtx(ctx)
	return database.Transaction(func(tx *gorm.DB) error {
		for _, binding := range bindings {
			if binding.Status != "revoked" {
				var tombstone db.PrincipalBindingRevocation
				err := tx.First(&tombstone, "binding_id = ?", binding.ID).Error
				switch {
				case err == nil:
					return fmt.Errorf("revoked principal binding id %q cannot be reused", binding.ID)
				case errors.Is(err, gorm.ErrRecordNotFound):
					continue
				default:
					return err
				}
			}
			revokedAt, err := time.Parse(time.RFC3339, binding.RevokedAt)
			if err != nil {
				return fmt.Errorf("principal binding %q has invalid revocation time: %w", binding.ID, err)
			}
			candidate := db.PrincipalBindingRevocation{
				BindingID: binding.ID, BindingRevision: binding.BindingRevision,
				IssuerInstanceID: binding.IssuerInstanceID, IssuerSubject: binding.Subject,
				PrincipalUserID: binding.PrincipalID, RevokedAt: revokedAt.UTC(), ObservedAt: time.Now().UTC(),
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&candidate).Error; err != nil {
				return err
			}
			var persisted db.PrincipalBindingRevocation
			if err := tx.First(&persisted, "binding_id = ?", binding.ID).Error; err != nil {
				return err
			}
			if persisted.IssuerInstanceID != binding.IssuerInstanceID ||
				persisted.IssuerSubject != binding.Subject ||
				persisted.PrincipalUserID != binding.PrincipalID {
				return fmt.Errorf("revoked principal binding id %q cannot be reused for another identity", binding.ID)
			}
		}
		return nil
	})
}

func (s *Service) principalBindingDurablyRevoked(ctx context.Context, binding sessionauthority.PrincipalBinding) (bool, error) {
	var tombstone db.PrincipalBindingRevocation
	err := s.DBForCtx(ctx).First(&tombstone, "binding_id = ?", strings.TrimSpace(binding.ID)).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if tombstone.IssuerInstanceID != binding.IssuerInstanceID ||
		tombstone.IssuerSubject != binding.Subject ||
		tombstone.PrincipalUserID != binding.PrincipalID {
		return true, fmt.Errorf("revoked principal binding id %q was reused for another identity", binding.ID)
	}
	return true, nil
}
