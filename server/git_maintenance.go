package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/gitstore"
)

// Maintenance belongs to the primary lifecycle, not cron or an admin console.
// One serial, paginated scan also visits repositories that receive no pushes.
func startGitMaintenanceWorker(deps *bootstrapDeps) {
	if deps == nil || deps.Store == nil {
		return
	}
	if deps.Cfg.GitMaintenanceDisabled {
		deps.Store.SetMaintenanceWorkerState("disabled")
		return
	}
	if deps.Options.authenticator != nil {
		deps.Store.SetMaintenanceWorkerState("embedded_auth")
		return
	}
	if deps.SvcDeps == nil || deps.DB == nil || deps.SrvCtx == nil {
		deps.Store.SetMaintenanceWorkerState("dependencies_missing")
		return
	}
	deps.Store.SetMaintenanceWorkerState("running")
	interval := deps.Cfg.GitMaintenanceInterval
	if interval <= 0 {
		interval = time.Hour
	}
	timeout := deps.Cfg.GitMaintenanceTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	svc := deps.SvcDeps
	svc.Wg.Add(1)
	go func() {
		defer svc.Wg.Done()
		defer deps.Store.SetMaintenanceWorkerState("stopped")
		slog.InfoContext(deps.SrvCtx, "git maintenance worker started", "interval", interval.String(), "timeout", timeout.String())
		var cursor uint
		var retryBatch []db.Repository
		retries := 0
		runMaintenanceLoop(deps.SrvCtx, 30*time.Second, interval, func(ctx context.Context) bool {
			deps.Store.RecordMaintenanceScan()
			var repos []db.Repository
			retrying := len(retryBatch) > 0
			if retrying {
				repos, retryBatch = retryBatch, nil
				retries++
			} else {
				retries = 0
				if err := deps.DB.WithContext(ctx).Select("id", "full_name", "default_branch", "delete_branch_on_merge").Where("id > ?", cursor).Order("id").Limit(16).Find(&repos).Error; err != nil {
					slog.WarnContext(ctx, "git maintenance inventory failed")
					return false
				}
				if len(repos) == 0 {
					cursor = 0
					return false
				}
			}
			for _, repo := range repos {
				if ctx.Err() != nil {
					return false
				}
				if !retrying {
					cursor = repo.ID
				}
				runCtx, cancel := context.WithTimeout(ctx, timeout)
				stats, err := deps.Store.InspectStorage(runCtx, repo.FullName)
				if err != nil {
					cancel()
					slog.WarnContext(ctx, "git maintenance inspection failed", "repository_id", repo.ID)
					continue
				}
				previous, receiptErr := deps.Store.ReadMaintenanceReceipt(runCtx, repo.FullName)
				if receiptErr != nil && !os.IsNotExist(receiptErr) {
					cancel()
					slog.WarnContext(ctx, "git maintenance receipt invalid", "repository_id", repo.ID)
					continue
				}
				due := storageMaintenanceDue(stats, previous, time.Now().UTC())
				if !due {
					cancel()
					continue
				}
				receipt, err := deps.Store.MaintainStorage(runCtx, repo.FullName, func(rootCtx context.Context) ([]string, error) { return svc.GitMaintenanceRoots(rootCtx, repo) })
				cancel()
				switch {
				case err == nil:
					slog.InfoContext(ctx, "git repository maintenance finished", "repository_id", repo.ID, "status", receipt.Status, "before_kib", receipt.Before.LooseKiB+receipt.Before.PackKiB, "after_kib", receipt.After.LooseKiB+receipt.After.PackKiB, "protected_objects", receipt.ProtectedObjects)
					if receipt.MissingApplicationObjects > 0 && (previous.ErrorCode != receipt.ErrorCode || previous.MissingApplicationObjects != receipt.MissingApplicationObjects) {
						slog.WarnContext(ctx, "git maintenance preserved all objects because application references are missing", "repository_id", repo.ID, "missing_objects", receipt.MissingApplicationObjects, "pruning_enabled", false)
					}
				case errors.Is(err, gitstore.ErrMaintenanceBusy), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
					activity := deps.Store.MaintenanceHealth()
					slog.InfoContext(ctx, "git repository maintenance deferred", "repository_id", repo.ID, "phase", receipt.Phase, "global_operations", activity.GlobalOperations, "scoped_operations", activity.ScopedOperations)
					// Two near-term retries are enough to catch a quiet window without
					// indefinitely starving the next bounded inventory page.
					if retries < 2 && ctx.Err() == nil {
						retryBatch = append(retryBatch, repo)
					}
				default:
					slog.WarnContext(ctx, "git repository maintenance failed", "repository_id", repo.ID, "code", receipt.ErrorCode, "phase", receipt.Phase, "inventory_source", receipt.InventorySource)
				}
			}
			return len(retryBatch) > 0
		})
	}()
}

func storageMaintenanceDue(stats gitstore.StorageStats, previous gitstore.MaintenanceReceipt, now time.Time) bool {
	if stats.LooseObjects == 0 && stats.PackedObjects == 0 {
		return false
	}
	if previous.Status == "" {
		return true
	}
	age := now.Sub(previous.FinishedAt)
	if age < 0 {
		return false
	}
	if previous.Status == "failed" {
		// Older receipts masked cancelled preflights as failures. The first scan
		// after this upgrade must re-evaluate them without manual file removal.
		return previous.Phase == "" || age >= time.Hour
	}
	if previous.Status == "completed" || previous.Status == "compacted_no_prune" {
		if age >= 7*24*time.Hour {
			return true
		}
		if age < 24*time.Hour && stats.LooseObjects < 8192 && stats.Packs < 32 {
			return false
		}
	}
	return stats.LooseObjects >= 1024 || stats.Packs >= 8
}

func runMaintenanceLoop(ctx context.Context, initial, interval time.Duration, run func(context.Context) bool) {
	timer := time.NewTimer(initial)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if ctx.Err() != nil {
				return
			}
			delay := interval
			if run(ctx) {
				delay = min(interval, time.Minute)
			}
			timer.Reset(delay)
		}
	}
}

// maintenanceRequestRepositories narrows only known read-only single-repo
// routes. Writes and arbitrary GraphQL/extension operations may touch fork or
// integration repositories, so unknown scope remains conservatively global.
func maintenanceRequestRepositories(r *http.Request) []string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return nil
	}
	path := r.URL.Path
	if strings.Contains(path, "%") || strings.Contains(path, "\\") {
		return nil
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 5 && parts[0] == "api" && parts[1] == "v3" && parts[2] == "repos" {
		if parts[3] == "" || parts[4] == "" || parts[3] == "." || parts[4] == "." || parts[3] == ".." || parts[4] == ".." {
			return nil
		}
		return []string{parts[3] + "/" + parts[4], parts[3] + "/" + parts[4] + ".wiki"}
	}
	if len(parts) >= 3 && strings.HasSuffix(parts[1], ".git") && parts[0] != "" && parts[0] != "." && parts[0] != ".." {
		name := strings.TrimSuffix(parts[1], ".git")
		if name != "" && name != "." && name != ".." {
			return []string{parts[0] + "/" + name}
		}
	}
	return nil
}

// This gate spans DB facts and subsequent Git publication. Its separate lock
// preserves the existing repo-lock -> snapshot-barrier acquisition order.
// Health polling must not continuously cancel idle maintenance.
func repositoryMaintenanceAdmission(store *gitstore.Store, next http.Handler) http.Handler {
	if store == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" || r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		release, err := store.BeginMaintenanceAccess(r.Context(), maintenanceRequestRepositories(r)...)
		if err != nil {
			http.Error(w, "repository operation cancelled", http.StatusServiceUnavailable)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}
