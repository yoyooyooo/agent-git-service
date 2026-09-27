package snapshotstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func restartRetentionFixture(t *testing.T) (*Store, string, edgeprotocol.Manifest, RetentionPolicy) {
	t.Helper()
	source, manifest := testSource(t, "sha1", true)
	s := openTestStore(t, 0)
	past := time.Now().Add(-2 * time.Hour)
	s.openedAt = past
	s.now = func() time.Time { return past }
	policy := RetentionPolicy{MaxBytes: 1 << 20, MaxViews: 1, MinAge: time.Minute, MaxAge: 24 * time.Hour}
	if err := s.ConfigureRetention(policy); err != nil {
		t.Fatal(err)
	}
	if err := installForRetention(context.Background(), s, source, manifest); err != nil {
		t.Fatal(err)
	}
	key, _ := edgeprotocol.SnapshotKey(manifest.Snapshot)
	if err := os.Chtimes(filepath.Join(s.root, "published", key), past, past); err != nil {
		t.Fatal(err)
	}
	s.now = time.Now
	return s, source, manifest, policy
}

func reopenRetention(t *testing.T, root string, policy RetentionPolicy) *Store {
	t.Helper()
	s, err := Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.ConfigureRetention(policy); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCleanRestartDoesNotProtectIdleHistoryAgainAtFullCapacity(t *testing.T) {
	s, source, first, policy := restartRetentionFixture(t)
	root := s.root
	second := nextRetentionView(t, source, first, "new main after upgrade")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "staging", retentionCheckpoint)); err != nil {
		t.Fatal("no clean close checkpoint", err)
	}
	reopened := reopenRetention(t, root, policy)
	if _, err := os.Stat(filepath.Join(root, "staging", retentionCheckpoint)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checkpoint not consumed before admission", err)
	}
	if err := installForRetention(context.Background(), reopened, source, second); err != nil {
		t.Fatal("clean upgrade stranded new snapshot behind idle history", err)
	}
	missingRetained(t, reopened, first.Snapshot)
	stats, err := reopened.Prune(context.Background())
	if err != nil || stats.Views != 1 || stats.Bytes > policy.MaxBytes {
		t.Fatal("restart enlarged retention budget", stats, err)
	}
}

func TestCleanRestartRetainsRecentLeaseProtectionWithoutHotPathWrites(t *testing.T) {
	s, source, first, policy := restartRetentionFixture(t)
	used := time.Now().Add(-5 * time.Second)
	s.now = func() time.Time { return used }
	lease, err := s.Acquire(context.Background(), first.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); !errors.Is(err, ErrPinned) {
		t.Fatal("close abandoned an active lease", err)
	}
	if _, err = os.Stat(filepath.Join(s.root, "staging", retentionCheckpoint)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("checkpoint written before leases drained", err)
	}
	lease.Release()
	s.now = time.Now
	root := s.root
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reopenRetention(t, root, policy)
	second := nextRetentionView(t, source, first, "new view")
	if err = installForRetention(context.Background(), reopened, source, second); !errors.Is(err, ErrBudget) {
		t.Fatal("recent Git negotiation lost its protection", err)
	}
	later := time.Now().Add(2 * time.Minute)
	reopened.now = func() time.Time { return later }
	if err = installForRetention(context.Background(), reopened, source, second); err != nil {
		t.Fatal("protection extended beyond real usage", err)
	}
}

func TestCrashAfterReopenCannotReuseOldCleanCheckpoint(t *testing.T) {
	s, source, first, policy := restartRetentionFixture(t)
	root := s.root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	running := reopenRetention(t, root, policy)
	lease, err := running.Acquire(context.Background(), first.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	// Emulate kernel releasing the exclusive file lock on process death, with
	// no Close checkpoint. The test never invokes another owner on a live store.
	if err = unlockRoot(running.lock); err != nil {
		t.Fatal(err)
	}
	running.closed = true
	recovered := reopenRetention(t, root, policy)
	second := nextRetentionView(t, source, first, "after crash")
	if err = installForRetention(context.Background(), recovered, source, second); !errors.Is(err, ErrBudget) {
		t.Fatal("crash discarded unknown recent uses", err)
	}
}

func TestMalformedOrUnknownCheckpointFallsBackToSafeRestartGrace(t *testing.T) {
	for _, payload := range []string{`not-json`, `{"version":2,"closed_at":"2020-01-01T00:00:00Z","last_use":{}}`} {
		t.Run(payload, func(t *testing.T) {
			s, source, first, policy := restartRetentionFixture(t)
			root := s.root
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "staging", retentionCheckpoint), []byte(payload), 0600); err != nil {
				t.Fatal(err)
			}
			recovered := reopenRetention(t, root, policy)
			second := nextRetentionView(t, source, first, "after invalid checkpoint")
			if err := installForRetention(context.Background(), recovered, source, second); !errors.Is(err, ErrBudget) {
				t.Fatal("invalid age evidence bypassed restart grace", err)
			}
		})
	}
}
