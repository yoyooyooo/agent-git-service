package server

import (
	"testing"
	"time"
)

func TestReplicationTransferTimeoutIsSeparateAndBounded(t *testing.T) {
	f := newReplicationRuntimeFixture(t)
	f.file.RequestTimeout = "1m"
	f.file.TransferTimeout = "30m"
	f.save(t)
	loaded, err := loadReplicationConfig(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.options.RequestTimeout != time.Minute || loaded.transferTimeout != 30*time.Minute {
		t.Fatal("control and object-transfer timeouts were conflated")
	}
	for _, value := range []string{"0s", "30s", "61m", "invalid"} {
		f.file.TransferTimeout = value
		f.save(t)
		if _, err := loadReplicationConfig(f.path); err == nil {
			t.Fatalf("accepted transfer timeout %q", value)
		}
	}
}
