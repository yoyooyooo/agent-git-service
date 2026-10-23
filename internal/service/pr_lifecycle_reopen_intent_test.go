package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func reopenIntentFixture(t *testing.T) (*Service, db.PullRequest, db.PullRequestProjection) {
	t.Helper()
	return seedReopenIntentFixture(t, setupProjectionStateService(t))
}

func seedReopenIntentFixture(t *testing.T, s *Service) (*Service, db.PullRequest, db.PullRequestProjection) {
	t.Helper()
	s.BaseURL = "https://ags.example"
	require.NoError(t, s.DB.AutoMigrate(&db.Milestone{}, &db.PullRequestMulticaLink{}, &db.OutboundDelivery{}))
	var repo db.Repository
	require.NoError(t, s.DB.First(&repo).Error)
	pr := db.PullRequest{Number: 901, RepositoryID: repo.ID, Repository: repo, State: db.StateOpen, HeadRef: "feature", BaseRef: "main", HeadSHA: "abc1234", LifecycleGeneration: 1, ForgejoReopenPending: true}
	require.NoError(t, s.DB.Create(&pr).Error)
	binding := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, ExternalRepo: "forgejo/repo", ExternalNumber: 42, ExternalURL: "https://forgejo.example/forgejo/repo/pulls/42", State: ProjectionStateClosed}
	require.NoError(t, s.DB.Create(&binding).Error)
	s.MulticaProjection = multicaprojection.New(multicaprojection.Config{Enabled: true, TargetInstance: "fixture", AppURL: "https://multica.example"})
	s.OutboundEventTargets = map[string][]OutboundTarget{OutboundEventMulticaExternalPRTerminal: {{Name: OutboundTargetNameMulticaExternalPR, Type: OutboundTargetTypeMulticaExternalPR}}}
	require.NoError(t, s.DB.Create(&db.PullRequestMulticaLink{PullRequestID: pr.ID, RepositoryID: repo.ID, Workspace: "workspace-alpha", WorkspaceID: "workspace-id", IssueID: "issue-id", IssueKey: "TEST-42", Confidence: db.MulticaLinkConfidenceAuthoritative, Source: db.MulticaLinkSourceTaskToken}).Error)
	return s, pr, binding
}

func reopenIntentSnapshot(pr db.PullRequest, revision string) forgejointegration.PullRequestSnapshot {
	return forgejointegration.PullRequestSnapshot{Number: 42, State: db.StateOpen, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, UpdatedAt: revision}
}

func TestLifecycleReviewOldPendingWorkerReopensNewClose(t *testing.T) {
	for _, timing := range []string{"before-get", "during-get"} {
		t.Run(timing, func(t *testing.T) {
			s, pr, binding := reopenIntentFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			job := seedGenericProjectionJob(t, s, pr, ForgejoProjectionPhaseQueued)
			claim, err := s.claimGenericForgejoProjectionJob(ctx, job)
			require.NoError(t, err)
			workerCtx := contextWithGenericProjectionClaim(ctx, claim)
			require.NoError(t, s.updateForgejoProjectionJobPhase(workerCtx, job.ID, ForgejoProjectionPhasePushingRef, nil))
			var mu sync.Mutex
			state, revision, patches := db.StateClosed, "2001-01-01T00:00:03Z", 0
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if timing == "during-get" && r.Method == http.MethodGet {
					once.Do(func() {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
						}
					})
				}
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path != "/api/v1/repos/forgejo/repo/pulls/42" {
					t.Errorf("unexpected URL %s", r.URL.Path)
					http.Error(w, "unexpected", 400)
					return
				}
				if r.Method == http.MethodPatch {
					patches++
					state, revision = db.StateOpen, "2001-01-01T00:00:04Z"
				} else if r.Method != http.MethodGet {
					t.Errorf("unexpected method %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"number":42,"state":%q,"merged":false,"updated_at":%q,"head":{"ref":"feature","sha":"abc1234"},"base":{"ref":"main"}}`, state, revision)
			}))
			defer server.Close()
			s.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, Token: "fixture-only", BaseURL: server.URL, RepoMap: map[string]forgejointegration.RepoMapping{pr.Repository.FullName: {Owner: "forgejo", Repo: "repo"}}}, forgejointegration.NewHTTPClient(server.URL, "fixture-only"), nil)
			// The claimed worker retains pending=true. Pause before its helper or
			// inside exact GET, then let synchronous confirmation consume T2.
			done := make(chan error, 1)
			run := func() {
				done <- s.reopenForgejoProjection(workerCtx, pr.Repository.FullName, pr, func(c context.Context) error { return s.revalidateGenericProjectionProviderWrite(c, claim, pr) })
			}
			if timing == "during-get" {
				go run()
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			require.NoError(t, s.confirmForgejoReopen(ctx, pr, binding, reopenIntentSnapshot(pr, "2001-01-01T00:00:02Z")))
			// The provider stub now represents the user's legitimate T3 close.
			if timing == "during-get" {
				close(release)
			} else {
				go run()
			}
			err = <-done
			mu.Lock()
			gotPatches, gotState := patches, state
			mu.Unlock()
			t.Logf("old worker result=%v; provider PATCH count=%d state=%s", err, gotPatches, gotState)
			require.Zero(t, gotPatches, "completed reopen intent must not reopen a subsequent legitimate provider close")
			require.ErrorIs(t, err, ErrStaleProjectionClose)
			require.Equal(t, db.StateClosed, gotState)
			var current db.PullRequest
			require.NoError(t, s.DB.First(&current, pr.ID).Error)
			require.False(t, current.ForgejoReopenPending)
			require.Equal(t, "2001-01-01T00:00:02Z", current.ForgejoLifecycleAt.UTC().Format(time.RFC3339))
			// Exercise the actual close transaction and its durable outbox, then
			// replay to prove that the only legitimate close is not lost.
			for range 2 {
				closed, err := s.MarkPullRequestClosedFromProjection(ctx, ProjectionProviderForgejo, "forgejo/repo", 42, "fixture", "2001-01-01T00:00:03Z")
				require.NoError(t, err)
				require.Equal(t, db.StateClosed, closed.State)
				require.NotNil(t, closed.ClosedAt)
			}
			require.NoError(t, s.DB.First(&binding, binding.ID).Error)
			require.Equal(t, ProjectionStateClosed, binding.State)
			var deliveries []db.OutboundDelivery
			require.NoError(t, s.DB.Find(&deliveries).Error)
			require.Len(t, deliveries, 1)
			require.Equal(t, OutboundDeliveryStatusPending, deliveries[0].Status)
		})
	}
}

func TestLifecycleReviewCompletedIntentCannotConfirmAgain(t *testing.T) {
	s, pr, binding := reopenIntentFixture(t)
	ctx := context.Background()
	require.NoError(t, s.confirmForgejoReopen(ctx, pr, binding, reopenIntentSnapshot(pr, "2001-01-01T00:00:02Z")))
	var before db.PullRequest
	require.NoError(t, s.DB.First(&before, pr.ID).Error)
	require.ErrorIs(t, s.confirmForgejoReopen(ctx, pr, binding, reopenIntentSnapshot(pr, "2001-01-01T00:00:04Z")), ErrStaleProjectionClose)
	var after db.PullRequest
	require.NoError(t, s.DB.First(&after, pr.ID).Error)
	require.Equal(t, before, after, "duplicate confirmation cannot advance watermark or update the row")
}

func TestLifecycleReviewReopenAdmissionHoldsIntentUntilConsumption(t *testing.T) {
	// A real TiDB transaction is required: SQLite cannot prove FOR UPDATE.
	gdb, cleanup := openMigratedServiceTestDB(t)
	t.Cleanup(cleanup)
	owner := db.User{Login: "fixture-owner", Type: "User"}
	require.NoError(t, gdb.Create(&owner).Error)
	require.NoError(t, gdb.Create(&db.Repository{OwnerID: owner.ID, Name: "demo", FullName: "fixture-owner/demo", DefaultBranch: "main"}).Error)
	s, pr, binding := seedReopenIntentFixture(t, &Service{DB: gdb})
	// The Forgejo row is second, so the admission path must lock the entire
	// ordered set rather than acquire Forgejo first and another provider later.
	require.NoError(t, gdb.Model(&binding).Updates(map[string]any{"provider": ProjectionProviderGitHub, "external_repo": "backup/repo"}).Error)
	binding = db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo, ExternalRepo: "forgejo/repo", ExternalNumber: 42, State: ProjectionStateClosed}
	require.NoError(t, gdb.Create(&binding).Error)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admitted, release, contender := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce, contenderOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	type laneKey struct{}
	var mu sync.Mutex
	var queries []string
	require.NoError(t, gdb.Callback().Query().Before("gorm:query").Register("reopen_contender", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(laneKey{}) == "confirm" && tx.Statement.Table == "pull_request_projections" {
			contenderOnce.Do(func() { close(contender) })
		}
	}))
	t.Cleanup(func() { gdb.Callback().Query().Remove("reopen_contender") })
	require.NoError(t, gdb.Callback().Query().After("gorm:query").Register("reopen_lock_order", func(tx *gorm.DB) {
		sql := tx.Statement.SQL.String()
		if tx.Statement.Context.Value(laneKey{}) == "write" && strings.Contains(sql, "FOR UPDATE") {
			mu.Lock()
			queries = append(queries, sql)
			mu.Unlock()
		}
	}))
	t.Cleanup(func() { gdb.Callback().Query().Remove("reopen_lock_order") })
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/api/v1/repos/forgejo/repo/pulls/42" {
			t.Errorf("unexpected URL %s", r.URL.Path)
			http.Error(w, "unexpected", 400)
			return
		}
		if r.Method == http.MethodPatch {
			patches++
		} else if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
		}
		state, revision := db.StateClosed, "2001-01-01T00:00:01Z"
		if patches > 0 {
			state, revision = db.StateOpen, "2001-01-01T00:00:02Z"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"number":42,"state":%q,"updated_at":%q,"head":{"ref":"feature"},"base":{"ref":"main"}}`, state, revision)
	}))
	defer server.Close()
	s.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, Token: "fixture-only", BaseURL: server.URL, RepoMap: map[string]forgejointegration.RepoMapping{pr.Repository.FullName: {Owner: "forgejo", Repo: "repo"}}}, forgejointegration.NewHTTPClient(server.URL, "fixture-only"), nil)
	writeDone, confirmDone := make(chan error, 1), make(chan error, 1)
	go func() {
		writeDone <- s.reopenForgejoProjection(context.WithValue(ctx, laneKey{}, "write"), pr.Repository.FullName, pr, func(context.Context) error {
			close(admitted)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-admitted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		confirmDone <- s.confirmForgejoReopen(context.WithValue(ctx, laneKey{}, "confirm"), pr, binding, reopenIntentSnapshot(pr, "2001-01-01T00:00:04Z"))
	}()
	select {
	case <-contender:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-writeDone)
	require.ErrorIs(t, <-confirmDone, ErrStaleProjectionClose)
	mu.Lock()
	gotPatches, lockSQL := patches, append([]string(nil), queries...)
	mu.Unlock()
	require.Equal(t, 1, gotPatches)
	require.GreaterOrEqual(t, len(lockSQL), 3)
	require.Contains(t, lockSQL[0], "pull_request_projections")
	require.Contains(t, lockSQL[0], "ORDER BY id")
	require.NotContains(t, lockSQL[0], "external_number")
	require.Contains(t, lockSQL[2], "pull_requests")
	var current db.PullRequest
	require.NoError(t, gdb.First(&current, pr.ID).Error)
	require.False(t, current.ForgejoReopenPending)
	require.Equal(t, "2001-01-01T00:00:02Z", current.ForgejoLifecycleAt.UTC().Format(time.RFC3339))
}
