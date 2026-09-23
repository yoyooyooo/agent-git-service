package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestRetryOutboundDeliveryAttemptsExistingRow(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{results: []service.OutboundDeliveryResult{{Delivered: true}}}
	svc.OutboundDispatcher = dispatcher
	delivery, _, err := svc.EnqueueOutboundDelivery(context.Background(), service.OutboundDeliveryIntent{
		EventType:      service.OutboundEventPullRequestMerged,
		TargetName:     "department_official",
		TargetType:     service.OutboundTargetTypeFeishuWebhook,
		IdempotencyKey: "retry-me",
		PayloadVersion: "pr_merge_text_v1",
		PayloadJSON:    `{"text":"retry"}`,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := svc.DB.Model(&delivery).Update("status", service.OutboundDeliveryStatusRetryWait).Error; err != nil {
		t.Fatalf("mark retry: %v", err)
	}

	stored, err := svc.RetryOutboundDelivery(context.Background(), delivery.ID)
	if err != nil {
		t.Fatalf("RetryOutboundDelivery: %v", err)
	}
	if stored.Status != service.OutboundDeliveryStatusDelivered || len(dispatcher.calls) != 1 {
		t.Fatalf("expected delivered retry, stored=%#v calls=%#v", stored, dispatcher.calls)
	}
}

func TestReplayPullRequestMergedOutboundUsesFreshKeyWhenOriginalRowWasRemoved(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{}
	svc.OutboundDispatcher = dispatcher
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{
		service.OutboundEventPullRequestMerged: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}},
	}

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "replay-removed-user", "repo")
	merged, err := svc.MergePR(authCtx, pr.Repository.FullName, pr.Number, "merge", "")
	if err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	var original db.OutboundDelivery
	if err := svc.DB.Where("event_type = ? AND repo_full_name = ? AND pr_number = ?", service.OutboundEventPullRequestMerged, pr.Repository.FullName, pr.Number).First(&original).Error; err != nil {
		t.Fatalf("load original delivery: %v", err)
	}
	if err := svc.DB.Delete(&original).Error; err != nil {
		t.Fatalf("delete original delivery: %v", err)
	}

	rows, err := svc.ReplayPullRequestMergedOutbound(context.Background(), pr.Repository.FullName, pr.Number, false)
	if err != nil {
		t.Fatalf("ReplayPullRequestMergedOutbound after retention: %v", err)
	}
	if len(rows) != 1 || len(dispatcher.calls) != 2 {
		t.Fatalf("expected a fresh replay dispatch, rows=%#v calls=%d", rows, len(dispatcher.calls))
	}
	if rows[0].IdempotencyKey == original.IdempotencyKey || !strings.Contains(string(rows[0].PayloadJSON), "manual_replay") || rows[0].MergeSHA != merged.MergeCommitSHA {
		t.Fatalf("retention replay reused key or lost replay facts: original=%#v replay=%#v", original, rows[0])
	}
}

func TestReplayPullRequestMergedOutboundIsIdempotentUnlessForced(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{}
	svc.OutboundDispatcher = dispatcher
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{
		service.OutboundEventPullRequestMerged: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}},
	}

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "replay-user", "repo")
	merged, err := svc.MergePR(authCtx, pr.Repository.FullName, pr.Number, "merge", "")
	if err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if len(dispatcher.calls) != 1 {
		t.Fatalf("expected merge dispatch, got %d", len(dispatcher.calls))
	}

	rows, err := svc.ReplayPullRequestMergedOutbound(context.Background(), pr.Repository.FullName, pr.Number, false)
	if err != nil {
		t.Fatalf("ReplayPullRequestMergedOutbound: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != service.OutboundDeliveryStatusDelivered || len(dispatcher.calls) != 1 {
		t.Fatalf("expected idempotent no-op replay, rows=%#v calls=%d", rows, len(dispatcher.calls))
	}

	forced, err := svc.ReplayPullRequestMergedOutbound(context.Background(), pr.Repository.FullName, pr.Number, true)
	if err != nil {
		t.Fatalf("ReplayPullRequestMergedOutbound force: %v", err)
	}
	if len(forced) != 1 || len(dispatcher.calls) != 2 {
		t.Fatalf("expected forced replay dispatch, rows=%#v calls=%d", forced, len(dispatcher.calls))
	}
	if !strings.Contains(string(dispatcher.calls[1].Delivery.PayloadJSON), "manual_replay") || dispatcher.calls[1].Delivery.MergeSHA != merged.MergeCommitSHA {
		t.Fatalf("forced payload mismatch: %#v", dispatcher.calls[1].Delivery)
	}

	var count int64
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("event_type = ?", service.OutboundEventPullRequestMerged).Count(&count).Error; err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected original + forced replay rows, got %d", count)
	}
}
