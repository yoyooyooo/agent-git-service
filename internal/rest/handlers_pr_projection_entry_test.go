package rest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

type restPRProjectionClient struct {
	mu       sync.Mutex
	headSHA  string
	requests []forgejointegration.PullRequestRequest
}

func (f *restPRProjectionClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}

func (f *restPRProjectionClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, in)
	return forgejointegration.PullRequestResult{Number: 42, URL: "http://forgejo.local/pulls/42", ExternalRepo: in.Owner + "/" + in.Repo, HeadSHA: f.headSHA}, nil
}

func (f *restPRProjectionClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{Number: number, URL: "http://forgejo.local/pulls/42", ExternalRepo: owner + "/" + repo, HeadSHA: f.headSHA}, nil
}

func (f *restPRProjectionClient) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func TestCreatePRDispatchesForgejoProjectionFromRESTEntryPoint(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	compatSeedRepo(t, h, "projection-rest-entry")
	full := "testuser/projection-rest-entry"
	branch := "agent/rest-entry"
	if err := h.Svc.Git.CreateBranch(ctx, full, branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := h.Svc.Git.WriteFile(ctx, full, branch, "large-fixture.bin", "large fixture", []byte("repeatable projection fixture\n"))
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	client := &restPRProjectionClient{headSHA: headSHA}
	var pushes []forgejointegration.PushRequest
	h.Svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			full: {Owner: "forgejo", Repo: "projection-rest-entry"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	w := h.DoRESTJSON(t, "POST", fmt.Sprintf("/api/v3/repos/%s/pulls", full), map[string]any{
		"title": "REST entry projection",
		"head":  branch,
		"base":  "main",
	})
	assertStatusCode(t, w, 201)
	h.Svc.Wg.Wait()
	if len(pushes) != 1 {
		t.Fatalf("REST CreatePR push count=%d, want 1", len(pushes))
	}
	if pushes[0].Refspec != "refs/heads/agent/rest-entry:refs/heads/agent/rest-entry" {
		t.Fatalf("REST CreatePR refspec=%q, want refs/heads/agent/rest-entry:refs/heads/agent/rest-entry", pushes[0].Refspec)
	}
	if client.requestCount() != 1 {
		t.Fatalf("REST CreatePR did not ensure Forgejo PR projection, requests=%d", client.requestCount())
	}
	var count int64
	if err := h.DB.Model(&db.PullRequestProjection{}).Where("provider = ? AND external_number = ? AND last_synced_sha = ?", service.ProjectionProviderForgejo, 42, headSHA).Count(&count).Error; err != nil {
		t.Fatalf("count projection: %v", err)
	}
	if count != 1 {
		t.Fatalf("projection rows=%d, want 1", count)
	}
}

func TestCreatePREnqueuesForgejoProjectionWhenRESTWorkerDisabled(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	compatSeedRepo(t, h, "projection-rest-disabled")
	full := "testuser/projection-rest-disabled"
	branch := "agent/rest-disabled"
	if err := h.Svc.Git.CreateBranch(ctx, full, branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := h.Svc.Git.WriteFile(ctx, full, branch, "large-fixture.bin", "large fixture", []byte("repeatable projection fixture\n"))
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	client := &restPRProjectionClient{headSHA: headSHA}
	pushes := 0
	h.Svc.DisableForgejoProjectionWorker = true
	h.Svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			full: {Owner: "forgejo", Repo: "projection-rest-disabled"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushes++
		return nil
	})

	w := h.DoRESTJSON(t, "POST", fmt.Sprintf("/api/v3/repos/%s/pulls", full), map[string]any{
		"title": "REST disabled projection worker",
		"head":  branch,
		"base":  "main",
	})
	assertStatusCode(t, w, 201)
	var createResponse struct {
		Number        int `json:"number"`
		ProjectionJob *struct {
			AGSPRNumber int    `json:"ags_pr_number"`
			Phase       string `json:"phase"`
			Status      string `json:"status"`
		} `json:"projection_job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createResponse); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if createResponse.Number != 1 || createResponse.ProjectionJob == nil || createResponse.ProjectionJob.AGSPRNumber != 1 ||
		createResponse.ProjectionJob.Phase != service.ForgejoProjectionPhaseQueued || createResponse.ProjectionJob.Status != "queued" {
		t.Fatalf("create response projection job=%#v, want AGS #1 queued", createResponse.ProjectionJob)
	}
	if pushes != 0 || client.requestCount() != 0 {
		t.Fatalf("disabled worker should not push/ensure synchronously, pushes=%d requests=%d", pushes, client.requestCount())
	}
	var job db.PullRequestProjectionJob
	if err := h.DB.Where("provider = ? AND head_sha = ?", service.ProjectionProviderForgejo, headSHA).First(&job).Error; err != nil {
		t.Fatalf("load queued job: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseQueued || job.Attempt != 0 {
		t.Fatalf("job phase/attempt=%s/%d, want queued/0", job.Phase, job.Attempt)
	}
	var projections int64
	if err := h.DB.Model(&db.PullRequestProjection{}).Where("provider = ? AND last_synced_sha = ?", service.ProjectionProviderForgejo, headSHA).Count(&projections).Error; err != nil {
		t.Fatalf("count projections: %v", err)
	}
	if projections != 0 {
		t.Fatalf("worker-disabled request should not create final projection row yet, rows=%d", projections)
	}

	get := h.DoREST(t, "GET", fmt.Sprintf("/api/v3/repos/%s/pulls/1", full), nil)
	assertStatusCode(t, get, 200)
	var getResponse struct {
		ProjectionJob *struct {
			JobID  uint   `json:"job_id"`
			Phase  string `json:"phase"`
			Status string `json:"status"`
		} `json:"projection_job"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &getResponse); err != nil {
		t.Fatalf("decode exact PR response: %v", err)
	}
	if getResponse.ProjectionJob == nil || getResponse.ProjectionJob.JobID != job.ID ||
		getResponse.ProjectionJob.Phase != service.ForgejoProjectionPhaseQueued || getResponse.ProjectionJob.Status != "queued" {
		t.Fatalf("exact PR response projection job=%#v, want queued job %d", getResponse.ProjectionJob, job.ID)
	}
}

func TestProjectionStatusEndpointHidesPrivateRepoFromUnauthorizedViewers(t *testing.T) {
	h := testharness.New(t)
	w := h.DoRESTJSON(t, "POST", "/api/v3/user/repos", map[string]any{
		"name":      "projection-private-status",
		"private":   true,
		"auto_init": true,
	})
	assertStatusCode(t, w, 201)

	anon := h.DoRESTNoAuth(t, "GET", "/api/v3/repos/testuser/projection-private-status/projection/status")
	assertStatusCode(t, anon, 404)

	_, outsiderToken := seedHarnessUser(t, h, "projection-status-outsider", false)
	outsider := h.DoRESTWithToken(t, "GET", "/api/v3/repos/testuser/projection-private-status/projection/status", outsiderToken)
	assertStatusCode(t, outsider, 404)

	owner := h.DoREST(t, "GET", "/api/v3/repos/testuser/projection-private-status/projection/status", nil)
	assertStatusCode(t, owner, 200)
}

func TestProjectionRetryEndpointRequeuesFailedRetryableJob(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	compatSeedRepo(t, h, "projection-retry")
	full := "testuser/projection-retry"
	branch := "agent/rest-retry"
	if err := h.Svc.Git.CreateBranch(ctx, full, branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := h.Svc.Git.WriteFile(ctx, full, branch, "large-fixture.bin", "large fixture", []byte("repeatable projection fixture\n"))
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	h.Svc.DisableForgejoProjectionWorker = true
	h.Svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			full: {Owner: "forgejo", Repo: "projection-retry"},
		},
	}, &restPRProjectionClient{headSHA: headSHA}, nil)

	w := h.DoRESTJSON(t, "POST", fmt.Sprintf("/api/v3/repos/%s/pulls", full), map[string]any{
		"title": "REST projection retry",
		"head":  branch,
		"base":  "main",
	})
	assertStatusCode(t, w, 201)
	var pr db.PullRequest
	if err := h.DB.Where("repository_id = (SELECT id FROM repositories WHERE full_name = ?) AND number = ?", full, 1).First(&pr).Error; err != nil {
		t.Fatalf("load pr: %v", err)
	}
	if err := h.DB.Model(&db.PullRequestProjectionJob{}).Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).Updates(map[string]any{
		"phase":           service.ForgejoProjectionPhaseFailedRetryable,
		"attempt":         1,
		"last_error_type": "unknown_projection_error",
		"last_error":      "git push: signal: killed",
	}).Error; err != nil {
		t.Fatalf("mark retryable: %v", err)
	}

	_, outsiderToken := seedHarnessUser(t, h, "projection-retry-outsider", false)
	unauthorizedRetry := h.DoRESTJSONWithToken(t, "POST", fmt.Sprintf("/api/v3/repos/%s/projection/forgejo/pulls/%d/retry", full, pr.Number), outsiderToken, nil)
	assertStatusCode(t, unauthorizedRetry, 404)
	var job db.PullRequestProjectionJob
	if err := h.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&job).Error; err != nil {
		t.Fatalf("reload job after unauthorized retry: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseFailedRetryable || job.LastErrorType == "" || job.LastError == "" {
		t.Fatalf("unauthorized retry mutated job: %#v", job)
	}

	retry := h.DoRESTJSON(t, "POST", fmt.Sprintf("/api/v3/repos/%s/projection/forgejo/pulls/%d/retry", full, pr.Number), nil)
	assertStatusCode(t, retry, 202)
	if err := h.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&job).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseQueued || job.LastErrorType != "" || job.LastError != "" || job.NextRunAt == nil {
		t.Fatalf("retry endpoint did not requeue cleanly: %#v", job)
	}
}
