package service

import (
	"context"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

func TestListProjectionDriftNotificationsHonorsGracePeriod(t *testing.T) {
	svc := setupProjectionStateService(t)
	projectionErr := &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureSHADrift,
		Repo:         "example-owner/demo",
		TargetRepo:   "example-owner/demo",
		Ref:          "refs/heads/main",
		Branch:       "main",
		ExpectedSHA:  "abc123",
		ActualSHA:    "def456",
		ErrorSummary: "short lived drift",
	}
	if err := svc.RecordForgejoProjectionFailure(context.Background(), "example-owner/demo", nil, projectionErr); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	rows, err := svc.ListProjectionDriftNotifications(context.Background(), time.Hour, time.Hour, 10)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected grace period to suppress fresh drift, got %#v", rows)
	}
}

func TestListProjectionDriftNotificationsHonorsThrottle(t *testing.T) {
	svc := setupProjectionStateService(t)
	projectionErr := &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureNonFastForward,
		Repo:         "example-owner/demo",
		TargetRepo:   "example-owner/demo",
		Ref:          "refs/heads/main",
		Branch:       "main",
		ExpectedSHA:  "abc123",
		ActualSHA:    "def456",
		ErrorSummary: "rejected",
	}
	if err := svc.RecordForgejoProjectionFailure(context.Background(), "example-owner/demo", nil, projectionErr); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	rows, err := svc.ListProjectionDriftNotifications(context.Background(), time.Hour, 0, 10)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(rows) != 1 || rows[0].RepoFullName != "example-owner/demo" || rows[0].ForgejoSHA != "def456" {
		t.Fatalf("rows=%#v", rows)
	}
	if err := svc.MarkProjectionDriftNotified(context.Background(), rows[0].StateID, time.Now().UTC()); err != nil {
		t.Fatalf("mark notified: %v", err)
	}
	rows, err = svc.ListProjectionDriftNotifications(context.Background(), time.Hour, 0, 10)
	if err != nil {
		t.Fatalf("list notifications after mark: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected throttle to suppress rows, got %#v", rows)
	}
}

func TestNewProjectionDriftGenerationIsNotThrottledByPreviousAlert(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	ref := "refs/heads/agent/recurrent"
	first := &forgejointegration.ProjectionError{
		Type: forgejointegration.ProjectionFailureSHADrift, Repo: "example-owner/demo", TargetRepo: "example-owner/demo",
		Ref: ref, Branch: "agent/recurrent", ExpectedSHA: "abc123", ActualSHA: "def456", ErrorSummary: "first drift",
	}
	if err := svc.RecordForgejoProjectionFailure(ctx, "example-owner/demo", nil, first); err != nil {
		t.Fatalf("record first drift: %v", err)
	}
	rows, err := svc.ListProjectionDriftNotifications(ctx, time.Hour, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list first drift: rows=%#v err=%v", rows, err)
	}
	if err := svc.MarkProjectionDriftNotified(ctx, rows[0].StateID, time.Now().UTC()); err != nil {
		t.Fatalf("mark first drift notified: %v", err)
	}
	if err := svc.ResolveProjectionRefState(ctx, "example-owner/demo", ProjectionProviderForgejo, ref, "abc123", "abc123", time.Now().UTC()); err != nil {
		t.Fatalf("resolve first drift: %v", err)
	}
	second := &forgejointegration.ProjectionError{
		Type: forgejointegration.ProjectionFailureSHADrift, Repo: "example-owner/demo", TargetRepo: "example-owner/demo",
		Ref: ref, Branch: "agent/recurrent", ExpectedSHA: "fedcba", ActualSHA: "654321", ErrorSummary: "second drift",
	}
	if err := svc.RecordForgejoProjectionFailure(ctx, "example-owner/demo", nil, second); err != nil {
		t.Fatalf("record second drift: %v", err)
	}

	rows, err = svc.ListProjectionDriftNotifications(ctx, time.Hour, 0, 10)
	if err != nil {
		t.Fatalf("list second drift: %v", err)
	}
	if len(rows) != 1 || rows[0].Generation != 2 || rows[0].ErrorSummary != "second drift" {
		t.Fatalf("new generation inherited previous notification throttle: %#v", rows)
	}
}

func TestListResolvedProjectionDriftNotificationsOnlyAfterActiveAlert(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	projectionErr := &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureSHADrift,
		Repo:         "example-owner/demo",
		TargetRepo:   "example-owner/demo",
		Ref:          "refs/heads/agent/done",
		Branch:       "agent/done",
		ExpectedSHA:  "abc123",
		ActualSHA:    "",
		ErrorSummary: "Forgejo ref missing for AGS authoritative ref",
	}
	if err := svc.RecordForgejoProjectionFailure(ctx, "example-owner/demo", nil, projectionErr); err != nil {
		t.Fatalf("record failure: %v", err)
	}
	activeRows, err := svc.ListProjectionDriftNotifications(ctx, time.Hour, 0, 10)
	if err != nil {
		t.Fatalf("list active notifications: %v", err)
	}
	if len(activeRows) != 1 {
		t.Fatalf("activeRows=%#v", activeRows)
	}
	if err := svc.MarkProjectionDriftNotified(ctx, activeRows[0].StateID, time.Now().UTC()); err != nil {
		t.Fatalf("mark active notified: %v", err)
	}
	since := time.Now().UTC().Add(-time.Second)
	if err := svc.ResolveProjectionRefState(ctx, "example-owner/demo", ProjectionProviderForgejo, "refs/heads/agent/done", "abc123", "abc123", time.Now().UTC()); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	resolvedRows, err := svc.ListResolvedProjectionDriftNotifications(ctx, since, 10)
	if err != nil {
		t.Fatalf("list resolved notifications: %v", err)
	}
	if len(resolvedRows) != 1 {
		t.Fatalf("resolvedRows=%#v", resolvedRows)
	}
	if resolvedRows[0].ForgejoSHA != "abc123" || resolvedRows[0].ResolvedAt.IsZero() {
		t.Fatalf("resolved row missing final state: %#v", resolvedRows[0])
	}
	if err := svc.MarkProjectionDriftNotified(ctx, resolvedRows[0].StateID, time.Now().UTC()); err != nil {
		t.Fatalf("mark resolved notified: %v", err)
	}
	resolvedRows, err = svc.ListResolvedProjectionDriftNotifications(ctx, since, 10)
	if err != nil {
		t.Fatalf("list resolved after mark: %v", err)
	}
	if len(resolvedRows) != 0 {
		t.Fatalf("expected resolved notification to be one-shot, got %#v", resolvedRows)
	}
}
