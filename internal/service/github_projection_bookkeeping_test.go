package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
)

type bookkeepingGitHubClient struct {
	handled     bool
	err         error
	pr          githubintegration.PullRequest
	calls       int
	ensureCalls int
}

func (c *bookkeepingGitHubClient) EnsurePullRequest(context.Context, string, githubintegration.PullRequestInput) (githubintegration.PullRequest, error) {
	c.ensureCalls++
	return githubintegration.PullRequest{Number: 8, HTMLURL: "https://github.com/backup/demo/pull/8"}, nil
}

func (c *bookkeepingGitHubClient) ClosePullRequestForBranch(context.Context, string, string, string, string) (githubintegration.PullRequest, bool, error) {
	c.calls++
	return c.pr, c.handled, c.err
}

func TestDispatchGitHubIntegrationAdvancesOpenGitHubPRLastSyncedSHA(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pushedSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 88)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "backup/demo", 6, ProjectionStateOpen, createSHA)
	seedProjection(t, svc, pr, ProjectionProviderForgejo, "forgejo/demo", 42, ProjectionStateOpen, createSHA)
	seedProjection(t, svc, pr, ProjectionProviderGitLab, "backup/demo", 12, ProjectionStateOpen, createSHA)

	client := &bookkeepingGitHubClient{}
	pushes := 0
	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled: true, Token: "token", MergeAuthority: "forgejo",
		MirrorBranchIncludes: []string{"example-team/*"},
		Repos:                map[string]githubintegration.RepoMapping{repo.FullName: {Owner: "backup", Repo: "demo"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	}, client)

	if err := svc.DispatchGitHubIntegration(context.Background(), repo.FullName, "/tmp/demo.git", []ForgejoRefChange{{
		Ref: "refs/heads/example-team/app-fixture", After: pushedSHA,
	}}); err != nil {
		t.Fatalf("DispatchGitHubIntegration: %v", err)
	}
	if pushes != 1 {
		t.Fatalf("pushes=%d, want 1", pushes)
	}
	if client.ensureCalls != 0 {
		t.Fatalf("EnsurePullRequest calls=%d, want 0: last_synced_sha must advance from PushRef, not EnsureShadowPR", client.ensureCalls)
	}
	gitHub := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	if gitHub.State != ProjectionStateOpen || gitHub.LastSyncedSHA != pushedSHA {
		t.Fatalf("GitHub projection after PushRef success stayed stale: %#v want sha=%s", gitHub, pushedSHA)
	}
	forgejo := loadProjection(t, svc, pr.ID, ProjectionProviderForgejo)
	if forgejo.State != ProjectionStateOpen || forgejo.LastSyncedSHA != createSHA {
		t.Fatalf("Forgejo projection must stay at create SHA: %#v", forgejo)
	}
	gitLab := loadProjection(t, svc, pr.ID, ProjectionProviderGitLab)
	if gitLab.State != ProjectionStateOpen || gitLab.LastSyncedSHA != createSHA {
		t.Fatalf("GitLab projection must stay at create SHA: %#v", gitLab)
	}
}

func TestDispatchGitHubIntegrationDoesNotAdvanceClosedOrUnrelatedProjections(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pushedSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	repo, openPR := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 86)
	_, closedPR := seedOpenHeadPR(t, svc, "example-team/other", createSHA, 87)
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", closedPR.ID).Updates(map[string]any{"state": db.StateClosed, "merged": true}).Error; err != nil {
		t.Fatalf("close other PR: %v", err)
	}
	seedProjection(t, svc, openPR, ProjectionProviderGitHub, "backup/demo", 6, ProjectionStateClosed, createSHA)
	seedProjection(t, svc, closedPR, ProjectionProviderGitHub, "backup/demo", 7, ProjectionStateOpen, createSHA)

	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled: true, Token: "token", MergeAuthority: "forgejo",
		MirrorBranchIncludes: []string{"example-team/*"},
		Repos:                map[string]githubintegration.RepoMapping{repo.FullName: {Owner: "backup", Repo: "demo"}},
	}, func(context.Context, string, string, string, string) error { return nil }, &bookkeepingGitHubClient{})

	if err := svc.DispatchGitHubIntegration(context.Background(), repo.FullName, "/tmp/demo.git", []ForgejoRefChange{{
		Ref: "refs/heads/example-team/app-fixture", After: pushedSHA,
	}}); err != nil {
		t.Fatalf("DispatchGitHubIntegration: %v", err)
	}
	openRow := loadProjection(t, svc, openPR.ID, ProjectionProviderGitHub)
	if openRow.State != ProjectionStateClosed || openRow.LastSyncedSHA != createSHA {
		t.Fatalf("closed GitHub projection must not advance: %#v", openRow)
	}
	closedHead := loadProjection(t, svc, closedPR.ID, ProjectionProviderGitHub)
	if closedHead.LastSyncedSHA != createSHA {
		t.Fatalf("unrelated PR GitHub projection advanced: %#v", closedHead)
	}
}

func TestDispatchGitHubIntegrationPushFailureDoesNotAdvanceLastSyncedSHA(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 88)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "backup/demo", 6, ProjectionStateOpen, createSHA)
	pushErr := errors.New("git push: github unavailable")
	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled: true, Token: "token", MergeAuthority: "forgejo",
		MirrorBranchIncludes: []string{"example-team/*"},
		Repos:                map[string]githubintegration.RepoMapping{repo.FullName: {Owner: "backup", Repo: "demo"}},
	}, func(context.Context, string, string, string, string) error { return pushErr }, &bookkeepingGitHubClient{})

	err := svc.DispatchGitHubIntegration(context.Background(), repo.FullName, "/tmp/demo.git", []ForgejoRefChange{{
		Ref: "refs/heads/example-team/app-fixture", After: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}})
	if err == nil || !strings.Contains(err.Error(), "github unavailable") {
		t.Fatalf("dispatch error=%v", err)
	}
	row := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	if row.LastSyncedSHA != createSHA || row.State != ProjectionStateOpen {
		t.Fatalf("failed PushRef must not rewrite GitHub last_synced_sha: %#v", row)
	}
}

func TestDispatchGitLabIntegrationDoesNotAdvanceGitHubOpenPRLastSyncedSHA(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pushedSHA := "cccccccccccccccccccccccccccccccccccccccc"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 85)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "backup/demo", 6, ProjectionStateOpen, createSHA)
	seedProjection(t, svc, pr, ProjectionProviderGitLab, "backup/demo", 12, ProjectionStateOpen, createSHA)
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled: true, BaseURL: "http://gitlab.local", Token: "token", MergeAuthority: "forgejo",
		MirrorBranchIncludes: []string{"example-team/*"},
		Repos:                map[string]gitlabintegration.RepoMapping{repo.FullName: {ProjectPath: "backup/demo"}},
	}, func(context.Context, string, string, string, string) error { return nil }, &bookkeepingGitLabClient{})

	if err := svc.DispatchGitLabIntegration(context.Background(), repo.FullName, "/tmp/demo.git", []ForgejoRefChange{{
		Ref: "refs/heads/example-team/app-fixture", After: pushedSHA,
	}}); err != nil {
		t.Fatalf("DispatchGitLabIntegration: %v", err)
	}
	gitLab := loadProjection(t, svc, pr.ID, ProjectionProviderGitLab)
	if gitLab.LastSyncedSHA != pushedSHA || gitLab.State != ProjectionStateOpen {
		t.Fatalf("GitLab projection after optional push: %#v", gitLab)
	}
	gitHub := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	if gitHub.LastSyncedSHA != createSHA || gitHub.State != ProjectionStateOpen {
		t.Fatalf("GitLab dispatch must not rewrite GitHub last_synced_sha: %#v", gitHub)
	}
}

func TestCloseGitHubShadowPullRequestRecordsProjectionClosed(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mergedSHA := "dddddddddddddddddddddddddddddddddddddddd"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 88)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "backup/demo", 6, ProjectionStateOpen, createSHA)
	seedProjection(t, svc, pr, ProjectionProviderForgejo, "forgejo/demo", 42, ProjectionStateOpen, createSHA)
	client := &bookkeepingGitHubClient{
		handled: true,
		pr:      githubintegration.PullRequest{Number: 6, HTMLURL: "https://github.com/backup/demo/pull/6"},
	}
	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled: true, Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]githubintegration.RepoMapping{repo.FullName: {Owner: "backup", Repo: "demo", TargetBranch: "main"}},
	}, func(context.Context, string, string, string, string) error {
		t.Fatal("shadow close must not merge or push GitHub")
		return nil
	}, client)

	result, handled, err := svc.closeGitHubShadowPullRequest(context.Background(), forgejointegration.MergedPullRequestEvent{
		RepoFullName: repo.FullName, PRNumber: 42, HeadBranch: pr.HeadRef, BaseBranch: pr.BaseRef,
	}, mergedSHA, pr)
	if err != nil || !handled || client.calls != 1 {
		t.Fatalf("close GitHub shadow: handled=%v err=%v calls=%d result=%#v", handled, err, client.calls, result)
	}
	gitHub := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	if gitHub.State != ProjectionStateClosed || gitHub.State == ProjectionStateMerged || gitHub.LastSyncedSHA != mergedSHA || gitHub.ExternalNumber != 6 {
		t.Fatalf("GitHub projection after shadow close: %#v", gitHub)
	}
	forgejo := loadProjection(t, svc, pr.ID, ProjectionProviderForgejo)
	if forgejo.State != ProjectionStateOpen || forgejo.LastSyncedSHA != createSHA {
		t.Fatalf("Forgejo required projection must be unchanged by GitHub shadow close: %#v", forgejo)
	}
}

func TestCloseGitHubShadowPullRequestRecordsClosedWhenAlreadyMergedByAncestry(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mergedSHA := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 86)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "backup/demo", 9, ProjectionStateOpen, createSHA)
	client := &bookkeepingGitHubClient{handled: false}
	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled: true, Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]githubintegration.RepoMapping{repo.FullName: {Owner: "backup", Repo: "demo"}},
	}, nil, client)

	result, handled, err := svc.closeGitHubShadowPullRequest(context.Background(), forgejointegration.MergedPullRequestEvent{
		RepoFullName: repo.FullName, PRNumber: 41, HeadBranch: pr.HeadRef, BaseBranch: pr.BaseRef,
	}, mergedSHA, pr)
	if err != nil || handled || client.calls != 1 {
		t.Fatalf("ancestry-merged close: handled=%v err=%v calls=%d result=%#v", handled, err, client.calls, result)
	}
	gitHub := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	if gitHub.State != ProjectionStateClosed || gitHub.State == ProjectionStateMerged || gitHub.LastSyncedSHA != mergedSHA || gitHub.ExternalNumber != 9 {
		t.Fatalf("ancestry-merged GitHub row must be AGS-closed with merged SHA: %#v", gitHub)
	}
}

func TestCloseGitHubShadowPullRequestFailureDoesNotRewriteProjection(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 88)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "backup/demo", 6, ProjectionStateOpen, createSHA)
	closeErr := errors.New("github close transport failed")
	client := &bookkeepingGitHubClient{err: closeErr}
	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled: true, Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]githubintegration.RepoMapping{repo.FullName: {Owner: "backup", Repo: "demo"}},
	}, nil, client)

	_, handled, err := svc.closeGitHubShadowPullRequest(context.Background(), forgejointegration.MergedPullRequestEvent{
		RepoFullName: repo.FullName, PRNumber: 42, HeadBranch: pr.HeadRef, BaseBranch: pr.BaseRef,
	}, "ffffffffffffffffffffffffffffffffffffffff", pr)
	if err == nil || !errors.Is(err, closeErr) || handled {
		t.Fatalf("expected close error, handled=%v err=%v", handled, err)
	}
	gitHub := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	if gitHub.State != ProjectionStateOpen || gitHub.LastSyncedSHA != createSHA {
		t.Fatalf("failed close must not upsert GitHub projection: %#v", gitHub)
	}
}
