package db

import (
	"errors"

	"gorm.io/gorm"
)

// MigrateDelegatedSessionActorSnapshots backfills the principal login and the
// AGS-owned human binding for sessions created before actor projection existed.
// These snapshots are presentation/audit provenance only; authorization
// continues to use the immutable principal_user_id stored on the session.
func MigrateDelegatedSessionActorSnapshots(database *gorm.DB) error {
	return database.Transaction(func(tx *gorm.DB) error {
		var sessions []DelegatedAgentSession
		if err := tx.Select("id", "principal_user_id", "principal_login", "delegated_by_user_id", "delegated_by_login", "delegated_by_source").
			Where("principal_login IS NULL OR principal_login = '' OR delegated_by_source IS NULL OR delegated_by_source = '' OR (delegated_by_source <> ? AND (delegated_by_user_id IS NULL OR delegated_by_login IS NULL OR delegated_by_login = ''))", DelegatedBySourcePrincipalOnly).
			Find(&sessions).Error; err != nil {
			return err
		}
		for _, session := range sessions {
			updates := map[string]any{}
			if session.PrincipalLogin == "" {
				var principal User
				if err := tx.Select("id", "login").First(&principal, "id = ?", session.PrincipalUserID).Error; err != nil {
					return err
				}
				updates["principal_login"] = principal.Login
			}
			needsDelegator := session.DelegatedBySource == "" || (session.DelegatedBySource != DelegatedBySourcePrincipalOnly && (session.DelegatedByUserID == nil || session.DelegatedByLogin == ""))
			if needsDelegator {
				humanID, humanLogin, ok, err := delegatedSessionHumanSnapshot(tx, session.PrincipalUserID)
				if err != nil {
					return err
				}
				if ok {
					updates["delegated_by_user_id"] = humanID
					updates["delegated_by_login"] = humanLogin
					updates["delegated_by_source"] = DelegatedBySourceMigrationBackfill
				} else {
					updates["delegated_by_source"] = DelegatedBySourcePrincipalOnly
				}
			}
			if len(updates) == 0 {
				continue
			}
			if err := tx.Model(&DelegatedAgentSession{}).Where("id = ?", session.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func delegatedSessionHumanSnapshot(tx *gorm.DB, principalUserID uint) (uint, string, bool, error) {
	var binding AgentBinding
	if err := tx.Select("human_user_id").First(&binding, "agent_user_id = ?", principalUserID).Error; err == nil {
		var human User
		if err := tx.Select("id", "login").First(&human, "id = ?", binding.HumanUserID).Error; err != nil {
			return 0, "", false, err
		}
		return human.ID, human.Login, true, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, "", false, err
	}

	var principal User
	if err := tx.Select("id", "login", "user_kind").First(&principal, "id = ?", principalUserID).Error; err != nil {
		return 0, "", false, err
	}
	if principal.UserKind == UserKindHuman {
		return principal.ID, principal.Login, true, nil
	}
	return 0, "", false, nil
}
