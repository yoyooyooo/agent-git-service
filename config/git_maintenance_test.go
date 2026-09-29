package config

import (
	"testing"
	"time"
)

func TestGitMaintenanceDefaultAndExplicitConfiguration(t *testing.T) {
	t.Setenv("AGS_GIT_MAINTENANCE_ENABLED", "")
	t.Setenv("AGS_GIT_MAINTENANCE_INTERVAL", "")
	t.Setenv("AGS_GIT_MAINTENANCE_TIMEOUT", "")
	var cfg Config
	if err := loadGitMaintenance(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitMaintenanceDisabled || cfg.GitMaintenanceInterval != time.Hour || cfg.GitMaintenanceTimeout != 2*time.Minute {
		t.Fatalf("defaults %+v", cfg)
	}
	t.Setenv("AGS_GIT_MAINTENANCE_ENABLED", "false")
	t.Setenv("AGS_GIT_MAINTENANCE_INTERVAL", "2h")
	t.Setenv("AGS_GIT_MAINTENANCE_TIMEOUT", "45s")
	if err := loadGitMaintenance(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.GitMaintenanceDisabled || cfg.GitMaintenanceInterval != 2*time.Hour || cfg.GitMaintenanceTimeout != 45*time.Second {
		t.Fatal("explicit policy not applied")
	}
}

func TestGitMaintenanceRejectsInvalidAndUnboundedConfiguration(t *testing.T) {
	for _, tc := range []struct{ key, value string }{{"AGS_GIT_MAINTENANCE_ENABLED", "maybe"}, {"AGS_GIT_MAINTENANCE_INTERVAL", "0s"}, {"AGS_GIT_MAINTENANCE_TIMEOUT", "24h"}} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv("AGS_GIT_MAINTENANCE_ENABLED", "")
			t.Setenv("AGS_GIT_MAINTENANCE_INTERVAL", "")
			t.Setenv("AGS_GIT_MAINTENANCE_TIMEOUT", "")
			t.Setenv(tc.key, tc.value)
			if err := loadGitMaintenance(&Config{}); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
}
