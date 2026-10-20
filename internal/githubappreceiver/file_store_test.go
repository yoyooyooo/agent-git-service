package githubappreceiver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFileStorePersistsAcrossRestartAndConcurrentRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	delivery := Delivery{Provider: Provider, AppID: "123", DeliveryID: "fixture-delivery", Digest: "fixture-digest"}
	body := []byte(`{"private":"payload"}`)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			stored, err := store.Save(delivery, map[string]string{"X-GitHub-Event": "ping"}, body)
			if err != nil || (stored.Status != statusReceived && stored.Status != statusDuplicate) {
				t.Error("concurrent save failed")
			}
		})
	}
	wg.Wait()
	restarted, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := restarted.Save(delivery, nil, body); err != nil || stored.Status != statusDuplicate {
		t.Fatal("restart lost idempotency")
	}
	delivery.Digest = "different-digest"
	if stored, err := restarted.Save(delivery, nil, []byte("changed")); err != nil || stored.Status != statusConflict || stored.Digest != "fixture-digest" {
		t.Fatal("conflict replaced original receipt")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 {
		t.Fatal("expected exactly one committed file")
	}
	file := filepath.Join(path, entries[0].Name())
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("delivery file is not private")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if json.Unmarshal(data, &envelope) != nil || !bytes.Equal(envelope.Body, body) || envelope.Headers["X-GitHub-Event"] != "ping" {
		t.Fatal("original envelope changed")
	}
}

func TestFileStoreFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "blocked")
	if err := os.WriteFile(path, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(path); err != ErrSpoolUnavailable {
		t.Fatal("non-directory spool accepted")
	}
	if _, err := NewFileStore(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(root); err != ErrSpoolUnavailable {
		t.Fatal("non-private spool accepted")
	}
	path = filepath.Join(t.TempDir(), "spool")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	delivery := Delivery{Provider: Provider, AppID: "123", DeliveryID: "fixture", Digest: "digest"}
	if _, err := store.Save(delivery, nil, []byte("body")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 {
		t.Fatal("missing receipt")
	}
	if err := os.WriteFile(filepath.Join(path, entries[0].Name()), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(delivery, nil, []byte("body")); err != ErrSpoolUnavailable {
		t.Fatal("corrupt receipt acknowledged")
	}
	if err := os.Rename(path, path+".unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(delivery, nil, []byte("body")); err != ErrSpoolUnavailable {
		t.Fatal("unavailable spool acknowledged")
	}
}
