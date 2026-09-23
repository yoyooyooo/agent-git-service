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

// observeTeamAuthorityEpoch records a monotonic high-water mark at exchange
// time and enforces both desired-state and durable revocation floors.
func (s *Service) observeTeamAuthorityEpoch(ctx context.Context, resolved sessionauthority.Resolved, epoch int64) error {
	return s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		return observeTeamAuthorityEpochTx(tx, resolved, epoch)
	})
}

// observeTeamAuthorityEpochTx must share the session creation transaction.
// Otherwise an epoch floor could advance after the high-water observation and
// before the newly authorized session is persisted.
func observeTeamAuthorityEpochTx(tx *gorm.DB, resolved sessionauthority.Resolved, epoch int64) error {
	if resolved.TeamBinding.ID == "" || epoch <= 0 {
		return errors.New("canonical team authority is required")
	}
	issuer, team, policy := resolved.Issuer.ID, resolved.TeamBinding.TeamIdentityID, resolved.TeamBinding.PolicyClass
	var state db.TeamAuthorityEpoch
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"issuer_instance_id = ? AND team_identity_id = ? AND policy_class = ?", issuer, team, policy,
	).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		state = db.TeamAuthorityEpoch{IssuerInstanceID: issuer, TeamIdentityID: team, PolicyClass: policy, EpochFloor: resolved.TeamBinding.EpochFloor, HighWater: epoch, UpdatedAt: time.Now().UTC()}
		return tx.Create(&state).Error
	}
	if err != nil {
		return err
	}
	floor := state.EpochFloor
	if resolved.TeamBinding.EpochFloor > floor {
		floor = resolved.TeamBinding.EpochFloor
	}
	if epoch < floor || epoch < state.HighWater {
		return errors.New("membership epoch is below floor or high-water mark")
	}
	return tx.Model(&state).Updates(map[string]any{"epoch_floor": floor, "high_water": epoch, "updated_at": time.Now().UTC()}).Error
}

// ReconcileTeamAuthorityEpochFloors persists configured active binding floors
// before sessions can be exchanged or resolved. Configuration is desired state;
// this ledger is the monotonic state that prevents a later config rollback from
// restoring a lower membership epoch.
func (s *Service) ReconcileTeamAuthorityEpochFloors(ctx context.Context) error {
	if s == nil || s.PrincipalSessions == nil {
		return nil
	}
	bindings := s.PrincipalSessions.Config().TeamBindings
	return s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		for _, binding := range bindings {
			if binding.Status != "active" {
				continue
			}
			if err := reconcileTeamAuthorityEpochFloorTx(tx, binding); err != nil {
				return err
			}
		}
		return nil
	})
}

func reconcileTeamAuthorityEpochFloorTx(tx *gorm.DB, binding sessionauthority.TeamBinding) error {
	var state db.TeamAuthorityEpoch
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(
		"issuer_instance_id = ? AND team_identity_id = ? AND policy_class = ?", binding.IssuerInstanceID, binding.TeamIdentityID, binding.PolicyClass,
	).First(&state).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return tx.Create(&db.TeamAuthorityEpoch{
			IssuerInstanceID: binding.IssuerInstanceID, TeamIdentityID: binding.TeamIdentityID, PolicyClass: binding.PolicyClass,
			EpochFloor: binding.EpochFloor, HighWater: binding.EpochFloor, UpdatedAt: time.Now().UTC(),
		}).Error
	}
	if err != nil {
		return err
	}
	floor, highWater := state.EpochFloor, state.HighWater
	if binding.EpochFloor > floor {
		floor = binding.EpochFloor
	}
	if floor > highWater {
		highWater = floor
	}
	if floor == state.EpochFloor && highWater == state.HighWater {
		return nil
	}
	return tx.Model(&state).Updates(map[string]any{"epoch_floor": floor, "high_water": highWater, "updated_at": time.Now().UTC()}).Error
}

func (s *Service) teamAuthorityEpochCurrent(ctx context.Context, issuer, team, policy string, epoch int64) bool {
	if epoch <= 0 || strings.TrimSpace(issuer) == "" || strings.TrimSpace(team) == "" || strings.TrimSpace(policy) == "" {
		return false
	}
	var state db.TeamAuthorityEpoch
	if err := s.DBForCtx(ctx).Where("issuer_instance_id = ? AND team_identity_id = ? AND policy_class = ?", issuer, team, policy).First(&state).Error; err != nil {
		return false
	}
	return epoch >= state.EpochFloor && epoch >= state.HighWater
}

// AdvanceTeamAuthorityEpochFloor is the target-local, auditable revocation
// seam. It never lowers a floor and invalidates older sessions on their next
// credential resolution. Callers must provide an explicit higher epoch.
func (s *Service) AdvanceTeamAuthorityEpochFloor(ctx context.Context, issuerInstanceID, teamIdentityID, policyClass string, floor int64) error {
	actor, ok := UserFromContext(ctx)
	if !ok || !actor.SiteAdmin {
		return ErrForbidden
	}
	issuerInstanceID, teamIdentityID, policyClass = strings.TrimSpace(issuerInstanceID), strings.TrimSpace(teamIdentityID), strings.TrimSpace(policyClass)
	if issuerInstanceID == "" || teamIdentityID == "" || policyClass == "" || floor <= 0 {
		return fmt.Errorf("%w: issuer_instance_id, team_identity_id, policy_class, and positive floor are required", ErrValidation)
	}
	if s.PrincipalSessions == nil {
		return ErrForbidden
	}
	binding, ok := s.PrincipalSessions.TeamBinding(issuerInstanceID, teamIdentityID, policyClass)
	if !ok {
		return ErrForbidden
	}
	if err := s.ReconcileTeamAuthorityEpochFloors(ctx); err != nil {
		return fmt.Errorf("reconcile team authority epoch floors: %w", err)
	}
	return s.DBForCtx(ctx).Transaction(func(tx *gorm.DB) error {
		var state db.TeamAuthorityEpoch
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("issuer_instance_id = ? AND team_identity_id = ? AND policy_class = ?", issuerInstanceID, teamIdentityID, policyClass).First(&state).Error; err != nil {
			return err
		}
		if floor <= binding.EpochFloor || floor <= state.EpochFloor {
			return fmt.Errorf("%w: membership epoch floor must advance", ErrValidation)
		}
		highWater := state.HighWater
		if highWater < floor {
			highWater = floor
		}
		if err := tx.Model(&state).Updates(map[string]any{"epoch_floor": floor, "high_water": highWater, "updated_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return s.LogAudit(ContextWithDB(ctx, tx), AuditEvent{
			UserID: &actor.ID, Action: AuditActionTeamAuthorityEpochAdvance,
			Details: teamAuthorityEpochAuditDetails(binding, floor),
		})
	})
}

func teamAuthorityEpochAuditDetails(binding sessionauthority.TeamBinding, floor int64) string {
	return fmt.Sprintf("team authority epoch floor advanced: issuer=%s team=%s policy_class=%s binding_revision=%s floor=%d", binding.IssuerInstanceID, binding.TeamIdentityID, binding.PolicyClass, binding.BindingRevision, floor)
}
