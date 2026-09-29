package gitstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMaintenanceHealthSeparatesRecentYieldFromCompletedReceipt(t *testing.T) {
	s, _ := maintenanceFixture(t)
	s.SetMaintenanceWorkerState("running")
	s.RecordMaintenanceScan()
	receipt, err := s.MaintainStorage(context.Background(), "owner/repo", func(context.Context) ([]string, error) { return nil, nil })
	if err != nil || receipt.Status != "completed" {
		t.Fatalf("initial completion: %+v %v", receipt, err)
	}
	release, err := s.BeginMaintenanceAccess(context.Background(), "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	yielded, err := s.MaintainStorage(context.Background(), "owner/repo", func(context.Context) ([]string, error) { t.Fatal("entered occupied repo"); return nil, nil })
	if !errors.Is(err, ErrMaintenanceBusy) || yielded.Status != "deferred" {
		t.Fatalf("yield: %+v %v", yielded, err)
	}
	health := s.MaintenanceHealth()
	if health.WorkerState != "running" || health.LastScanAt.IsZero() || health.Attempts != 2 || health.Completed != 1 || health.Deferred != 1 || health.Failed != 0 || health.LastResult != "deferred" || health.LastPhase != "admission" || health.ScopedOperations != 1 {
		t.Fatalf("health: %+v", health)
	}
	stored, err := s.ReadMaintenanceReceipt(context.Background(), "owner/repo")
	if err != nil || stored.Status != "completed" || !stored.FinishedAt.Equal(receipt.FinishedAt) {
		t.Fatalf("yield overwrote useful completion: %+v %v", stored, err)
	}
	output, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), "owner/repo") {
		t.Fatal("public health leaked a repository name")
	}
}

func TestMaintenanceHealthShowsActivePhaseWithoutRacing(t *testing.T) {
	s, _ := maintenanceFixture(t)
	entered, proceed := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.MaintainStorage(context.Background(), "owner/repo", func(context.Context) ([]string, error) {
			close(entered)
			<-proceed
			return nil, errors.New("private database value")
		})
		done <- err
	}()
	<-entered
	health := s.MaintenanceHealth()
	if !health.Running || health.Attempts != 1 || health.LastPhase != "application_inventory" {
		t.Fatalf("active phase unavailable: %+v", health)
	}
	close(proceed)
	if err := <-done; err == nil {
		t.Fatal("inventory failure was hidden")
	}
	health = s.MaintenanceHealth()
	if health.Running || health.Failed != 1 || health.LastResult != "failed" || health.LastPhase != "application_inventory" {
		t.Fatalf("failed phase: %+v", health)
	}
	encoded, _ := json.Marshal(health)
	if strings.Contains(string(encoded), "private database value") {
		t.Fatal("raw error leaked")
	}
}
