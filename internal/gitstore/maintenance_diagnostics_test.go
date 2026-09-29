package gitstore

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceCancelledInventoryIsDeferredNotPersistedFailure(t *testing.T) {
	s, dir := maintenanceFixture(t)
	before := maintenanceTestGit(t, dir, nil, "show-ref")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	receipt, err := s.MaintainStorage(ctx, "owner/repo", func(inner context.Context) ([]string, error) {
		cancel()
		<-inner.Done()
		// Application adapters deliberately suppress private SQL/driver text.
		return nil, errors.New("sanitized inventory failure")
	})
	if !errors.Is(err, context.Canceled) || receipt.Status != "deferred" || receipt.Phase != "application_inventory" {
		t.Fatalf("normal cancellation became failure: %+v %v", receipt, err)
	}
	if _, err = s.ReadMaintenanceReceipt(context.Background(), "owner/repo"); !os.IsNotExist(err) {
		t.Fatalf("cancelled preflight must not persist a failed receipt: %v", err)
	}
	if after := maintenanceTestGit(t, dir, nil, "show-ref"); after != before {
		t.Fatal("cancelled inventory changed refs")
	}
}

func TestMaintenanceInventoryDiagnosticNeverLeaksDriverOrData(t *testing.T) {
	for _, test := range []struct {
		name         string
		err          error
		code, source string
	}{
		{"typed", &RootInventoryError{Code: "query_failed", Source: "pull_requests.head_sha"}, "application_query_failed", "pull_requests.head_sha"},
		{"private-driver", errors.New("private token=not-a-real-credential"), "application_inventory_failed", ""},
		{"unknown-code", &RootInventoryError{Code: "private-token", Source: "pull_requests.head_sha"}, "application_inventory_failed", ""},
		{"data-in-source", &RootInventoryError{Code: "query_failed", Source: "SELECT token='private'"}, "application_inventory_failed", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := maintenanceFixture(t)
			receipt, err := s.MaintainStorage(context.Background(), "owner/repo", func(context.Context) ([]string, error) { return nil, test.err })
			if err == nil || receipt.Status != "failed" || receipt.ErrorCode != test.code || receipt.InventorySource != test.source || receipt.Phase != "application_inventory" {
				t.Fatalf("unexpected diagnostic: %+v %v", receipt, err)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("private data in public error")
			}
			stored, err := s.ReadMaintenanceReceipt(context.Background(), "owner/repo")
			if err != nil || stored.ErrorCode != test.code || stored.InventorySource != test.source {
				t.Fatalf("stored diagnostic: %+v %v", stored, err)
			}
		})
	}
}

func TestMaintenanceOutputBudgetCannotBeBypassedByReadFrom(t *testing.T) {
	out := &boundedMaintenanceOutput{limit: 4}
	reader := io.LimitReader(strings.NewReader("unbounded-output"), 16)
	if _, err := io.Copy(out, reader); err == nil {
		t.Fatal("exec-style copy bypassed output budget")
	}
	if len(out.Bytes()) > 4 {
		t.Fatal("retained bytes exceeded budget")
	}
}

func TestMaintenanceDeadlineDuringInventoryIsDeferred(t *testing.T) {
	s, _ := maintenanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	receipt, err := s.MaintainStorage(ctx, "owner/repo", func(inner context.Context) ([]string, error) {
		<-inner.Done()
		return nil, &RootInventoryError{Code: "query_failed", Source: "pull_requests.head_sha"}
	})
	if !errors.Is(err, context.DeadlineExceeded) || receipt.Status != "deferred" {
		t.Fatalf("deadline became failure: %+v %v", receipt, err)
	}
}
