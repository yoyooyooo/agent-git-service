package snapshotstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

// Capture is a PRIVATE object pin, never an exportable SnapshotView. Its object
// files may include non-exported source objects. Only WritePack exposes content,
// restricted to the captured manifest roots; Install verifies exact closure.
// No refs, hooks, credentials or local configuration are copied from the source.
// Concurrent source unlink/repack/delete is safe after PinSource returns. In-place
// modifications of object files and unmanaged writes DURING pinning are unsupported.
type Capture struct {
	store    *Store
	stage    string
	repo     string
	manifest edgeprotocol.Manifest
	mu       sync.RWMutex
	closed   bool
	pinned   bool
}

// NewCapture reserves its lifetime and private staging OUTSIDE the primary
// barrier. In particular, waiting for Store.mu must not block primary writes
// behind a different snapshot's verification or retention scan.
func (s *Store) NewCapture(ctx context.Context) (*Capture, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.active++
	s.mu.Unlock()
	var stage string
	var err error
	ok := false
	defer func() {
		if !ok {
			if stage != "" {
				_ = os.RemoveAll(stage)
			}
			s.mu.Lock()
			s.active--
			s.mu.Unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stage, err = os.MkdirTemp(filepath.Join(s.root, "staging"), "capture-")
	if err != nil {
		return nil, err
	}
	ok = true
	return &Capture{store: s, stage: stage}, nil
}

// PinSource runs under the owning primary's capture/GC barrier with m observed
// under THAT SAME barrier. It links immutable files instead of reading/packing
// their bytes. Source and snapshot root must share a link-capable filesystem;
// failure never falls back to a slow copy. This method never takes Store.mu.
// Release the primary barrier BEFORE packing, verification and publication.
func (c *Capture) PinSource(ctx context.Context, source string, m edgeprotocol.Manifest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.repo != "" {
		return errors.New("capture is closed or already attempted")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("capture requires a real source repository")
	}
	repo, err := initRepo(ctx, c.stage, m)
	if err != nil {
		return err
	}
	c.repo = repo
	files, err := objectFiles(ctx, source, m.Snapshot.ObjectFormat)
	if err != nil {
		return err
	}
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := linkObjectFile(filepath.Join(source, "objects", name), filepath.Join(repo, "objects", name), false); err != nil {
			return err
		}
	}
	c.manifest, c.pinned = m.Clone(), true
	return nil
}

func (c *Capture) Manifest() edgeprotocol.Manifest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.manifest.Clone()
}

func (c *Capture) WritePack(ctx context.Context, dst io.Writer) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed || !c.pinned {
		return errors.New("capture is not pinned")
	}
	return WritePack(ctx, c.repo, c.manifest, dst)
}

// Retain verifies and publishes the pinned roots OUTSIDE the primary barrier.
// A compatible retained base saves packing and disk space on ordinary growth.
// Enumerate reachability from manifest roots, never all private captured objects.
func (c *Capture) Retain(ctx context.Context) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed || !c.pinned {
		return errors.New("capture is not pinned")
	}
	base, err := c.store.AcquireBase(ctx, c.manifest.Snapshot)
	if errors.Is(err, ErrMissing) {
		return c.store.Install(ctx, c.manifest, func(ctx context.Context, dst io.Writer) error {
			return WritePack(ctx, c.repo, c.manifest, dst)
		})
	}
	if err != nil {
		return err
	}
	defer base.Release()
	roots := strings.Join(c.manifest.Roots(), "\n")
	if roots != "" {
		roots += "\n"
	}
	data, err := runGit(ctx, c.repo, strings.NewReader(roots), "rev-list", "--objects", "--no-object-names", "--stdin")
	if err != nil {
		return err
	}
	targetIDs := strings.Fields(string(data))
	sort.Strings(targetIDs)
	baseIDs, err := objectSet(ctx, base.RepoPath())
	if err != nil {
		return err
	}
	missing := missingObjects(targetIDs, baseIDs)
	return c.store.InstallIncremental(ctx, c.manifest, base.Snapshot(), func(ctx context.Context, dst io.Writer) error {
		return writeObjectPack(ctx, c.repo, missing, dst)
	})
}

// Close joins readers, unlinks private pins, and releases store ownership. The
// capture is not durable work: process restart discards it with other staging.
func (c *Capture) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if err := os.RemoveAll(c.stage); err != nil {
		return err
	}
	c.closed = true
	c.store.mu.Lock()
	c.store.active--
	c.store.mu.Unlock()
	return nil
}
