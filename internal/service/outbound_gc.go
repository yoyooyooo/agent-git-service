package service

import (
	"context"
	"fmt"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
)

type OutboundDeliveryGCRetention struct {
	Delivered  time.Duration
	DeadLetter time.Duration
	Limit      int
}

func DefaultOutboundDeliveryGCRetention() OutboundDeliveryGCRetention {
	return OutboundDeliveryGCRetention{
		Delivered:  14 * 24 * time.Hour,
		DeadLetter: 30 * 24 * time.Hour,
		Limit:      500,
	}
}

func (s *Service) GCOutboundDeliveries(ctx context.Context, retention OutboundDeliveryGCRetention) (int, error) {
	if s == nil {
		return 0, fmt.Errorf("service is nil")
	}
	if retention.Delivered <= 0 {
		retention.Delivered = 14 * 24 * time.Hour
	}
	if retention.DeadLetter <= 0 {
		retention.DeadLetter = 30 * 24 * time.Hour
	}
	if retention.Limit <= 0 || retention.Limit > 5000 {
		retention.Limit = 500
	}
	now := time.Now().UTC()
	cutoffs := map[string]time.Time{
		OutboundDeliveryStatusDelivered:  now.Add(-retention.Delivered),
		OutboundDeliveryStatusDeadLetter: now.Add(-retention.DeadLetter),
	}
	deleted := 0
	for status, cutoff := range cutoffs {
		var ids []uint
		if err := s.DBForCtx(ctx).Model(&db.OutboundDelivery{}).
			Where("status = ? AND updated_at < ?", status, cutoff).
			Order("updated_at ASC, id ASC").
			Limit(retention.Limit).
			Pluck("id", &ids).Error; err != nil {
			return deleted, err
		}
		if len(ids) == 0 {
			continue
		}
		res := s.DBForCtx(ctx).Where("id IN ?", ids).Delete(&db.OutboundDelivery{})
		if res.Error != nil {
			return deleted, res.Error
		}
		deleted += int(res.RowsAffected)
	}
	return deleted, nil
}
