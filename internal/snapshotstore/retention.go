package snapshotstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

var ErrBudget = errors.New("snapshot retention budget exhausted")

// RetentionPolicy bounds published logical file bytes and view count. Recent
// views are protected for MinAge since last local use; pinned readers/exports
// are protected regardless of age. MaxAge enables idle-time collection. No
// policy is a filesystem quota: staging is separately bounded by pack limits
// and synchronization concurrency, and the host still needs disk headroom.
type RetentionPolicy struct {
	MaxBytes int64
	MaxViews int
	MinAge   time.Duration
	MaxAge   time.Duration
}

func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{MaxBytes: 8 << 30, MaxViews: 256, MinAge: 15 * time.Minute, MaxAge: 24 * time.Hour}
}

func (p RetentionPolicy) Validate() error {
	if p.MaxBytes <= 0 || p.MaxViews <= 0 || p.MaxViews > 100000 || p.MinAge < time.Second || p.MaxAge <= p.MinAge {
		return errors.New("retention requires positive byte/view limits and ordered protection/expiry windows")
	}
	return nil
}

type retainedInfo struct {
	key        string
	descriptor edgeprotocol.RepositorySnapshot
	bytes      int64
	used       time.Time
	pinned     bool
}

type RetentionStats struct {
	Views        int   `json:"views"`
	Bytes        int64 `json:"bytes"`
	Pinned       int   `json:"pinned"`
	Removed      int   `json:"removed"`
	RemovedBytes int64 `json:"removed_bytes"`
}

// ConfigureRetention is called before runtime admission. Persisted views are
// inventoried, but no object/hash verification is skipped by Acquire. On restart
// every existing view gets a fresh MinAge grace because in-memory access times
// are not durable evidence of when a Git negotiation began.
func (s *Store) ConfigureRetention(policy RetentionPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.active != 0 {
		return errors.New("cannot change retention during imports")
	}
	if _, err := s.inventoryLocked(context.Background()); err != nil {
		return err
	}
	s.retention = &policy
	return nil
}

func (s *Store) inventoryLocked(ctx context.Context) ([]retainedInfo, error) {
	items, err := os.ReadDir(filepath.Join(s.root, "published"))
	if err != nil {
		return nil, err
	}
	var result []retainedInfo
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := item.Name()
		if !item.IsDir() || len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {
			return nil, errors.New("unexpected published snapshot entry")
		}
		root := filepath.Join(s.root, "published", key)
		if err := checkPublishedRoot(root); err != nil {
			return nil, err
		}
		f, err := os.Open(filepath.Join(root, "manifest.json"))
		if err != nil {
			return nil, err
		}
		m, decodeErr := edgeprotocol.DecodeManifest(f)
		_ = f.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		actualKey, _ := edgeprotocol.SnapshotKey(m.Snapshot)
		if key != actualKey {
			return nil, errors.New("published directory identity mismatch")
		}
		size, err := treeBytes(ctx, root)
		if err != nil {
			return nil, err
		}
		info, err := item.Info()
		if err != nil {
			return nil, err
		}
		used := info.ModTime()
		if used.Before(s.openedAt) {
			used = s.openedAt
		}
		if last := s.lastUse[key]; last.After(used) {
			used = last
		}
		pinned := false
		if e := s.entries[key]; e != nil {
			pinned = e.refs > 0
		}
		result = append(result, retainedInfo{key: key, descriptor: m.Snapshot, bytes: size, used: used, pinned: pinned})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].used.Equal(result[j].used) {
			return result[i].key < result[j].key
		}
		return result[i].used.Before(result[j].used)
	})
	return result, nil
}

func treeBytes(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in snapshot accounting")
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("nonregular snapshot entry")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || info.Size() > int64(^uint64(0)>>1)-total {
			return errors.New("snapshot accounting overflow")
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// AcquireBase selects only content-compatible views; it does not grant user
// access, assert freshness, or choose the target. Corrupt candidate data fails
// closed rather than being hidden by a full-download fallback.
func (s *Store) AcquireBase(ctx context.Context, target edgeprotocol.RepositorySnapshot) (*Lease, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	items, err := s.inventoryLocked(ctx)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for i := len(items) - 1; i >= 0; i-- {
		candidate := items[i].descriptor
		if candidate == target || !edgeprotocol.CompatibleBase(target, candidate) {
			continue
		}
		view, err := s.Acquire(ctx, candidate)
		if errors.Is(err, ErrMissing) {
			continue
		} // Collected before lease acquisition.
		return view, err
	}
	return nil, ErrMissing
}

// Prune expires idle views and enforces configured limits without crossing read
// leases or protection windows. Capacity refusal is observable, never permission
// to destroy an in-flight or recently advertised view. No caller-supplied path.
func (s *Store) Prune(ctx context.Context) (RetentionStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RetentionStats{}, ErrClosed
	}
	return s.collectLocked(ctx, 0, 0, true)
}
func (s *Store) collectLocked(ctx context.Context, incomingBytes int64, incomingViews int, expire bool) (RetentionStats, error) {
	var stats RetentionStats
	items, err := s.inventoryLocked(ctx)
	if err != nil {
		return stats, err
	}
	for _, item := range items {
		if item.bytes > int64(^uint64(0)>>1)-stats.Bytes {
			return stats, errors.New("retention accounting overflow")
		}
		stats.Bytes += item.bytes
		stats.Views++
		if item.pinned {
			stats.Pinned++
		}
	}
	if s.retention == nil {
		return stats, nil
	}
	policy := *s.retention
	if incomingBytes > policy.MaxBytes || incomingViews > policy.MaxViews {
		return stats, ErrBudget
	}
	projectedBytes, projectedViews := stats.Bytes, stats.Views
	over := func() bool {
		return projectedBytes > policy.MaxBytes-incomingBytes || projectedViews > policy.MaxViews-incomingViews
	}
	var remove []retainedInfo
	now := s.now()
	// Plan the entire collection before mutation. If pinned/protected views
	// make admission impossible, preserve ALL existing views and refuse it.
	for _, item := range items {
		age := now.Sub(item.used)
		if item.pinned || age < policy.MinAge {
			continue
		}
		if !over() && (!expire || age < policy.MaxAge) {
			continue
		}
		remove = append(remove, item)
		projectedBytes -= item.bytes
		projectedViews--
	}
	if over() {
		return stats, ErrBudget
	}
	for _, item := range remove {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if err := os.RemoveAll(filepath.Join(s.root, "published", item.key)); err != nil {
			return stats, err
		}
		delete(s.entries, item.key)
		delete(s.lastUse, item.key)
		stats.Bytes -= item.bytes
		stats.Views--
		stats.Removed++
		stats.RemovedBytes += item.bytes
	}
	if len(remove) != 0 {
		err = syncDirectory(filepath.Join(s.root, "published"))
	}
	return stats, err
}
