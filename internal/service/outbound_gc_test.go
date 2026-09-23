package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestGCOutboundDeliveriesDeletesOnlyExpiredTerminalRows(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	now := time.Now().UTC()
	rows := []db.OutboundDelivery{
		{IdempotencyKey: "old-delivered", EventType: service.OutboundEventPullRequestMerged, TargetName: "t", TargetType: service.OutboundTargetTypeFeishuWebhook, Status: service.OutboundDeliveryStatusDelivered, CreatedAt: now.Add(-20 * 24 * time.Hour), UpdatedAt: now.Add(-20 * 24 * time.Hour)},
		{IdempotencyKey: "recent-delivered", EventType: service.OutboundEventPullRequestMerged, TargetName: "t", TargetType: service.OutboundTargetTypeFeishuWebhook, Status: service.OutboundDeliveryStatusDelivered, CreatedAt: now.Add(-2 * 24 * time.Hour), UpdatedAt: now.Add(-2 * 24 * time.Hour)},
		{IdempotencyKey: "old-dead", EventType: service.OutboundEventPullRequestMerged, TargetName: "t", TargetType: service.OutboundTargetTypeFeishuWebhook, Status: service.OutboundDeliveryStatusDeadLetter, CreatedAt: now.Add(-40 * 24 * time.Hour), UpdatedAt: now.Add(-40 * 24 * time.Hour)},
		{IdempotencyKey: "old-retry", EventType: service.OutboundEventPullRequestMerged, TargetName: "t", TargetType: service.OutboundTargetTypeFeishuWebhook, Status: service.OutboundDeliveryStatusRetryWait, CreatedAt: now.Add(-100 * 24 * time.Hour), UpdatedAt: now.Add(-100 * 24 * time.Hour)},
	}
	if err := svc.DB.Create(&rows).Error; err != nil {
		t.Fatalf("seed outbound deliveries: %v", err)
	}

	deleted, err := svc.GCOutboundDeliveries(context.Background(), service.OutboundDeliveryGCRetention{
		Delivered:  14 * 24 * time.Hour,
		DeadLetter: 30 * 24 * time.Hour,
		Limit:      100,
	})
	if err != nil {
		t.Fatalf("GCOutboundDeliveries: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 deleted rows, got %d", deleted)
	}

	var remaining []db.OutboundDelivery
	if err := svc.DB.Order("idempotency_key asc").Find(&remaining).Error; err != nil {
		t.Fatalf("list remaining: %v", err)
	}
	keys := make([]string, 0, len(remaining))
	for _, row := range remaining {
		keys = append(keys, row.IdempotencyKey)
	}
	want := []string{"old-retry", "recent-delivered"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("remaining keys mismatch: got %#v want %#v", keys, want)
	}
}
