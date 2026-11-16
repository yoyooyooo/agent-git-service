package snapshotstore

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedOSMetadataPreservedAcrossRetentionAndRestart(t *testing.T) {
	for _, path := range []string{"retention", "restart"} {
		for _, name := range []string{".DS_Store", "._snapshot"} {
			t.Run(path+"/"+name, func(t *testing.T) {
				s, _, manifest, policy := restartRetentionFixture(t)
				metadata := filepath.Join(s.root, "published", name)
				content := []byte("synthetic OS metadata")
				if err := os.WriteFile(metadata, content, 0600); err != nil {
					t.Fatal(err)
				}
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				checkWarning := func() {
					t.Helper()
					var record struct {
						Level string `json:"level"`
						Entry string `json:"entry"`
					}
					if err := json.Unmarshal(logs.Bytes(), &record); err != nil || record.Level != "WARN" || record.Entry != name {
						t.Fatalf("want one warning for %s, got %q (%v)", name, logs.String(), err)
					}
					logs.Reset()
				}
				if path == "restart" {
					if err := s.Close(); err != nil {
						t.Fatalf("close with OS metadata: %v", err)
					}
					checkWarning()
					s = reopenRetention(t, s.root, policy)
				} else if err := s.ConfigureRetention(policy); err != nil {
					t.Fatalf("configure retention with OS metadata: %v", err)
				}
				checkWarning()
				stats, err := s.Prune(context.Background())
				if err != nil || stats.Views != 1 || stats.Removed != 0 {
					t.Fatalf("OS metadata changed retained views: %+v %v", stats, err)
				}
				checkWarning()
				lease, err := s.Acquire(context.Background(), manifest.Snapshot)
				if err != nil {
					t.Fatal("snapshot unavailable with OS metadata", err)
				}
				lease.Release()
				entries, err := os.ReadDir(filepath.Join(s.root, "published"))
				if err != nil || len(entries) != 2 {
					t.Fatalf("published entries changed: %v %v", entries, err)
				}
				got, err := os.ReadFile(metadata)
				if err != nil || !bytes.Equal(got, content) {
					t.Fatalf("OS metadata was changed or removed: %q %v", got, err)
				}
			})
		}
	}
}

func TestUnexpectedPublishedEntryStillRejectsRetentionAndRestart(t *testing.T) {
	for _, path := range []string{"retention", "restart"} {
		for _, setup := range []struct {
			name string
			make func(string) error
		}{
			{"unknown file", func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("unexpected"), 0600)
			}},
			{"symlink", func(dir string) error { return os.Symlink("elsewhere", filepath.Join(dir, ".DS_Store")) }},
			{"metadata directory", func(dir string) error { return os.Mkdir(filepath.Join(dir, ".DS_Store"), 0700) }},
			{"invalid directory", func(dir string) error { return os.Mkdir(filepath.Join(dir, "not-a-snapshot"), 0700) }},
		} {
			t.Run(path+"/"+setup.name, func(t *testing.T) {
				s, _, _, policy := restartRetentionFixture(t)
				// The fixture cleanup closes a healthy store. These cases intentionally
				// leave an invalid entry, so close the original owner here and reopen one
				// that the test closes only when the operation is ConfigureRetention.
				root := s.root
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := Open(root, 0)
				if err != nil {
					t.Fatal(err)
				}
				published := filepath.Join(root, "published")
				if err = setup.make(published); err != nil {
					t.Fatal(err)
				}
				if path == "restart" {
					err = reopened.Close()
				} else if err = reopened.ConfigureRetention(policy); err == nil {
					if closeErr := reopened.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
				} else if closeErr := unlockRoot(reopened.lock); closeErr != nil {
					t.Fatal(closeErr)
				}
				if err == nil || err.Error() != "unexpected published snapshot entry" {
					t.Fatalf("unexpected entry was accepted: %v", err)
				}
				entries, readErr := os.ReadDir(published)
				if readErr != nil || len(entries) != 2 {
					t.Fatalf("unexpected entry was moved or deleted: %v %v", entries, readErr)
				}
			})
		}
	}
}
