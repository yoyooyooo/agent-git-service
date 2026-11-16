package service_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestLifecycleReviewMultipleProjectionLockOrder(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	svc.DisableForgejoProjectionWorker = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	// Make Forgejo the SECOND mapping. This is the lock-cycle ordering from
	// review: native used to hold A while merge held F and tried to update A.
	require.NoError(t, svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ?", pr.ID).Updates(map[string]any{"provider": service.ProjectionProviderGitHub, "external_repo": "backup/repo"}).Error)
	require.NoError(t, svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo, ExternalRepo: "forgejo/repo", ExternalNumber: 42, State: service.ProjectionStateOpen}))
	type laneKey struct{}
	nativeCtx := context.WithValue(ctx, laneKey{}, "native")
	mergeCtx := context.WithValue(ctx, laneKey{}, "merge")
	nativeHeld, release, mergeQuery := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var heldOnce, mergeOnce sync.Once
	var mu sync.Mutex
	var lockSQL []string
	require.NoError(t, svc.DB.Callback().Query().Before("gorm:query").Register("review_merge_query_barrier", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(laneKey{}) == "merge" && tx.Statement.Table == "pull_request_projections" {
			mergeOnce.Do(func() { close(mergeQuery) })
		}
	}))
	require.NoError(t, svc.DB.Callback().Query().After("gorm:query").Register("review_lock_barrier", func(tx *gorm.DB) {
		sql := tx.Statement.SQL.String()
		if tx.Statement.Table != "pull_request_projections" || !strings.Contains(sql, "FOR UPDATE") {
			return
		}
		mu.Lock()
		lockSQL = append(lockSQL, sql)
		mu.Unlock()
		if tx.Statement.Context.Value(laneKey{}) == "native" {
			heldOnce.Do(func() {
				close(nativeHeld)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
	}))
	defer svc.DB.Callback().Query().Remove("review_merge_query_barrier")
	defer svc.DB.Callback().Query().Remove("review_lock_barrier")
	nativeDone, mergeDone := make(chan error, 1), make(chan error, 1)
	go func() {
		closed := db.StateClosed
		_, err := svc.UpdatePR(nativeCtx, pr.Repository.FullName, pr.Number, service.UpdatePRInput{State: &closed})
		nativeDone <- err
	}()
	select {
	case <-nativeHeld:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		_, err := svc.MarkPullRequestMergedFromProjection(mergeCtx, service.ProjectionProviderForgejo, "forgejo/repo", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "fixture")
		mergeDone <- err
	}()
	select {
	case <-mergeQuery:
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	close(release)
	require.NoError(t, <-nativeDone)
	require.NoError(t, <-mergeDone)
	mu.Lock()
	queries := append([]string(nil), lockSQL...)
	mu.Unlock()
	require.GreaterOrEqual(t, len(queries), 2)
	// Both paths must acquire the complete ordered set before any individual
	// binding/PR lock. Inspect actual TiDB SQL, not a mock locking implementation.
	require.Contains(t, queries[0], "ORDER BY id")
	require.Contains(t, queries[1], "ORDER BY id")
	require.NotContains(t, queries[1], "external_number")
	current, err := svc.GetPRByID(ctx, pr.ID)
	require.NoError(t, err)
	require.True(t, current.Merged)
	var projections []db.PullRequestProjection
	require.NoError(t, svc.DB.Where("pull_request_id = ?", pr.ID).Order("id").Find(&projections).Error)
	require.Len(t, projections, 2)
	for _, p := range projections {
		require.Equal(t, service.ProjectionStateClosed, p.State)
	}
}
