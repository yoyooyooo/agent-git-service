package snapshotstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/gitbackend"
)

func gitOK(t *testing.T, dir string, input string, args ...string) string {
	t.Helper()
	out, err := runGit(context.Background(), dir, strings.NewReader(input), args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func testSource(t *testing.T, format string, seed bool) (string, edgeprotocol.Manifest) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source.git")
	gitOK(t, root, "", "init", "--bare", "--template=", "--object-format="+format, source)
	gitOK(t, source, "", "symbolic-ref", "HEAD", "refs/heads/main")
	if seed {
		oid := commitObject(t, source, "public-content", "")
		gitOK(t, source, "", "update-ref", "refs/heads/main", oid)
	}
	m, err := ObserveManifest(context.Background(), source, edgeprotocol.RepositoryIdentity{AuthorityID: "primary", StoreID: "immutable-store", RepositoryID: 1, Kind: "repo"}, "public-refs-v1", []string{"refs/heads/", "refs/tags/", "refs/pull/"})
	if err != nil {
		t.Fatal(err)
	}
	return source, m
}

func commitObject(t *testing.T, source, content, parent string) string {
	t.Helper()
	blob := gitOK(t, source, content, "hash-object", "-w", "--stdin")
	tree := gitOK(t, source, "100644 blob "+blob+"\tREADME.md\n", "mktree")
	args := []string{"-c", "user.name=Snapshot Test", "-c", "user.email=snapshot@example.test", "commit-tree", tree, "-m", content}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return gitOK(t, source, "", args...)
}

func openTestStore(t *testing.T, max int64) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store"), max)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func importSource(t *testing.T, s *Store, source string, m edgeprotocol.Manifest) *Lease {
	t.Helper()
	if err := s.Install(context.Background(), m, func(ctx context.Context, w io.Writer) error { return WritePack(ctx, source, m, w) }); err != nil {
		t.Fatal(err)
	}
	v, err := s.Acquire(context.Background(), m.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Release)
	return v
}

func TestSnapshotOwnsOnlyExportedObjectsAndSurvivesSourceRemoval(t *testing.T) {
	source, m := testSource(t, "sha1", true)
	secret := commitObject(t, source, "private-unreachable-content", "")
	gitOK(t, source, "", "update-ref", "refs/internal/secret", secret)
	if err := os.MkdirAll(filepath.Join(source, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "hooks", "post-update"), []byte("DO NOT COPY"), 0700); err != nil {
		t.Fatal(err)
	}
	gitOK(t, source, "", "config", "remote.origin.url", "https://credential:secret@example.test/repo")
	s := openTestStore(t, 0)
	v := importSource(t, s, source, m)
	if _, err := runGit(context.Background(), v.RepoPath(), nil, "cat-file", "-e", secret); err == nil {
		t.Fatal("non-exported object leaked")
	}
	for _, p := range []string{"hooks/post-update", "objects/info/alternates"} {
		if _, err := os.Stat(filepath.Join(v.RepoPath(), p)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("copied source-only %s", p)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(v.RepoPath(), "config"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cfg, []byte("credential")) {
		t.Fatal("source config copied")
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	if got := gitOK(t, v.RepoPath(), "", "show", "HEAD:README.md"); got != "public-content" {
		t.Fatalf("lost independent objects: %s", got)
	}
	if err := s.Remove(m.Snapshot); !errors.Is(err, ErrPinned) {
		t.Fatalf("GC removed live reader: %v", err)
	}
	if err := s.Close(); !errors.Is(err, ErrPinned) {
		t.Fatalf("closed with reader: %v", err)
	}
	v.Release()
	v.Release()
	if err := s.Remove(m.Snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(context.Background(), m.Snapshot); !errors.Is(err, ErrMissing) {
		t.Fatalf("deleted snapshot still available: %v", err)
	}
}

func TestSnapshotNativeGitCloneProtocolsAndShallowFetch(t *testing.T) {
	source, m := testSource(t, "sha1", true)
	next := commitObject(t, source, "second-content", m.Snapshot.HEAD.OID)
	gitOK(t, source, "", "update-ref", "refs/heads/main", next)
	m, err := ObserveManifest(context.Background(), source, m.Snapshot.Identity, "public-refs-v1", []string{"refs/heads/"})
	if err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, 0)
	v := importSource(t, s, source, m)
	var protocolV2 atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Git-Protocol") == "version=2" {
			protocolV2.Store(true)
		}
		err := gitbackend.Serve(w, r, gitbackend.Request{ProjectRoot: v.ProjectRoot(), Repository: v.Repository(), Service: gitbackend.UploadPack, Advertise: r.Method == http.MethodGet, IsolatedRead: true})
		if err != nil {
			t.Errorf("backend: %v", err)
			http.Error(w, "backend failed", 500)
		}
	}))
	defer server.Close()
	for _, protocol := range []string{"0", "2"} {
		t.Run("v"+protocol, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "clone")
			gitOK(t, root, "", "-c", "protocol.http.allow=always", "-c", "protocol.version="+protocol, "clone", "--depth=1", server.URL+"/public/repo.git", dest)
			if head := gitOK(t, dest, "", "rev-parse", "HEAD"); head != next {
				t.Fatal("wrong cloned HEAD")
			}
			gitOK(t, dest, "", "-c", "protocol.http.allow=always", "-c", "protocol.version="+protocol, "fetch", "--unshallow")
			if count := gitOK(t, dest, "", "rev-list", "--count", "HEAD"); count != "2" {
				t.Fatalf("incomplete unshallow: %s", count)
			}
		})
	}
	if !protocolV2.Load() {
		t.Fatal("v2 test silently used another protocol")
	}
}

func TestSnapshotEmptyDetachedSHA256AndOldViewAfterForceUpdate(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		for _, seed := range []bool{false, true} {
			name := format + "-empty"
			if seed {
				name = format + "-detached"
			}
			t.Run(name, func(t *testing.T) {
				source, m := testSource(t, format, seed)
				if seed {
					if err := os.WriteFile(filepath.Join(source, "HEAD"), []byte(m.Snapshot.HEAD.OID+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					var err error
					m, err = ObserveManifest(context.Background(), source, m.Snapshot.Identity, "public-refs-v1", []string{"refs/heads/"})
					if err != nil {
						t.Fatal(err)
					}
				}
				s := openTestStore(t, 0)
				v := importSource(t, s, source, m)
				if v.Snapshot() != m.Snapshot {
					t.Fatal("changed manifest")
				}
			})
		}
	}
	source, a := testSource(t, "sha1", true)
	s := openTestStore(t, 0)
	old := importSource(t, s, source, a)
	force := commitObject(t, source, "unrelated-new-history", "")
	gitOK(t, source, "", "update-ref", "refs/heads/main", force)
	b, err := ObserveManifest(context.Background(), source, a.Snapshot.Identity, "public-refs-v1", []string{"refs/heads/"})
	if err != nil {
		t.Fatal(err)
	}
	newView := importSource(t, s, source, b)
	if got := gitOK(t, old.RepoPath(), "", "rev-parse", "HEAD"); got != a.Snapshot.HEAD.OID {
		t.Fatal("published old view mutated")
	}
	if got := gitOK(t, newView.RepoPath(), "", "rev-parse", "HEAD"); got != force {
		t.Fatal("new view not published")
	}
}

func TestSnapshotRejectsBadTransfersWithoutPublishing(t *testing.T) {
	source, m := testSource(t, "sha1", true)
	var good bytes.Buffer
	if err := WritePack(context.Background(), source, m, &good); err != nil {
		t.Fatal(err)
	}
	for name, produce := range map[string]Producer{
		"truncated": func(ctx context.Context, w io.Writer) error {
			_, err := w.Write(good.Bytes()[:good.Len()/2])
			return err
		},
		"transfer error": func(ctx context.Context, w io.Writer) error { _, _ = w.Write(good.Bytes()); return io.ErrUnexpectedEOF },
		"trailing bytes": func(ctx context.Context, w io.Writer) error {
			_, _ = w.Write(good.Bytes())
			_, err := w.Write([]byte("trailer"))
			return err
		},
		"wrong pack": func(ctx context.Context, w io.Writer) error { _, err := w.Write([]byte("not a Git pack")); return err },
	} {
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t, 0)
			if err := s.Install(context.Background(), m, produce); err == nil {
				t.Fatal("bad transfer accepted")
			}
			if _, err := s.Acquire(context.Background(), m.Snapshot); !errors.Is(err, ErrMissing) {
				t.Fatalf("failed import published: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(s.root, "staging"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("staging leaked: %v", err)
			}
		})
	}
	t.Run("size limit", func(t *testing.T) {
		s := openTestStore(t, 16)
		if err := s.Install(context.Background(), m, func(ctx context.Context, w io.Writer) error { return WritePack(ctx, source, m, w) }); err == nil {
			t.Fatal("limit ignored")
		}
	})
	t.Run("canceled", func(t *testing.T) {
		s := openTestStore(t, 0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.Install(ctx, m, func(context.Context, io.Writer) error { t.Fatal("called producer after cancellation"); return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled import: %v", err)
		}
	})
}

func TestSnapshotRejectsPackSupersetAndWrongManifest(t *testing.T) {
	source, m := testSource(t, "sha1", true)
	secret := commitObject(t, source, "secret-unreachable", "")
	gitOK(t, source, "", "update-ref", "refs/hidden/secret", secret)
	all, err := ObserveManifest(context.Background(), source, m.Snapshot.Identity, "public-refs-v1", []string{"refs/"})
	if err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, 0)
	if err := s.Install(context.Background(), m, func(ctx context.Context, w io.Writer) error { return WritePack(ctx, source, all, w) }); err == nil {
		t.Fatal("extra hidden object accepted")
	}
	wrong := m.Clone()
	wrong.Snapshot.RefsDigest = strings.Repeat("f", 64)
	if err := s.Install(context.Background(), wrong, func(context.Context, io.Writer) error { t.Fatal("producer called on malformed descriptor"); return nil }); err == nil {
		t.Fatal("bad descriptor accepted")
	}
}

func TestSnapshotExclusiveOwnerRecoveryAndCorruption(t *testing.T) {
	source, m := testSource(t, "sha1", true)
	root := filepath.Join(t.TempDir(), "store")
	s, err := Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	v := importSource(t, s, source, m)
	v.Release()
	if other, err := Open(root, 0); err == nil {
		_ = other.Close()
		t.Fatal("duplicate owner acquired root")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "staging", "interrupted"), 0700); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, err = s.Acquire(context.Background(), m.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	path := v.RepoPath()
	v.Release()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "staging", "interrupted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("interrupted staging survived recovery")
	}
	if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: refs/heads/other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Acquire(context.Background(), m.Snapshot); err == nil {
		t.Fatal("corrupt persisted view served after restart")
	}
}
