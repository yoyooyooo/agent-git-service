package snapshotstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func updatedManifest(t *testing.T, source string, old edgeprotocol.Manifest) edgeprotocol.Manifest {
	t.Helper()
	m, err := ObserveManifest(context.Background(), source, old.Snapshot.Identity, old.Snapshot.ExportPolicyRevision, []string{"refs/heads/", "refs/tags/", "refs/pull/"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func packProducer(data []byte) Producer {
	return func(_ context.Context, w io.Writer) error { _, err := w.Write(data); return err }
}

func TestIncrementalExactObjectsAndIndependentPublication(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			source, a := testSource(t, format, true)
			primary, local := openTestStore(t, 0), openTestStore(t, 0)
			base := importSource(t, primary, source, a)
			localBase := importSource(t, local, source, a)
			bOID := commitObject(t, source, "second", a.Snapshot.HEAD.OID)
			gitOK(t, source, "", "update-ref", "refs/heads/main", bOID)
			b := updatedManifest(t, source, a)
			target := importSource(t, primary, source, b)
			var delta, full bytes.Buffer
			if err := target.WriteIncrementalPack(ctx, base, &delta); err != nil {
				t.Fatal(err)
			}
			if err := target.WritePack(ctx, &full); err != nil {
				t.Fatal(err)
			}
			if n := binary.BigEndian.Uint32(delta.Bytes()[8:12]); n != 3 {
				t.Fatalf("missing objects=%d want 3", n)
			}
			if delta.Len() >= full.Len() {
				t.Fatalf("delta=%d full=%d", delta.Len(), full.Len())
			}
			if err := local.InstallIncremental(ctx, b, a.Snapshot, packProducer(delta.Bytes())); err != nil {
				t.Fatal(err)
			}
			localBase.Release()
			if err := local.Remove(a.Snapshot); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(source); err != nil {
				t.Fatal(err)
			}
			view, err := local.Acquire(ctx, b.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Release()
			if err := verifyRepo(ctx, view.RepoPath(), b); err != nil {
				t.Fatal(err)
			}
			if got := gitOK(t, view.RepoPath(), "", "rev-list", "--count", "HEAD"); got != "2" {
				t.Fatal(got)
			}
		})
	}
}

func TestIncrementalForceUpdateDeletionAndEmptyTarget(t *testing.T) {
	for _, mode := range []string{"unrelated", "delete-only", "empty"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			source, a := testSource(t, "sha1", true)
			orphan := commitObject(t, source, "branch-only-secret", "")
			gitOK(t, source, "", "update-ref", "refs/heads/remove-me", orphan)
			a = updatedManifest(t, source, a)
			primary, local := openTestStore(t, 0), openTestStore(t, 0)
			base := importSource(t, primary, source, a)
			localBase := importSource(t, local, source, a)
			gitOK(t, source, "", "update-ref", "-d", "refs/heads/remove-me")
			switch mode {
			case "unrelated":
				gitOK(t, source, "", "update-ref", "refs/heads/main", commitObject(t, source, "replacement", ""))
			case "empty":
				gitOK(t, source, "", "update-ref", "-d", "refs/heads/main")
			}
			b := updatedManifest(t, source, a)
			target := importSource(t, primary, source, b)
			var delta bytes.Buffer
			if err := target.WriteIncrementalPack(ctx, base, &delta); err != nil {
				t.Fatal(err)
			}
			if mode != "unrelated" && binary.BigEndian.Uint32(delta.Bytes()[8:12]) != 0 {
				t.Fatal("deletion sent historical objects")
			}
			if err := local.InstallIncremental(ctx, b, a.Snapshot, packProducer(delta.Bytes())); err != nil {
				t.Fatal(err)
			}
			localBase.Release()
			if err := local.Remove(a.Snapshot); err != nil {
				t.Fatal(err)
			}
			view, err := local.Acquire(ctx, b.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Release()
			if err := verifyRepo(ctx, view.RepoPath(), b); err != nil {
				t.Fatal(err)
			}
			if _, err := runGit(ctx, view.RepoPath(), nil, "cat-file", "-e", orphan); err == nil {
				t.Fatal("deleted branch objects leaked")
			}
		})
	}
}

func TestIncrementalRejectsMismatchCorruptionAndSuperset(t *testing.T) {
	ctx := context.Background()
	source, a := testSource(t, "sha1", true)
	primary, local := openTestStore(t, 0), openTestStore(t, 0)
	base := importSource(t, primary, source, a)
	importSource(t, local, source, a)
	gitOK(t, source, "", "update-ref", "refs/heads/main", commitObject(t, source, "second", a.Snapshot.HEAD.OID))
	b := updatedManifest(t, source, a)
	target := importSource(t, primary, source, b)
	var delta, full bytes.Buffer
	if err := target.WriteIncrementalPack(ctx, base, &delta); err != nil {
		t.Fatal(err)
	}
	if err := target.WritePack(ctx, &full); err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{delta.Bytes()[:len(delta.Bytes())-1], full.Bytes(), append(append([]byte{}, delta.Bytes()...), []byte("trailing")...)} {
		if err := local.InstallIncremental(ctx, b, a.Snapshot, packProducer(data)); err == nil {
			t.Fatal("accepted corrupt/superset/trailing pack")
		}
		if v, err := local.Acquire(ctx, b.Snapshot); !errors.Is(err, ErrMissing) {
			if v != nil {
				v.Release()
			}
			t.Fatal("failed import was published", err)
		}
	}
	wrong := a.Snapshot
	wrong.Identity.StoreID = "other"
	if err := local.InstallIncremental(ctx, b, wrong, packProducer(delta.Bytes())); err == nil {
		t.Fatal("cross-store base accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := local.InstallIncremental(cancelled, b, a.Snapshot, packProducer(delta.Bytes())); err == nil {
		t.Fatal("ignored cancellation")
	}
	staging, err := os.ReadDir(filepath.Join(local.root, "staging"))
	if err != nil || len(staging) != 0 {
		t.Fatal("failed staging not removed", err)
	}
	if err := local.InstallIncremental(ctx, b, a.Snapshot, packProducer(delta.Bytes())); err != nil {
		t.Fatal(err)
	}
	v, err := local.Acquire(ctx, b.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Release()
	if got := gitOK(t, v.RepoPath(), "", "show", "HEAD:README.md"); strings.TrimSpace(got) != "second" {
		t.Fatal(got)
	}
}
