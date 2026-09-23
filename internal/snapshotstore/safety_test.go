package snapshotstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotPreservesTagsPRRefsAndTheirDeletion(t *testing.T) {
	source, base := testSource(t, "sha1", true)
	gitOK(t, source, "", "-c", "user.name=Snapshot Test", "-c", "user.email=snapshot@example.test", "tag", "-a", "v1", "-m", "annotated tag", base.Snapshot.HEAD.OID)
	gitOK(t, source, "", "update-ref", "refs/pull/42/head", base.Snapshot.HEAD.OID)
	a, err := ObserveManifest(context.Background(), source, base.Snapshot.Identity, "policy-1", []string{"refs/heads/", "refs/tags/", "refs/pull/"})
	if err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, 0)
	first := importSource(t, s, source, a)
	if len(a.Refs) != 3 {
		t.Fatalf("not all export refs captured: %v", a.Refs)
	}
	if got := gitOK(t, first.RepoPath(), "", "rev-parse", "refs/tags/v1^{}"); got != base.Snapshot.HEAD.OID {
		t.Fatal("annotated tag object missing")
	}
	gitOK(t, source, "", "update-ref", "-d", "refs/tags/v1")
	gitOK(t, source, "", "update-ref", "-d", "refs/pull/42/head")
	b, err := ObserveManifest(context.Background(), source, base.Snapshot.Identity, "policy-1", []string{"refs/heads/", "refs/tags/", "refs/pull/"})
	if err != nil {
		t.Fatal(err)
	}
	second := importSource(t, s, source, b)
	if a.Snapshot.RefsDigest == b.Snapshot.RefsDigest {
		t.Fatal("deletion did not change manifest")
	}
	if _, err := runGit(context.Background(), second.RepoPath(), nil, "show-ref", "--verify", "refs/tags/v1"); err == nil {
		t.Fatal("deleted tag remained in new view")
	}
	if got := gitOK(t, first.RepoPath(), "", "rev-parse", "refs/pull/42/head"); got != base.Snapshot.HEAD.OID {
		t.Fatal("old pinned PR ref changed")
	}
}

func TestSnapshotReopenRejectsConfigAndManifestSymlink(t *testing.T) {
	for _, test := range []string{"config", "manifest symlink", "object symlink"} {
		t.Run(test, func(t *testing.T) {
			source, m := testSource(t, "sha1", true)
			root := filepath.Join(t.TempDir(), "store")
			s, err := Open(root, 0)
			if err != nil {
				t.Fatal(err)
			}
			v := importSource(t, s, source, m)
			viewRoot, repo := v.ProjectRoot(), v.RepoPath()
			v.Release()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			switch test {
			case "config":
				f, err := os.OpenFile(filepath.Join(repo, "config"), os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("[uploadpack]\n\tpackObjectsHook = should-never-run\n")
				if err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			case "manifest symlink":
				p := filepath.Join(viewRoot, "manifest.json")
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "manifest.json")
				if err := os.WriteFile(target, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, p); err != nil {
					t.Fatal(err)
				}
			case "object symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(repo, "objects", "untrusted")); err != nil {
					t.Fatal(err)
				}
			}
			s, err = Open(root, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if v, err := s.Acquire(context.Background(), m.Snapshot); err == nil {
				v.Release()
				t.Fatal("tampered snapshot passed startup verification")
			}
		})
	}
}

func TestSnapshotRootDoesNotFollowStagingSymlinkOrRelaxPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "preserve.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "staging")); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(root, 0); err == nil {
		_ = s.Close()
		t.Fatal("followed staging symlink")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatal("cleanup reached outside root")
	}
	if err := os.Remove(filepath.Join(root, "staging")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(root, 0); err == nil {
		_ = s.Close()
		t.Fatal("accepted non-private root")
	}
}

func TestObserveManifestRejectsUnresolvedPolicyAndSpecialSources(t *testing.T) {
	for _, key := range []string{"uploadpack.hideRefs", "transfer.hideRefs", "extensions.partialclone"} {
		t.Run(key, func(t *testing.T) {
			source, m := testSource(t, "sha1", true)
			gitOK(t, source, "", "config", key, "refs/heads/private")
			if _, err := ObserveManifest(context.Background(), source, m.Snapshot.Identity, "policy-1", []string{"refs/heads/"}); err == nil {
				t.Fatal("source policy silently ignored")
			}
		})
	}
	source, m := testSource(t, "sha1", true)
	if _, err := ObserveManifest(context.Background(), source, m.Snapshot.Identity, "policy-1", nil); err == nil {
		t.Fatal("implicit export-all accepted")
	}
	if err := os.WriteFile(filepath.Join(source, "shallow"), []byte(m.Snapshot.HEAD.OID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveManifest(context.Background(), source, m.Snapshot.Identity, "policy-1", []string{"refs/heads/"}); err == nil {
		t.Fatal("shallow source accepted as complete")
	}
	if err := os.Remove(filepath.Join(source, "shallow")); err != nil {
		t.Fatal(err)
	}
	gitOK(t, source, "", "symbolic-ref", "refs/heads/alias", "refs/heads/main")
	if _, err := ObserveManifest(context.Background(), source, m.Snapshot.Identity, "policy-1", []string{"refs/heads/"}); err == nil {
		t.Fatal("symbolic alias silently flattened")
	}
}

func TestSnapshotClosedAndCanceledAcquireAreNotCacheHits(t *testing.T) {
	source, m := testSource(t, "sha1", true)
	s := openTestStore(t, 0)
	v := importSource(t, s, source, m)
	v.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Acquire(ctx, m.Snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(context.Background(), m.Snapshot); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed store served data: %v", err)
	}
	bad := m.Snapshot
	bad.Identity.StoreID = "../" + strings.Repeat("x", 10)
	if err := s.Remove(bad); err == nil {
		t.Fatal("invalid snapshot selected a disk path")
	}
}
