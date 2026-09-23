package snapshotstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func installForRetention(ctx context.Context, s *Store, source string, m edgeprotocol.Manifest) error {
	return s.Install(ctx, m, func(ctx context.Context, w io.Writer) error { return WritePack(ctx, source, m, w) })
}
func testRetentionClock(s *Store) *time.Time {
	now := time.Now().Add(time.Hour)
	s.openedAt = now
	s.now = func() time.Time { return now }
	return &now
}
func nextRetentionView(t *testing.T, source string, old edgeprotocol.Manifest, content string) edgeprotocol.Manifest {
	gitOK(t, source, "", "update-ref", "refs/heads/main", commitObject(t, source, content, old.Snapshot.HEAD.OID))
	return updatedManifest(t, source, old)
}
func missingRetained(t *testing.T, s *Store, d edgeprotocol.RepositorySnapshot) {
	t.Helper()
	v, err := s.Acquire(context.Background(), d)
	if v != nil {
		v.Release()
	}
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("view should be missing: %v", err)
	}
}

func TestRetentionRefusesRecentAndPinnedWithoutDestroyingAnything(t *testing.T) {
	ctx := context.Background()
	source, a := testSource(t, "sha1", true)
	s := openTestStore(t, 0)
	clock := testRetentionClock(s)
	policy := RetentionPolicy{MaxBytes: 1 << 20, MaxViews: 1, MinAge: time.Minute, MaxAge: time.Hour}
	if err := s.ConfigureRetention(policy); err != nil {
		t.Fatal(err)
	}
	if err := installForRetention(ctx, s, source, a); err != nil {
		t.Fatal(err)
	}
	b := nextRetentionView(t, source, a, "second")
	if err := installForRetention(ctx, s, source, b); !errors.Is(err, ErrBudget) {
		t.Fatalf("protected capacity: %v", err)
	}
	missingRetained(t, s, b.Snapshot)
	view, err := s.Acquire(ctx, a.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(2 * time.Hour)
	if err := installForRetention(ctx, s, source, b); !errors.Is(err, ErrBudget) {
		t.Fatalf("pinned capacity: %v", err)
	}
	view.Release()
	*clock = clock.Add(2 * time.Minute)
	if err := installForRetention(ctx, s, source, b); err != nil {
		t.Fatal(err)
	}
	missingRetained(t, s, a.Snapshot)
	stats, err := s.Prune(ctx)
	if err != nil || stats.Views != 1 || stats.Bytes > policy.MaxBytes {
		t.Fatalf("budget %+v %v", stats, err)
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "staging"))
	if err != nil || len(entries) != 0 {
		t.Fatal("failed capacity admission leaked staging", err)
	}
}

func TestRetentionEvictsIdleUnpinnedAndKeepsActiveBase(t *testing.T) {
	ctx := context.Background()
	source, a := testSource(t, "sha1", true)
	s := openTestStore(t, 0)
	clock := testRetentionClock(s)
	if err := s.ConfigureRetention(RetentionPolicy{MaxBytes: 1 << 20, MaxViews: 2, MinAge: time.Second, MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := installForRetention(ctx, s, source, a); err != nil {
		t.Fatal(err)
	}
	b := nextRetentionView(t, source, a, "second")
	if err := installForRetention(ctx, s, source, b); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	pinned, err := s.Acquire(ctx, a.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	c := nextRetentionView(t, source, b, "third")
	if err := installForRetention(ctx, s, source, c); err != nil {
		t.Fatal(err)
	}
	missingRetained(t, s, b.Snapshot)
	*clock = clock.Add(2 * time.Hour)
	stats, err := s.Prune(ctx)
	if err != nil || stats.Views != 1 || stats.Pinned != 1 || stats.Removed != 1 {
		t.Fatalf("prune %+v %v", stats, err)
	}
	missingRetained(t, s, c.Snapshot)
	pinned.Release()
	*clock = clock.Add(2 * time.Hour)
	stats, err = s.Prune(ctx)
	if err != nil || stats.Views != 0 {
		t.Fatalf("final prune %+v %v", stats, err)
	}
}

func TestRetentionByteBudgetAndRestartGrace(t *testing.T) {
	ctx := context.Background()
	source, a := testSource(t, "sha1", true)
	root := filepath.Join(t.TempDir(), "store")
	s, err := Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureRetention(RetentionPolicy{MaxBytes: 1, MaxViews: 3, MinAge: time.Minute, MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := installForRetention(ctx, s, source, a); !errors.Is(err, ErrBudget) {
		t.Fatalf("byte budget: %v", err)
	}
	policy := RetentionPolicy{MaxBytes: 1 << 20, MaxViews: 1, MinAge: time.Minute, MaxAge: time.Hour}
	if err := s.ConfigureRetention(policy); err != nil {
		t.Fatal(err)
	}
	if err := installForRetention(ctx, s, source, a); err != nil {
		t.Fatal(err)
	}
	key, _ := edgeprotocol.SnapshotKey(a.Snapshot)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "published", key), old, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ConfigureRetention(policy); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Prune(ctx)
	if err != nil || stats.Views != 1 {
		t.Fatalf("restart lost protected negotiation: %+v %v", stats, err)
	}
	b := nextRetentionView(t, source, a, "second")
	base, err := s.AcquireBase(ctx, b.Snapshot)
	if err != nil || base.Snapshot() != a.Snapshot {
		t.Fatalf("base lookup after restart: %v", err)
	}
	base.Release()
	wrong := b.Snapshot
	wrong.Identity.StoreID = "another"
	if _, err := s.AcquireBase(ctx, wrong); !errors.Is(err, ErrMissing) {
		t.Fatal("cross-incarnation base selected", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Prune(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
