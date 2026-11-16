package service_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/stretchr/testify/require"
)

type reopenParityClient struct {
	projectionSyncForgejoClient
	state            string
	stateWrites      int
	headRef, baseRef string
}

func (c *reopenParityClient) GetPullRequest(context.Context, string, string, int) (forgejointegration.PullRequestSnapshot, bool, error) {
	return forgejointegration.PullRequestSnapshot{UpdatedAt: "2001-01-01T00:00:02Z", Number: 42, State: c.state, HeadRef: c.headRef, BaseRef: c.baseRef, HeadSHA: c.headSHA}, true, nil
}

func (c *reopenParityClient) UpdatePullRequestState(_ context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	c.state, c.stateWrites = state, c.stateWrites+1
	return forgejointegration.PullRequestResult{Number: number, ExternalRepo: owner + "/" + repo, HeadSHA: c.headSHA}, nil
}

func (c *reopenParityClient) EnsurePullRequest(ctx context.Context, request forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	if c.state != db.StateOpen {
		return forgejointegration.PullRequestResult{Number: 99, ExternalRepo: request.Owner + "/" + request.Repo, HeadSHA: c.headSHA}, nil
	}
	return c.projectionSyncForgejoClient.EnsurePullRequest(ctx, request)
}

func TestUpdatePRByIDReopenRefreshesForgejoProjection(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	svc.DisableForgejoProjectionWorker = true
	ctx := context.Background()
	require.NoError(t, svc.DB.Create(&db.User{Login: "reopen-fixture", Type: db.TypeUser}).Error)
	repo, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "reopen-fixture", Name: "project", DefaultBranch: "main", AddReadme: true})
	require.NoError(t, err)
	require.NoError(t, svc.Git.CreateBranch(ctx, repo.FullName, "feature", "main"))
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: repo.FullName, Title: "Reopen fixture", HeadRef: "feature", BaseRef: "main", AuthorLogin: "reopen-fixture"})
	require.NoError(t, err)
	client := &reopenParityClient{projectionSyncForgejoClient: projectionSyncForgejoClient{headSHA: pr.HeadSHA}, state: db.StateOpen, headRef: pr.HeadRef, baseRef: pr.BaseRef}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "https://ci.example.test", Token: "fixture-only", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"*"}, PRBranchIncludes: []string{"*"},
		RepoMap: map[string]forgejointegration.RepoMapping{repo.FullName: {Owner: "ci", Repo: "project"}},
	}, client, func(context.Context, forgejointegration.PushRequest) error { return nil })
	_, err = svc.DispatchPullRequestIntegrations(ctx, pr)
	require.NoError(t, err)
	closed := db.StateClosed
	require.NoError(t, svc.UpdatePRByID(ctx, pr.ID, &closed, nil))
	before := len(client.requests)
	open := db.StateOpen
	require.NoError(t, svc.UpdatePRByID(ctx, pr.ID, &open, nil))
	require.Len(t, client.requests, before+1, "GraphQL reopen must refresh the provider like REST PATCH")
	current, err := svc.GetPRByID(ctx, pr.ID)
	require.NoError(t, err)
	require.Equal(t, db.StateOpen, current.State)
	require.Nil(t, current.ClosedAt)
	var projection db.PullRequestProjection
	require.NoError(t, svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&projection).Error)
	require.Equal(t, service.ProjectionStateOpen, projection.State)
	require.Equal(t, pr.HeadSHA, projection.LastSyncedSHA)
	require.Equal(t, 42, projection.ExternalNumber, "reopen cannot replace the mapped provider PR")
	require.Equal(t, db.StateOpen, client.state)
	require.Equal(t, 2, client.stateWrites, "one close and one exact mapped reopen")
	// A merged terminal fact cannot be reopened through either transport.
	require.NoError(t, svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{"merged": true, "state": db.StateClosed}).Error)
	require.True(t, errors.Is(svc.UpdatePRByID(ctx, pr.ID, &open, nil), service.ErrInvalidState))
	_, err = svc.UpdatePR(ctx, repo.FullName, pr.Number, service.UpdatePRInput{State: &open})
	require.True(t, errors.Is(err, service.ErrInvalidState))
	require.Equal(t, 2, client.stateWrites)
}

func TestReopenRefreshesCommitsPushedWhileClosed(t *testing.T) {
	for _, transport := range []string{"graphql", "rest"} {
		t.Run(transport, func(t *testing.T) {
			svc, cleanup := setupTestService(t)
			defer cleanup()
			ctx := context.Background()
			pr, oldHead := seedStandingExecutorProjectionPR(t, svc, "reopen-head", "agent/feature")
			svc.DisableForgejoProjectionWorker = true
			closed, open := db.StateClosed, db.StateOpen
			require.NoError(t, svc.UpdatePRByID(ctx, pr.ID, &closed, nil))
			tip, err := svc.Git.WriteFile(ctx, pr.Repository.FullName, pr.HeadRef, "after-close.txt", "advance while closed", []byte("next\n"))
			require.NoError(t, err)
			require.NotEqual(t, oldHead, tip)
			stale, err := svc.GetPRByID(ctx, pr.ID)
			require.NoError(t, err)
			require.Equal(t, oldHead, stale.HeadSHA)
			if transport == "graphql" {
				require.NoError(t, svc.UpdatePRByID(ctx, pr.ID, &open, nil))
			} else {
				_, err = svc.UpdatePR(ctx, pr.Repository.FullName, pr.Number, service.UpdatePRInput{State: &open})
				require.NoError(t, err)
			}
			current, err := svc.GetPRByID(ctx, pr.ID)
			require.NoError(t, err)
			require.Equal(t, db.StateOpen, current.State)
			require.Equal(t, tip, current.HeadSHA)
			require.Nil(t, current.ClosedAt)
			path, err := svc.Git.GetRepoPath(ctx, pr.Repository.FullName)
			require.NoError(t, err)
			pullRef, err := exec.Command("git", "-C", path, "rev-parse", fmt.Sprintf("refs/pull/%d/head", pr.Number)).Output()
			require.NoError(t, err)
			require.Equal(t, tip+"\n", string(pullRef))
			branch, err := svc.Git.HeadSHA(ctx, pr.Repository.FullName, pr.HeadRef)
			require.NoError(t, err)
			require.Equal(t, tip, branch, "reopen cannot rewrite the source branch")
		})
	}
}
