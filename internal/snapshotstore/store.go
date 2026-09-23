package snapshotstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

var (
	ErrMissing = errors.New("snapshot not available")
	ErrPinned  = errors.New("snapshot is pinned")
	ErrClosed  = errors.New("snapshot store is closed")
)

const DefaultMaxPackBytes int64 = 4 << 30

type entry struct {
	manifest edgeprotocol.Manifest
	root     string
	refs     int
}

// Store has one exclusive process owner. Published views own their object paths;
// immutable packs can be hardlinked when the new view retains every base object.
// No alternate/source-path dependency survives. GC refuses pins.
// Staging directories are never returned by Acquire, even after a crash.
type Store struct {
	root      string
	maxPack   int64
	lock      *os.File
	mu        sync.Mutex
	entries   map[string]*entry
	closed    bool
	active    int
	retention *RetentionPolicy
	lastUse   map[string]time.Time
	openedAt  time.Time
	now       func() time.Time
	minFree   uint64
}

func Open(root string, maxPack int64) (*Store, error) {
	if root == "" {
		return nil, errors.New("snapshot root is required")
	}
	if maxPack < 0 || maxPack == int64(^uint64(0)>>1) {
		return nil, errors.New("invalid snapshot pack limit")
	}
	if maxPack == 0 {
		maxPack = DefaultMaxPackBytes
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("snapshot root must be owner-only (0700)")
	}
	lock, err := lockRoot(root)
	if err != nil {
		return nil, err
	}
	s := &Store{root: root, maxPack: maxPack, lock: lock, entries: make(map[string]*entry), lastUse: make(map[string]time.Time), openedAt: time.Now(), now: time.Now}
	for _, dir := range []string{"staging", "published"} {
		p := filepath.Join(root, dir)
		if err := os.MkdirAll(p, 0700); err != nil {
			_ = unlockRoot(lock)
			return nil, err
		}
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			_ = unlockRoot(lock)
			return nil, errors.New("snapshot subdirectories must be private real directories")
		}
	}
	// Owning the process lock proves no live producer can still own staging.
	staging, err := os.ReadDir(filepath.Join(root, "staging"))
	if err != nil {
		_ = unlockRoot(lock)
		return nil, err
	}
	for _, item := range staging {
		if err := os.RemoveAll(filepath.Join(root, "staging", item.Name())); err != nil {
			_ = unlockRoot(lock)
			return nil, err
		}
	}
	return s, nil
}

// Close refuses to abandon active imports or read leases. Callers
// must first stop accepting work, cancel/join imports and release serving views.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	for _, e := range s.entries {
		if e.refs != 0 {
			return ErrPinned
		}
	}
	if s.active != 0 {
		return errors.New("snapshot imports are still running")
	}
	s.closed = true
	return unlockRoot(s.lock)
}

// Acquire pins exactly required, never the latest or merely a same-name view.
// On process restart persisted content is reverified before its first use.
func (s *Store) Acquire(ctx context.Context, required edgeprotocol.RepositorySnapshot) (*Lease, error) {
	key, err := edgeprotocol.SnapshotKey(required)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e := s.entries[key]
	if e == nil {
		root := filepath.Join(s.root, "published", key)
		if err := checkPublishedRoot(root); err != nil {
			return nil, err
		}
		f, err := os.Open(filepath.Join(root, "manifest.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrMissing
		}
		if err != nil {
			return nil, err
		}
		m, readErr := edgeprotocol.DecodeManifest(f)
		closeErr := f.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if m.Snapshot != required {
			return nil, errors.New("persisted snapshot identity mismatch")
		}
		if err := verifyRepo(ctx, filepath.Join(root, "view", "repo.git"), m); err != nil {
			return nil, err
		}
		e = &entry{manifest: m, root: root}
		s.entries[key] = e
	}
	e.refs++
	s.lastUse[key] = s.now()
	return &Lease{store: s, entry: e}, nil
}

// Producer supplies a complete self-contained pack for the exact manifest.
// It must return errors on interrupted transfer/export, including when a copy
// returned partial bytes. A producer cannot publish anything itself.
type Producer func(context.Context, io.Writer) error

// Install writes an isolated staging view, verifies refs/HEAD/object closure,
// fsyncs it and atomically publishes it. A failed/canceled import cannot poison
// any previously published view. Size limiting applies even to local producers.
func (s *Store) Install(ctx context.Context, m edgeprotocol.Manifest, produce Producer) error {
	if produce == nil {
		return errors.New("snapshot pack producer is required")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	m = m.Clone()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.active++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	stage, err := os.MkdirTemp(filepath.Join(s.root, "staging"), "import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := ctx.Err(); err != nil {
		return err
	}
	repo, err := initRepo(ctx, stage, m)
	if err != nil {
		return err
	}
	if err := receivePack(ctx, repo, m.Snapshot.ObjectFormat, s.maxPack, s.guardedProducer(produce)); err != nil {
		return err
	}
	if err := installRefs(ctx, repo, m); err != nil {
		return err
	}
	return s.publishStage(ctx, stage, m)
}

// publishStage is shared by full imports and exact incremental pack reuse.
// Callers own a private staging directory and an active-operation reservation.
// Never publish an alternate or a superset of the manifest's reachable objects.
func (s *Store) publishStage(ctx context.Context, stage string, m edgeprotocol.Manifest) error {
	manifest, err := edgeprotocol.EncodeManifest(m)
	if err != nil {
		return err
	}
	key, _ := edgeprotocol.SnapshotKey(m.Snapshot)
	if err := verifyRepo(ctx, filepath.Join(stage, "view", "repo.git"), m); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), manifest, 0600); err != nil {
		return err
	}
	if err := syncTree(stage); err != nil {
		return err
	}
	if err := s.checkHeadroom(); err != nil {
		return err
	}
	stageBytes, err := treeBytes(ctx, stage)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	destination := filepath.Join(s.root, "published", key)
	if _, err := os.Lstat(destination); err == nil {
		// An existing view is never overwritten, including a corrupt one. The
		// caller must inspect/quarantine corrupt disk content explicitly.
		if err := checkPublishedRoot(destination); err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(destination, "manifest.json"))
		if err != nil {
			return err
		}
		old, decodeErr := edgeprotocol.DecodeManifest(f)
		_ = f.Close()
		if decodeErr != nil || old.Snapshot != m.Snapshot {
			return errors.New("existing snapshot binding mismatch")
		}
		if err := verifyRepo(ctx, filepath.Join(destination, "view", "repo.git"), old); err != nil {
			return err
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.retention != nil {
		if _, err := s.collectLocked(ctx, stageBytes, 1, false); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Join(s.root, "published")); err != nil {
		return err
	}
	s.entries[key] = &entry{manifest: m, root: destination}
	s.lastUse[key] = s.now()
	return nil
}

// Remove is an operator/retention operation, not a client-supplied path.
func (s *Store) Remove(required edgeprotocol.RepositorySnapshot) error {
	key, err := edgeprotocol.SnapshotKey(required)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if e := s.entries[key]; e != nil && e.refs > 0 {
		return ErrPinned
	}
	if s.active != 0 {
		return errors.New("cannot collect snapshots during imports")
	}
	if err := os.RemoveAll(filepath.Join(s.root, "published", key)); err != nil {
		return err
	}
	delete(s.entries, key)
	delete(s.lastUse, key)
	return syncDirectory(filepath.Join(s.root, "published"))
}

// Lease pins one immutable published view for one in-process reader. It is not
// a bearer credential, user Session, or grant to read at a later HTTP request.
type Lease struct {
	store *Store
	entry *entry
	once  sync.Once
}

func (v *Lease) Snapshot() edgeprotocol.RepositorySnapshot { return v.entry.manifest.Snapshot }
func (v *Lease) Manifest() edgeprotocol.Manifest           { return v.entry.manifest.Clone() }
func (v *Lease) ProjectRoot() string                       { return v.entry.root }
func (v *Lease) Repository() string                        { return "view/repo" }
func (v *Lease) RepoPath() string                          { return filepath.Join(v.entry.root, "view", "repo.git") }
func (v *Lease) Release() {
	v.once.Do(func() {
		v.store.mu.Lock()
		v.entry.refs--
		key, _ := edgeprotocol.SnapshotKey(v.entry.manifest.Snapshot)
		v.store.lastUse[key] = v.store.now()
		v.store.mu.Unlock()
	})
}

// WritePack reads only this retained local view. Releasing or removing the
// original primary source does not affect it. The caller owns the lease.
func (v *Lease) WritePack(ctx context.Context, dst io.Writer) error {
	return WritePack(ctx, v.RepoPath(), v.entry.manifest, dst)
}

type limitWriter struct {
	writer    io.Writer
	remaining int64
	ctx       context.Context
	err       error
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.err = w.ctx.Err(); w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.remaining {
		w.err = errors.New("snapshot pack exceeds configured size limit")
		return 0, w.err
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	if err != nil {
		w.err = err
	}
	return n, err
}

func checkPublishedRoot(root string) error {
	for _, part := range []string{"", "manifest.json", "view", "view/repo.git"} {
		info, err := os.Lstat(filepath.Join(root, part))
		if errors.Is(err, os.ErrNotExist) {
			return ErrMissing
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink in published snapshot")
		}
		if part == "manifest.json" && !info.Mode().IsRegular() || part != "manifest.json" && !info.IsDir() {
			return errors.New("invalid published snapshot entry")
		}
	}
	return nil
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncTree(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in staging snapshot")
		}
		if d.IsDir() {
			directories = append(directories, path)
			return os.Chmod(path, 0700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("non-regular snapshot entry")
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		// Reused pack/index inodes already have private permissions. Avoid
		// even a redundant metadata change through another view's hardlink.
		if info.Mode().Perm() != 0600 {
			if err := os.Chmod(path, 0600); err != nil {
				return err
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncDirectory(directories[i]); err != nil {
			return err
		}
	}
	return nil
}
