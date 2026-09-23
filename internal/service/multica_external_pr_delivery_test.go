package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
	"gorm.io/gorm"
)

func configureMulticaTerminalDelivery(svc *service.Service) {
	svc.MulticaProjection = multicaprojection.New(multicaprojection.Config{
		Enabled:        true,
		TargetInstance: "mini-prod",
		AppURL:         "https://multica.example",
	})
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{
		service.OutboundEventMulticaExternalPRTerminal: {{
			Name: service.OutboundTargetNameMulticaExternalPR,
			Type: service.OutboundTargetTypeMulticaExternalPR,
		}},
	}
}

func addAuthoritativeMulticaLink(t testing.TB, svc *service.Service, pr db.PullRequest) {
	t.Helper()
	if err := svc.DB.Create(&db.PullRequestMulticaLink{
		PullRequestID:    pr.ID,
		RepositoryID:     pr.RepositoryID,
		Workspace:        "workspace-alpha",
		WorkspaceID:      "workspace-id",
		IssueID:          "issue-id",
		IssueKey:         "HUM-42",
		Confidence:       db.MulticaLinkConfidenceAuthoritative,
		Source:           db.MulticaLinkSourceTaskToken,
		CompletionIntent: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentForgejoMergesKeepOneImmutableSHAAndDelivery(t *testing.T) {
	svc, cleanup := testharness.NewService(t, testharness.ServiceConfig{MaxOpenConns: 4})
	defer cleanup()
	configureMulticaTerminalDelivery(svc)
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	addAuthoritativeMulticaLink(t, svc, pr)
	shas := []string{strings.Repeat("a", 40), strings.Repeat("b", 40)}
	start := make(chan struct{})
	results := make(chan error, len(shas))
	for _, sha := range shas {
		sha := sha
		go func() {
			<-start
			_, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, sha, "forgejo")
			results <- err
		}()
	}
	close(start)
	var success, conflict int
	for range shas {
		err := <-results
		if err == nil {
			success++
		} else if strings.Contains(err.Error(), "terminal fact is immutable") {
			conflict++
		} else {
			t.Fatalf("concurrent merge returned unexpected error: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent merge outcomes success=%d conflict=%d", success, conflict)
	}
	var stored db.PullRequest
	if err := svc.DB.First(&stored, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !stored.Merged || (stored.MergeCommitSHA != shas[0] && stored.MergeCommitSHA != shas[1]) {
		t.Fatalf("concurrent merge changed immutable PR fact unexpectedly: %#v", stored)
	}
	var deliveries []db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Find(&deliveries).Error; err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].MergeSHA != stored.MergeCommitSHA {
		t.Fatalf("concurrent merge created duplicate or mismatched terminal delivery: %#v", deliveries)
	}
}

func TestCloseVsMergeBarrierKeepsCommittedMergeAsSingleTerminalDelivery(t *testing.T) {
	svc, cleanup := testharness.NewService(t, testharness.ServiceConfig{MaxOpenConns: 4})
	defer cleanup()
	configureMulticaTerminalDelivery(svc)
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	addAuthoritativeMulticaLink(t, svc, pr)
	mergeSHA := strings.Repeat("c", 40)

	// Hold the close caller behind a deterministic barrier, let the merge
	// commit, then release close. The close transaction must re-read merged=true
	// and must not append a closed-unmerged delivery.
	closeGate := make(chan struct{})
	closeStarted := make(chan struct{})
	closeResult := make(chan error, 1)
	go func() {
		close(closeStarted)
		<-closeGate
		_, err := svc.MarkPullRequestClosedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, "forgejo")
		closeResult <- err
	}()
	<-closeStarted
	if _, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, mergeSHA, "forgejo"); err != nil {
		t.Fatalf("merge before close barrier: %v", err)
	}
	close(closeGate)
	if err := <-closeResult; err != nil {
		t.Fatalf("close after committed merge: %v", err)
	}

	stored, err := svc.GetPR(ctx, pr.Repository.FullName, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Merged || stored.MergeCommitSHA != mergeSHA {
		t.Fatalf("close changed merged terminal fact: %#v", stored)
	}
	var deliveries []db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Find(&deliveries).Error; err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].MergeSHA != mergeSHA || !strings.Contains(string(deliveries[0].PayloadJSON), `"state":"merged"`) {
		t.Fatalf("close-vs-merge produced an incorrect terminal side effect: %#v", deliveries)
	}
}

func TestConcurrentForgejoClosesReuseOneObservedAtPayloadAndDelivery(t *testing.T) {
	svc, cleanup := testharness.NewService(t, testharness.ServiceConfig{MaxOpenConns: 4})
	defer cleanup()
	configureMulticaTerminalDelivery(svc)
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	addAuthoritativeMulticaLink(t, svc, pr)

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := svc.MarkPullRequestClosedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, "forgejo")
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent duplicate close: %v", err)
		}
	}

	stored, err := svc.GetPR(ctx, pr.Repository.FullName, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Merged || stored.State != db.StateClosed || stored.ClosedAt == nil {
		t.Fatalf("duplicate close changed terminal fact unexpectedly: %#v", stored)
	}
	var deliveries []db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Find(&deliveries).Error; err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].MergeSHA != "" {
		t.Fatalf("duplicate close produced duplicate or merged delivery: %#v", deliveries)
	}
	var envelope multicaprojection.ExternalPRTerminalDelivery
	if err := json.Unmarshal([]byte(deliveries[0].PayloadJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Source.ObservedAt != stored.ClosedAt.UTC().Format(time.RFC3339) || envelope.Request.State != "closed" || envelope.Request.CompletionIntent {
		t.Fatalf("duplicate close payload is not stable/correct: envelope=%#v stored=%#v", envelope, stored)
	}
}

func TestProjectedMergeRejectsNoncanonicalSHAWhenTypedDeliveryIsDisabled(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	for _, sha := range []string{"", strings.Repeat("a", 39), strings.Repeat("A", 40)} {
		if _, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", pr.Number, sha, "forgejo"); err == nil {
			t.Fatalf("noncanonical merge SHA %q was accepted with typed delivery disabled", sha)
		}
		stored, err := svc.GetPR(ctx, pr.Repository.FullName, pr.Number)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Merged || stored.MergeCommitSHA != "" {
			t.Fatalf("invalid merge fact escaped transaction: %#v", stored)
		}
	}
}

func TestMarkProjectedMergePersistsTypedMulticaDeliveryInTerminalTransaction(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	configureMulticaTerminalDelivery(svc)
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	addAuthoritativeMulticaLink(t, svc, pr)

	merged, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, strings.Repeat("b", 40), "forgejo")
	if err != nil {
		t.Fatal(err)
	}
	if !merged.Merged {
		t.Fatalf("PR did not become terminal: %#v", merged)
	}
	var rows []db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != service.OutboundDeliveryStatusPending || rows[0].TargetType != service.OutboundTargetTypeMulticaExternalPR {
		t.Fatalf("typed delivery row=%#v", rows)
	}
	payload := string(rows[0].PayloadJSON)
	for _, forbidden := range []string{"service-token", "Authorization", "Bearer"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("secret-shaped value leaked in durable payload: %q", forbidden)
		}
	}
	if !strings.Contains(payload, `"schema":"ags.multica-external-pr-projection.v1"`) || !strings.Contains(payload, `"request"`) || !strings.Contains(payload, `"provider":"ags"`) || !strings.Contains(payload, `"external_repo":"proj-user/repo"`) {
		t.Fatalf("payload is not the typed wrapper with canonical AGS request: %s", payload)
	}
	var delivery multicaprojection.ExternalPRTerminalDelivery
	if err := json.Unmarshal([]byte(payload), &delivery); err != nil {
		t.Fatal(err)
	}
	if !delivery.Request.CompletionIntent {
		t.Fatalf("business-constructed merged delivery lost completion intent: %#v", delivery.Request)
	}

	if _, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, strings.Repeat("b", 40), "forgejo"); err != nil {
		t.Fatalf("duplicate webhook must reuse durable row: %v", err)
	}
	var count int64
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate terminal fact created %d rows", count)
	}
}

func TestProjectedMergeRejectsChangedOrEmptyTerminalSHAWithoutSecondRow(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	configureMulticaTerminalDelivery(svc)
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	addAuthoritativeMulticaLink(t, svc, pr)
	shaA := strings.Repeat("a", 40)
	if _, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, shaA, "forgejo"); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{"", strings.Repeat("b", 40)} {
		if _, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, sha, "forgejo"); err == nil {
			t.Fatalf("expected terminal SHA conflict for %q", sha)
		}
	}
	stored, err := svc.GetPR(ctx, pr.Repository.FullName, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MergeCommitSHA != shaA {
		t.Fatalf("merged SHA was overwritten: %#v", stored)
	}
	var count int64
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("conflicting terminal fact created another delivery row: %d", count)
	}
}

func TestProjectedMergeRollsBackWhenTypedDeliveryCannotPersist(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	configureMulticaTerminalDelivery(svc)
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	addAuthoritativeMulticaLink(t, svc, pr)
	callbackName := "test:reject_multica_terminal_delivery"
	if err := svc.DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "OutboundDelivery" {
			tx.AddError(errors.New("forced typed delivery persistence failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.DB.Callback().Create().Remove(callbackName) })
	if _, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, strings.Repeat("c", 40), "forgejo"); err == nil {
		t.Fatal("expected terminal transaction failure")
	}
	unchanged, err := svc.GetPR(ctx, pr.Repository.FullName, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Merged {
		t.Fatalf("terminal PR fact escaped failed delivery transaction: %#v", unchanged)
	}
	var count int64
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("delivery row escaped failed transaction: %d", count)
	}
}

func TestExpiredFeishuLeaseIsNotReplayedByTypedReclaimer(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{}
	svc.OutboundDispatcher = dispatcher
	row, _, err := svc.EnqueueOutboundDelivery(context.Background(), service.OutboundDeliveryIntent{
		EventType: service.OutboundEventPullRequestMerged, TargetName: "feishu", TargetType: service.OutboundTargetTypeFeishuWebhook,
		IdempotencyKey: "feishu-expired-lease", PayloadVersion: "v1", PayloadJSON: `{"text":"notification"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().UTC().Add(-time.Minute)
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("id = ?", row.ID).Updates(map[string]any{"status": service.OutboundDeliveryStatusDelivering, "lease_owner": "dead-process", "lease_expires_at": expired}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessDueOutboundDeliveries(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var stored db.OutboundDelivery
	if err := svc.DB.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != service.OutboundDeliveryStatusDelivering || len(dispatcher.calls) != 0 {
		t.Fatalf("expired Feishu lease was replayed: row=%#v calls=%d", stored, len(dispatcher.calls))
	}
}

func TestTypedOutboundSameKeyPayloadTargetAndEventConflictFailsClosed(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	request := multicaprojection.ExternalPRLinkRequest{
		Provider: "ags", IssueID: "issue-id", WorkspaceID: "workspace-id", Workspace: "workspace-alpha", IssueKey: "HUM-42",
		ExternalRepo: "owner/repo", ExternalNumber: 7, ExternalURL: "https://ags.example/owner/repo/pull/7",
		MergeProvider: "forgejo", MergeRepo: "forgejo/repo", MergeNumber: 42, MergeURL: "https://forgejo.example/forgejo/repo/pulls/42",
		MergedSHA: strings.Repeat("b", 40), CompletionIntent: true, LinkConfidence: "authoritative", State: "merged",
	}
	delivery, err := multicaprojection.NewExternalPRTerminalDelivery(multicaprojection.ExternalPRTerminalDeliveryInput{TargetInstance: "mini-prod", Request: request, AGSPrID: "owner/repo#7"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := multicaprojection.MarshalExternalPRTerminalDelivery(delivery)
	if err != nil {
		t.Fatal(err)
	}
	intent := service.OutboundDeliveryIntent{EventType: service.OutboundEventMulticaExternalPRTerminal, TargetName: service.OutboundTargetNameMulticaExternalPR, TargetType: service.OutboundTargetTypeMulticaExternalPR, IdempotencyKey: delivery.IdempotencyKey, PayloadVersion: multicaprojection.ExternalPRTerminalDeliverySchema, PayloadJSON: payload}
	if _, created, err := svc.EnqueueOutboundDelivery(context.Background(), intent); err != nil || !created {
		t.Fatalf("first typed enqueue: created=%v err=%v", created, err)
	}
	changed := strings.Replace(payload, `"issue_key":"HUM-42"`, `"issue_key":"HUM-43"`, 1)
	intent.PayloadJSON = changed
	if _, _, err := svc.EnqueueOutboundDelivery(context.Background(), intent); err == nil {
		t.Fatal("same-key typed payload conflict was silently reused")
	}
	intent.PayloadJSON = payload
	intent.EventType = service.OutboundEventPullRequestMerged
	if _, _, err := svc.EnqueueOutboundDelivery(context.Background(), intent); err == nil {
		t.Fatal("typed row was allowed to conflict with a generic event")
	}
}

func TestGenericOutboundIdempotencyConflictsFailClosed(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	base := service.OutboundDeliveryIntent{
		EventType: service.OutboundEventPullRequestMerged, TargetName: "feishu-a", TargetType: service.OutboundTargetTypeFeishuWebhook,
		IdempotencyKey: "generic-same-key", PayloadVersion: "v1", PayloadJSON: `{"text":"one"}`,
	}
	if _, created, err := svc.EnqueueOutboundDelivery(context.Background(), base); err != nil || !created {
		t.Fatalf("seed generic delivery: created=%v err=%v", created, err)
	}
	cases := []struct {
		name   string
		change func(*service.OutboundDeliveryIntent)
	}{
		{name: "event", change: func(intent *service.OutboundDeliveryIntent) { intent.EventType = service.OutboundEventProjectionDrift }},
		{name: "target name", change: func(intent *service.OutboundDeliveryIntent) { intent.TargetName = "feishu-b" }},
		{name: "target type", change: func(intent *service.OutboundDeliveryIntent) {
			intent.TargetType = service.OutboundTargetTypeMulticaExternalPR
		}},
		{name: "payload version", change: func(intent *service.OutboundDeliveryIntent) { intent.PayloadVersion = "v2" }},
		{name: "payload", change: func(intent *service.OutboundDeliveryIntent) { intent.PayloadJSON = `{"text":"two"}` }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent := base
			tc.change(&intent)
			if _, _, err := svc.EnqueueOutboundDelivery(context.Background(), intent); err == nil {
				t.Fatal("same-key generic conflict was silently reused")
			}
		})
	}
}

func TestAGSOriginatedCloseEnqueuesInCloseTransactionAndRollsBackOnFailure(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	configureMulticaTerminalDelivery(svc)
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	addAuthoritativeMulticaLink(t, svc, pr)
	closed := db.StateClosed
	if _, err := svc.UpdatePR(ctx, pr.Repository.FullName, pr.Number, service.UpdatePRInput{State: &closed}); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.GetPR(ctx, pr.Repository.FullName, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != db.StateClosed {
		t.Fatalf("AGS close did not commit: %#v", stored)
	}
	var count int64
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("AGS close did not enqueue typed delivery in transaction: %d", count)
	}
	var delivery db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).First(&delivery).Error; err != nil {
		t.Fatal(err)
	}
	var requestEnvelope multicaprojection.ExternalPRTerminalDelivery
	if err := json.Unmarshal([]byte(delivery.PayloadJSON), &requestEnvelope); err != nil {
		t.Fatal(err)
	}
	if requestEnvelope.Request.CompletionIntent || requestEnvelope.Request.MergedSHA != "" {
		t.Fatalf("closed delivery incorrectly claims completion or carries merge SHA: %#v", requestEnvelope.Request)
	}

	svc2, cleanup2 := setupTestService(t)
	defer cleanup2()
	configureMulticaTerminalDelivery(svc2)
	pr2 := createProjectedPRForLifecycleTest(t, svc2, ctx)
	addAuthoritativeMulticaLink(t, svc2, pr2)
	callbackName := "test:reject_ags_close_terminal_delivery"
	if err := svc2.DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "OutboundDelivery" {
			tx.AddError(errors.New("forced close delivery failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc2.DB.Callback().Create().Remove(callbackName) })
	if _, err := svc2.UpdatePR(ctx, pr2.Repository.FullName, pr2.Number, service.UpdatePRInput{State: &closed}); err == nil {
		t.Fatal("expected AGS close transaction failure")
	}
	unchanged, err := svc2.GetPR(ctx, pr2.Repository.FullName, pr2.Number)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != db.StateOpen {
		t.Fatalf("AGS close escaped failed typed delivery transaction: %#v", unchanged)
	}
}

func TestAGSOriginatedDuplicateCloseReusesTerminalTimestampAndRepairsMissingDelivery(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	configureMulticaTerminalDelivery(svc)
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	addAuthoritativeMulticaLink(t, svc, pr)
	closed := db.StateClosed
	if _, err := svc.UpdatePR(ctx, pr.Repository.FullName, pr.Number, service.UpdatePRInput{State: &closed}); err != nil {
		t.Fatal(err)
	}
	var first db.PullRequest
	if err := svc.DB.First(&first, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	var firstDelivery db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).First(&firstDelivery).Error; err != nil {
		t.Fatal(err)
	}
	firstPayload := string(firstDelivery.PayloadJSON)
	if first.ClosedAt == nil {
		t.Fatal("first close did not persist terminal timestamp")
	}
	if err := svc.DB.Delete(&db.OutboundDelivery{}, firstDelivery.ID).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := svc.UpdatePR(ctx, pr.Repository.FullName, pr.Number, service.UpdatePRInput{State: &closed}); err != nil {
		t.Fatal(err)
	}
	var second db.PullRequest
	if err := svc.DB.First(&second, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	var secondDelivery db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).First(&secondDelivery).Error; err != nil {
		t.Fatal(err)
	}
	if second.ClosedAt == nil || !second.ClosedAt.Equal(*first.ClosedAt) || string(secondDelivery.PayloadJSON) != firstPayload {
		t.Fatalf("duplicate UpdatePR changed terminal identity: first=%#v second=%#v payload_equal=%v", first.ClosedAt, second.ClosedAt, string(secondDelivery.PayloadJSON) == firstPayload)
	}
	if err := svc.DB.Delete(&db.OutboundDelivery{}, secondDelivery.ID).Error; err != nil {
		t.Fatal(err)
	}

	if err := svc.UpdatePRByID(ctx, pr.ID, &closed, nil); err != nil {
		t.Fatal(err)
	}
	var third db.PullRequest
	if err := svc.DB.First(&third, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	var thirdDelivery db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).First(&thirdDelivery).Error; err != nil {
		t.Fatal(err)
	}
	if third.ClosedAt == nil || !third.ClosedAt.Equal(*first.ClosedAt) || string(thirdDelivery.PayloadJSON) != firstPayload {
		t.Fatalf("duplicate UpdatePRByID changed terminal identity: first=%#v third=%#v payload_equal=%v", first.ClosedAt, third.ClosedAt, string(thirdDelivery.PayloadJSON) == firstPayload)
	}
}

func TestAGSOriginatedUpdatePRByIDInitialCloseUsesStableTimestampAndRepairsDelivery(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	configureMulticaTerminalDelivery(svc)
	ctx := context.Background()
	pr, _, _ := setupPRWithRealBranches(t, svc, "close-by-id", "repo")
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "forgejo/repo", ExternalNumber: 42, ExternalURL: "http://forgejo.local/forgejo/repo/pulls/42",
		SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: service.ProjectionStateOpen,
	}); err != nil {
		t.Fatal(err)
	}
	addAuthoritativeMulticaLink(t, svc, pr)
	closed := db.StateClosed
	if err := svc.UpdatePRByID(ctx, pr.ID, &closed, nil); err != nil {
		t.Fatal(err)
	}
	var first db.PullRequest
	if err := svc.DB.First(&first, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	var firstDelivery db.OutboundDelivery
	if err := svc.DB.Where("event_type = ? AND repo_full_name = ? AND pr_number = ?", service.OutboundEventMulticaExternalPRTerminal, pr.Repository.FullName, pr.Number).First(&firstDelivery).Error; err != nil {
		t.Fatalf("load first UpdatePRByID delivery: %v", err)
	}
	if first.ClosedAt == nil {
		t.Fatal("UpdatePRByID did not persist closed_at")
	}
	firstPayload := string(firstDelivery.PayloadJSON)
	if err := svc.DB.Delete(&db.OutboundDelivery{}, firstDelivery.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdatePRByID(ctx, pr.ID, &closed, nil); err != nil {
		t.Fatal(err)
	}
	var second db.PullRequest
	if err := svc.DB.First(&second, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	var secondDelivery db.OutboundDelivery
	if err := svc.DB.Where("event_type = ? AND repo_full_name = ? AND pr_number = ?", service.OutboundEventMulticaExternalPRTerminal, pr.Repository.FullName, pr.Number).First(&secondDelivery).Error; err != nil {
		t.Fatalf("load repaired UpdatePRByID delivery: %v", err)
	}
	if second.ClosedAt == nil || !second.ClosedAt.Equal(*first.ClosedAt) || string(secondDelivery.PayloadJSON) != firstPayload {
		t.Fatalf("UpdatePRByID changed terminal identity: first=%#v second=%#v payload_equal=%v", first.ClosedAt, second.ClosedAt, string(secondDelivery.PayloadJSON) == firstPayload)
	}
}

func TestOutboundWorkerReclaimsExpiredMulticaLeaseAndDeadLettersTerminalFailure(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	dispatcher := &captureOutboundDispatcher{results: []service.OutboundDeliveryResult{{Delivered: false, Retryable: false, Code: "schema_conflict", Message: "typed contract rejected"}}}
	svc.OutboundDispatcher = dispatcher
	request := multicaprojection.ExternalPRLinkRequest{
		Provider: "ags", IssueID: "issue-id", WorkspaceID: "workspace-id", Workspace: "workspace-alpha", IssueKey: "HUM-42",
		ExternalRepo: "owner/repo", ExternalNumber: 7, ExternalURL: "https://ags.example/owner/repo/pull/7",
		MergeProvider: "forgejo", MergeRepo: "forgejo/repo", MergeNumber: 42, MergeURL: "https://forgejo.example/forgejo/repo/pulls/42",
		MergedSHA: strings.Repeat("b", 40), CompletionIntent: true, LinkConfidence: "authoritative", State: "merged",
	}
	delivery, err := multicaprojection.NewExternalPRTerminalDelivery(multicaprojection.ExternalPRTerminalDeliveryInput{TargetInstance: "mini-prod", Request: request, AGSPrID: "owner/repo#7"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := multicaprojection.MarshalExternalPRTerminalDelivery(delivery)
	if err != nil {
		t.Fatal(err)
	}
	row, _, err := svc.EnqueueOutboundDelivery(context.Background(), service.OutboundDeliveryIntent{
		EventType:      service.OutboundEventMulticaExternalPRTerminal,
		TargetName:     service.OutboundTargetNameMulticaExternalPR,
		TargetType:     service.OutboundTargetTypeMulticaExternalPR,
		IdempotencyKey: delivery.IdempotencyKey,
		PayloadVersion: multicaprojection.ExternalPRTerminalDeliverySchema,
		PayloadJSON:    payload,
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().UTC().Add(-time.Minute)
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("id = ?", row.ID).Updates(map[string]any{
		"status":           service.OutboundDeliveryStatusDelivering,
		"lease_owner":      "dead-process",
		"lease_expires_at": expired,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessDueOutboundDeliveries(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var stored db.OutboundDelivery
	if err := svc.DB.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != service.OutboundDeliveryStatusDeadLetter || stored.AttemptCount != 1 || len(dispatcher.calls) != 1 {
		t.Fatalf("expired lease was not reclaimed and terminalized: row=%#v calls=%d", stored, len(dispatcher.calls))
	}
}
