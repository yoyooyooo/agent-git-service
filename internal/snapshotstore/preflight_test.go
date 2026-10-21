package snapshotstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	low, err := CheckCaptureFilesystem(source, snapshots, ^uint64(0))
	var failure *HeadroomError
	if !errors.Is(err, ErrHeadroom) || !errors.As(err, &failure) {
		t.Fatalf("missing typed headroom failure: %v", err)
	}
	if low.Reason != "disk_free_below_threshold" || low.AvailableBytes == 0 || low.MinimumBytes != ^uint64(0) ||
		failure.Reason != low.Reason || failure.AvailableBytes != low.AvailableBytes || failure.MinimumBytes != low.MinimumBytes {
		t.Fatalf("incomplete low-space result: report=%+v error=%+v", low, failure)
	}
	for _, value := range []any{low, failure} {
		data, err := json.Marshal(value)
		if err != nil || !strings.Contains(string(data), `"reason":"disk_free_below_threshold"`) ||
			!strings.Contains(string(data), `"available_bytes":`+strconv.FormatUint(low.AvailableBytes, 10)) ||
			!strings.Contains(string(data), `"minimum_bytes":18446744073709551615`) {
			t.Fatalf("missing JSON diagnostics: %s err=%v", data, err)
		}
	}
	if !strings.Contains(failure.Error(), "available_bytes="+strconv.FormatUint(low.AvailableBytes, 10)) ||
		!strings.Contains(failure.Error(), "minimum_bytes=18446744073709551615") {
		t.Fatalf("missing log diagnostics: %v", failure)
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
