package projectionwatch

import (
	"context"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/service"
)

func TestPollOnceSendsResolvedProjectionNotificationsWithoutProviderScan(t *testing.T) {
	source := &fakeSource{
		active:   []service.ProjectionDriftNotification{{StateID: 1, RepoFullName: "example-owner/demo", Ref: "refs/heads/main"}},
		resolved: []service.ProjectionDriftNotification{{StateID: 2, RepoFullName: "example-owner/demo", Ref: "refs/heads/agent/done"}},
	}
	notifier := &fakeNotifier{}
	watcher := New(Config{Enabled: true, GracePeriod: time.Nanosecond, ThrottleWindow: time.Hour}, source, notifier)

	sent, err := watcher.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if sent != 2 {
		t.Fatalf("sent=%d, want 2", sent)
	}
	if notifier.active != 1 || notifier.resolved != 1 {
		t.Fatalf("notifications active=%d resolved=%d", notifier.active, notifier.resolved)
	}
	if len(source.marked) != 2 || source.marked[0] != 1 || source.marked[1] != 2 {
		t.Fatalf("marked=%v", source.marked)
	}
	if source.activeScans != 0 || source.fullAudits != 0 {
		t.Fatalf("PollOnce performed provider I/O: active=%d full=%d", source.activeScans, source.fullAudits)
	}
}

func TestReconcileAndAuditUseSeparateProviderScopes(t *testing.T) {
	source := &fakeSource{}
	watcher := New(Config{Enabled: true}, source, &fakeNotifier{})

	if _, err := watcher.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if source.activeScans != 1 || source.fullAudits != 0 {
		t.Fatalf("active reconciliation calls: active=%d full=%d", source.activeScans, source.fullAudits)
	}
	if _, err := watcher.AuditOnce(context.Background()); err != nil {
		t.Fatalf("AuditOnce: %v", err)
	}
	if source.activeScans != 1 || source.fullAudits != 1 {
		t.Fatalf("full audit calls: active=%d full=%d", source.activeScans, source.fullAudits)
	}
}

func TestRunDefersProviderScansAtStartup(t *testing.T) {
	source := &fakeSource{active: []service.ProjectionDriftNotification{{StateID: 1, RepoFullName: "example-owner/demo", Ref: "refs/heads/main"}}}
	notified := make(chan struct{}, 1)
	watcher := New(Config{
		Enabled: true, PollInterval: time.Hour, ScanInterval: time.Hour,
		FullAuditInterval: time.Hour, StartupAuditDelay: time.Hour,
		GracePeriod: time.Nanosecond, ThrottleWindow: time.Hour,
	}, source, &fakeNotifier{notified: notified})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(done)
	}()

	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("startup notification dispatch did not run")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop")
	}
	if source.activeScans != 0 || source.fullAudits != 0 {
		t.Fatalf("startup performed provider I/O: active=%d full=%d", source.activeScans, source.fullAudits)
	}
}

type fakeSource struct {
	active      []service.ProjectionDriftNotification
	resolved    []service.ProjectionDriftNotification
	marked      []uint
	activeScans int
	fullAudits  int
}

func (s *fakeSource) ListProjectionDriftNotifications(context.Context, time.Duration, time.Duration, int) ([]service.ProjectionDriftNotification, error) {
	return s.active, nil
}

func (s *fakeSource) ListResolvedProjectionDriftNotifications(context.Context, time.Time, int) ([]service.ProjectionDriftNotification, error) {
	return s.resolved, nil
}

func (s *fakeSource) MarkProjectionDriftNotified(_ context.Context, id uint, _ time.Time) error {
	s.marked = append(s.marked, id)
	return nil
}

func (s *fakeSource) ScanForgejoProjectionDrift(context.Context, int) (int, error) {
	s.activeScans++
	return 0, nil
}

func (s *fakeSource) AuditForgejoProjectionDrift(context.Context, int) (int, error) {
	s.fullAudits++
	return 0, nil
}

type fakeNotifier struct {
	active   int
	resolved int
	notified chan struct{}
}

func (n *fakeNotifier) NotifyProjectionDrift(context.Context, service.ProjectionDriftNotification) error {
	n.active++
	if n.notified != nil {
		select {
		case n.notified <- struct{}{}:
		default:
		}
	}
	return nil
}

func (n *fakeNotifier) NotifyProjectionDriftResolved(context.Context, service.ProjectionDriftNotification) error {
	n.resolved++
	return nil
}
