package gitstore

import (
	"sync"
	"time"
)

// MaintenanceHealth is bounded, credential-free runtime evidence. No repository
// name, object identity, request path, SQL or caller input is exposed here.
// A skipped/cancelled operation is not reported as successful maintenance.
type MaintenanceHealth struct {
	WorkerState      string    `json:"worker_state"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	LastScanAt       time.Time `json:"last_scan_at,omitempty"`
	LastAttemptAt    time.Time `json:"last_attempt_at,omitempty"`
	LastFinishedAt   time.Time `json:"last_finished_at,omitempty"`
	LastResult       string    `json:"last_result,omitempty"`
	LastPhase        string    `json:"last_phase,omitempty"`
	Attempts         uint64    `json:"attempts"`
	Completed        uint64    `json:"completed"`
	Deferred         uint64    `json:"deferred"`
	Failed           uint64    `json:"failed"`
	Running          bool      `json:"running"`
	GlobalOperations int       `json:"global_operations"`
	ScopedOperations int       `json:"scoped_operations"`
}

type maintenanceHealthState struct {
	mu    sync.Mutex
	value MaintenanceHealth
}

func (s *Store) SetMaintenanceWorkerState(state string) {
	s.maintenanceHealth.mu.Lock()
	defer s.maintenanceHealth.mu.Unlock()
	s.maintenanceHealth.value.WorkerState = state
	if state == "running" && s.maintenanceHealth.value.StartedAt.IsZero() {
		s.maintenanceHealth.value.StartedAt = time.Now().UTC()
	}
}
func (s *Store) RecordMaintenanceScan() {
	s.maintenanceHealth.mu.Lock()
	defer s.maintenanceHealth.mu.Unlock()
	s.maintenanceHealth.value.LastScanAt = time.Now().UTC()
}
func (s *Store) recordMaintenanceAttempt(at time.Time) {
	s.maintenanceHealth.mu.Lock()
	defer s.maintenanceHealth.mu.Unlock()
	s.maintenanceHealth.value.Attempts++
	s.maintenanceHealth.value.LastAttemptAt = at
	s.maintenanceHealth.value.LastPhase = "admission"
}
func (s *Store) setMaintenancePhase(receipt *MaintenanceReceipt, phase string) {
	receipt.Phase = phase
	s.maintenanceHealth.mu.Lock()
	defer s.maintenanceHealth.mu.Unlock()
	s.maintenanceHealth.value.LastPhase = phase
}
func (s *Store) recordMaintenanceResult(receipt MaintenanceReceipt) {
	s.maintenanceHealth.mu.Lock()
	defer s.maintenanceHealth.mu.Unlock()
	value := &s.maintenanceHealth.value
	value.LastFinishedAt = receipt.FinishedAt
	value.LastResult = receipt.Status
	value.LastPhase = receipt.Phase
	switch receipt.Status {
	case "completed", "compacted_no_prune":
		value.Completed++
	case "deferred":
		value.Deferred++
	case "failed":
		value.Failed++
	}
}
func (s *Store) MaintenanceHealth() MaintenanceHealth {
	if s == nil {
		return MaintenanceHealth{WorkerState: "unavailable"}
	}
	s.maintenanceHealth.mu.Lock()
	result := s.maintenanceHealth.value
	s.maintenanceHealth.mu.Unlock()
	if result.WorkerState == "" {
		result.WorkerState = "not_started"
	}
	s.maintenanceAccess.mu.Lock()
	defer s.maintenanceAccess.mu.Unlock()
	result.Running = s.maintenanceAccess.running != nil
	result.GlobalOperations = s.maintenanceAccess.all
	for _, count := range s.maintenanceAccess.repos {
		result.ScopedOperations += count
	}
	return result
}
