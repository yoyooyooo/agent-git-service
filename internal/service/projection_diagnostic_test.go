package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gittransport"
)

// Exercise the legacy DB/status/notification output surfaces, not just the new
// DTO. Storage remains the existing schema: no diagnostic migration or JSON
// hidden in ErrorSummary and no guessed persisted principal.
func TestProjectionDiagnosticLegacyStatusAndNotificationSafety(t *testing.T) {
	svc := setupProjectionStateService(t) // test-owned SQLite, no external DSN
	if err := svc.DB.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		t.Fatal(err)
	}
	raw := errors.New("pre-receive hook declined; Authorization: sentinel-header; https://user:sentinel-token@example.test/repo /private/sentinel-path sentinel-pr-body \u202e\x1b")
	safe := gittransport.SafeError(raw)
	pe := forgejointegration.ClassifyProjectionError("refs/heads/topic", strings.Repeat("a", 40), safe)
	pe.Repo = "example-owner/demo"
	if err := svc.RecordForgejoProjectionFailure(context.Background(), pe.Repo, []ForgejoRefChange{{Ref: pe.Ref, After: pe.ExpectedSHA}}, &pe); err != nil {
		t.Fatal(err)
	}
	status, err := svc.GetProjectionStatus(context.Background(), pe.Repo)
	if err != nil {
		t.Fatal(err)
	}
	var rows []db.ProjectionRefState
	if err := svc.DB.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	notes := projectionDriftNotificationsFromRows(rows)
	b, _ := json.Marshal(struct {
		Status any
		Rows   any
		Notes  any
	}{status, rows, notes})
	for _, secret := range []string{"sentinel-header", "sentinel-token", "sentinel-path", "sentinel-pr-body"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("secret reached legacy surface")
		}
	}
	if len(status.Refs) != 1 || status.Refs[0].Type != forgejointegration.ProjectionFailureHookRejected || len(status.Events) != 1 {
		t.Fatalf("legacy status shape changed: %+v", status)
	}
	if len(notes) != 1 || notes[0].Type != forgejointegration.ProjectionFailureHookRejected {
		t.Fatal("notification classification lost")
	}
	for _, note := range notes {
		text := renderProjectionDriftOutboundText(note, false)
		for _, secret := range []string{"sentinel-header", "sentinel-token", "sentinel-path", "sentinel-pr-body"} {
			if strings.Contains(text, secret) {
				t.Fatal("secret reached rendered outbound")
			}
		}
	}
}
func TestProjectionDiagnosticJobFailureCompatibility(t *testing.T) {
	for _, tc := range []struct{ message, kind string }{
		{"403 forbidden", forgejointegration.ProjectionFailurePermissionDenied},
		{"protected branch", forgejointegration.ProjectionFailureProtectedBranch},
		{"pre-receive hook declined", forgejointegration.ProjectionFailureHookRejected},
		{"non-fast-forward", forgejointegration.ProjectionFailureNonFastForward},
	} {
		f := newProjectionJobFailure("refs/heads/topic", strings.Repeat("a", 40), gittransport.SafeError(errors.New(tc.message+" sentinel-token")), true)
		if f.failureType != tc.kind || f.retryable || strings.Contains(f.summary, "sentinel-token") {
			t.Fatalf("unsafe retry/summary: %+v", f)
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		f := newProjectionJobFailure("refs/heads/topic", "", gittransport.SafeError(cause), false)
		if !errors.Is(f.err, cause) || !f.retryable {
			t.Fatal("legacy context semantics changed")
		}
	}
}
