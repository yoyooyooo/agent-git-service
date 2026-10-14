package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestReadyzAuthorityRespectsCallerDeadline(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var deadlineRemaining time.Duration
	handler := readyzHandler(readyzConfig{MainDB: db, ProjectionAlertingRequired: true, ProjectionAlertingConfigured: true, AuthorityPolicyCheck: func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("readiness probe has no deadline")
		}
		deadlineRemaining = time.Until(deadline)
		<-ctx.Done()
		return ctx.Err()
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil).WithContext(ctx))
	if response.Code != http.StatusServiceUnavailable || deadlineRemaining > 25*time.Millisecond {
		t.Fatal("canceled authority probe must fail readiness within caller deadline")
	}
}

func TestReadyzCreatesProbeDeadline(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	handler := readyzHandler(readyzConfig{MainDB: db, ProjectionAlertingRequired: true, ProjectionAlertingConfigured: true, AuthorityPolicyCheck: func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 4*time.Second {
			t.Error("readiness budget missing")
		}
		return nil
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil))
	if response.Code != 200 {
		t.Fatal("healthy authority must preserve readiness")
	}
}
