package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/gorm"
)

type captureOutboundDispatcher struct {
	results []service.OutboundDeliveryResult
	calls   []service.OutboundDispatchCall
}

func (d *captureOutboundDispatcher) DispatchOutbound(ctx context.Context, call service.OutboundDispatchCall) service.OutboundDeliveryResult {
	d.calls = append(d.calls, call)
	if len(d.results) == 0 {
		return service.OutboundDeliveryResult{Delivered: true}
	}
	result := d.results[0]
	d.results = d.results[1:]
	return result
}

func TestEnqueueOutboundDeliveryIsIdempotentAfterDelivered(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{}
	svc.OutboundDispatcher = dispatcher

	intent := service.OutboundDeliveryIntent{
		EventType:      service.OutboundEventPullRequestMerged,
		TargetName:     "department_official",
		TargetType:     service.OutboundTargetTypeFeishuWebhook,
		SubjectType:    "pull_request",
		SubjectKey:     "example-team/team-docs-fixture#2",
		IdempotencyKey: "pull_request_merged:ux/team-docs-fixture:2:abc123:department_official",
		PayloadVersion: "pr_merge_text_v1",
		PayloadJSON:    `{"text":"AGS PR merged"}`,
		RepoFullName:   "example-team/team-docs-fixture",
		PRNumber:       2,
		MergeSHA:       "abc123",
	}

	first, created, err := svc.EnqueueOutboundDelivery(context.Background(), intent)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if !created {
		t.Fatal("expected first enqueue to create a delivery")
	}
	if err := svc.DeliverOutboundDeliveryNow(context.Background(), first.ID); err != nil {
		t.Fatalf("deliver first: %v", err)
	}

	second, created, err := svc.EnqueueOutboundDelivery(context.Background(), intent)
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if created {
		t.Fatal("expected second enqueue to reuse existing delivery")
	}
	if second.ID != first.ID || second.Status != service.OutboundDeliveryStatusDelivered {
		t.Fatalf("expected delivered existing row, got %#v", second)
	}
	if len(dispatcher.calls) != 1 {
		t.Fatalf("expected one dispatch call, got %d", len(dispatcher.calls))
	}

	var rows []db.OutboundDelivery
	if err := svc.DB.Find(&rows).Error; err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one persisted delivery, got %#v", rows)
	}
}

func TestOutboundDeliveryRetryableFailureSchedulesRetry(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{results: []service.OutboundDeliveryResult{{Delivered: false, Retryable: true, Code: "feishu_11232", Message: "frequency limited"}}}
	svc.OutboundDispatcher = dispatcher

	delivery, _, err := svc.EnqueueOutboundDelivery(context.Background(), service.OutboundDeliveryIntent{
		EventType:      service.OutboundEventPullRequestMerged,
		TargetName:     "department_official",
		TargetType:     service.OutboundTargetTypeFeishuWebhook,
		SubjectType:    "pull_request",
		SubjectKey:     "example-team/team-docs-fixture#2",
		IdempotencyKey: "pull_request_merged:ux/team-docs-fixture:2:def456:department_official",
		PayloadVersion: "pr_merge_text_v1",
		PayloadJSON:    `{"text":"AGS PR merged"}`,
		RepoFullName:   "example-team/team-docs-fixture",
		PRNumber:       2,
		MergeSHA:       "def456",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	before := time.Now()
	if err := svc.DeliverOutboundDeliveryNow(context.Background(), delivery.ID); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	var stored db.OutboundDelivery
	if err := svc.DB.First(&stored, delivery.ID).Error; err != nil {
		t.Fatalf("load delivery: %v", err)
	}
	if stored.Status != service.OutboundDeliveryStatusRetryWait {
		t.Fatalf("expected retry_wait, got %#v", stored)
	}
	if stored.AttemptCount != 1 || stored.NextAttemptAt == nil || !stored.NextAttemptAt.After(before) || stored.LastErrorCode != "feishu_11232" {
		t.Fatalf("retry metadata mismatch: %#v", stored)
	}
}

type leaseRecordingDispatcher struct {
	owners []string
	stale  *staleLeaseMutation
}

type staleLeaseMutation struct {
	db *gorm.DB
	id uint
}

func (d *leaseRecordingDispatcher) DispatchOutbound(_ context.Context, call service.OutboundDispatchCall) service.OutboundDeliveryResult {
	d.owners = append(d.owners, call.Delivery.LeaseOwner)
	if d.stale != nil && d.stale.id == call.Delivery.ID {
		if err := d.stale.db.Model(&db.OutboundDelivery{}).Where("id = ?", d.stale.id).Update("lease_owner", "other-process-owner").Error; err != nil {
			return service.OutboundDeliveryResult{Retryable: false, Code: "test_mutation_failed", Message: err.Error()}
		}
	}
	return service.OutboundDeliveryResult{Delivered: true}
}

func TestOutboundLeaseOwnersAreUniqueAndStaleOwnerCannotWriteBack(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &leaseRecordingDispatcher{}
	svc.OutboundDispatcher = dispatcher
	makeIntent := func(key string) service.OutboundDeliveryIntent {
		return service.OutboundDeliveryIntent{
			EventType: service.OutboundEventPullRequestMerged, TargetName: "department_official", TargetType: service.OutboundTargetTypeFeishuWebhook,
			IdempotencyKey: key, PayloadVersion: "v1", PayloadJSON: `{"text":"lease"}`,
		}
	}
	first, _, err := svc.EnqueueOutboundDelivery(context.Background(), makeIntent("lease-owner-a"))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.EnqueueOutboundDelivery(context.Background(), makeIntent("lease-owner-b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeliverOutboundDeliveryNow(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeliverOutboundDeliveryNow(context.Background(), second.ID); err != nil {
		t.Fatal(err)
	}
	if len(dispatcher.owners) != 2 || dispatcher.owners[0] == "" || dispatcher.owners[0] == dispatcher.owners[1] {
		t.Fatalf("first claims reused a lease owner: %#v", dispatcher.owners)
	}

	stale, _, err := svc.EnqueueOutboundDelivery(context.Background(), makeIntent("lease-owner-stale"))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.stale = &staleLeaseMutation{db: svc.DB, id: stale.ID}
	err = svc.DeliverOutboundDeliveryNow(context.Background(), stale.ID)
	if err == nil || !strings.Contains(err.Error(), "lease lost") {
		t.Fatalf("stale owner write-back was accepted: %v", err)
	}
	var stored db.OutboundDelivery
	if err := svc.DB.First(&stored, stale.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != service.OutboundDeliveryStatusDelivering || stored.LeaseOwner != "other-process-owner" {
		t.Fatalf("stale owner overwrote replacement lease: %#v", stored)
	}
}

func TestOutboundActiveLeaseClaimIsNoOp(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{}
	svc.OutboundDispatcher = dispatcher
	row, _, err := svc.EnqueueOutboundDelivery(context.Background(), service.OutboundDeliveryIntent{
		EventType: service.OutboundEventPullRequestMerged, TargetName: "department_official", TargetType: service.OutboundTargetTypeFeishuWebhook,
		IdempotencyKey: "active-lease-no-op", PayloadVersion: "v1", PayloadJSON: `{"text":"active"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Minute)
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("id = ?", row.ID).Updates(map[string]any{
		"status": service.OutboundDeliveryStatusDelivering, "lease_owner": "active-process-owner", "lease_expires_at": future,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DeliverOutboundDeliveryNow(context.Background(), row.ID); err != nil {
		t.Fatalf("active lease no-op claim returned error: %v", err)
	}
	var stored db.OutboundDelivery
	if err := svc.DB.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != service.OutboundDeliveryStatusDelivering || stored.LeaseOwner != "active-process-owner" || len(dispatcher.calls) != 0 {
		t.Fatalf("active lease was unexpectedly reclaimed: row=%#v calls=%d", stored, len(dispatcher.calls))
	}
}

func TestMergePRCreatesOutboundDeliveryAndIgnoresDeliveryFailure(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{results: []service.OutboundDeliveryResult{{Delivered: false, Retryable: true, Code: "feishu_11232", Message: "frequency limited"}}}
	svc.OutboundDispatcher = dispatcher
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{
		service.OutboundEventPullRequestMerged: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}},
	}

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "outbound-merge", "repo")
	merged, err := svc.MergePR(authCtx, pr.Repository.FullName, pr.Number, "merge", "")
	if err != nil {
		t.Fatalf("MergePR should ignore outbound delivery failure: %v", err)
	}
	if merged.MergeCommitSHA == "" {
		t.Fatal("expected merge sha")
	}

	var rows []db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventPullRequestMerged).Find(&rows).Error; err != nil {
		t.Fatalf("list outbound deliveries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one outbound delivery, got %#v", rows)
	}
	row := rows[0]
	if row.Status != service.OutboundDeliveryStatusRetryWait || row.TargetName != "department_official" || row.RepoFullName != pr.Repository.FullName || row.PRNumber != pr.Number || row.MergeSHA != merged.MergeCommitSHA {
		t.Fatalf("outbound delivery mismatch: %#v", row)
	}
	if len(dispatcher.calls) != 1 {
		t.Fatalf("expected one dispatch attempt, got %d", len(dispatcher.calls))
	}
}
