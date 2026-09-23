package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestRecordMulticaTaskFailureCreatesRollingRepoIncidentIssue(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	setupRepoForTest(t, svc, "incuser", "increpo")

	now := time.Date(2026, 6, 9, 10, 31, 0, 0, time.UTC)
	res, err := svc.RecordMulticaTaskFailure(ctx, service.MulticaTaskFailedInput{
		RepoFullName:      "incuser/increpo",
		WorkspaceID:       "ws-1",
		MulticaIssueKey:   "MUL-123",
		MulticaIssueTitle: "Fix checkout drift",
		TaskID:            "task-new",
		AgentID:           "lane-b-id",
		AgentName:         "lane-b",
		FailureReason:     "agent_error.provider_auth_or_access",
		Error:             "refresh token expired; raw token must not be printed",
		RuntimeProvider:   "codex",
		RuntimeID:         "runtime-1",
		Attempt:           2,
		MaxAttempts:       3,
		OccurredAt:        now,
	})
	if err != nil {
		t.Fatalf("RecordMulticaTaskFailure: %v", err)
	}
	if !res.EventAccepted {
		t.Fatal("first event must be accepted")
	}
	if !res.IssueCreated {
		t.Fatal("first event must create an incident issue")
	}
	if res.Issue.Number == 0 {
		t.Fatalf("incident issue number not populated: %#v", res.Issue)
	}
	if !strings.Contains(res.Issue.Title, "[multica-failure]") ||
		!strings.Contains(res.Issue.Title, "MUL-123") ||
		!strings.Contains(res.Issue.Title, "lane-b") ||
		!strings.Contains(res.Issue.Title, "agent_error.provider_auth_or_access") {
		t.Fatalf("unexpected incident title: %q", res.Issue.Title)
	}
	body := string(res.Issue.Body)
	for _, want := range []string{
		"window: last 30 days",
		"count: 1",
		"task-new",
		"MUL-123",
		"lane-b",
		"agent_error.provider_auth_or_access",
		"refresh token expired",
		"events_outside_window: 0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("incident body missing %q:\n%s", want, body)
		}
	}

	labels, err := svc.ListIssueLabels(ctx, "incuser/increpo", res.Issue.Number)
	if err != nil {
		t.Fatalf("ListIssueLabels: %v", err)
	}
	labelNames := map[string]bool{}
	for _, l := range labels {
		labelNames[l.Name] = true
	}
	for _, want := range []string{"source/multica", "kind/runtime-failure", "status/needs-triage"} {
		if !labelNames[want] {
			t.Fatalf("missing incident label %q; got %#v", want, labelNames)
		}
	}
}

func TestRecordMulticaTaskFailureDeduplicatesByTaskAndCompletedAt(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	setupRepoForTest(t, svc, "dedupeuser", "deduperepo")

	now := time.Date(2026, 6, 9, 10, 31, 0, 0, time.UTC)
	input := service.MulticaTaskFailedInput{
		RepoFullName:    "dedupeuser/deduperepo",
		WorkspaceID:     "ws-1",
		MulticaIssueKey: "MUL-124",
		TaskID:          "task-dup",
		AgentID:         "lane-c-id",
		AgentName:       "lane-c",
		FailureReason:   "timeout",
		Error:           "task timed out",
		RuntimeProvider: "codex",
		OccurredAt:      now,
	}
	first, err := svc.RecordMulticaTaskFailure(ctx, input)
	if err != nil {
		t.Fatalf("first RecordMulticaTaskFailure: %v", err)
	}
	second, err := svc.RecordMulticaTaskFailure(ctx, input)
	if err != nil {
		t.Fatalf("second RecordMulticaTaskFailure: %v", err)
	}
	if !first.EventAccepted || second.EventAccepted {
		t.Fatalf("dedupe mismatch: first=%v second=%v", first.EventAccepted, second.EventAccepted)
	}
	if first.Issue.ID != second.Issue.ID {
		t.Fatalf("deduped event should return same issue: first=%d second=%d", first.Issue.ID, second.Issue.ID)
	}

	var eventCount int64
	if err := svc.DB.Model(&db.ExternalEvent{}).Where("source = ?", "multica").Count(&eventCount).Error; err != nil {
		t.Fatalf("count external events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("external_events count = %d, want 1", eventCount)
	}
	var incidents []db.RepoIncident
	if err := svc.DB.Find(&incidents).Error; err != nil {
		t.Fatalf("list incidents: %v", err)
	}
	if len(incidents) != 1 || incidents[0].TotalEventCount != 1 || incidents[0].EventCount30d != 1 {
		t.Fatalf("unexpected incidents after dedupe: %#v", incidents)
	}
}

func TestRecordMulticaTaskFailureDuplicateUsesPersistedAggregateAfterMetadataMove(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	setupRepoForTest(t, svc, "oldowner", "oldrepo")
	setupRepoForTest(t, svc, "newowner", "newrepo")

	input := service.MulticaTaskFailedInput{
		RepoFullName:    "oldowner/oldrepo",
		WorkspaceID:     "ws-1",
		MulticaIssueKey: "MUL-124",
		TaskID:          "task-moved-metadata",
		AgentID:         "lane-c-id",
		FailureReason:   "agent_error.provider_auth_or_access",
		RuntimeProvider: "codex",
		OccurredAt:      time.Date(2026, 6, 9, 10, 31, 0, 0, time.UTC),
	}
	first, err := svc.RecordMulticaTaskFailure(ctx, input)
	if err != nil {
		t.Fatalf("first RecordMulticaTaskFailure: %v", err)
	}

	input.RepoFullName = "newowner/newrepo"
	duplicate, err := svc.RecordMulticaTaskFailure(ctx, input)
	if err != nil {
		t.Fatalf("duplicate after metadata move: %v", err)
	}
	if duplicate.EventAccepted {
		t.Fatalf("duplicate event was accepted again: %#v", duplicate.Event)
	}
	if duplicate.Event.AggregateKey != first.Event.AggregateKey || duplicate.Incident.ID != first.Incident.ID || duplicate.Issue.ID != first.Issue.ID {
		t.Fatalf("duplicate did not use persisted aggregate: first=%#v duplicate=%#v", first, duplicate)
	}
}

func TestMulticaIncidentIssueRollsWindowAndArchivesOlderEvents(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	setupRepoForTest(t, svc, "rolluser", "rollrepo")

	now := time.Date(2026, 6, 9, 10, 31, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -40)
	base := service.MulticaTaskFailedInput{
		RepoFullName:    "rolluser/rollrepo",
		WorkspaceID:     "ws-1",
		MulticaIssueKey: "MUL-125",
		AgentID:         "lane-d-id",
		AgentName:       "lane-d",
		FailureReason:   "agent_error.process_failure",
		RuntimeProvider: "claude",
	}
	base.TaskID = "old-task"
	base.Error = "old panic should roll out"
	base.OccurredAt = old
	if _, err := svc.RecordMulticaTaskFailure(ctx, base); err != nil {
		t.Fatalf("record old event: %v", err)
	}
	base.TaskID = "new-task"
	base.Error = "new panic remains visible"
	base.OccurredAt = now
	res, err := svc.RecordMulticaTaskFailure(ctx, base)
	if err != nil {
		t.Fatalf("record new event: %v", err)
	}

	body := string(res.Issue.Body)
	if strings.Contains(body, "old-task") || strings.Contains(body, "old panic should roll out") {
		t.Fatalf("old event leaked into rolling body:\n%s", body)
	}
	for _, want := range []string{"count: 1", "new-task", "new panic remains visible", "events_outside_window: 1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("incident body missing %q:\n%s", want, body)
		}
	}
}

func TestGCMulticaIncidentIssuesClosesIncidentsWithoutRecentEvents(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	setupRepoForTest(t, svc, "gcuser", "gcrepo")

	now := time.Date(2026, 6, 9, 10, 31, 0, 0, time.UTC)
	res, err := svc.RecordMulticaTaskFailure(ctx, service.MulticaTaskFailedInput{
		RepoFullName:    "gcuser/gcrepo",
		WorkspaceID:     "ws-1",
		MulticaIssueKey: "MUL-126",
		TaskID:          "expired-task",
		AgentID:         "lane-a-id",
		AgentName:       "lane-a",
		FailureReason:   "runtime_offline",
		Error:           "runtime went offline",
		RuntimeProvider: "codex",
		OccurredAt:      now.AddDate(0, 0, -31),
	})
	if err != nil {
		t.Fatalf("record expired event: %v", err)
	}

	gc, err := svc.GCMulticaIncidentIssues(ctx, now)
	if err != nil {
		t.Fatalf("GCMulticaIncidentIssues: %v", err)
	}
	if gc.Closed != 1 {
		t.Fatalf("GC closed = %d, want 1", gc.Closed)
	}
	issue, err := svc.GetIssue(ctx, "gcuser/gcrepo", res.Issue.Number)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.State != db.StateClosed || issue.StateReason != db.StateReasonNotPlanned {
		t.Fatalf("expired issue state=(%s,%s), want closed/not_planned", issue.State, issue.StateReason)
	}
	if !strings.Contains(string(issue.Body), "status: expired") {
		t.Fatalf("expired body missing status marker:\n%s", issue.Body)
	}
}

type captureMulticaIncidentNotifier struct {
	notes []service.MulticaIncidentNotification
}

func (n *captureMulticaIncidentNotifier) NotifyMulticaIncident(ctx context.Context, note service.MulticaIncidentNotification) error {
	n.notes = append(n.notes, note)
	return nil
}

func TestRecordMulticaTaskFailureNotifiesAcceptedEventsAndThrottles(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	svc.BaseURL = "http://ags.local"
	notifier := &captureMulticaIncidentNotifier{}
	svc.MulticaIncidentNotifier = notifier
	svc.MulticaIncidentNotifyThrottle = time.Hour
	ctx := context.Background()
	setupRepoForTest(t, svc, "notifyuser", "notifyrepo")
	now := time.Date(2026, 6, 9, 10, 31, 0, 0, time.UTC)
	input := service.MulticaTaskFailedInput{
		RepoFullName:      "notifyuser/notifyrepo",
		WorkspaceID:       "ws-1",
		MulticaIssueKey:   "MUL-200",
		MulticaIssueTitle: "Fix notify",
		TaskID:            "task-notify-1",
		AgentID:           "lane-n-id",
		AgentName:         "lane-n",
		FailureReason:     "timeout",
		Error:             "runtime timed out",
		RuntimeProvider:   "codex",
		RuntimeID:         "runtime-n",
		OccurredAt:        now,
	}
	first, err := svc.RecordMulticaTaskFailure(ctx, input)
	if err != nil {
		t.Fatalf("first RecordMulticaTaskFailure: %v", err)
	}
	if !first.NotificationSent || first.NotificationSuppressed || first.NotificationError != "" {
		t.Fatalf("first notification state mismatch: %#v", first)
	}
	if first.Issue.State != db.StateClosed || first.Issue.StateReason != db.StateReasonCompleted || !strings.Contains(string(first.Issue.Body), "notification: feishu_delivered") {
		t.Fatalf("first incident issue should be closed as delivered: state=%s reason=%s body=%s", first.Issue.State, first.Issue.StateReason, first.Issue.Body)
	}
	if len(notifier.notes) != 1 {
		t.Fatalf("notification count after first = %d", len(notifier.notes))
	}
	note := notifier.notes[0]
	if note.RepoFullName != "notifyuser/notifyrepo" || note.MulticaIssueKey != "MUL-200" || note.TaskID != "task-notify-1" || note.IssueNumber == 0 || !strings.Contains(note.IssueURL, "/api/v3/repos/notifyuser/notifyrepo/issues/") {
		t.Fatalf("unexpected notification: %#v", note)
	}
	duplicate, err := svc.RecordMulticaTaskFailure(ctx, input)
	if err != nil {
		t.Fatalf("duplicate RecordMulticaTaskFailure: %v", err)
	}
	if duplicate.NotificationSent || duplicate.NotificationSuppressed || len(notifier.notes) != 1 {
		t.Fatalf("duplicate should not notify: duplicate=%#v count=%d", duplicate, len(notifier.notes))
	}
	input.TaskID = "task-notify-2"
	input.OccurredAt = now.Add(time.Minute)
	second, err := svc.RecordMulticaTaskFailure(ctx, input)
	if err != nil {
		t.Fatalf("second RecordMulticaTaskFailure: %v", err)
	}
	if !second.EventAccepted || second.NotificationSent || !second.NotificationSuppressed || len(notifier.notes) != 1 {
		t.Fatalf("second should be throttled: second=%#v count=%d", second, len(notifier.notes))
	}
	if second.Issue.State != db.StateClosed || second.Issue.StateReason != db.StateReasonCompleted || !strings.Contains(string(second.Issue.Body), "notification: feishu_throttled_covered") {
		t.Fatalf("throttled incident issue should be closed as covered: state=%s reason=%s body=%s", second.Issue.State, second.Issue.StateReason, second.Issue.Body)
	}
	var incident db.RepoIncident
	if err := svc.DB.First(&incident, first.Incident.ID).Error; err != nil {
		t.Fatalf("load incident: %v", err)
	}
	if incident.LastNotifiedEventKey != first.Event.EventKey || incident.LastNotifiedAt == nil || incident.LastNotifiedAt.IsZero() {
		t.Fatalf("notification checkpoint mismatch: %#v", incident)
	}
}
