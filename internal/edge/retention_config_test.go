package edge_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/ngaut/agent-git-service/internal/edge"
)

func TestReadConfigurationRetentionCannotUndercutNegotiation(t *testing.T) {
	f := newControlFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*edge.ReadFileConfig)
		valid  bool
	}{
		{"explicit bounded policy", func(c *edge.ReadFileConfig) {
			c.MaxCacheBytes = 8 << 20
			c.MaxCacheViews = 8
			c.ProtectRecent = "10m"
			c.ExpireIdle = "1h"
			c.MaintenanceInterval = "1s"
		}, true},
		{"protection shorter than negotiation", func(c *edge.ReadFileConfig) { c.ProtectRecent = "1s" }, false},
		{"negative bytes", func(c *edge.ReadFileConfig) { c.MaxCacheBytes = -1 }, false},
		{"negative views", func(c *edge.ReadFileConfig) { c.MaxCacheViews = -1 }, false},
		{"expires before protection", func(c *edge.ReadFileConfig) { c.ExpireIdle = "1m" }, false},
		{"disabled collector", func(c *edge.ReadFileConfig) { c.MaintenanceInterval = "0s" }, false},
		{"unbounded collector", func(c *edge.ReadFileConfig) { c.MaintenanceInterval = "24h" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeReadConfig(t, f)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var cfg edge.ReadFileConfig
			if err := json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			tc.change(&cfg)
			data, err = json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			resources, err := edge.OpenReadResources(context.Background(), "edge-fixture-test", path)
			if resources != nil {
				if closeErr := resources.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
			}
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
