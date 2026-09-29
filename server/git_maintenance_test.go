package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/gitstore"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMaintenanceHealthReadinessIsInformational(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	handler := readyzHandler(readyzConfig{
		MainDB: database,
		GitMaintenanceHealth: func() gitstore.MaintenanceHealth {
			return gitstore.MaintenanceHealth{WorkerState: "running", Attempts: 1, Deferred: 1, LastResult: "deferred", LastPhase: "application_inventory"}
		},
	})
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("maintenance yield made primary unavailable: %d", response.Code)
	}
	var body struct {
		Status      string                     `json:"status"`
		Maintenance gitstore.MaintenanceHealth `json:"git_maintenance"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ready" || body.Maintenance.LastResult != "deferred" || body.Maintenance.LastPhase != "application_inventory" || body.Maintenance.Completed != 0 {
		t.Fatalf("missing or misleading maintenance evidence: %+v", body)
	}
}

func TestMaintenanceLoopStartsAutomaticallyAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	ran := make(chan struct{}, 2)
	var count atomic.Int32
	go func() {
		defer close(done)
		runMaintenanceLoop(ctx, time.Millisecond, time.Millisecond, func(context.Context) bool {
			count.Add(1)
			select {
			case ran <- struct{}{}:
			default:
			}
			return false
		})
	}()
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("automatic startup scan missing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maintenance worker outlived shutdown")
	}
	after := count.Load()
	time.Sleep(5 * time.Millisecond)
	if count.Load() != after {
		t.Fatal("work after shutdown")
	}
}

func TestMaintenanceAdmissionDoesNotDeadlockSnapshotCapture(t *testing.T) {
	s, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Init(context.Background(), "owner/repo", "main", true); err != nil {
		t.Fatal(err)
	}
	h := repositoryMaintenanceAdmission(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.WithSnapshotCapture(r.Context(), func(context.Context) error { return nil }); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/ext/v1/replication/register", nil).WithContext(ctx))
	if w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
}

func TestHealthPollDoesNotTakeMaintenanceAdmission(t *testing.T) {
	s, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithMaintenance(context.Background(), func(ctx context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-entered
	h := repositoryMaintenanceAdmission(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal("health cancelled maintenance", err)
	}
}
