package snapshotstore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxLinkedObjectFiles = 1_000_000

// objectFiles selects only ordinary loose objects and complete pack/index pairs.
// Never follow symlinks or import alternates, hooks, config, bitmap/MIDX caches,
// promisor state or source maintenance files. The source is quiesced or pinned.
func objectFiles(ctx context.Context, repo, format string) ([]string, error) {
	root := filepath.Join(repo, "objects")
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("object directory must be a real directory")
	}
	for _, name := range []string{"info/alternates", "info/http-alternates"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("linked capture cannot borrow external objects")
		}
	}
	length := 40
	if format == "sha256" {
		length = 64
	} else if format != "sha1" {
		return nil, errors.New("unsupported linked object format")
	}
	var files []string
	packs := make(map[string]uint8)
	visited := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > maxLinkedObjectFiles {
			return errors.New("object file inventory limit exceeded")
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in source object inventory")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if rel == "info" {
				return filepath.SkipDir
			}
			if len(parts) != 1 || (rel != "pack" && !hexName(rel, 2)) {
				return errors.New("unsupported source object directory")
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return errors.New("nonregular source object file")
		}
		if len(parts) == 2 && hexName(parts[0], 2) && hexName(parts[1], length-2) {
			files = append(files, rel)
			return nil
		}
		if len(parts) != 2 || parts[0] != "pack" {
			return nil // Unfinished temporary files are never linked.
		}
		if strings.HasSuffix(parts[1], ".promisor") {
			return errors.New("promisor packs are unsupported for linked capture")
		}
		ext := filepath.Ext(parts[1])
		stem := strings.TrimSuffix(parts[1], ext)
		if !strings.HasPrefix(stem, "pack-") || !hexName(strings.TrimPrefix(stem, "pack-"), length) {
			return nil
		}
		switch ext {
		case ".pack":
			packs[stem] |= 1
		case ".idx":
			packs[stem] |= 2
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for stem, mask := range packs {
		if mask != 3 {
			return nil, errors.New("incomplete source pack/index pair")
		}
		files = append(files, filepath.Join("pack", stem+".pack"), filepath.Join("pack", stem+".idx"))
	}
	sort.Strings(files)
	return files, nil
}

func hexName(s string, length int) bool {
	return len(s) == length && strings.Trim(s, "0123456789abcdef") == ""
}

// linkObjectFile creates an independent directory entry, not a borrowed path.
// No copy fallback: cross-device capture must fail rather than copy history while
// primary writes are blocked. Neither content nor permissions are modified.
func linkObjectFile(source, destination string, allowIdentical bool) error {
	before, err := os.Lstat(source)
	if err != nil || !before.Mode().IsRegular() {
		return errors.New("invalid object link source")
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	if err := os.Link(source, destination); err != nil {
		if !allowIdentical || !errors.Is(err, os.ErrExist) {
			return errors.New("object hardlink failed; source and snapshots must share a link-capable filesystem")
		}
		existing, statErr := os.Lstat(destination)
		if statErr != nil || !existing.Mode().IsRegular() || existing.Size() != before.Size() {
			return errors.New("object link destination collision")
		}
		if os.SameFile(before, existing) {
			return nil
		}
		// Empty packs can legitimately have the same name in a base and delta.
		// Prove equality instead of trusting a filename as a content proof.
		return equalObjectFiles(source, destination)
	}
	after, err := os.Lstat(destination)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() != before.Size() {
		_ = os.Remove(destination)
		return errors.New("source object changed during link")
	}
	return nil
}

func equalObjectFiles(left, right string) error {
	a, err := os.Open(left)
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := os.Open(right)
	if err != nil {
		return err
	}
	defer b.Close()
	ab, bb := make([]byte, 32<<10), make([]byte, 32<<10)
	for {
		an, ae := io.ReadFull(a, ab)
		bn, be := io.ReadFull(b, bb)
		if an != bn || string(ab[:an]) != string(bb[:bn]) || ae != be {
			return errors.New("object link destination has different content")
		}
		if ae == io.EOF || ae == io.ErrUnexpectedEOF {
			return nil
		}
		if ae != nil {
			return ae
		}
	}
}
