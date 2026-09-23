package server

import (
	"context"
	"log/slog"
	"time"
)

// Failed construction can already own workers or a retained-store lock. Stop
// those resources before returning; do not leave a half-constructed peer alive.
func cleanupFailedBootstrap(deps *bootstrapDeps) {
	if deps == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	instance := &Server{deps: deps}
	if err := instance.Shutdown(ctx); err != nil {
		slog.Error("failed bootstrap cleanup did not drain")
		return
	}
	if deps.DB != nil {
		if sqlDB, err := deps.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
}
