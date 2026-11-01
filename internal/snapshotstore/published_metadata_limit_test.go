package snapshotstore

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestMetadataFilesDoNotConsumeCheckpointViewLimit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	s, err := Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !s.closed {
			_ = unlockRoot(s.lock)
		}
	})
	published := filepath.Join(root, "published")
	for i := 0; i < 100001; i++ {
		if err := os.WriteFile(filepath.Join(published, fmt.Sprintf("._%06d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	if err := s.Close(); err != nil {
		t.Fatalf("OS metadata consumed checkpoint view limit with zero snapshots: %v", err)
	}
}

func TestUnknownFilesStillConsumeCheckpointViewLimit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	s, err := Open(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !s.closed {
			_ = unlockRoot(s.lock)
		}
	})
	published := filepath.Join(root, "published")
	for i := 0; i < 100001; i++ {
		if err := os.WriteFile(filepath.Join(published, fmt.Sprintf("unknown-%06d.txt", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	err = s.Close()
	if err == nil || err.Error() != "snapshot retention checkpoint exceeds view limit" {
		t.Fatalf("unknown files were not rejected by the checkpoint view limit: %v", err)
	}
	entries, readErr := os.ReadDir(published)
	if readErr != nil || len(entries) != 100001 {
		t.Fatalf("unknown files were moved or deleted: %d %v", len(entries), readErr)
	}
}
