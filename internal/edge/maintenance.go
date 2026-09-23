package edge

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

// Counters count successfully published transfers. TransferBytes includes
// received pack bytes of failed attempts but excludes framing/control requests.
// These are process-local observations, not authentication or freshness facts.
type MirrorStats struct {
	FullTransfers        int64  `json:"full_transfers"`
	IncrementalTransfers int64  `json:"incremental_transfers"`
	TransferBytes        int64  `json:"transfer_bytes"`
	RetentionError       string `json:"retention_error,omitempty"`
	PendingSyncs         int    `json:"pending_syncs"`
	ActiveSyncs          int    `json:"active_syncs"`
}

func (m *Mirror) Stats() MirrorStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MirrorStats{FullTransfers: m.fullTransfers.Load(), IncrementalTransfers: m.incrementalTransfers.Load(), TransferBytes: m.transferBytes.Load(), RetentionError: m.retentionError, PendingSyncs: len(m.jobs), ActiveSyncs: len(m.slots)}
}
func (m *Mirror) Prune(ctx context.Context) (snapshotstore.RetentionStats, error) {
	stats, err := m.cache.Prune(ctx)
	m.mu.Lock()
	m.retentionError = ""
	if err != nil && !errors.Is(err, context.Canceled) {
		m.retentionError = "retention_unavailable"
		if errors.Is(err, snapshotstore.ErrBudget) {
			m.retentionError = "retention_budget_exhausted"
		}
	}
	m.mu.Unlock()
	return stats, err
}
func (m *Mirror) maintain() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.MaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(m.ctx, m.cfg.MaintenanceInterval)
			_, _ = m.Prune(ctx) // Failure is retained in Stats, not silently ready.
			cancel()
		}
	}
}

type transferCounter struct {
	reader io.Reader
	count  *atomic.Int64
}

func (r transferCounter) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.count.Add(int64(n))
	return n, err
}
