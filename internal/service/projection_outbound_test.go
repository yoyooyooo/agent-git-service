package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/gorm"
)

func TestRecordForgejoProjectionFailureImmediatelyCreatesDurableOutbound(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	owner := db.User{Login: "projection-alert", Name: "projection-alert", Type: db.TypeUser}
	if err := svc.DB.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	repo := db.Repository{OwnerID: owner.ID, Owner: owner, Name: "demo", FullName: "projection-alert/demo", DefaultBranch: "main"}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	dispatcher := &captureOutboundDispatcher{}
	svc.OutboundDispatcher = dispatcher
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{
		service.OutboundEventProjectionDrift: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}},
	}

	projectionErr := &forgejointegration.ProjectionError{
		Type: forgejointegration.ProjectionFailureNonFastForward, Repo: repo.FullName, TargetRepo: "mirror/demo",
		Ref: "refs/heads/agent/rebase", Branch: "agent/rebase", ExpectedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActualSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ErrorSummary: "lease rejected",
	}
	if err := svc.RecordForgejoProjectionFailureWithAlert(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: projectionErr.Ref, After: projectionErr.ExpectedSHA}}, projectionErr, service.ProjectionFailureAlertMetadata{}); err != nil {
		t.Fatalf("RecordForgejoProjectionFailure: %v", err)
	}

	var deliveries []db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventProjectionDrift).Find(&deliveries).Error; err != nil {
		t.Fatalf("list outbound deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected one immediate durable outbound delivery, got %d", len(deliveries))
	}
	if deliveries[0].Status != service.OutboundDeliveryStatusDelivered || len(dispatcher.calls) != 1 {
		t.Fatalf("expected immediate delivery attempt, row=%#v calls=%d", deliveries[0], len(dispatcher.calls))
	}
}

func TestRecordProjectionFailureWithoutAlertTargetReturnsExplicitOutcomeAndKeepsDrift(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	owner := db.User{Login: "projection-no-target", Name: "projection-no-target", Type: db.TypeUser}
	if err := svc.DB.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	repo := db.Repository{OwnerID: owner.ID, Name: "demo", FullName: "projection-no-target/demo", DefaultBranch: "main"}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	projectionErr := &forgejointegration.ProjectionError{Type: forgejointegration.ProjectionFailureSHADrift, Repo: repo.FullName, Ref: "refs/heads/agent/rebase", ExpectedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ActualSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ErrorSummary: "drift"}
	err := svc.RecordForgejoProjectionFailureWithAlert(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: projectionErr.Ref, After: projectionErr.ExpectedSHA}}, projectionErr, service.ProjectionFailureAlertMetadata{})
	var alertErr *service.ProjectionAlertingError
	if !errors.As(err, &alertErr) || alertErr.Outcome != service.ProjectionAlertingOutcomeTargetMissing {
		t.Fatalf("expected explicit target_missing alert outcome, got %v", err)
	}
	var states []db.ProjectionRefState
	if err := svc.DB.Where("repository_id = ? AND status = ?", repo.ID, service.ProjectionStatusActive).Find(&states).Error; err != nil {
		t.Fatalf("load durable drift: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("alert config failure rolled back durable drift: %#v", states)
	}
}

func TestProjectionFailureAndOutboundIntentRollbackTogetherWhenIntentPersistenceFails(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	owner := db.User{Login: "projection-tx", Name: "projection-tx", Type: db.TypeUser}
	if err := svc.DB.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	repo := db.Repository{OwnerID: owner.ID, Name: "demo", FullName: "projection-tx/demo", DefaultBranch: "main"}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	svc.OutboundDispatcher = &captureOutboundDispatcher{}
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{service.OutboundEventProjectionDrift: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}}}
	if err := svc.DB.Callback().Create().Before("gorm:create").Register("test:fail_projection_outbound_intent", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "OutboundDelivery" {
			tx.AddError(errors.New("forced outbound intent persistence failure"))
		}
	}); err != nil {
		t.Fatalf("register outbound failure callback: %v", err)
	}
	t.Cleanup(func() { _ = svc.DB.Callback().Create().Remove("test:fail_projection_outbound_intent") })
	projectionErr := &forgejointegration.ProjectionError{Type: forgejointegration.ProjectionFailureSHADrift, Repo: repo.FullName, Ref: "refs/heads/agent/rebase", ExpectedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ActualSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ErrorSummary: "drift"}
	err := svc.RecordForgejoProjectionFailureWithAlert(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: projectionErr.Ref, After: projectionErr.ExpectedSHA}}, projectionErr, service.ProjectionFailureAlertMetadata{})
	if err == nil || service.IsProjectionAlertingError(err) {
		t.Fatalf("intent persistence failure must be a transaction error, got %v", err)
	}
	for name, model := range map[string]any{"event": &db.ProjectionEvent{}, "state": &db.ProjectionRefState{}, "delivery": &db.OutboundDelivery{}} {
		var count int64
		if err := svc.DB.Model(model).Count(&count).Error; err != nil {
			t.Fatalf("count %s rows: %v", name, err)
		}
		if count != 0 {
			t.Fatalf("%s rows survived rolled-back drift/intent transaction: %d", name, count)
		}
	}
}

func TestProjectionFailureWithTargetButNoDispatcherReturnsExplicitOutcomeAndKeepsPendingIntent(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	owner := db.User{Login: "projection-no-dispatcher", Name: "projection-no-dispatcher", Type: db.TypeUser}
	if err := svc.DB.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	repo := db.Repository{OwnerID: owner.ID, Name: "demo", FullName: "projection-no-dispatcher/demo", DefaultBranch: "main"}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{service.OutboundEventProjectionDrift: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}}}
	projectionErr := &forgejointegration.ProjectionError{Type: forgejointegration.ProjectionFailureSHADrift, Repo: repo.FullName, Ref: "refs/heads/agent/rebase", ExpectedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ActualSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ErrorSummary: "drift"}
	err := svc.RecordForgejoProjectionFailureWithAlert(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: projectionErr.Ref, After: projectionErr.ExpectedSHA}}, projectionErr, service.ProjectionFailureAlertMetadata{})
	var alertErr *service.ProjectionAlertingError
	if !errors.As(err, &alertErr) || alertErr.Outcome != service.ProjectionAlertingOutcomeDispatcherMissing {
		t.Fatalf("expected explicit dispatcher_missing outcome, got %v", err)
	}
	var delivery db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventProjectionDrift).First(&delivery).Error; err != nil {
		t.Fatalf("load durable pending delivery: %v", err)
	}
	if delivery.Status != service.OutboundDeliveryStatusPending {
		t.Fatalf("dispatcher config failure should leave durable pending intent, got %#v", delivery)
	}
	var state db.ProjectionRefState
	if err := svc.DB.Where("repository_id = ? AND status = ?", repo.ID, service.ProjectionStatusActive).First(&state).Error; err != nil {
		t.Fatalf("dispatcher config failure lost durable drift: %v", err)
	}
}

func TestProjectionDriftRetryReachesDeliveredAndMaxAttemptsDeadLetters(t *testing.T) {
	for _, tc := range []struct {
		name       string
		second     service.OutboundDeliveryResult
		wantStatus string
	}{
		{name: "retry succeeds", second: service.OutboundDeliveryResult{Delivered: true}, wantStatus: service.OutboundDeliveryStatusDelivered},
		{name: "retry exhausted", second: service.OutboundDeliveryResult{Retryable: true, Code: "network_error", Message: "still unavailable"}, wantStatus: service.OutboundDeliveryStatusDeadLetter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, cleanup := setupTestService(t)
			defer cleanup()
			ctx := context.Background()
			owner := db.User{Login: "projection-retry-" + strings.ReplaceAll(tc.name, " ", "-"), Name: tc.name, Type: db.TypeUser}
			if err := svc.DB.Create(&owner).Error; err != nil {
				t.Fatalf("create owner: %v", err)
			}
			repo := db.Repository{OwnerID: owner.ID, Name: "demo", FullName: owner.Login + "/demo", DefaultBranch: "main"}
			if err := svc.DB.Create(&repo).Error; err != nil {
				t.Fatalf("create repo: %v", err)
			}
			dispatcher := &captureOutboundDispatcher{results: []service.OutboundDeliveryResult{
				{Retryable: true, Code: "feishu_11232", Message: "frequency limited"}, tc.second,
			}}
			svc.OutboundDispatcher = dispatcher
			svc.OutboundEventTargets = map[string][]service.OutboundTarget{service.OutboundEventProjectionDrift: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}}}
			projectionErr := &forgejointegration.ProjectionError{Type: forgejointegration.ProjectionFailureSHADrift, Repo: repo.FullName, Ref: "refs/heads/agent/rebase", ExpectedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ActualSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ErrorSummary: "drift"}
			if err := svc.RecordForgejoProjectionFailureWithAlert(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: projectionErr.Ref, After: projectionErr.ExpectedSHA}}, projectionErr, service.ProjectionFailureAlertMetadata{}); err != nil {
				t.Fatalf("record drift: %v", err)
			}
			var delivery db.OutboundDelivery
			if err := svc.DB.Where("event_type = ?", service.OutboundEventProjectionDrift).First(&delivery).Error; err != nil {
				t.Fatalf("load retry delivery: %v", err)
			}
			if delivery.Status != service.OutboundDeliveryStatusRetryWait {
				t.Fatalf("first retryable failure status=%s", delivery.Status)
			}
			past := time.Now().Add(-time.Minute)
			if err := svc.DB.Model(&delivery).Updates(map[string]any{"next_attempt_at": past, "max_attempts": 2}).Error; err != nil {
				t.Fatalf("make retry due: %v", err)
			}
			if _, err := svc.ProcessDueOutboundDeliveries(ctx, 10); err != nil {
				t.Fatalf("process retry: %v", err)
			}
			if err := svc.DB.First(&delivery, delivery.ID).Error; err != nil {
				t.Fatalf("reload delivery: %v", err)
			}
			if delivery.Status != tc.wantStatus || len(dispatcher.calls) != 2 {
				t.Fatalf("retry result status=%s calls=%d want=%s", delivery.Status, len(dispatcher.calls), tc.wantStatus)
			}
		})
	}
}

func TestProjectionAlertRedactsCredentialBearingSummaryAndURL(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	owner := db.User{Login: "projection-redact", Name: "projection-redact", Type: db.TypeUser}
	if err := svc.DB.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	repo := db.Repository{OwnerID: owner.ID, Name: "demo", FullName: "projection-redact/demo", DefaultBranch: "main"}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	svc.OutboundDispatcher = &captureOutboundDispatcher{}
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{service.OutboundEventProjectionDrift: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}}}
	projectionErr := &forgejointegration.ProjectionError{Type: forgejointegration.ProjectionFailureAuthFailed, Repo: repo.FullName, TargetRepo: "mirror/demo", Ref: "refs/heads/agent/rebase", ExpectedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ErrorSummary: "push https://secret-token@forgejo.example/mirror/demo.git?access_token=webhook-secret failed"}
	metadata := service.ProjectionFailureAlertMetadata{ForgejoPRNumber: 7, ForgejoPRURL: "https://secret-token@forgejo.example/mirror/demo/pulls/7?access_token=webhook-secret"}
	if err := svc.RecordForgejoProjectionFailureWithAlert(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: projectionErr.Ref, After: projectionErr.ExpectedSHA}}, projectionErr, metadata); err != nil {
		t.Fatalf("record redacted drift: %v", err)
	}
	var state db.ProjectionRefState
	if err := svc.DB.Where("repository_id = ?", repo.ID).First(&state).Error; err != nil {
		t.Fatalf("load state: %v", err)
	}
	var delivery db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventProjectionDrift).First(&delivery).Error; err != nil {
		t.Fatalf("load delivery: %v", err)
	}
	for label, text := range map[string]string{"state": string(state.ErrorSummary), "payload": string(delivery.PayloadJSON)} {
		if strings.Contains(text, "secret-token") || strings.Contains(text, "webhook-secret") {
			t.Fatalf("%s leaked credential: %s", label, text)
		}
	}
}

func TestResolveUnnotifiedProjectionDriftDoesNotEmitResolvedOutbound(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	owner := db.User{Login: "projection-unconfirmed", Name: "projection-unconfirmed", Type: db.TypeUser}
	if err := svc.DB.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	repo := db.Repository{OwnerID: owner.ID, Owner: owner, Name: "demo", FullName: "projection-unconfirmed/demo", DefaultBranch: "main"}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	dispatcher := &captureOutboundDispatcher{}
	svc.OutboundDispatcher = dispatcher
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{
		service.OutboundEventProjectionDrift: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}},
	}
	ref := "refs/pull/7/projection-integrity"
	projectionErr := &forgejointegration.ProjectionError{
		Type: forgejointegration.ProjectionFailurePullRequestProjectionMissing, Repo: repo.FullName, TargetRepo: "mirror/demo",
		Ref: ref, ExpectedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ErrorSummary: "transient bulk snapshot miss",
	}
	if err := svc.RecordForgejoProjectionFailure(ctx, repo.FullName, nil, projectionErr); err != nil {
		t.Fatalf("record unconfirmed drift: %v", err)
	}
	if err := svc.ResolveProjectionRefState(ctx, repo.FullName, service.ProjectionProviderForgejo, ref, projectionErr.ExpectedSHA, projectionErr.ExpectedSHA, time.Now().UTC()); err != nil {
		t.Fatalf("resolve unconfirmed drift: %v", err)
	}

	var deliveries int64
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("event_type = ?", service.OutboundEventProjectionDrift).Count(&deliveries).Error; err != nil {
		t.Fatalf("count projection deliveries: %v", err)
	}
	if deliveries != 0 || len(dispatcher.calls) != 0 {
		t.Fatalf("unconfirmed drift emitted resolved notification: deliveries=%d calls=%d", deliveries, len(dispatcher.calls))
	}
	var state db.ProjectionRefState
	if err := svc.DB.Where("repository_id = ? AND ref = ?", repo.ID, ref).First(&state).Error; err != nil {
		t.Fatalf("load resolved state: %v", err)
	}
	if state.Status != service.ProjectionStatusResolved || state.LastNotifiedAt != nil {
		t.Fatalf("unexpected unconfirmed resolution state: %#v", state)
	}
}

func TestProjectionDriftResolvedAndNewGenerationEachCreateOneOutbound(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	owner := db.User{Login: "projection-generation", Name: "projection-generation", Type: db.TypeUser}
	if err := svc.DB.Create(&owner).Error; err != nil {
		t.Fatalf("create owner: %v", err)
	}
	repo := db.Repository{OwnerID: owner.ID, Owner: owner, Name: "demo", FullName: "projection-generation/demo", DefaultBranch: "main"}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatalf("create repo: %v", err)
	}
	dispatcher := &captureOutboundDispatcher{}
	svc.OutboundDispatcher = dispatcher
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{
		service.OutboundEventProjectionDrift: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}},
	}
	ref := "refs/heads/agent/rebase"
	first := &forgejointegration.ProjectionError{
		Type: forgejointegration.ProjectionFailureNonFastForward, Repo: repo.FullName, TargetRepo: "mirror/demo",
		Ref: ref, Branch: "agent/rebase", ExpectedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActualSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ErrorSummary: "first lease rejected",
	}
	if err := svc.RecordForgejoProjectionFailureWithAlert(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: ref, After: first.ExpectedSHA}}, first, service.ProjectionFailureAlertMetadata{}); err != nil {
		t.Fatalf("record first drift: %v", err)
	}
	if err := svc.RecordForgejoProjectionFailureWithAlert(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: ref, After: first.ExpectedSHA}}, first, service.ProjectionFailureAlertMetadata{}); err != nil {
		t.Fatalf("record duplicate active drift: %v", err)
	}
	if len(dispatcher.calls) != 1 {
		t.Fatalf("duplicate active drift should be deduped, got %d calls", len(dispatcher.calls))
	}
	if err := svc.ResolveProjectionRefState(ctx, repo.FullName, service.ProjectionProviderForgejo, ref, first.ExpectedSHA, first.ExpectedSHA, time.Now().UTC()); err != nil {
		t.Fatalf("resolve drift: %v", err)
	}
	second := &forgejointegration.ProjectionError{
		Type: forgejointegration.ProjectionFailureSHADrift, Repo: repo.FullName, TargetRepo: "mirror/demo",
		Ref: ref, Branch: "agent/rebase", ExpectedSHA: "cccccccccccccccccccccccccccccccccccccccc",
		ActualSHA: "dddddddddddddddddddddddddddddddddddddddd", ErrorSummary: "new drift",
	}
	if err := svc.RecordForgejoProjectionFailureWithAlert(ctx, repo.FullName, []service.ForgejoRefChange{{Ref: ref, After: second.ExpectedSHA}}, second, service.ProjectionFailureAlertMetadata{}); err != nil {
		t.Fatalf("record new drift generation: %v", err)
	}

	var deliveries []db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventProjectionDrift).Order("id asc").Find(&deliveries).Error; err != nil {
		t.Fatalf("list projection deliveries: %v", err)
	}
	if len(deliveries) != 3 || len(dispatcher.calls) != 3 {
		t.Fatalf("expected active, resolved, and next-generation deliveries; rows=%d calls=%d", len(deliveries), len(dispatcher.calls))
	}
	if !strings.Contains(deliveries[0].IdempotencyKey, ":forgejo:active:") || !strings.Contains(deliveries[0].IdempotencyKey, ":g1:") || !strings.Contains(deliveries[1].IdempotencyKey, ":g1:") || !strings.Contains(deliveries[2].IdempotencyKey, ":g2:") {
		t.Fatalf("delivery generation keys=%q, %q, %q", deliveries[0].IdempotencyKey, deliveries[1].IdempotencyKey, deliveries[2].IdempotencyKey)
	}
	var state db.ProjectionRefState
	if err := svc.DB.Where("repository_id = ? AND ref = ?", repo.ID, ref).First(&state).Error; err != nil {
		t.Fatalf("load generated state: %v", err)
	}
	if state.Generation != 2 || state.Status != service.ProjectionStatusActive {
		t.Fatalf("new drift generation state=%#v", state)
	}
}
