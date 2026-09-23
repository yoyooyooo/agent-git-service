package snapshotstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func receivePack(ctx context.Context, repo, format string, limit int64, produce Producer) error {
	path := filepath.Join(repo, "objects", "pack", "incoming.pack")
	pack, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	limited := &limitWriter{writer: pack, remaining: limit, ctx: ctx}
	produceErr := produce(ctx, limited)
	syncErr, closeErr := pack.Sync(), pack.Close()
	if produceErr != nil {
		return produceErr
	}
	if limited.err != nil {
		return limited.err
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A complete file is indexed, not an EOF-delimited stdin pack which could
	// hide trailing protocol bytes. Native Git verifies checksums and links.
	hash, err := runGit(ctx, repo, nil, "index-pack", "--strict", "--index-version=2", path)
	if err != nil {
		return err
	}
	packHash := strings.TrimSpace(string(hash))
	length := 40
	if format == "sha256" {
		length = 64
	}
	if len(packHash) != length || strings.Trim(packHash, "0123456789abcdef") != "" {
		return errors.New("invalid Git pack index result")
	}
	final := filepath.Join(repo, "objects", "pack", "pack-"+packHash)
	if err := os.Rename(path, final+".pack"); err != nil {
		return err
	}
	return os.Rename(strings.TrimSuffix(path, ".pack")+".idx", final+".idx")
}

func objectSet(ctx context.Context, repo string) ([]string, error) {
	data, err := runGit(ctx, repo, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	if err != nil {
		return nil, err
	}
	result := strings.Fields(string(data))
	sort.Strings(result)
	return result, nil
}
func missingObjects(target, base []string) []string {
	known := make(map[string]struct{}, len(base))
	for _, oid := range base {
		known[oid] = struct{}{}
	}
	var missing []string
	for _, oid := range target {
		if _, ok := known[oid]; !ok {
			missing = append(missing, oid)
		}
	}
	return missing
}

// WriteIncrementalPack exports an exact set difference between two pinned,
// verified views. It works for force updates/deletions and unrelated histories;
// it does NOT assume the base commit is reachable from the target repository.
// --revs and --thin are intentionally absent: only enumerated missing objects
// enter this pack and no compression base outside this pack is required.
func (v *Lease) WriteIncrementalPack(ctx context.Context, base *Lease, dst io.Writer) error {
	if base == nil || !edgeprotocol.CompatibleBase(v.Snapshot(), base.Snapshot()) {
		return errors.New("incompatible incremental base")
	}
	targetIDs, err := objectSet(ctx, v.RepoPath())
	if err != nil {
		return err
	}
	baseIDs, err := objectSet(ctx, base.RepoPath())
	if err != nil {
		return err
	}
	return writeObjectPack(ctx, v.RepoPath(), missingObjects(targetIDs, baseIDs), dst)
}

func writeObjectPack(ctx context.Context, repo string, ids []string, dst io.Writer) error {
	input := strings.Join(ids, "\n")
	if input != "" {
		input += "\n"
	}
	cmd := gitCommand(ctx, repo, "pack-objects", "--stdout", "--no-reuse-delta", "--delta-base-offset", "--threads=1", "--window-memory=32m", "-q")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(input), dst, io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("incremental pack export failed")
	}
	return nil
}

// InstallIncremental borrows a pinned local base ONLY inside private staging.
// If its entire object set remains reachable, link its immutable pack files;
// otherwise repack exact target reachability. No alternate path survives
// publication, and deleting another view cannot break this one. Incoming sets
// must equal target-minus-base, including on deletion/force-update; extra hidden
// objects must not disappear silently during the final repack.
func (s *Store) InstallIncremental(ctx context.Context, m edgeprotocol.Manifest, requiredBase edgeprotocol.RepositorySnapshot, produce Producer) error {
	if produce == nil || m.Validate() != nil || !edgeprotocol.CompatibleBase(m.Snapshot, requiredBase) {
		return errors.New("invalid incremental installation")
	}
	base, err := s.Acquire(ctx, requiredBase)
	if err != nil {
		return err
	}
	defer base.Release()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.active++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	stage, err := os.MkdirTemp(filepath.Join(s.root, "staging"), "delta-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	repo, err := initRepo(ctx, stage, m)
	if err != nil {
		return err
	}
	baseIDs, err := objectSet(ctx, base.RepoPath())
	if err != nil {
		return err
	}
	// A relative path to a trusted lease, not a wire/caller-supplied path.
	relative, err := filepath.Rel(filepath.Join(repo, "objects"), filepath.Join(base.RepoPath(), "objects"))
	if err != nil || strings.ContainsAny(relative, "\r\n\x00") {
		return errors.New("invalid local base path")
	}
	alternate := filepath.Join(repo, "objects", "info", "alternates")
	if err := os.WriteFile(alternate, []byte(relative+"\n"), 0600); err != nil {
		return err
	}
	if err := receivePack(ctx, repo, m.Snapshot.ObjectFormat, s.maxPack, s.guardedProducer(produce)); err != nil {
		return err
	}
	// Enumerate the actual received set WITHOUT including borrowed objects.
	if err := os.Remove(alternate); err != nil {
		return err
	}
	incoming, err := objectSet(ctx, repo)
	if err != nil {
		return err
	}
	if err := os.WriteFile(alternate, []byte(relative+"\n"), 0600); err != nil {
		return err
	}
	if err := installRefs(ctx, repo, m); err != nil {
		return err
	}
	roots := strings.Join(m.Roots(), "\n")
	if roots != "" {
		roots += "\n"
	}
	data, err := runGit(ctx, repo, strings.NewReader(roots), "rev-list", "--objects", "--no-object-names", "--stdin")
	if err != nil {
		return err
	}
	reachable := strings.Fields(string(data))
	sort.Strings(reachable)
	if strings.Join(incoming, "\n") != strings.Join(missingObjects(reachable, baseIDs), "\n") {
		return errors.New("incremental pack differs from exact missing object set")
	}
	if len(missingObjects(baseIDs, reachable)) == 0 {
		// The complete old object set remains allowed by the new manifest.
		// Link pack/index files instead of repacking history. These are real
		// directory entries: base eviction unlinks only its own entries.
		if err := os.Remove(alternate); err != nil {
			return err
		}
		files, err := objectFiles(ctx, base.RepoPath(), m.Snapshot.ObjectFormat)
		if err != nil {
			return err
		}
		for _, name := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := linkObjectFile(filepath.Join(base.RepoPath(), "objects", name), filepath.Join(repo, "objects", name), true); err != nil {
				return err
			}
		}
		// Preserve the historical total pack-size bound, not merely the size
		// of this small network delta. Index/metadata bytes use retention limits.
		if err := checkPackBudget(ctx, repo, s.maxPack); err != nil {
			return err
		}
		return s.publishStage(ctx, stage, m)
	}
	// Deletion/force-update may exclude old objects. Never share the superset
	// and hope upload-pack hides it; rebuild and verify the exact closure.
	return s.Install(ctx, m, func(ctx context.Context, dst io.Writer) error { return WritePack(ctx, repo, m, dst) })
}

func checkPackBudget(ctx context.Context, repo string, limit int64) error {
	files, err := os.ReadDir(filepath.Join(repo, "objects", "pack"))
	if err != nil {
		return err
	}
	var total int64
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(file.Name(), ".pack") {
			continue
		}
		info, err := file.Info()
		if err != nil {
			return err
		}
		if info.Size() < 0 || info.Size() > limit-total {
			return errors.New("snapshot packs exceed configured size limit")
		}
		total += info.Size()
	}
	return nil
}
