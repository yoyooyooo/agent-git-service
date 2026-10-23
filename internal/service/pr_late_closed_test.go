package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
	"github.com/stretchr/testify/require"
)

// The provider clock is deliberately unrelated to the AGS clock. No lifecycle
// transition changes head SHA, and no ordering assertion depends on a sleep.
type lateClosedClient struct {
	projectionSyncForgejoClient
	mu                       sync.Mutex
	state, revision          string
	head, base               string
	readStarted, readRelease chan struct{}
	readError                error
	notFound                 bool
	wrongNumber              bool
}

func (c *lateClosedClient) GetPullRequest(ctx context.Context, _, _ string, number int) (forgejointegration.PullRequestSnapshot, bool, error) {
	c.mu.Lock()
	if c.readError != nil {
		err := c.readError
		c.mu.Unlock()
		return forgejointegration.PullRequestSnapshot{}, false, err
	}
	if c.notFound {
		c.mu.Unlock()
		return forgejointegration.PullRequestSnapshot{}, false, nil
	}
	if c.wrongNumber {
		number++
	}
	raw := fmt.Sprintf(`{"number":%d,"state":%q,"updated_at":%q,"head_ref":%q,"base_ref":%q,"head_sha":%q}`, number, c.state, c.revision, c.head, c.base, c.headSHA)
	started, release := c.readStarted, c.readRelease
	c.readStarted, c.readRelease = nil, nil
	c.mu.Unlock()
	var snapshot forgejointegration.PullRequestSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return snapshot, false, err
	}
	if started != nil {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return snapshot, false, ctx.Err()
		}
	}
	return snapshot, true, nil
}
func (c *lateClosedClient) UpdatePullRequestState(_ context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
	if state == db.StateOpen {
		c.revision = "2001-01-01T00:00:02Z"
	}
	return forgejointegration.PullRequestResult{Number: number, ExternalRepo: owner + "/" + repo, HeadSHA: c.headSHA}, nil
}
func (c *lateClosedClient) set(state, revision string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state, c.revision = state, revision
}

func signedLateClose(revision string) (http.Header, []byte) {
	body := []byte(fmt.Sprintf(`{"action":"closed","repository":{"full_name":"forgejo/repo"},"pull_request":{"number":42,"merged":false,"updated_at":%q,"head":{"ref":"agent/projection"},"base":{"ref":"main"}}}`, revision))
	mac := hmac.New(sha256.New, []byte("late-close-fixture"))
	_, _ = mac.Write(body)
	return http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body
}
func lateCloseFixture(t *testing.T) (*service.Service, db.PullRequest, *lateClosedClient) {
	t.Helper()
	svc, cleanup := setupTestService(t)
	t.Cleanup(cleanup)
	svc.DisableForgejoProjectionWorker = true
	pr := createProjectedPRForLifecycleTest(t, svc, context.Background())
	client := &lateClosedClient{projectionSyncForgejoClient: projectionSyncForgejoClient{headSHA: pr.HeadSHA}, state: db.StateClosed, revision: "2001-01-01T00:00:01Z", head: pr.HeadRef, base: pr.BaseRef}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, WebhookSecret: "late-close-fixture", AutoPullRequest: true, BaseURL: "https://forgejo.example.test", Token: "fixture-only", MirrorBranchIncludes: []string{"*"}, PRBranchIncludes: []string{"*"}, RepoMap: map[string]forgejointegration.RepoMapping{pr.Repository.FullName: {Owner: "forgejo", Repo: "repo"}}}, client, func(context.Context, forgejointegration.PushRequest) error { return nil })
	return svc, pr, client
}
func reopenLateClose(t *testing.T, svc *service.Service, pr db.PullRequest, transport string) {
	t.Helper()
	open := db.StateOpen
	if transport == "rest" {
		_, err := svc.UpdatePR(context.Background(), pr.Repository.FullName, pr.Number, service.UpdatePRInput{State: &open})
		require.NoError(t, err)
	} else {
		require.NoError(t, svc.UpdatePRByID(context.Background(), pr.ID, &open, nil))
	}
}
func assertLateCloseOpen(t *testing.T, svc *service.Service, pr db.PullRequest) {
	t.Helper()
	current, err := svc.GetPRByID(context.Background(), pr.ID)
	require.NoError(t, err)
	require.Equal(t, db.StateOpen, current.State, "late signed close must not overwrite reopen")
	require.Nil(t, current.ClosedAt)
	require.EqualValues(t, 1, current.LifecycleGeneration)
	require.False(t, current.ForgejoReopenPending)
	require.NotNil(t, current.ForgejoLifecycleAt)
	require.Equal(t, pr.HeadSHA, current.HeadSHA)
	var projection db.PullRequestProjection
	require.NoError(t, svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&projection).Error)
	require.Equal(t, service.ProjectionStateOpen, projection.State)
}

func TestLateClosedAfterReopen(t *testing.T) {
	for _, transport := range []string{"rest", "graphql"} {
		for _, mode := range []string{"first-disabled", "replay-disabled", "replay-no-link", "replay-linked"} {
			t.Run(transport+"/"+mode, func(t *testing.T) {
				svc, pr, _ := lateCloseFixture(t)
				ctx := context.Background()
				if mode == "replay-no-link" || mode == "replay-linked" {
					configureMulticaTerminalDelivery(svc)
				}
				if mode == "replay-linked" {
					addAuthoritativeMulticaLink(t, svc, pr)
				}
				headers, body := signedLateClose("2001-01-01T00:00:01Z")
				if mode != "first-disabled" {
					_, err := svc.HandleForgejoWebhook(ctx, headers, body)
					require.NoError(t, err)
				} else {
					// The first ever delivery is retained outside AGS; only native/provider close is known.
					require.NoError(t, svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("state", db.StateClosed).Error)
					require.NoError(t, svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ?", pr.ID).Update("state", service.ProjectionStateClosed).Error)
				}
				gate, started, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() { close(started); <-gate; _, err := svc.HandleForgejoWebhook(ctx, headers, body); result <- err }()
				<-started
				reopenLateClose(t, svc, pr, transport)
				assertLateCloseOpen(t, svc, pr)
				var before db.PullRequest
				require.NoError(t, svc.DB.First(&before, pr.ID).Error)
				var projectionBefore db.PullRequestProjection
				require.NoError(t, svc.DB.Where("pull_request_id = ?", pr.ID).First(&projectionBefore).Error)
				var deliveriesBefore []db.OutboundDelivery
				require.NoError(t, svc.DB.Order("id").Find(&deliveriesBefore).Error)
				close(gate)
				require.NoError(t, <-result)
				assertLateCloseOpen(t, svc, pr)
				var after db.PullRequest
				require.NoError(t, svc.DB.First(&after, pr.ID).Error)
				require.Equal(t, before, after, "stale callback must perform zero PR writes")
				var projectionAfter db.PullRequestProjection
				require.NoError(t, svc.DB.First(&projectionAfter, projectionBefore.ID).Error)
				require.Equal(t, projectionBefore, projectionAfter)
				var deliveriesAfter []db.OutboundDelivery
				require.NoError(t, svc.DB.Order("id").Find(&deliveriesAfter).Error)
				require.Equal(t, deliveriesBefore, deliveriesAfter)
			})
		}
	}
}

func TestLateClosedAllowsNewCloseAndProtectsMerged(t *testing.T) {
	svc, pr, client := lateCloseFixture(t)
	ctx := context.Background()
	configureMulticaTerminalDelivery(svc)
	addAuthoritativeMulticaLink(t, svc, pr)
	headers, body := signedLateClose("2001-01-01T00:00:01Z")
	_, err := svc.HandleForgejoWebhook(ctx, headers, body)
	require.NoError(t, err)
	reopenLateClose(t, svc, pr, "rest")
	client.set(db.StateClosed, "2001-01-01T00:00:03Z")
	headers, body = signedLateClose("2001-01-01T00:00:03Z")
	_, err = svc.HandleForgejoWebhook(ctx, headers, body)
	require.NoError(t, err)
	current, err := svc.GetPRByID(ctx, pr.ID)
	require.NoError(t, err)
	require.Equal(t, db.StateClosed, current.State)
	var deliveries []db.OutboundDelivery
	require.NoError(t, svc.DB.Order("id").Find(&deliveries).Error)
	require.Len(t, deliveries, 2, "each close lifecycle needs a distinct stable delivery identity")
	_, err = svc.HandleForgejoWebhook(ctx, headers, body)
	require.NoError(t, err)
	var count int64
	require.NoError(t, svc.DB.Model(&db.OutboundDelivery{}).Count(&count).Error)
	require.EqualValues(t, 2, count)
	merged, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "forgejo")
	require.NoError(t, err)
	_, err = svc.HandleForgejoWebhook(ctx, headers, body)
	require.NoError(t, err)
	after, err := svc.GetPRByID(ctx, pr.ID)
	require.NoError(t, err)
	require.True(t, after.Merged)
	require.Equal(t, merged.MergeCommitSHA, after.MergeCommitSHA)
}

func TestLateClosedReadbackBeforeReopenCAS(t *testing.T) {
	for _, transport := range []string{"rest", "graphql"} {
		t.Run(transport, func(t *testing.T) {
			svc, pr, client := lateCloseFixture(t)
			ctx := context.Background()
			// The provider read completes against E1 but its result is held outside the
			// write transaction. E2 must commit while that old readback is in flight.
			client.mu.Lock()
			client.readStarted = make(chan struct{})
			client.readRelease = make(chan struct{})
			started, release := client.readStarted, client.readRelease
			client.mu.Unlock()
			headers, body := signedLateClose("2001-01-01T00:00:01Z")
			result := make(chan error, 1)
			go func() { _, err := svc.HandleForgejoWebhook(ctx, headers, body); result <- err }()
			<-started
			require.NoError(t, svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("state", db.StateClosed).Error)
			require.NoError(t, svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ?", pr.ID).Update("state", service.ProjectionStateClosed).Error)
			reopenLateClose(t, svc, pr, transport)
			close(release)
			require.NoError(t, <-result)
			assertLateCloseOpen(t, svc, pr)
			var count int64
			require.NoError(t, svc.DB.Model(&db.OutboundDelivery{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestLateClosedRevisionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, event, live string
		wantError         bool
	}{
		{"missing", "", "2001-01-01T00:00:03Z", true},
		{"invalid", "not-a-time", "2001-01-01T00:00:03Z", true},
		{"older", "2001-01-01T00:00:01Z", "2001-01-01T00:00:03Z", false},
		{"equal-conflict", "2001-01-01T00:00:02Z", "2001-01-01T00:00:02Z", false},
		{"future-signed", "2099-01-01T00:00:00Z", "2001-01-01T00:00:03Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, pr, client := lateCloseFixture(t)
			ctx := context.Background()
			h, b := signedLateClose("2001-01-01T00:00:01Z")
			_, err := svc.HandleForgejoWebhook(ctx, h, b)
			require.NoError(t, err)
			reopenLateClose(t, svc, pr, "rest")
			client.set(db.StateClosed, tc.live)
			h, b = signedLateClose(tc.event)
			res, err := svc.HandleForgejoWebhook(ctx, h, b)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, "stale_lifecycle", res.IgnoredReason)
			}
			assertLateCloseOpen(t, svc, pr)
		})
	}
}

func TestLateClosedReadbackFailuresAreZeroWrite(t *testing.T) {
	for _, mode := range []string{"404", "timeout", "wrong-number", "provider-open", "bad-signature", "pending-reopen"} {
		t.Run(mode, func(t *testing.T) {
			svc, pr, client := lateCloseFixture(t)
			ctx := context.Background()
			switch mode {
			case "404":
				client.notFound = true
			case "timeout":
				client.readError = context.DeadlineExceeded
			case "wrong-number":
				client.wrongNumber = true
			case "provider-open":
				client.set(db.StateOpen, "2001-01-01T00:00:02Z")
			case "pending-reopen":
				require.NoError(t, svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("forgejo_reopen_pending", true).Error)
			}
			var before db.PullRequest
			require.NoError(t, svc.DB.First(&before, pr.ID).Error)
			var projectionBefore db.PullRequestProjection
			require.NoError(t, svc.DB.Where("pull_request_id = ?", pr.ID).First(&projectionBefore).Error)
			h, b := signedLateClose("2001-01-01T00:00:01Z")
			if mode == "bad-signature" {
				h.Set("X-Hub-Signature-256", "sha256=bad")
			}
			result, err := svc.HandleForgejoWebhook(ctx, h, b)
			if mode == "provider-open" {
				require.NoError(t, err)
				require.Equal(t, "stale_lifecycle", result.IgnoredReason)
			} else {
				require.Error(t, err)
			}
			var after db.PullRequest
			require.NoError(t, svc.DB.First(&after, pr.ID).Error)
			require.Equal(t, before, after)
			var projectionAfter db.PullRequestProjection
			require.NoError(t, svc.DB.First(&projectionAfter, projectionBefore.ID).Error)
			require.Equal(t, projectionBefore, projectionAfter)
		})
	}
}

func TestLateClosedRollbackIncludesWatermark(t *testing.T) {
	svc, pr, _ := lateCloseFixture(t)
	configureMulticaTerminalDelivery(svc)
	addAuthoritativeMulticaLink(t, svc, pr)
	name := "test:late_closed_outbox_failure"
	require.NoError(t, svc.DB.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "OutboundDelivery" {
			tx.AddError(errors.New("fixture outbox failure"))
		}
	}))
	t.Cleanup(func() { _ = svc.DB.Callback().Create().Remove(name) })
	var before db.PullRequest
	require.NoError(t, svc.DB.First(&before, pr.ID).Error)
	h, b := signedLateClose("2001-01-01T00:00:01Z")
	_, err := svc.HandleForgejoWebhook(context.Background(), h, b)
	require.Error(t, err)
	var after db.PullRequest
	require.NoError(t, svc.DB.First(&after, pr.ID).Error)
	require.Equal(t, before, after)
	var projection db.PullRequestProjection
	require.NoError(t, svc.DB.Where("pull_request_id = ?", pr.ID).First(&projection).Error)
	require.Equal(t, service.ProjectionStateOpen, projection.State)
}

func TestLateClosedSameSecondPrecisionAndRestart(t *testing.T) {
	svc, pr, client := lateCloseFixture(t)
	ctx := context.Background()
	h, b := signedLateClose("2001-01-01T00:00:01Z")
	_, err := svc.HandleForgejoWebhook(ctx, h, b)
	require.NoError(t, err)
	reopenLateClose(t, svc, pr, "graphql")
	// A new service instance shares only durable DB and Git facts, not a mutex.
	restarted := &service.Service{DB: svc.DB, Git: svc.Git, ForgejoIntegration: svc.ForgejoIntegration, BaseURL: svc.BaseURL, Embedder: svc.Embedder}
	client.set(db.StateClosed, "2001-01-01T02:00:02.123456+02:00")
	h, b = signedLateClose("2001-01-01T00:00:02.123456Z")
	_, err = restarted.HandleForgejoWebhook(ctx, h, b)
	require.NoError(t, err)
	var current db.PullRequest
	require.NoError(t, svc.DB.First(&current, pr.ID).Error)
	require.Equal(t, db.StateClosed, current.State)
	require.NotNil(t, current.ForgejoLifecycleAt)
	require.Equal(t, time.Date(2001, 1, 1, 0, 0, 2, 123456000, time.UTC), current.ForgejoLifecycleAt.UTC())
}

func TestLateClosedConcurrentWorkersUseDurableGeneration(t *testing.T) {
	svc, pr, client := lateCloseFixture(t)
	ctx := context.Background()
	configureMulticaTerminalDelivery(svc)
	addAuthoritativeMulticaLink(t, svc, pr)
	oldHeaders, oldBody := signedLateClose("2001-01-01T00:00:01Z")
	_, err := svc.HandleForgejoWebhook(ctx, oldHeaders, oldBody)
	require.NoError(t, err)
	reopenLateClose(t, svc, pr, "graphql")
	client.set(db.StateClosed, "2001-01-01T00:00:03Z")
	newHeaders, newBody := signedLateClose("2001-01-01T00:00:03Z")
	start := make(chan struct{})
	results := make(chan error, 4)
	for i := range 4 {
		// Independent worker services have no shared in-process lifecycle mutex.
		worker := &service.Service{DB: svc.DB, Git: svc.Git, ForgejoIntegration: svc.ForgejoIntegration, BaseURL: svc.BaseURL, Embedder: svc.Embedder, MulticaProjection: svc.MulticaProjection, OutboundEventTargets: svc.OutboundEventTargets}
		h, b := oldHeaders, oldBody
		if i%2 == 0 {
			h, b = newHeaders, newBody
		}
		go func() { <-start; _, err := worker.HandleForgejoWebhook(ctx, h, b); results <- err }()
	}
	close(start)
	for range 4 {
		require.NoError(t, <-results)
	}
	var current db.PullRequest
	require.NoError(t, svc.DB.First(&current, pr.ID).Error)
	require.Equal(t, db.StateClosed, current.State)
	require.EqualValues(t, 1, current.LifecycleGeneration)
	require.NotNil(t, current.ForgejoLifecycleAt)
	require.Equal(t, time.Date(2001, 1, 1, 0, 0, 3, 0, time.UTC), current.ForgejoLifecycleAt.UTC())
	var rows []db.OutboundDelivery
	require.NoError(t, svc.DB.Find(&rows).Error)
	require.Len(t, rows, 2)
}

func TestLateClosedHTTPReopenEntrypoints(t *testing.T) {
	for _, transport := range []string{"rest", "graphql"} {
		t.Run(transport, func(t *testing.T) {
			h := testharness.New(t)
			svc := h.Svc
			svc.DisableForgejoProjectionWorker = true
			ctx := context.Background()
			pr := createProjectedPRForLifecycleTest(t, svc, ctx)
			client := &lateClosedClient{projectionSyncForgejoClient: projectionSyncForgejoClient{headSHA: pr.HeadSHA}, state: db.StateClosed, revision: "2001-01-01T00:00:01Z", head: pr.HeadRef, base: pr.BaseRef}
			svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, WebhookSecret: "late-close-fixture", AutoPullRequest: true, BaseURL: "https://forgejo.example.test", Token: "fixture-only", MirrorBranchIncludes: []string{"*"}, PRBranchIncludes: []string{"*"}, RepoMap: map[string]forgejointegration.RepoMapping{pr.Repository.FullName: {Owner: "forgejo", Repo: "repo"}}}, client, func(context.Context, forgejointegration.PushRequest) error { return nil })
			headers, body := signedLateClose("2001-01-01T00:00:01Z")
			_, err := svc.HandleForgejoWebhook(ctx, headers, body)
			require.NoError(t, err)
			started, gate, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() { close(started); <-gate; _, err := svc.HandleForgejoWebhook(ctx, headers, body); result <- err }()
			<-started
			if transport == "rest" {
				response := h.DoRESTJSON(t, http.MethodPatch, fmt.Sprintf("/api/v3/repos/%s/pulls/%d", pr.Repository.FullName, pr.Number), map[string]any{"state": "open"})
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			} else {
				data := h.DoGraphQL(t, `mutation($input: ReopenPullRequestInput!) { reopenPullRequest(input:$input) { pullRequest { state } } }`, map[string]any{"input": map[string]any{"pullRequestId": fmt.Sprintf("PullRequest_%d", pr.ID)}})
				require.Equal(t, "OPEN", data["reopenPullRequest"].(map[string]any)["pullRequest"].(map[string]any)["state"])
			}
			close(gate)
			require.NoError(t, <-result)
			assertLateCloseOpen(t, svc, pr)
		})
	}
}
