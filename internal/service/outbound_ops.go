package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

type OutboundDeliveryFilter struct {
	EventType    string
	Status       string
	RepoFullName string
	Limit        int
}

func (s *Service) ListOutboundDeliveries(ctx context.Context, filter OutboundDeliveryFilter) ([]db.OutboundDelivery, error) {
	if s == nil {
		return nil, fmt.Errorf("service is nil")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := s.DBForCtx(ctx).Model(&db.OutboundDelivery{})
	if strings.TrimSpace(filter.EventType) != "" {
		q = q.Where("event_type = ?", strings.TrimSpace(filter.EventType))
	}
	if strings.TrimSpace(filter.Status) != "" {
		q = q.Where("status = ?", strings.TrimSpace(filter.Status))
	}
	if strings.TrimSpace(filter.RepoFullName) != "" {
		q = q.Where("repo_full_name = ?", strings.TrimSpace(filter.RepoFullName))
	}
	var rows []db.OutboundDelivery
	if err := q.Order("updated_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Service) RetryOutboundDelivery(ctx context.Context, id uint) (db.OutboundDelivery, error) {
	if err := s.DeliverOutboundDeliveryNow(ctx, id); err != nil {
		return db.OutboundDelivery{}, err
	}
	var delivery db.OutboundDelivery
	if err := s.DBForCtx(ctx).First(&delivery, id).Error; err != nil {
		return db.OutboundDelivery{}, err
	}
	return delivery, nil
}

func (s *Service) ReplayPullRequestMergedOutbound(ctx context.Context, repoFullName string, prNumber int, force bool) ([]db.OutboundDelivery, error) {
	if s == nil {
		return nil, fmt.Errorf("service is nil")
	}
	if len(s.OutboundEventTargets[OutboundEventPullRequestMerged]) == 0 {
		return nil, fmt.Errorf("outbound pull_request_merged targets are not configured")
	}
	pr, err := s.GetPR(ctx, repoFullName, prNumber)
	if err != nil {
		return nil, err
	}
	if !pr.Merged {
		return nil, fmt.Errorf("pull request %s#%d is not merged", repoFullName, prNumber)
	}
	note := s.buildPullRequestMergedNotification(ctx, pr, "manual_replay", "manual_replay")
	results := make([]db.OutboundDelivery, 0, len(s.OutboundEventTargets[OutboundEventPullRequestMerged]))
	for _, target := range s.OutboundEventTargets[OutboundEventPullRequestMerged] {
		intent := pullRequestMergedOutboundIntent(note, target, force)
		if force {
			intent.IdempotencyKey = intent.IdempotencyKey + fmt.Sprintf(":manual:%d", time.Now().UnixNano())
		}
		if !force {
			// A non-forced replay is an explicit read/retry of the original
			// business key. Resolve that row before constructing a new payload so
			// manual-replay metadata cannot collide with the original event; all
			// actual enqueue callers still fail closed on same-key payload drift.
			var existing db.OutboundDelivery
			lookupErr := s.DBForCtx(ctx).Where("idempotency_key = ?", intent.IdempotencyKey).First(&existing).Error
			if lookupErr == nil {
				if existing.Status != OutboundDeliveryStatusDelivered {
					if err := s.DeliverOutboundDeliveryNow(ctx, existing.ID); err != nil {
						return results, err
					}
					if err := s.DBForCtx(ctx).First(&existing, existing.ID).Error; err != nil {
						return results, err
					}
				}
				results = append(results, existing)
				continue
			}
			if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
				return results, lookupErr
			}
			// The original row may have been removed by retention. The replay
			// payload is intentionally marked as a manual replay, so it must use
			// a fresh key rather than reusing the old business key with changed
			// bytes.
			intent.IdempotencyKey = intent.IdempotencyKey + fmt.Sprintf(":manual:%d", time.Now().UnixNano())
		}
		delivery, created, err := s.EnqueueOutboundDelivery(ctx, intent)
		if err != nil {
			return results, err
		}
		if created || delivery.Status != OutboundDeliveryStatusDelivered {
			if err := s.DeliverOutboundDeliveryNow(ctx, delivery.ID); err != nil {
				return results, err
			}
			if err := s.DBForCtx(ctx).First(&delivery, delivery.ID).Error; err != nil {
				return results, err
			}
		}
		results = append(results, delivery)
	}
	return results, nil
}

func firstNonEmptyOutboundString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
