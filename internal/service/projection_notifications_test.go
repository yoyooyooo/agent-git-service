package service

import (
	"context"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
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

func TestProjectionDriftUnchangedFactWaitsForDailyReminder(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	fact := projectionFailureRecord{Provider: ProjectionProviderGitHub, RepoFullName: "example-owner/demo", Ref: "refs/heads/work", AGSSHA: "aaa", ExternalSHA: "bbb", Type: "sha_drift"}
	if err := svc.recordProjectionFailure(ctx, fact); err != nil {
		t.Fatal(err)
	}
	notes, err := svc.ListProjectionDriftNotifications(ctx, 15*time.Minute, 0, 50)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes=%v err=%v", notes, err)
	}
	if err := svc.MarkProjectionDriftNotified(ctx, notes[0].StateID, time.Now().UTC().Add(-16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A new scan updates last_seen_at, but does not create a new drift fact.
	if err := svc.recordProjectionFailure(ctx, fact); err != nil {
		t.Fatal(err)
	}
	notes, err = svc.ListProjectionDriftNotifications(ctx, 15*time.Minute, 0, 50)
	if err != nil || len(notes) != 0 {
		t.Fatalf("unchanged drift repeated next round: %v %v", notes, err)
	}
	all, err := svc.ListProjectionDriftNotifications(ctx, 0, 0, 50)
	if err != nil || len(all) != 0 {
		t.Fatalf("zero throttle bypassed reminder floor: %v %v", all, err)
	}
	var id uint
	if err := svc.DB.Table("projection_ref_states").Select("id").Scan(&id).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkProjectionDriftNotified(ctx, id, time.Now().UTC().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	notes, err = svc.ListProjectionDriftNotifications(ctx, 15*time.Minute, 0, 50)
	if err != nil || len(notes) != 1 {
		t.Fatalf("daily reminder absent: %v %v", notes, err)
	}
}

func TestProjectionDriftChangedActiveFactBecomesEligible(t *testing.T) {
	for _, field := range []string{"target", "branch", "type", "authority", "ags_sha", "external_sha", "summary"} {
		t.Run(field, func(t *testing.T) {
			svc := setupProjectionStateService(t)
			ctx := context.Background()
			firstObserved := time.Now().UTC().Add(-time.Hour)
			fact := projectionFailureRecord{Provider: ProjectionProviderGitHub, RepoFullName: "example-owner/demo", TargetRepo: "fixture/repo", Ref: "refs/heads/work", Branch: "work", Type: "sha_drift", Authority: ProjectionAuthorityAGS, AGSSHA: "aaa", ExternalSHA: "bbb", ErrorSummary: "first drift", OccurredAt: firstObserved}
			if err := svc.recordProjectionFailure(ctx, fact); err != nil {
				t.Fatal(err)
			}
			var before db.ProjectionRefState
			if err := svc.DB.Where("ref = ?", fact.Ref).First(&before).Error; err != nil {
				t.Fatal(err)
			}
			if err := svc.MarkProjectionDriftNotified(ctx, before.ID, time.Now().UTC().Add(-16*time.Minute)); err != nil {
				t.Fatal(err)
			}
			fact.OccurredAt = time.Now().UTC()
			if err := svc.recordProjectionFailure(ctx, fact); err != nil {
				t.Fatal(err)
			}
			notes, err := svc.ListProjectionDriftNotifications(ctx, 15*time.Minute, 0, 50)
			if err != nil || len(notes) != 0 {
				t.Fatalf("unchanged refresh eligible: notes=%v err=%v", notes, err)
			}
			switch field {
			case "target":
				fact.TargetRepo = "fixture/changed"
			case "branch":
				fact.Branch = "renamed"
			case "type":
				fact.Type = "transport_error"
			case "authority":
				fact.Authority = "changed-authority"
			case "ags_sha":
				fact.AGSSHA = "ccc"
			case "external_sha":
				fact.ExternalSHA = "ddd"
			case "summary":
				fact.ErrorSummary = "changed drift"
			}
			if err := svc.recordProjectionFailure(ctx, fact); err != nil {
				t.Fatal(err)
			}
			notes, err = svc.ListProjectionDriftNotifications(ctx, 15*time.Minute, 0, 50)
			if err != nil || len(notes) != 1 {
				t.Fatalf("changed active fact suppressed: notes=%v err=%v", notes, err)
			}
			var after db.ProjectionRefState
			if err := svc.DB.First(&after, before.ID).Error; err != nil {
				t.Fatal(err)
			}
			if after.Generation != before.Generation || !after.FirstSeenAt.Equal(before.FirstSeenAt) || after.LastNotifiedAt != nil {
				t.Fatalf("changed fact lost lifecycle: before=%+v after=%+v", before, after)
			}
		})
	}
}
