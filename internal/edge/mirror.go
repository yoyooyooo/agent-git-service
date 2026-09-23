package edge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

var ErrSnapshotQueueFull = errors.New("snapshot synchronization queue is full")

type MirrorConfig struct {
	Root                string
	MaxPackBytes        int64
	Concurrency         int
	MaxPending          int
	SyncTimeout         time.Duration
	Retention           snapshotstore.RetentionPolicy
	MaintenanceInterval time.Duration
	MinFreeBytes        uint64
}

type syncJob struct {
	done chan struct{}
	err  error
}
type storeQueue struct {
	slot  chan struct{}
	users int
}

// Mirror implements materialization, not request authorization or Git protocol
// view selection. ReadRuntime composes it with those independent checks.
// All producers are owned by the runtime context, never a single HTTP waiter.
type Mirror struct {
	cache                *snapshotstore.Store
	source               SnapshotSource
	cfg                  MirrorConfig
	ctx                  context.Context
	cancel               context.CancelFunc
	slots                chan struct{}
	mu                   sync.Mutex
	jobs                 map[string]*syncJob
	stores               map[string]*storeQueue
	closed               bool
	wg                   sync.WaitGroup
	fullTransfers        atomic.Int64
	incrementalTransfers atomic.Int64
	transferBytes        atomic.Int64
	retentionError       string
}

var _ SnapshotReader = (*Mirror)(nil)

func NewMirror(ctx context.Context, cfg MirrorConfig, source SnapshotSource) (*Mirror, error) {
	if source == nil || cfg.Concurrency < 1 || cfg.Concurrency > 16 || cfg.MaxPending < 1 || cfg.MaxPending > 1024 || cfg.SyncTimeout <= 0 || cfg.SyncTimeout > time.Hour {
		return nil, errors.New("mirror requires a source and bounded synchronization limits")
	}
	if cfg.Retention == (snapshotstore.RetentionPolicy{}) {
		cfg.Retention = snapshotstore.DefaultRetentionPolicy()
	}
	if err := cfg.Retention.Validate(); err != nil {
		return nil, err
	}
	if cfg.MaintenanceInterval == 0 {
		cfg.MaintenanceInterval = time.Minute
	}
	if cfg.MaintenanceInterval < time.Second || cfg.MaintenanceInterval > time.Hour {
		return nil, errors.New("invalid mirror maintenance interval")
	}
	cache, err := snapshotstore.Open(cfg.Root, cfg.MaxPackBytes)
	if err != nil {
		return nil, err
	}
	if err := cache.ConfigureRetention(cfg.Retention); err != nil {
		_ = cache.Close()
		return nil, err
	}
	if err := cache.ConfigureHeadroom(cfg.MinFreeBytes); err != nil {
		_ = cache.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Mirror{cache: cache, source: source, cfg: cfg, ctx: ctx, cancel: cancel, slots: make(chan struct{}, cfg.Concurrency), jobs: make(map[string]*syncJob), stores: make(map[string]*storeQueue)}
	m.wg.Add(1)
	go m.maintain()
	return m, nil
}

func identityKey(identity edgeprotocol.RepositoryIdentity) string {
	data, _ := json.Marshal(identity)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (m *Mirror) EnsureSnapshot(ctx context.Context, required edgeprotocol.RepositorySnapshot) (SnapshotView, error) {
	key, err := edgeprotocol.SnapshotKey(required)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	view, err := m.cache.Acquire(ctx, required)
	if err == nil {
		traceCache(ctx, "hit")
		return view, nil
	}
	traceCache(ctx, "wait_for_sync")
	if !errors.Is(err, snapshotstore.ErrMissing) {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, snapshotstore.ErrClosed
	}
	job := m.jobs[key]
	if job == nil {
		if len(m.jobs) >= m.cfg.MaxPending {
			m.mu.Unlock()
			return nil, ErrSnapshotQueueFull
		}
		job = &syncJob{done: make(chan struct{})}
		m.jobs[key] = job
		storeKey := identityKey(required.Identity)
		queue := m.stores[storeKey]
		if queue == nil {
			queue = &storeQueue{slot: make(chan struct{}, 1)}
			m.stores[storeKey] = queue
		}
		queue.users++
		m.wg.Add(1)
		go m.runSync(key, storeKey, queue, job, required)
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, m.ctx.Err()
	case <-job.done:
		if job.err != nil {
			return nil, job.err
		}
		return m.cache.Acquire(ctx, required)
	}
}

func (m *Mirror) runSync(key, storeKey string, queue *storeQueue, job *syncJob, required edgeprotocol.RepositorySnapshot) {
	defer m.wg.Done()
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.SyncTimeout)
	defer cancel()
	err := func() error {
		// Waiting on another generation of this store consumes no node-wide
		// download slot, so unrelated repositories can still make progress.
		select {
		case queue.slot <- struct{}{}:
			defer func() { <-queue.slot }()
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case m.slots <- struct{}{}:
			defer func() { <-m.slots }()
		case <-ctx.Done():
			return ctx.Err()
		}
		// Recheck after queueing: another completed producer may have published
		// this exact view between the caller's cache lookup and job admission.
		if view, err := m.cache.Acquire(ctx, required); err == nil {
			view.Release()
			return nil
		} else if !errors.Is(err, snapshotstore.ErrMissing) {
			return err
		}
		if m.cfg.MinFreeBytes > 0 {
			free, err := snapshotstore.FilesystemAvailable(m.cfg.Root)
			if err != nil {
				return errors.New("filesystem_probe_unavailable")
			}
			if free < m.cfg.MinFreeBytes {
				return snapshotstore.ErrHeadroom
			}
		}
		var base *snapshotstore.Lease
		var header edgeprotocol.TransferHeader
		var manifest edgeprotocol.Manifest
		var body io.ReadCloser
		var err error
		if source, ok := m.source.(IncrementalSnapshotSource); ok {
			base, err = m.cache.AcquireBase(ctx, required)
			if err != nil && !errors.Is(err, snapshotstore.ErrMissing) {
				return err
			}
			if base != nil {
				defer base.Release()
				descriptor := base.Snapshot()
				header, body, err = source.OpenTransfer(ctx, required, &descriptor)
				if err == nil && (header.Validate() != nil || header.Manifest.Snapshot != required || header.Base != nil && *header.Base != descriptor) {
					if body != nil {
						_ = body.Close()
					}
					return errors.New("incremental transfer binding mismatch")
				}
				manifest = header.Manifest
			}
		}
		if base == nil {
			manifest, body, err = m.source.OpenSnapshot(ctx, required)
		}
		if err != nil {
			return err
		}
		if body == nil {
			return errors.New("peer supplied no pack stream")
		}
		defer body.Close()
		stopClose := context.AfterFunc(ctx, func() { _ = body.Close() })
		defer stopClose()
		if manifest.Snapshot != required {
			return errors.New("synchronization manifest mismatch")
		}
		produce := func(ctx context.Context, dst io.Writer) error {
			_, err := io.Copy(dst, transferCounter{reader: body, count: &m.transferBytes})
			return err
		}
		if header.Base != nil {
			err = m.cache.InstallIncremental(ctx, manifest, *header.Base, produce)
			if err == nil {
				m.incrementalTransfers.Add(1)
			}
		} else {
			err = m.cache.Install(ctx, manifest, produce)
			if err == nil {
				m.fullTransfers.Add(1)
			}
		}
		return err
	}()
	m.mu.Lock()
	job.err = err
	delete(m.jobs, key)
	queue.users--
	if queue.users == 0 {
		delete(m.stores, storeKey)
	}
	close(job.done)
	m.mu.Unlock()
}

// Evict cannot collect a view still used by a reader, and does not select by
// public path or latest-snapshot heuristics. Automatic retention is a later gate.
func (m *Mirror) Evict(required edgeprotocol.RepositorySnapshot) error {
	return m.cache.Remove(required)
}

func (m *Mirror) Close() error {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
	return m.cache.Close()
}
