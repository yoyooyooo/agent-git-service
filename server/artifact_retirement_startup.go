package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ngaut/agent-git-service/internal/artifactretirement"
)

// Both standalone and embedded startup check the persistent interruption gate
// before they start any periodic application workers or expose an HTTP handler.
func runStartupArtifactRetirement(deps *bootstrapDeps) error {
	if deps == nil {
		return nil
	}
	if err := artifactretirement.CheckStartupGate(deps.Cfg.GitRepoDir, deps.Cfg.ArtifactRetirementIntentFile); err != nil {
		return fmt.Errorf("startup artifact retirement: %w", err)
	}
	if deps.Cfg.ArtifactRetirementIntentFile == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(deps.SrvCtx, 20*time.Minute)
	defer cancel()
	receipt, err := deps.SvcDeps.RunConfiguredArtifactRetirement(ctx, deps.Cfg.ArtifactRetirementIntentFile)
	if err != nil {
		return fmt.Errorf("startup artifact retirement: %w", err)
	}
	if receipt.Status == "completed" {
		slog.Info("artifact retirement startup migration completed", "operation_id", receipt.OperationID,
			"repository", receipt.Repository, "changed_refs", receipt.ChangedRefs, "changed_commits", receipt.ChangedCommits,
			"before_kib", receipt.BeforeDiskKiB, "after_kib", receipt.AfterDiskKiB)
	}
	return nil
}
