package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

func loadGitMaintenance(cfg *Config) error {
	if raw := os.Getenv("AGS_GIT_MAINTENANCE_ENABLED"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("AGS_GIT_MAINTENANCE_ENABLED must be a boolean")
		}
		cfg.GitMaintenanceDisabled = !enabled
	}
	cfg.GitMaintenanceInterval = time.Hour
	cfg.GitMaintenanceTimeout = 2 * time.Minute
	for _, field := range []struct {
		name     string
		dst      *time.Duration
		min, max time.Duration
	}{
		{"AGS_GIT_MAINTENANCE_INTERVAL", &cfg.GitMaintenanceInterval, time.Minute, 24 * time.Hour},
		{"AGS_GIT_MAINTENANCE_TIMEOUT", &cfg.GitMaintenanceTimeout, time.Second, 5 * time.Minute},
	} {
		if raw := os.Getenv(field.name); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d < field.min || d > field.max {
				return fmt.Errorf("%s must be a duration between %s and %s", field.name, field.min, field.max)
			}
			*field.dst = d
		}
	}
	return nil
}
