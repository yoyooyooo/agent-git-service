package service_test

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUpdatePRByIDReopenRefreshesForgejoProjection(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	require.NoError(t, svc.DB.Create(&db.User{Login: "reopen-fixture", Type: db.TypeUser}).Error)
	repo, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "reopen-fixture", Name: "project", DefaultBranch: "main", AddReadme: true})
	require.NoError(t, err)
	require.NoError(t, svc.Git.CreateBranch(ctx, repo.FullName, "feature", "main"))
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: repo.FullName, Title: "Reopen fixture", HeadRef: "feature", BaseRef: "main", AuthorLogin: "reopen-fixture"})
	require.NoError(t, err)
	client := &projectionSyncForgejoClient{headSHA: pr.HeadSHA}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "https://ci.example.test", Token: "fixture-only", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"*"}, PRBranchIncludes: []string{"*"},
		RepoMap: map[string]forgejointegration.RepoMapping{repo.FullName: {Owner: "ci", Repo: "project"}},
	}, client, func(context.Context, forgejointegration.PushRequest) error { return nil })
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
}
