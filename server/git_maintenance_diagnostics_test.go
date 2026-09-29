package server

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ngaut/agent-git-service/internal/gitstore"
)

func TestMaintenanceYieldRetriesSoonThenReturnsToNormalCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		start := time.Now()
		var calls []time.Duration
		go func() {
			defer close(done)
			runMaintenanceLoop(ctx, 10*time.Second, time.Hour, func(context.Context) bool {
				calls = append(calls, time.Since(start))
				if len(calls) == 3 {
					cancel()
				}
				return len(calls) == 1
			})
		}()
		<-done
		want := []time.Duration{10 * time.Second, 70 * time.Second, time.Hour + 70*time.Second}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("cadence %v want %v", calls, want)
		}
	})
}

func TestMaintenanceUpgradeRechecksOldUnclassifiedFailure(t *testing.T) {
	now := time.Now().UTC()
	stats := gitstore.StorageStats{LooseObjects: 4}
	old := gitstore.MaintenanceReceipt{Status: "failed", FinishedAt: now.Add(-time.Minute)}
	if !storageMaintenanceDue(stats, old, now) {
		t.Fatal("old unclassified failure requires manual receipt removal")
	}
	old.Phase = "application_inventory"
	if storageMaintenanceDue(stats, old, now) {
		t.Fatal("classified failure should retain backoff")
	}
	old.FinishedAt = now.Add(-2 * time.Hour)
	if !storageMaintenanceDue(stats, old, now) {
		t.Fatal("classified failure did not retry after backoff")
	}
}
