package graphql_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
)

type gqlPRProjectionClient struct {
	mu       sync.Mutex
	headSHA  string
	requests []forgejointegration.PullRequestRequest
}

func (f *gqlPRProjectionClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}

func (f *gqlPRProjectionClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, in)
	return forgejointegration.PullRequestResult{Number: 43, URL: "http://forgejo.local/pulls/43", ExternalRepo: in.Owner + "/" + in.Repo, HeadSHA: f.headSHA}, nil
}

func (f *gqlPRProjectionClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{Number: number, URL: "http://forgejo.local/pulls/43", ExternalRepo: owner + "/" + repo, HeadSHA: f.headSHA}, nil
}

func (f *gqlPRProjectionClient) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func TestCreatePRDispatchesForgejoProjectionFromGraphQLEntryPoint(t *testing.T) {
	svc, mux, u, cleanup := setupTestEnvironment(t)
	defer cleanup()
	ctx := context.Background()
	repo, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: u.Login, Name: "projection-gql-entry", AutoInit: true})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	branch := "agent/gql-entry"
	if err := svc.Git.CreateBranch(ctx, repo.FullName, branch, "main"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, repo.FullName, branch, "large-fixture.bin", "large fixture", []byte("repeatable projection fixture\n"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	client := &gqlPRProjectionClient{headSHA: headSHA}
	var pushes []forgejointegration.PushRequest
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			repo.FullName: {Owner: "forgejo", Repo: "projection-gql-entry"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	q := `
	mutation($input: CreatePullRequestInput!) {
		createPullRequest(input: $input) {
			pullRequest { number title headRefName baseRefName }
		}
	}`
	data := doGql(t, mux, q, map[string]any{
		"input": map[string]any{
			"repositoryId": fmt.Sprintf("Repository_%d", repo.ID),
			"title":        "GraphQL entry projection",
			"headRefName":  branch,
			"baseRefName":  "main",
		},
	})
	if data["createPullRequest"] == nil {
		t.Fatalf("missing createPullRequest data: %#v", data)
	}
	svc.Wg.Wait()
	if len(pushes) != 1 {
		t.Fatalf("GraphQL CreatePR push count=%d, want 1", len(pushes))
	}
	if pushes[0].Refspec != "refs/heads/agent/gql-entry:refs/heads/agent/gql-entry" {
		t.Fatalf("GraphQL CreatePR refspec=%q, want refs/heads/agent/gql-entry:refs/heads/agent/gql-entry", pushes[0].Refspec)
	}
	if client.requestCount() != 1 {
		t.Fatalf("GraphQL CreatePR did not ensure Forgejo PR projection, requests=%d", client.requestCount())
	}
	var count int64
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("provider = ? AND external_number = ? AND last_synced_sha = ?", service.ProjectionProviderForgejo, 43, headSHA).Count(&count).Error; err != nil {
		t.Fatalf("count projection: %v", err)
	}
	if count != 1 {
		t.Fatalf("projection rows=%d, want 1", count)
	}
}

func TestCreatePREnqueuesForgejoProjectionWhenGraphQLWorkerDisabled(t *testing.T) {
	svc, mux, u, cleanup := setupTestEnvironment(t)
	defer cleanup()
	ctx := context.Background()
	repo, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: u.Login, Name: "projection-gql-disabled", AutoInit: true})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	branch := "agent/gql-disabled"
	if err := svc.Git.CreateBranch(ctx, repo.FullName, branch, "main"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, repo.FullName, branch, "large-fixture.bin", "large fixture", []byte("repeatable projection fixture\n"))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	client := &gqlPRProjectionClient{headSHA: headSHA}
	pushes := 0
	svc.DisableForgejoProjectionWorker = true
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			repo.FullName: {Owner: "forgejo", Repo: "projection-gql-disabled"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushes++
		return nil
	})

	q := `
	mutation($input: CreatePullRequestInput!) {
		createPullRequest(input: $input) {
			pullRequest { number title headRefName baseRefName }
		}
	}`
	data := doGql(t, mux, q, map[string]any{
		"input": map[string]any{
			"repositoryId": fmt.Sprintf("Repository_%d", repo.ID),
			"title":        "GraphQL disabled projection worker",
			"headRefName":  branch,
			"baseRefName":  "main",
		},
	})
	if data["createPullRequest"] == nil {
		t.Fatalf("missing createPullRequest data: %#v", data)
	}
	if pushes != 0 || client.requestCount() != 0 {
		t.Fatalf("disabled worker should not push/ensure synchronously, pushes=%d requests=%d", pushes, client.requestCount())
	}
	var job db.PullRequestProjectionJob
	if err := svc.DB.Where("provider = ? AND head_sha = ?", service.ProjectionProviderForgejo, headSHA).First(&job).Error; err != nil {
		t.Fatalf("load queued job: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseQueued || job.Attempt != 0 {
		t.Fatalf("job phase/attempt=%s/%d, want queued/0", job.Phase, job.Attempt)
	}
	var projections int64
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("provider = ? AND last_synced_sha = ?", service.ProjectionProviderForgejo, headSHA).Count(&projections).Error; err != nil {
		t.Fatalf("count projections: %v", err)
	}
	if projections != 0 {
		t.Fatalf("worker-disabled request should not create final projection row yet, rows=%d", projections)
	}
}
