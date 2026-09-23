package snapshotstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureFilesystemPreflightAndProbeCleanup(t *testing.T) {
	root := t.TempDir()
	source, snapshots := filepath.Join(root, "git"), filepath.Join(root, "snapshots")
	for _, dir := range []string{source, snapshots} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	report, err := CheckCaptureFilesystem(source, snapshots, 1)
	if err != nil || !report.HardlinkVerified || report.AvailableBytes == 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := CheckCaptureFilesystem(source, snapshots, ^uint64(0)); err == nil {
		t.Fatal("impossible headroom accepted")
	}
	if _, err := CheckCaptureFilesystem(source, snapshots, 0); err == nil {
		t.Fatal("missing capacity requirement accepted")
	}
	for _, dir := range []string{source, snapshots} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("preflight leaked files in %s: %v", dir, err)
		}
	}
	if _, err := CheckCaptureFilesystem(source, source, 1); err == nil {
		t.Fatal("same root accepted")
	}
	if _, err := CheckCaptureFilesystem(root, source, 1); err == nil {
		t.Fatal("overlapping roots accepted")
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckCaptureFilesystem(alias, source, 1); err == nil {
		t.Fatal("symlink alias escaped root overlap check")
	}
}
