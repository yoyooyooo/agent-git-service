package service

import (
	"context"
	"errors"

	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

// The fork's delegated-session provenance still needs this lookup after
// upstream retired anonymous-account helpers. No binding grants repo access;
// current native permissions are independently revalidated before effects.
func (s *Service) boundHumanIDForAgent(ctx context.Context, agentID uint) (uint, bool, error) {
	var binding db.AgentBinding
	err := s.DBForCtx(ctx).WithContext(ctx).Select("human_user_id").First(&binding, "agent_user_id = ?", agentID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || isMissingTableErr(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return binding.HumanUserID, true, nil
}
