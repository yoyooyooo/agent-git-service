package snapshotstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapturePinsLooseAndPackedObjectsWithoutCopyingSourceState(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		for _, layout := range []string{"loose", "packed"} {
			t.Run(format+"/"+layout, func(t *testing.T) {
				ctx := context.Background()
				source, m := testSource(t, format, true)
				secret := commitObject(t, source, "never-export-this", "")
				gitOK(t, source, "", "update-ref", "refs/internal/secret", secret)
				gitOK(t, source, "", "config", "remote.origin.url", "https://user:secret@example.test/repo")
				if layout == "packed" {
					gitOK(t, source, "", "repack", "-ad")
				}
				store := openTestStore(t, 0)
				capture, err := store.NewCapture(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer capture.Close()
				if err := capture.PinSource(ctx, source, m); err != nil {
					t.Fatal(err)
				}
				files, err := objectFiles(ctx, source, format)
				if err != nil || len(files) == 0 {
					t.Fatal("missing source inventory", err)
				}
				for _, name := range files {
					a, err := os.Stat(filepath.Join(source, "objects", name))
					if err != nil {
						t.Fatal(err)
					}
					b, err := os.Stat(filepath.Join(capture.repo, "objects", name))
					if err != nil || !os.SameFile(a, b) || a.Mode() != b.Mode() {
						t.Fatal("capture copied or changed source object", name, err)
					}
				}
				cfg, err := os.ReadFile(filepath.Join(capture.repo, "config"))
				if err != nil || bytes.Contains(cfg, []byte("secret")) {
					t.Fatal("copied source configuration", err)
				}
				if _, err := store.Acquire(ctx, m.Snapshot); !errors.Is(err, ErrMissing) {
					t.Fatal("private capture was publicly available", err)
				}
				// Deleting the source also removes its refs and all object names.
				// Links, not a live directory or alternates, must keep the data alive.
				if err := os.RemoveAll(source); err != nil {
					t.Fatal(err)
				}
				if err := capture.Retain(ctx); err != nil {
					t.Fatal(err)
				}
				if err := capture.Close(); err != nil {
					t.Fatal(err)
				}
				view, err := store.Acquire(ctx, m.Snapshot)
				if err != nil {
					t.Fatal(err)
				}
				defer view.Release()
				if err := verifyRepo(ctx, view.RepoPath(), m); err != nil {
					t.Fatal(err)
				}
				if _, err := runGit(ctx, view.RepoPath(), nil, "cat-file", "-e", secret); err == nil {
					t.Fatal("private source objects escaped capture")
				}
			})
		}
	}
}

func TestCaptureLifetimeCancellationAndUnsafeSource(t *testing.T) {
	for _, mode := range []string{"cancel", "alternate", "object-symlink", "pack-without-index", "promisor"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			source, m := testSource(t, "sha1", true)
			store := openTestStore(t, 0)
			capture, err := store.NewCapture(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer capture.Close()
			if err := capture.WritePack(ctx, &bytes.Buffer{}); err == nil {
				t.Fatal("unpinned capture was readable")
			}
			if err := store.Close(); err == nil {
				t.Fatal("store closed with active capture")
			}
			switch mode {
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "alternate":
				if err := os.WriteFile(filepath.Join(source, "objects/info/alternates"), []byte("/not-a-trusted-store\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "object-symlink":
				path := filepath.Join(source, "objects", m.Snapshot.HEAD.OID[:2], m.Snapshot.HEAD.OID[2:])
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(source, "config"), path); err != nil {
					t.Fatal(err)
				}
			case "pack-without-index", "promisor":
				ext := ".pack"
				if mode == "promisor" {
					ext = ".promisor"
				}
				if err := os.WriteFile(filepath.Join(source, "objects/pack/pack-"+strings.Repeat("a", 40)+ext), []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := capture.PinSource(ctx, source, m); err == nil {
				t.Fatal("unsafe pin succeeded")
			}
			if err := capture.Retain(context.Background()); err == nil {
				t.Fatal("failed pin became publishable")
			}
			if err := capture.Close(); err != nil {
				t.Fatal(err)
			}
			items, err := os.ReadDir(filepath.Join(store.root, "staging"))
			if err != nil || len(items) != 0 {
				t.Fatal("failed capture staging leaked", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal("capture lifetime leaked", err)
			}
		})
	}
}

func TestCaptureReusesRetainedBaseAndSurvivesBaseEviction(t *testing.T) {
	ctx := context.Background()
	source, a := testSource(t, "sha1", true)
	store := openTestStore(t, 0)
	base := importSource(t, store, source, a)
	base.Release()
	gitOK(t, source, "", "update-ref", "refs/heads/main", commitObject(t, source, "next", a.Snapshot.HEAD.OID))
	b := updatedManifest(t, source, a)
	capture, err := store.NewCapture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	if err := capture.PinSource(ctx, source, b); err != nil {
		t.Fatal(err)
	}
	if err := capture.Retain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	view, err := store.Acquire(ctx, b.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Release()
	files, err := objectFiles(ctx, base.RepoPath(), "sha1")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		oldInfo, err := os.Stat(filepath.Join(base.RepoPath(), "objects", name))
		if err != nil {
			t.Fatal(err)
		}
		newInfo, err := os.Stat(filepath.Join(view.RepoPath(), "objects", name))
		if err != nil || !os.SameFile(oldInfo, newInfo) {
			t.Fatal("retention repacked unchanged history", name, err)
		}
	}
	if err := store.Remove(a.Snapshot); err != nil {
		t.Fatal(err)
	}
	if err := verifyRepo(ctx, view.RepoPath(), b); err != nil {
		t.Fatal("base eviction damaged linked view", err)
	}
}
