package snapshotstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIncrementalReusesPackInodesAndSurvivesRestartAndBaseEviction(t *testing.T) {
	ctx := context.Background()
	source, a := testSource(t, "sha1", false)
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	blob := gitOK(t, source, string(payload), "hash-object", "-w", "--stdin")
	tree := gitOK(t, source, "100644 blob "+blob+"\tlarge.bin\n", "mktree")
	root := gitOK(t, source, "", "-c", "user.name=Reuse Test", "-c", "user.email=reuse@example.test", "commit-tree", tree, "-m", "baseline")
	gitOK(t, source, "", "update-ref", "refs/heads/main", root)
	a = updatedManifest(t, source, a)
	primary, local := openTestStore(t, 0), openTestStore(t, 0)
	base := importSource(t, primary, source, a)
	localBase := importSource(t, local, source, a)
	head := commitObject(t, source, "small addition", root)
	gitOK(t, source, "", "update-ref", "refs/heads/main", head)
	b := updatedManifest(t, source, a)
	target := importSource(t, primary, source, b)
	var delta bytes.Buffer
	if err := target.WriteIncrementalPack(ctx, base, &delta); err != nil {
		t.Fatal(err)
	}
	if err := local.InstallIncremental(ctx, b, a.Snapshot, packProducer(delta.Bytes())); err != nil {
		t.Fatal(err)
	}
	view, err := local.Acquire(ctx, b.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Release()
	files, err := objectFiles(ctx, localBase.RepoPath(), "sha1")
	if err != nil {
		t.Fatal(err)
	}
	var sharedBytes, newPackBytes int64
	for _, name := range files {
		oldInfo, err := os.Stat(filepath.Join(localBase.RepoPath(), "objects", name))
		if err != nil {
			t.Fatal(err)
		}
		newInfo, err := os.Stat(filepath.Join(view.RepoPath(), "objects", name))
		if err != nil || !os.SameFile(oldInfo, newInfo) {
			t.Fatalf("base object was repacked/copied: %s %v", name, err)
		}
		if strings.HasSuffix(name, ".pack") {
			sharedBytes += oldInfo.Size()
		}
	}
	newFiles, err := objectFiles(ctx, view.RepoPath(), "sha1")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range newFiles {
		if !strings.HasSuffix(name, ".pack") {
			continue
		}
		info, err := os.Stat(filepath.Join(view.RepoPath(), "objects", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(localBase.RepoPath(), "objects", name)); os.IsNotExist(err) {
			newPackBytes += info.Size()
		}
	}
	if sharedBytes < 1<<20 || newPackBytes >= 8<<10 {
		t.Fatalf("unexpected reuse: shared=%d new=%d", sharedBytes, newPackBytes)
	}
	t.Logf("shared_pack_bytes=%d new_pack_bytes=%d delta_bytes=%d", sharedBytes, newPackBytes, delta.Len())
	localBase.Release()
	view.Release()
	if err := local.Remove(a.Snapshot); err != nil {
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(local.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.Acquire(ctx, b.Snapshot)
	if err != nil {
		t.Fatal("restarted linked snapshot failed verification", err)
	}
	defer persisted.Release()
	if err := verifyRepo(ctx, persisted.RepoPath(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, persisted.RepoPath(), nil, "cat-file", "-e", blob); err != nil {
		t.Fatal("base eviction removed a still-required blob", err)
	}
}

func TestPackReuseRejectsOldSupersetAndSharedSizeOverflow(t *testing.T) {
	ctx := context.Background()
	source, a := testSource(t, "sha1", true)
	removed := commitObject(t, source, "old branch only", "")
	gitOK(t, source, "", "update-ref", "refs/heads/deleted", removed)
	a = updatedManifest(t, source, a)
	primary, local := openTestStore(t, 0), openTestStore(t, 0)
	base := importSource(t, primary, source, a)
	old := importSource(t, local, source, a)
	gitOK(t, source, "", "update-ref", "-d", "refs/heads/deleted")
	b := updatedManifest(t, source, a)
	target := importSource(t, primary, source, b)
	var delta bytes.Buffer
	if err := target.WriteIncrementalPack(ctx, base, &delta); err != nil {
		t.Fatal(err)
	}
	if err := local.InstallIncremental(ctx, b, a.Snapshot, packProducer(delta.Bytes())); err != nil {
		t.Fatal(err)
	}
	view, err := local.Acquire(ctx, b.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Release()
	oldFiles, err := objectFiles(ctx, old.RepoPath(), "sha1")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range oldFiles {
		if _, err := os.Stat(filepath.Join(view.RepoPath(), "objects", name)); !os.IsNotExist(err) {
			t.Fatal("superset pack was shared after deletion", name, err)
		}
	}
	if _, err := runGit(ctx, view.RepoPath(), nil, "cat-file", "-e", removed); err == nil {
		t.Fatal("removed object leaked")
	}
	// A tiny incremental transfer must not bypass the total resulting-pack cap.
	gitOK(t, source, "", "update-ref", "refs/heads/main", commitObject(t, source, "next", b.Snapshot.HEAD.OID))
	c := updatedManifest(t, source, a)
	last := importSource(t, primary, source, c)
	var small bytes.Buffer
	if err := last.WriteIncrementalPack(ctx, target, &small); err != nil {
		t.Fatal(err)
	}
	local.maxPack = int64(small.Len())
	if err := local.InstallIncremental(ctx, c, b.Snapshot, packProducer(small.Bytes())); err == nil {
		t.Fatal("small delta bypassed total pack cap")
	}
}

func TestCapturePinDoesNotWaitForSnapshotStoreVerificationMutex(t *testing.T) {
	source, m := testSource(t, "sha1", true)
	store := openTestStore(t, 0)
	capture, err := store.NewCapture(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	// A retained snapshot verification may hold Store.mu. PinSource must not
	// wait on that mutex while the caller holds the primary write barrier.
	store.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- capture.PinSource(ctx, source, m) }()
	select {
	case err = <-done:
		store.mu.Unlock()
	case <-ctx.Done():
		store.mu.Unlock()
		<-done
		t.Fatal("pin waited for the snapshot store mutex")
	}
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := capture.WritePack(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if err := store.Install(context.Background(), m, func(_ context.Context, w io.Writer) error {
		_, err := w.Write(out.Bytes())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
