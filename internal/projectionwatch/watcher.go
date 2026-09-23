package projectionwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ngaut/agent-git-service/internal/service"
)

type Config struct {
	Enabled           bool
	PollInterval      time.Duration
	ScanInterval      time.Duration
	FullAuditInterval time.Duration
	StartupAuditDelay time.Duration
	ThrottleWindow    time.Duration
	GracePeriod       time.Duration
	Limit             int
}

type Source interface {
	ListProjectionDriftNotifications(context.Context, time.Duration, time.Duration, int) ([]service.ProjectionDriftNotification, error)
	ListResolvedProjectionDriftNotifications(context.Context, time.Time, int) ([]service.ProjectionDriftNotification, error)
	MarkProjectionDriftNotified(context.Context, uint, time.Time) error
}

type driftScanner interface {
	ScanForgejoProjectionDrift(context.Context, int) (int, error)
}

type driftAuditor interface {
	AuditForgejoProjectionDrift(context.Context, int) (int, error)
}

type Notifier interface {
	NotifyProjectionDrift(context.Context, service.ProjectionDriftNotification) error
	NotifyProjectionDriftResolved(context.Context, service.ProjectionDriftNotification) error
}

type Watcher struct {
	cfg       Config
	source    Source
	notifier  Notifier
	logger    *slog.Logger
	startedAt time.Time
}

type Option func(*Watcher)

func WithLogger(logger *slog.Logger) Option {
	return func(w *Watcher) {
		if logger != nil {
			w.logger = logger
		}
	}
}

func New(cfg Config, source Source, notifier Notifier, opts ...Option) *Watcher {
	if !cfg.Enabled || source == nil || notifier == nil {
		return nil
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Minute
	}
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = 15 * time.Minute
	}
	if cfg.FullAuditInterval <= 0 {
		cfg.FullAuditInterval = 24 * time.Hour
	}
	if cfg.StartupAuditDelay <= 0 {
		cfg.StartupAuditDelay = 5 * time.Minute
	}
	if cfg.StartupAuditDelay > cfg.FullAuditInterval {
		cfg.StartupAuditDelay = cfg.FullAuditInterval
	}
	if cfg.ThrottleWindow <= 0 {
		cfg.ThrottleWindow = 30 * time.Minute
	}
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = 2 * time.Minute
	}
	if cfg.Limit <= 0 || cfg.Limit > 100 {
		cfg.Limit = 50
	}
	w := &Watcher{cfg: cfg, source: source, notifier: notifier, logger: slog.Default(), startedAt: time.Now().UTC()}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	return w
}

func (w *Watcher) Run(ctx context.Context) {
	if w == nil {
		return
	}
	// Startup only dispatches already-persisted notifications. Provider reads
	// are delayed so restart and readiness recovery do not compete with a full
	// integrity audit.
	w.pollAndLog(ctx)
	pollTicker := time.NewTicker(w.cfg.PollInterval)
	scanTicker := time.NewTicker(w.cfg.ScanInterval)
	auditTimer := time.NewTimer(w.cfg.StartupAuditDelay)
	defer pollTicker.Stop()
	defer scanTicker.Stop()
	defer auditTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			w.pollAndLog(ctx)
		case <-scanTicker.C:
			w.reconcileAndLog(ctx, false)
		case <-auditTimer.C:
			w.reconcileAndLog(ctx, true)
			auditTimer.Reset(w.cfg.FullAuditInterval)
		}
	}
}

func (w *Watcher) pollAndLog(ctx context.Context) {
	n, err := w.PollOnce(ctx)
	if err != nil {
		w.logger.Warn("projection watch poll failed", "sent", n, "error", err)
		return
	}
	if n > 0 {
		w.logger.Info("projection watch notifications sent", "sent", n)
	}
}

func (w *Watcher) reconcileAndLog(ctx context.Context, fullAudit bool) {
	startedAt := time.Now()
	sent, recorded, err := w.reconcileOnce(ctx, fullAudit)
	mode := "active"
	if fullAudit {
		mode = "full_audit"
	}
	if err != nil {
		w.logger.Warn("projection reconciliation failed", "mode", mode, "recorded", recorded, "sent", sent, "duration", time.Since(startedAt), "error", err)
		return
	}
	w.logger.Info("projection reconciliation completed", "mode", mode, "recorded", recorded, "sent", sent, "duration", time.Since(startedAt))
}

// ReconcileOnce runs the bounded active-provider scan and then dispatches any
// newly eligible notifications.
func (w *Watcher) ReconcileOnce(ctx context.Context) (int, error) {
	sent, _, err := w.reconcileOnce(ctx, false)
	return sent, err
}

// AuditOnce runs the complete historical provider audit and then dispatches
// any newly eligible notifications.
func (w *Watcher) AuditOnce(ctx context.Context) (int, error) {
	sent, _, err := w.reconcileOnce(ctx, true)
	return sent, err
}

func (w *Watcher) reconcileOnce(ctx context.Context, fullAudit bool) (int, int, error) {
	if w == nil || w.source == nil || w.notifier == nil {
		return 0, 0, nil
	}
	var errs []error
	recorded := 0
	if fullAudit {
		if auditor, ok := w.source.(driftAuditor); ok {
			n, err := auditor.AuditForgejoProjectionDrift(ctx, w.cfg.Limit)
			recorded += n
			if err != nil {
				errs = append(errs, err)
			}
		}
	} else if scanner, ok := w.source.(driftScanner); ok {
		n, err := scanner.ScanForgejoProjectionDrift(ctx, w.cfg.Limit)
		recorded += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	sent, err := w.PollOnce(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	return sent, recorded, joinErrors(errs)
}

// PollOnce only dispatches already-persisted drift notifications. It performs
// no provider I/O.
func (w *Watcher) PollOnce(ctx context.Context) (int, error) {
	if w == nil || w.source == nil || w.notifier == nil {
		return 0, nil
	}
	var errs []error
	rows, err := w.source.ListProjectionDriftNotifications(ctx, w.cfg.ThrottleWindow, w.cfg.GracePeriod, w.cfg.Limit)
	if err != nil {
		errs = append(errs, err)
		return 0, joinErrors(errs)
	}
	resolvedRows, err := w.source.ListResolvedProjectionDriftNotifications(ctx, w.startedAt, w.cfg.Limit)
	if err != nil {
		errs = append(errs, err)
		return 0, joinErrors(errs)
	}
	sent := 0
	now := time.Now().UTC()
	for _, row := range rows {
		if err := w.notifier.NotifyProjectionDrift(ctx, row); err != nil {
			errs = append(errs, fmt.Errorf("notify active %s %s: %w", row.RepoFullName, row.Ref, err))
			continue
		}
		if err := w.source.MarkProjectionDriftNotified(ctx, row.StateID, now); err != nil {
			errs = append(errs, fmt.Errorf("mark active notified %d: %w", row.StateID, err))
			continue
		}
		sent++
	}
	for _, row := range resolvedRows {
		if err := w.notifier.NotifyProjectionDriftResolved(ctx, row); err != nil {
			errs = append(errs, fmt.Errorf("notify resolved %s %s: %w", row.RepoFullName, row.Ref, err))
			continue
		}
		if err := w.source.MarkProjectionDriftNotified(ctx, row.StateID, now); err != nil {
			errs = append(errs, fmt.Errorf("mark resolved notified %d: %w", row.StateID, err))
			continue
		}
		sent++
	}
	return sent, joinErrors(errs)
}

func joinErrors(errs []error) error {
	return errors.Join(errs...)
}
