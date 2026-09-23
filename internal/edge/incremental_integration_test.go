package edge_test

import (
	"context"
	"crypto/rand"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func TestEdgeNativeFetchUsesSmallIncrementWithUnchangedRemote(t *testing.T) {
	f := newControlFixture(t)
	// Incompressible deterministic-size fixture: do not confuse Git/zlib
	// compression of repetitive content with actual incremental transfer.
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Git.WriteFile(f.ctx, f.repo.FullName, "main", "large.bin", "base data", data); err != nil {
		t.Fatal(err)
	}
	mirror := newReplicaMirror(t, f.peer)
	ingress := newReadIngress(t, f, mirror, 5*time.Minute, nil)
	dir := filepath.Join(t.TempDir(), "clone")
	ingress.git(t, f, "", "2", "clone", ingress.remote, dir)
	before := mirror.Stats()
	if before.FullTransfers != 1 || before.IncrementalTransfers != 0 || before.TransferBytes < 1<<20 {
		t.Fatalf("cold transfer %+v", before)
	}
	sha, err := f.svc.Git.WriteFile(f.ctx, f.repo.FullName, "main", "small.txt", "tiny change", []byte("incremental\n"))
	if err != nil {
		t.Fatal(err)
	}
	ingress.git(t, f, dir, "2", "fetch", "origin")
	after := mirror.Stats()
	increment := after.TransferBytes - before.TransferBytes
	if after.IncrementalTransfers != 1 || after.FullTransfers != 1 || increment <= 0 || increment > before.TransferBytes/20 {
		t.Fatalf("not incremental: before=%+v after=%+v", before, after)
	}
	if got := replicaGit(t, dir, "", "rev-parse", "origin/main"); got != sha {
		t.Fatalf("stale incremental result %s", got)
	}
	if got := replicaGit(t, dir, "", "remote", "get-url", "origin"); got != ingress.remote {
		t.Fatal("changed remote")
	}
	if ingress.upstreamReads.Load() != 0 {
		t.Fatal("download fell back to primary")
	}
	t.Logf("cold_pack_bytes=%d incremental_pack_bytes=%d completed_full=%d completed_incremental=%d", before.TransferBytes, increment, after.FullTransfers, after.IncrementalTransfers)
	// Cache survives restarting just the Mirror and still chooses a verified
	// base from disk; it does not require a remembered user or previous plan.
	root := filepath.Join(t.TempDir(), "restart-cache")
	cfg := edge.MirrorConfig{Root: root, Concurrency: 1, MaxPending: 8, SyncTimeout: time.Minute, MaxPackBytes: 8 << 20}
	m, err := edge.NewMirror(context.Background(), cfg, f.peer)
	if err != nil {
		t.Fatal(err)
	}
	p := f.prepare(t)
	v, err := m.EnsureSnapshot(context.Background(), p.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	v.Release()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = edge.NewMirror(context.Background(), cfg, f.peer)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := f.svc.Git.WriteFile(f.ctx, f.repo.FullName, "main", "later.txt", "later change", []byte("later\n")); err != nil {
		t.Fatal(err)
	}
	p = f.prepare(t)
	v, err = m.EnsureSnapshot(context.Background(), p.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	v.Release()
	if stats := m.Stats(); stats.IncrementalTransfers != 1 || stats.FullTransfers != 0 {
		t.Fatalf("restart ignored base: %+v", stats)
	}
}

func TestIncrementalPeerMissingBaseDeclaresExactFullTarget(t *testing.T) {
	f := newControlFixture(t)
	first := f.prepare(t)
	mirror := newReplicaMirror(t, f.peer)
	v, err := mirror.EnsureSnapshot(context.Background(), first.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	v.Release()
	if _, err := f.svc.Git.WriteFile(f.ctx, f.repo.FullName, "main", "next", "advance", []byte("new")); err != nil {
		t.Fatal(err)
	}
	second := f.prepare(t)
	if err := f.retained.Remove(first.Snapshot); err != nil {
		t.Fatal(err)
	}
	header, body, err := f.peer.OpenTransfer(context.Background(), second.Snapshot, &first.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(io.Discard, body)
	_ = body.Close()
	if copyErr != nil || header.Base != nil || header.Manifest.Snapshot != second.Snapshot {
		t.Fatal("wrong full fallback", copyErr)
	}
	v, err = mirror.EnsureSnapshot(context.Background(), second.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	v.Release()
	if stats := mirror.Stats(); stats.FullTransfers != 2 || stats.IncrementalTransfers != 0 {
		t.Fatalf("fallback not declared %+v", stats)
	}
}

type substitutingTransferSource struct{ *edge.PeerClient }

func (s substitutingTransferSource) OpenTransfer(ctx context.Context, target edgeprotocol.RepositorySnapshot, base *edgeprotocol.RepositorySnapshot) (edgeprotocol.TransferHeader, io.ReadCloser, error) {
	h, b, err := s.PeerClient.OpenTransfer(ctx, target, base)
	if err == nil {
		wrong := target
		h.Base = &wrong
	}
	return h, b, err
}

func TestMirrorRejectsSubstitutedIncrementalBaseWithoutPublishing(t *testing.T) {
	f := newControlFixture(t)
	mirror := newReplicaMirror(t, substitutingTransferSource{f.peer})
	first := f.prepare(t)
	v, err := mirror.EnsureSnapshot(context.Background(), first.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	v.Release()
	if _, err := f.svc.Git.WriteFile(f.ctx, f.repo.FullName, "main", "next", "advance", []byte("new")); err != nil {
		t.Fatal(err)
	}
	second := f.prepare(t)
	if v, err := mirror.EnsureSnapshot(context.Background(), second.Snapshot); err == nil {
		v.Release()
		t.Fatal("accepted substituted base")
	}
	if stats := mirror.Stats(); stats.IncrementalTransfers != 0 || stats.FullTransfers != 1 {
		t.Fatalf("silently downgraded %+v", stats)
	}
	v, err = mirror.EnsureSnapshot(context.Background(), first.Snapshot)
	if err != nil {
		t.Fatal("old view poisoned", err)
	}
	v.Release()
}
