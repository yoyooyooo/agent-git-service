package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/service"
)

func TestOutboundWorkerRecordsDatabasePollErrors(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	svc.OutboundDispatcher = &captureOutboundDispatcher{}
	sqlDB, err := svc.DB.DB()
	if err != nil {
		t.Fatalf("get sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close sql.DB: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.RunOutboundDeliveryWorker(ctx, time.Hour, 10)
		close(done)
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		health := svc.OutboundWorkerHealth()
		if health.LastError != "" {
			if !strings.Contains(health.LastError, "database clock") {
				t.Fatalf("unexpected outbound worker error: %q", health.LastError)
			}
			break
		}
		select {
		case <-deadline.C:
			cancel()
			<-done
			t.Fatal("outbound worker did not record database poll error")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("outbound worker did not stop")
	}
}

func TestProcessDueOutboundDeliveriesRetriesOnlyDueRows(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{results: []service.OutboundDeliveryResult{{Delivered: true}}}
	svc.OutboundDispatcher = dispatcher

	due, _, err := svc.EnqueueOutboundDelivery(context.Background(), service.OutboundDeliveryIntent{
		EventType:      service.OutboundEventPullRequestMerged,
		TargetName:     "department_official",
		TargetType:     service.OutboundTargetTypeFeishuWebhook,
		SubjectType:    "pull_request",
		SubjectKey:     "example-team/team-docs-fixture#2",
		IdempotencyKey: "due",
		PayloadVersion: "pr_merge_text_v1",
		PayloadJSON:    `{"text":"due"}`,
	})
	if err != nil {
		t.Fatalf("enqueue due: %v", err)
	}
	future, _, err := svc.EnqueueOutboundDelivery(context.Background(), service.OutboundDeliveryIntent{
		EventType:      service.OutboundEventPullRequestMerged,
		TargetName:     "department_official",
		TargetType:     service.OutboundTargetTypeFeishuWebhook,
		SubjectType:    "pull_request",
		SubjectKey:     "example-team/team-docs-fixture#3",
		IdempotencyKey: "future",
		PayloadVersion: "pr_merge_text_v1",
		PayloadJSON:    `{"text":"future"}`,
	})
	if err != nil {
		t.Fatalf("enqueue future: %v", err)
	}
	next := time.Now().Add(time.Hour)
	if err := svc.DB.Model(&future).Updates(map[string]any{"status": service.OutboundDeliveryStatusRetryWait, "next_attempt_at": &next}).Error; err != nil {
		t.Fatalf("set future retry: %v", err)
	}

	processed, err := svc.ProcessDueOutboundDeliveries(context.Background(), 10)
	if err != nil {
		t.Fatalf("ProcessDueOutboundDeliveries: %v", err)
	}
	if processed != 1 || len(dispatcher.calls) != 1 || dispatcher.calls[0].Delivery.ID != due.ID {
		t.Fatalf("expected only due delivery processed, processed=%d calls=%#v", processed, dispatcher.calls)
	}
}
