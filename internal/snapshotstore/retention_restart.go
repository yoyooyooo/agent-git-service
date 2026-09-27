package snapshotstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// A quiescent close records retention age, not object validity or permission.
// Open consumes this one checkpoint before admitting work. A crash afterwards
// cannot reuse it to prune a view that was recently used by the crashed process.
// Keep it under staging: older binaries always discard staging on Open, so a
// downgrade cannot leave stale clean-close evidence for a future upgrade.
const retentionCheckpoint = "retention-close.json"
const retentionCheckpointLimit = 16 << 20

type retentionClose struct {
	Version  int                  `json:"version"`
	ClosedAt time.Time            `json:"closed_at"`
	LastUse  map[string]time.Time `json:"last_use"`
}

func (s *Store) restoreRetentionAge() error {
	path := filepath.Join(s.root, "staging", retentionCheckpoint)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > retentionCheckpointLimit {
		return errors.New("unsafe snapshot retention checkpoint")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Consume even a malformed/stale checkpoint. A missing or untrusted age
	// checkpoint falls back to the existing conservative restart grace.
	if err = os.Remove(path); err != nil {
		return err
	}
	if err = syncDirectory(filepath.Join(s.root, "staging")); err != nil {
		return err
	}
	var saved retentionClose
	if json.Unmarshal(data, &saved) != nil || saved.Version != 1 || saved.ClosedAt.IsZero() || saved.ClosedAt.After(s.openedAt) || len(saved.LastUse) > 100000 {
		return nil
	}
	for key, used := range saved.LastUse {
		if !snapshotKeyString(key) || used.IsZero() || used.After(saved.ClosedAt) {
			return nil
		}
	}
	s.restoredUse = saved.LastUse
	return nil
}

func snapshotKeyString(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, c := range key {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Called only with Store.mu held and after all leases/imports have drained.
// No I/O is added to the hot read path; only a bounded metadata record is saved.
func (s *Store) saveRetentionAge() error {
	items, err := os.ReadDir(filepath.Join(s.root, "published"))
	if err != nil {
		return err
	}
	if len(items) > 100000 {
		return errors.New("snapshot retention checkpoint exceeds view limit")
	}
	saved := retentionClose{Version: 1, ClosedAt: s.now(), LastUse: make(map[string]time.Time, len(items))}
	for _, item := range items {
		if !snapshotKeyString(item.Name()) || !item.IsDir() {
			return errors.New("unexpected published snapshot entry")
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		saved.LastUse[item.Name()] = s.retentionUsedAt(item.Name(), info.ModTime())
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	if len(data) > retentionCheckpointLimit {
		return errors.New("snapshot retention checkpoint exceeds byte limit")
	}
	file, err := os.CreateTemp(filepath.Join(s.root, "staging"), ".retention-close-")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporary, filepath.Join(s.root, "staging", retentionCheckpoint)); err != nil {
		return err
	}
	return syncDirectory(filepath.Join(s.root, "staging"))
}

func (s *Store) retentionUsedAt(key string, published time.Time) time.Time {
	// Unknown/unclean histories still receive full restart protection. Known
	// histories retain their age, including leases released just before shutdown.
	floor, known := s.restoredUse[key]
	if !known {
		floor = s.openedAt
	}
	used := published
	if floor.After(used) {
		used = floor
	}
	if last := s.lastUse[key]; last.After(used) {
		used = last
	}
	return used
}
