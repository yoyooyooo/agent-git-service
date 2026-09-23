package service_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestDispatchPullRequestIntegrationsReturnsErrorAndLeavesPartialStateAfterForgejoBranchPush(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "partial-proj", Name: "partial-proj", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "partial-proj", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/partial-large-binary"
	if err := svc.Git.CreateBranch(ctx, "partial-proj/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, "partial-proj/repo", branch, "large.bin", "large fixture", []byte("large binary fixture\n"))
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "partial-proj/repo", Title: "partial projection", HeadRef: branch, BaseRef: "main", AuthorLogin: "partial-proj"})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	remoteRefs := map[string]string{}
	client := &projectionSyncForgejoClient{headSHA: headSHA}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"partial-proj/repo": {Owner: "forgejo", Repo: "repo"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		remoteRefs["refs/heads/"+req.BranchName] = headSHA
		return errors.New("git push: signal: killed\nunknown_projection_error refs/heads/agent/partial-large-binary")
	})
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	dispatch, dispatchErr := svc.DispatchPullRequestIntegrations(canceledCtx, pr)
	if dispatchErr == nil || !strings.Contains(dispatchErr.Error(), "ensure Forgejo PR") {
		t.Fatalf("DispatchPullRequestIntegrations must surface the partial projection failure, got %v", dispatchErr)
	}
	if !dispatch.Forgejo.Required || dispatch.Forgejo.Projected || dispatch.Forgejo.Err == nil {
		t.Fatalf("Forgejo provider outcome=%#v", dispatch.Forgejo)
	}
	if got := remoteRefs["refs/heads/"+branch]; got != headSHA {
		t.Fatalf("simulated Forgejo branch SHA after killed push=%q, want AGS head %q", got, headSHA)
	}
	if len(client.requests) != 0 {
		t.Fatalf("canceled request context unexpectedly ensured Forgejo PR: %#v", client.requests)
	}
	var projections int64
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("provider = ? AND pull_request_id = ?", service.ProjectionProviderForgejo, pr.ID).Count(&projections).Error; err != nil {
		t.Fatalf("count projections: %v", err)
	}
	if projections != 0 {
		t.Fatalf("partial projection should leave AGS projection row absent, rows=%d", projections)
	}
}

func TestDispatchPullRequestIntegrationsKeepsOptionalGitLabFailureSeparate(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "provider-policy", Name: "provider-policy", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "provider-policy", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/provider-policy"
	if err := svc.Git.CreateBranch(ctx, "provider-policy/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, "provider-policy/repo", branch, "feature.txt", "feature", []byte("feature\n"))
	if err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "provider-policy/repo", Title: "provider policy", HeadRef: branch, BaseRef: "main", AuthorLogin: "provider-policy"})
	if err != nil {
		t.Fatalf("create PR: %v", err)
	}

	forgejoClient := &projectionSyncForgejoClient{headSHA: headSHA}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"provider-policy/repo": {Owner: "forgejo", Repo: "repo"}},
	}, forgejoClient, func(ctx context.Context, req forgejointegration.PushRequest) error { return nil })
	gitLabFailure := errors.New("GitLab shadow transport unavailable")
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled: true, BaseURL: "http://gitlab.local", Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]gitlabintegration.RepoMapping{"provider-policy/repo": {ProjectPath: "backup/repo"}},
	}, func(ctx context.Context, repoPath, remoteURL, refspec, token string) error {
		return gitLabFailure
	}, &projectionCloseGitLabClient{})

	result, dispatchErr := svc.DispatchPullRequestIntegrationsWithPolicy(ctx, pr, service.PullRequestIntegrationPolicy{RequireForgejo: true})
	if dispatchErr != nil {
		t.Fatalf("optional GitLab failure must not fail required Forgejo action: %v", dispatchErr)
	}
	if !result.Forgejo.Required || !result.Forgejo.Projected || result.Forgejo.Err != nil {
		t.Fatalf("Forgejo outcome=%#v", result.Forgejo)
	}
	if result.GitLab.Required || result.GitLab.Projected || !errors.Is(result.GitLab.Err, gitLabFailure) {
		t.Fatalf("GitLab outcome=%#v", result.GitLab)
	}
}

func TestDispatchPullRequestIntegrationsKeepsOptionalGitHubFailureSeparate(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "github-policy", Name: "github-policy", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "github-policy", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/github-policy"
	if err := svc.Git.CreateBranch(ctx, "github-policy/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, "github-policy/repo", branch, "feature.txt", "feature", []byte("feature\n"))
	if err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "github-policy/repo", Title: "provider policy", HeadRef: branch, BaseRef: "main", AuthorLogin: "github-policy"})
	if err != nil {
		t.Fatalf("create PR: %v", err)
	}

	forgejoClient := &projectionSyncForgejoClient{headSHA: headSHA}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"github-policy/repo": {Owner: "forgejo", Repo: "repo"}},
	}, forgejoClient, func(ctx context.Context, req forgejointegration.PushRequest) error { return nil })
	gitHubFailure := errors.New("GitHub shadow transport unavailable")
	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled: true, Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]githubintegration.RepoMapping{"github-policy/repo": {Owner: "example-org", Repo: "project-kit"}},
	}, func(ctx context.Context, repoPath, remoteURL, refspec, token string) error {
		return gitHubFailure
	}, &projectionCloseGitHubClient{})

	result, dispatchErr := svc.DispatchPullRequestIntegrationsWithPolicy(ctx, pr, service.PullRequestIntegrationPolicy{RequireForgejo: true})
	if dispatchErr != nil {
		t.Fatalf("optional GitHub failure must not fail required Forgejo action: %v", dispatchErr)
	}
	if !result.Forgejo.Required || !result.Forgejo.Projected || result.Forgejo.Err != nil {
		t.Fatalf("Forgejo outcome=%#v", result.Forgejo)
	}
	if result.GitHub.Required || result.GitHub.Projected || !errors.Is(result.GitHub.Err, gitHubFailure) {
		t.Fatalf("GitHub outcome=%#v", result.GitHub)
	}
}

func TestEnqueuedForgejoProjectionUsesWorkerContextAfterRequestCancel(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "async-proj", Name: "async-proj", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "async-proj", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/async-projection"
	if err := svc.Git.CreateBranch(ctx, "async-proj/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, "async-proj/repo", branch, "large.bin", "large fixture", []byte("large binary fixture\n"))
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "async-proj/repo", Title: "async projection", HeadRef: branch, BaseRef: "main", AuthorLogin: "async-proj"})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	client := &projectionSyncForgejoClient{headSHA: headSHA}
	pushCtxErr := make(chan error, 1)
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"async-proj/repo": {Owner: "forgejo", Repo: "repo"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushCtxErr <- ctx.Err()
		return nil
	})
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := svc.EnqueuePullRequestCreatedIntegrations(canceledCtx, pr); err != nil {
		t.Fatalf("enqueue integrations with canceled request context: %v", err)
	}
	svc.Wg.Wait()
	select {
	case err := <-pushCtxErr:
		if err != nil {
			t.Fatalf("worker git push used canceled request context: %v", err)
		}
	default:
		t.Fatal("worker did not invoke Forgejo git push")
	}
	if len(client.requests) != 1 {
		t.Fatalf("worker did not ensure Forgejo PR, requests=%#v", client.requests)
	}
	var projection db.PullRequestProjection
	if err := svc.DB.Where("provider = ? AND pull_request_id = ?", service.ProjectionProviderForgejo, pr.ID).First(&projection).Error; err != nil {
		t.Fatalf("load projection: %v", err)
	}
	if projection.LastSyncedSHA != headSHA || projection.ExternalNumber != 42 {
		t.Fatalf("projection=%#v, want head %s external #42", projection, headSHA)
	}
	var job db.PullRequestProjectionJob
	if err := svc.DB.Where("provider = ? AND pull_request_id = ?", service.ProjectionProviderForgejo, pr.ID).First(&job).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseProjected || job.Attempt != 1 {
		t.Fatalf("job phase/attempt=%s/%d, want projected/1", job.Phase, job.Attempt)
	}
}

func TestLargeBinaryForgejoProjectionUsesWorkerTimeoutAndPushTuning(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "large-proj", Name: "large-proj", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "large-proj", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/large-binary-projection"
	if err := svc.Git.CreateBranch(ctx, "large-proj/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pngLike := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x89, 0x50, 0x4e, 0x47}, 256*1024)...)
	headSHA, err := svc.Git.WriteFile(ctx, "large-proj/repo", branch, "ppt-images/deck.png", "large PNG-like fixture", pngLike)
	if err != nil {
		t.Fatalf("write large fixture: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "large-proj/repo", Title: "large binary projection", HeadRef: branch, BaseRef: "main", AuthorLogin: "large-proj"})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	client := &projectionSyncForgejoClient{headSHA: headSHA}
	pushCtxErr := make(chan error, 1)
	pushHasDeadline := make(chan bool, 1)
	pushReq := make(chan forgejointegration.PushRequest, 1)
	svc.ForgejoProjectionWorkerTimeout = 5 * time.Second
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		PushGitConfig:        []string{"core.compression=0", "pack.window=0"},
		PushTimeout:          3 * time.Minute,
		RepoMap: map[string]forgejointegration.RepoMapping{
			"large-proj/repo": {Owner: "forgejo", Repo: "repo"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		_, ok := ctx.Deadline()
		pushHasDeadline <- ok
		pushCtxErr <- ctx.Err()
		pushReq <- req
		return nil
	})
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := svc.EnqueuePullRequestCreatedIntegrations(canceledCtx, pr); err != nil {
		t.Fatalf("enqueue integrations with canceled request context: %v", err)
	}
	svc.Wg.Wait()
	select {
	case err := <-pushCtxErr:
		if err != nil {
			t.Fatalf("worker git push used canceled request context: %v", err)
		}
	default:
		t.Fatal("worker did not invoke Forgejo git push")
	}
	if ok := <-pushHasDeadline; !ok {
		t.Fatal("worker git push context had no worker-owned deadline")
	}
	gotReq := <-pushReq
	if gotReq.Timeout != 3*time.Minute {
		t.Fatalf("push timeout=%s, want 3m", gotReq.Timeout)
	}
	if len(gotReq.GitConfig) != 2 || gotReq.GitConfig[0] != "core.compression=0" || gotReq.GitConfig[1] != "pack.window=0" {
		t.Fatalf("push git config=%#v", gotReq.GitConfig)
	}
	var job db.PullRequestProjectionJob
	if err := svc.DB.Where("provider = ? AND pull_request_id = ?", service.ProjectionProviderForgejo, pr.ID).First(&job).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseProjected || job.Attempt != 1 {
		t.Fatalf("large binary job phase/attempt=%s/%d, want projected/1", job.Phase, job.Attempt)
	}
}

func TestForgejoProjectionJobCanResumeFromQueuedDBState(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "resume-proj", Name: "resume-proj", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "resume-proj", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/resume-projection"
	if err := svc.Git.CreateBranch(ctx, "resume-proj/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, "resume-proj/repo", branch, "large.bin", "large fixture", []byte("large binary fixture\n"))
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "resume-proj/repo", Title: "resume projection", HeadRef: branch, BaseRef: "main", AuthorLogin: "resume-proj"})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	client := &projectionSyncForgejoClient{headSHA: headSHA}
	pushes := 0
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"resume-proj/repo": {Owner: "forgejo", Repo: "repo"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushes++
		return nil
	})
	svc.DisableForgejoProjectionWorker = true
	job, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr)
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseQueued {
		t.Fatalf("queued job phase=%s, want queued", job.Phase)
	}
	if pushes != 0 {
		t.Fatalf("disabled worker still pushed %d times", pushes)
	}
	svc.DisableForgejoProjectionWorker = false
	if err := svc.ResumePendingForgejoProjectionJobs(ctx); err != nil {
		t.Fatalf("resume jobs: %v", err)
	}
	svc.Wg.Wait()
	if pushes != 1 || len(client.requests) != 1 {
		t.Fatalf("resume did not run one projection, pushes=%d requests=%#v", pushes, client.requests)
	}
	var projected db.PullRequestProjectionJob
	if err := svc.DB.First(&projected, job.ID).Error; err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if projected.Phase != service.ForgejoProjectionPhaseProjected || projected.Attempt != 1 {
		t.Fatalf("resumed job phase/attempt=%s/%d, want projected/1", projected.Phase, projected.Attempt)
	}
}

func TestDuplicateForgejoProjectionEnqueuesConvergeToOneProjection(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "dupe-proj", Name: "dupe-proj", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "dupe-proj", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/duplicate-projection"
	if err := svc.Git.CreateBranch(ctx, "dupe-proj/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, "dupe-proj/repo", branch, "large.bin", "large fixture", []byte("large binary fixture\n"))
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "dupe-proj/repo", Title: "duplicate projection", HeadRef: branch, BaseRef: "main", AuthorLogin: "dupe-proj"})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	client := &projectionSyncForgejoClient{headSHA: headSHA}
	firstPushStarted := make(chan struct{})
	allowPushReturn := make(chan struct{})
	releasedPush := false
	releasePush := func() {
		if !releasedPush {
			close(allowPushReturn)
			releasedPush = true
		}
	}
	defer releasePush()
	var pushCount atomic.Int32
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"dupe-proj/repo": {Owner: "forgejo", Repo: "repo"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		if pushCount.Add(1) == 1 {
			close(firstPushStarted)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-allowPushReturn:
			return nil
		}
	})

	firstJob, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	select {
	case <-firstPushStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first projection worker did not start git push")
	}
	secondJob, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr)
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if firstJob.ID != secondJob.ID {
		t.Fatalf("duplicate enqueue created different jobs: %d vs %d", firstJob.ID, secondJob.ID)
	}
	releasePush()
	svc.Wg.Wait()

	var jobRows int64
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("provider = ? AND pull_request_id = ?", service.ProjectionProviderForgejo, pr.ID).Count(&jobRows).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobRows != 1 {
		t.Fatalf("duplicate enqueue should converge to one durable job row, got %d", jobRows)
	}
	var projectionRows int64
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("provider = ? AND pull_request_id = ?", service.ProjectionProviderForgejo, pr.ID).Count(&projectionRows).Error; err != nil {
		t.Fatalf("count projections: %v", err)
	}
	if projectionRows != 1 {
		t.Fatalf("duplicate enqueue should converge to one projection row, got %d", projectionRows)
	}
	if got := pushCount.Load(); got != 1 {
		t.Fatalf("duplicate in-process workers should coalesce one push, got %d", got)
	}
	if len(client.requests) != 1 {
		t.Fatalf("duplicate jobs should ensure one Forgejo PR, requests=%#v", client.requests)
	}
}

func TestForgejoProjectionJobRecordsMissingProjectionRowForExistingForgejoPR(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "existing-pr", Name: "existing-pr", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "existing-pr", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/existing-pr-projection"
	if err := svc.Git.CreateBranch(ctx, "existing-pr/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	headSHA, err := svc.Git.WriteFile(ctx, "existing-pr/repo", branch, "large.bin", "large fixture", []byte("large binary fixture\n"))
	if err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "existing-pr/repo", Title: "existing PR projection", HeadRef: branch, BaseRef: "main", AuthorLogin: "existing-pr"})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	client := &projectionSyncForgejoClient{headSHA: headSHA}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"existing-pr/repo": {Owner: "forgejo", Repo: "repo"},
		},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error { return nil })

	if _, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr); err != nil {
		t.Fatalf("enqueue projection: %v", err)
	}
	svc.Wg.Wait()
	var projection db.PullRequestProjection
	if err := svc.DB.Where("provider = ? AND pull_request_id = ?", service.ProjectionProviderForgejo, pr.ID).First(&projection).Error; err != nil {
		t.Fatalf("load projection: %v", err)
	}
	if projection.ExternalRepo != "forgejo/repo" || projection.ExternalNumber != 42 || projection.LastSyncedSHA != headSHA {
		t.Fatalf("projection=%#v, want existing Forgejo PR #42 recorded at head %s", projection, headSHA)
	}
}

func TestForgejoProjectionJobClassifiesRetryableAndTerminalFailures(t *testing.T) {
	cases := []struct {
		name          string
		pushErr       string
		wantPhase     string
		wantErrorType string
	}{
		{
			name:          "retryable killed push",
			pushErr:       "git push: signal: killed\nunknown_projection_error refs/heads/agent/retryable-projection",
			wantPhase:     service.ForgejoProjectionPhaseFailedRetryable,
			wantErrorType: forgejointegration.ProjectionFailureUnknown,
		},
		{
			name:          "terminal non fast forward",
			pushErr:       "updates were rejected because the remote contains work that you do not have locally (non-fast-forward)",
			wantPhase:     service.ForgejoProjectionPhaseFailedTerminal,
			wantErrorType: forgejointegration.ProjectionFailureNonFastForward,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, cleanup := setupTestService(t)
			defer cleanup()
			ctx := context.Background()
			if err := svc.DB.Create(&db.User{Login: "fail-proj", Name: "fail-proj", Type: db.TypeUser}).Error; err != nil {
				t.Fatalf("create user: %v", err)
			}
			_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "fail-proj", Name: "repo", DefaultBranch: "main", AddReadme: true})
			if err != nil {
				t.Fatalf("create repo: %v", err)
			}
			branch := "agent/failure-projection"
			if err := svc.Git.CreateBranch(ctx, "fail-proj/repo", branch, "main"); err != nil {
				t.Fatalf("create branch: %v", err)
			}
			headSHA, err := svc.Git.WriteFile(ctx, "fail-proj/repo", branch, "large.bin", "large fixture", []byte("large binary fixture\n"))
			if err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "fail-proj/repo", Title: "failure projection", HeadRef: branch, BaseRef: "main", AuthorLogin: "fail-proj"})
			if err != nil {
				t.Fatalf("CreatePR: %v", err)
			}
			svc.ForgejoProjectionWorkerMaxAttempts = 1
			// This test classifies an injected push failure, not a worker
			// deadline. Real Git/SQLite preflight must reach that seam even
			// when other packages are running concurrently under go test ./....
			svc.ForgejoProjectionWorkerTimeout = 5 * time.Second
			var pushCalls atomic.Int32
			svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
				Enabled:              true,
				BaseURL:              "http://forgejo.local",
				Token:                "secret-token",
				AutoPullRequest:      true,
				MirrorBranchIncludes: []string{"agent/*"},
				PRBranchIncludes:     []string{"agent/*"},
				RepoMap: map[string]forgejointegration.RepoMapping{
					"fail-proj/repo": {Owner: "forgejo", Repo: "repo"},
				},
			}, &projectionSyncForgejoClient{headSHA: headSHA}, func(ctx context.Context, req forgejointegration.PushRequest) error {
				pushCalls.Add(1)
				return errors.New(tc.pushErr)
			})
			job, err := svc.EnqueueForgejoPullRequestProjection(ctx, pr)
			if err != nil {
				t.Fatalf("enqueue job: %v", err)
			}
			svc.Wg.Wait()
			if err := svc.DB.First(&job, job.ID).Error; err != nil {
				t.Fatalf("reload job: %v", err)
			}
			if got := pushCalls.Load(); got != 1 {
				t.Fatalf("push calls=%d, want exactly one injected failure", got)
			}
			if job.Phase != tc.wantPhase || job.LastErrorType != tc.wantErrorType || job.Attempt != 1 {
				t.Fatalf("job phase/type/attempt=%s/%s/%d, want %s/%s/1; error=%q", job.Phase, job.LastErrorType, job.Attempt, tc.wantPhase, tc.wantErrorType, job.LastError)
			}
		})
	}
}
