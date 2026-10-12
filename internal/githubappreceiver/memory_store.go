package githubappreceiver

import "sync"

// MemoryStore is the fixture spool. It keeps raw bodies in process memory and
// does not open a network connection.
type MemoryStore struct {
	mu        sync.Mutex
	records   map[string]storedDelivery
	failSaves int
}

type storedDelivery struct {
	delivery Delivery
	headers  map[string]string
	body     []byte
}

// NewMemoryStore returns an empty fixture spool.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: map[string]storedDelivery{}}
}

// FailNext marks the next n Save calls as an unavailable spool.
func (s *MemoryStore) FailNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failSaves += n
}

// Save stores a delivery once. A repeated GUID returns the original receipt.
func (s *MemoryStore) Save(delivery Delivery, headers map[string]string, body []byte) (Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSaves > 0 {
		s.failSaves--
		return Delivery{}, ErrSpoolUnavailable
	}
	key := delivery.Provider + "\x00" + delivery.AppID + "\x00" + delivery.DeliveryID
	if existing, ok := s.records[key]; ok {
		stored := existing.delivery
		if stored.Digest == delivery.Digest {
			stored.Status = statusDuplicate
		} else {
			stored.Status = statusConflict
		}
		return stored, nil
	}
	copied := append([]byte(nil), body...)
	copiedHeaders := map[string]string{}
	for name, value := range headers {
		copiedHeaders[name] = value
	}
	delivery.Status = statusReceived
	s.records[key] = storedDelivery{delivery: delivery, headers: copiedHeaders, body: copied}
	return delivery, nil
}

// Body returns the stored raw body for tests.
func (s *MemoryStore) Body(appID, deliveryID string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[Provider+"\x00"+appID+"\x00"+deliveryID]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), record.body...), true
}
