package snapshotstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// FilesystemPreflight is a point-in-time observation, not a quota, reservation,
// or proof that external writers/GC participate in the primary capture barrier.
type FilesystemPreflight struct {
	HardlinkVerified bool   `json:"hardlink_verified"`
	AvailableBytes   uint64 `json:"available_bytes"`
	MinimumBytes     uint64 `json:"minimum_bytes"`
}

// CheckCaptureFilesystem checks disjoint existing roots, actual hardlink support
// and available blocks. Probe files are empty/private and always removed. No
// existing repository data, permissions or configuration are changed.
func CheckCaptureFilesystem(sourceRoot, snapshotRoot string, minimum uint64) (FilesystemPreflight, error) {
	var report FilesystemPreflight
	report.MinimumBytes = minimum
	if minimum == 0 {
		return report, errors.New("explicit positive filesystem headroom is required")
	}
	resolve := func(root string) (string, error) {
		root, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", errors.New("capture root is unavailable")
		}
		root, err = filepath.Abs(root)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return "", errors.New("capture root is not a directory")
		}
		return root, nil
	}
	source, err := resolve(sourceRoot)
	if err != nil {
		return report, err
	}
	target, err := resolve(snapshotRoot)
	if err != nil {
		return report, err
	}
	contains := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	if contains(source, target) || contains(target, source) {
		return report, errors.New("Git and snapshot roots must not overlap")
	}
	probe, err := os.CreateTemp(source, ".ags-capture-probe-")
	if err != nil {
		return report, errors.New("cannot create capture link probe")
	}
	defer os.Remove(probe.Name())
	if err := probe.Close(); err != nil {
		return report, errors.New("cannot close capture link probe")
	}
	// A private temporary directory makes the destination unguessable and
	// prevents collisions with data belonging to another preflight invocation.
	dir, err := os.MkdirTemp(target, ".ags-preflight-")
	if err != nil {
		return report, errors.New("cannot create snapshot link probe")
	}
	defer os.RemoveAll(dir)
	link := filepath.Join(dir, "probe")
	if err := os.Link(probe.Name(), link); err != nil {
		return report, errors.New("Git and snapshots require one hardlink-capable filesystem")
	}
	a, err := os.Stat(probe.Name())
	if err != nil {
		return report, errors.New("capture probe disappeared")
	}
	b, err := os.Stat(link)
	if err != nil || !os.SameFile(a, b) {
		return report, errors.New("capture hardlink verification failed")
	}
	report.HardlinkVerified = true
	report.AvailableBytes, err = availableBytes(target)
	if err != nil {
		return report, errors.New("filesystem free-space inspection failed")
	}
	if report.AvailableBytes < minimum {
		return report, errors.New("insufficient filesystem headroom for replication")
	}
	return report, nil
}
