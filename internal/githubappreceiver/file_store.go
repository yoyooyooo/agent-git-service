package githubappreceiver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// FileStore commits private delivery envelopes before the receiver acknowledges
// them. Files survive restarts; atomic links keep concurrent saves from replacing
// the first delivery with the same provider, App ID and delivery ID.
type FileStore struct {
	directory string
}

func NewFileStore(directory string) (*FileStore, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, ErrSpoolUnavailable
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, ErrSpoolUnavailable
	}
	return &FileStore{directory: directory}, nil
}

func (s *FileStore) Save(delivery Delivery, headers map[string]string, body []byte) (Delivery, error) {
	key := sha256.Sum256([]byte(delivery.Provider + "\x00" + delivery.AppID + "\x00" + delivery.DeliveryID))
	path := filepath.Join(s.directory, hex.EncodeToString(key[:])+".json")
	if stored, err := s.existing(path, delivery); !errors.Is(err, os.ErrNotExist) {
		return stored, err
	}
	delivery.Status = statusReceived
	file, err := os.CreateTemp(s.directory, ".delivery-*")
	if err != nil {
		return Delivery{}, ErrSpoolUnavailable
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(Envelope{Delivery: delivery, Headers: headers, Body: body}); err != nil {
		file.Close()
		return Delivery{}, ErrSpoolUnavailable
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return Delivery{}, ErrSpoolUnavailable
	}
	if err := file.Close(); err != nil {
		return Delivery{}, ErrSpoolUnavailable
	}
	if err := os.Link(file.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return s.existing(path, delivery)
		}
		return Delivery{}, ErrSpoolUnavailable
	}
	if err := s.syncDirectory(); err != nil {
		return Delivery{}, ErrSpoolUnavailable
	}
	return delivery, nil
}

func (s *FileStore) existing(path string, incoming Delivery) (Delivery, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Delivery{}, err
	}
	if err != nil {
		return Delivery{}, ErrSpoolUnavailable
	}
	var envelope Envelope
	if json.Unmarshal(body, &envelope) != nil || envelope.Delivery.Provider != incoming.Provider || envelope.Delivery.AppID != incoming.AppID || envelope.Delivery.DeliveryID != incoming.DeliveryID {
		return Delivery{}, ErrSpoolUnavailable
	}
	// Also sync on retries: a preceding save may have linked the file but
	// failed its directory sync and therefore returned 503.
	if err := s.syncDirectory(); err != nil {
		return Delivery{}, ErrSpoolUnavailable
	}
	stored := envelope.Delivery
	stored.Status = statusDuplicate
	if stored.Digest != incoming.Digest {
		stored.Status = statusConflict
	}
	return stored, nil
}

func (s *FileStore) syncDirectory() error {
	directory, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
