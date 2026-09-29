package gitstore

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sync/semaphore"
)

// Capture excludes all AGS-managed mutations in this Store, including native
// receive-pack and filesystem lifecycle operations. Mutations remain concurrent
// with each other: existing ref CAS/repo locks still arbitrate their conflicts.
// This intentionally starts as a process/store-wide barrier. It is NOT a
// distributed lock and does not make unmanaged git/filesystem writes safe.
const captureWeight int64 = 1 << 30

type mutationContextKey struct{}
type snapshotCaptureKey struct{}
type mutationLease struct {
	store              *Store
	mu                 sync.Mutex
	refs               int
	maintenanceRelease func()
}

func (held *mutationLease) retain() bool {
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.refs == 0 {
		return false
	}
	held.refs++
	return true
}

func (held *mutationLease) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			held.mu.Lock()
			held.refs--
			last := held.refs == 0
			held.mu.Unlock()
			if last {
				held.store.captureBarrier().Release(1)
				held.maintenanceRelease()
			}
		})
	}
}

func (held *mutationLease) active() bool {
	held.mu.Lock()
	defer held.mu.Unlock()
	return held.refs > 0
}

func (s *Store) captureBarrier() *semaphore.Weighted {
	s.captureOnce.Do(func() { s.captureSem = semaphore.NewWeighted(captureWeight) })
	return s.captureSem
}

// beginObjectRead protects go-git's lazily opened pack indexes from repacking.
// A snapshot callback already excludes maintenance, so its read-only access is
// legal without acquiring a nested shared lease.
func (s *Store) beginObjectRead(ctx context.Context, names ...string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if captured, _ := ctx.Value(snapshotCaptureKey{}).(*Store); captured == s {
		return ctx, func() {}, nil
	}
	return s.BeginMutation(ctx, names...)
}

// BeginMutation follows any existing repo-local lock; captures never acquire
// repo locks. Pass the returned context to nested Store methods so they do not
// deadlock behind a queued capture. Never acquire a repo lock while holding an
// outer mutation lease. The
// context is lexical: do not carry it into detached background goroutines.
func (s *Store) BeginMutation(ctx context.Context, names ...string) (context.Context, func(), error) {
	if captured, _ := ctx.Value(snapshotCaptureKey{}).(*Store); captured == s {
		return ctx, nil, errors.New("cannot mutate inside snapshot capture")
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	accessRelease, err := s.BeginMaintenanceAccess(ctx, names...)
	if err != nil {
		return ctx, nil, err
	}
	if held, _ := ctx.Value(mutationContextKey{}).(*mutationLease); held != nil && held.store == s && held.retain() {
		sharedRelease := held.releaseFunc()
		var once sync.Once
		return ctx, func() { once.Do(func() { sharedRelease(); accessRelease() }) }, nil
	}
	if err := s.captureBarrier().Acquire(ctx, 1); err != nil {
		accessRelease()
		return ctx, nil, err
	}
	held := &mutationLease{store: s, refs: 1, maintenanceRelease: accessRelease}
	return context.WithValue(ctx, mutationContextKey{}, held), held.releaseFunc(), nil
}

// WithSnapshotCapture coordinates LOCAL policy/ref observation and immutable
// object-file pinning. Do not pack, fsck, publish, wait for a snapshot-store
// verification mutex or perform network transfer here. The callback must not
// mutate the source through Store methods. Release before retaining/exporting
// the private pins; this barrier is neither a transaction nor a distributed lock.
func (s *Store) WithSnapshotCapture(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil {
		return errors.New("snapshot capture callback is required")
	}
	if captured, _ := ctx.Value(snapshotCaptureKey{}).(*Store); captured == s {
		return errors.New("nested snapshot capture is not supported")
	}
	if held, _ := ctx.Value(mutationContextKey{}).(*mutationLease); held != nil && held.store == s && held.active() {
		return errors.New("cannot capture inside a mutation")
	}
	accessRelease, err := s.BeginMaintenanceAccess(ctx)
	if err != nil {
		return err
	}
	defer accessRelease()
	if err := s.captureBarrier().Acquire(ctx, captureWeight); err != nil {
		return err
	}
	defer s.captureBarrier().Release(captureWeight)
	return fn(context.WithValue(ctx, snapshotCaptureKey{}, s))
}
