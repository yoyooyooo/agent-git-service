package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
)

type bookkeepingGitLabClient struct {
	handled bool
	err     error
	mr      gitlabintegration.MergeRequest
	calls   int
}

func (c *bookkeepingGitLabClient) EnsureMergeRequest(context.Context, string, gitlabintegration.MergeRequestInput) (gitlabintegration.MergeRequest, error) {
	return gitlabintegration.MergeRequest{IID: 7, WebURL: "http://gitlab.local/mr/7"}, nil
}

func (c *bookkeepingGitLabClient) CloseMergeRequestForBranch(context.Context, string, string, string, string) (gitlabintegration.MergeRequest, bool, error) {
	c.calls++
	return c.mr, c.handled, c.err
}

func seedOpenHeadPR(t *testing.T, svc *Service, headRef, headSHA string, number int) (db.Repository, db.PullRequest) {
	t.Helper()
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	pr := db.PullRequest{
		Number: number, RepositoryID: repo.ID, HeadRepositoryID: repo.ID, Repository: repo,
		Title: "gitlab bookkeeping", State: db.StateOpen, HeadRef: headRef, HeadSHA: headSHA, BaseRef: "main",
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create PR: %v", err)
	}
	return repo, pr
}

func seedProjection(t *testing.T, svc *Service, pr db.PullRequest, provider, externalRepo string, externalNumber int, state, sha string) {
	t.Helper()
	if err := svc.DB.Create(&db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: provider,
		ExternalRepo: externalRepo, ExternalNumber: externalNumber, ExternalURL: "http://example.local/" + provider,
		SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: state, LastSyncedSHA: sha,
	}).Error; err != nil {
		t.Fatalf("create %s projection: %v", provider, err)
	}
}

func loadProjection(t *testing.T, svc *Service, prID uint, provider string) db.PullRequestProjection {
	t.Helper()
	var row db.PullRequestProjection
	if err := svc.DB.Where("pull_request_id = ? AND provider = ?", prID, provider).First(&row).Error; err != nil {
		t.Fatalf("load %s projection: %v", provider, err)
	}
	return row
}

func TestDispatchGitLabIntegrationAdvancesOpenGitLabPRLastSyncedSHA(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pushedSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 99)
	seedProjection(t, svc, pr, ProjectionProviderGitLab, "backup/demo", 12, ProjectionStateOpen, createSHA)
	seedProjection(t, svc, pr, ProjectionProviderForgejo, "forgejo/demo", 42, ProjectionStateOpen, createSHA)

	pushes := 0
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled: true, BaseURL: "http://gitlab.local", Token: "token", MergeAuthority: "forgejo",
		MirrorBranchIncludes: []string{"example-team/*"},
		Repos:                map[string]gitlabintegration.RepoMapping{repo.FullName: {ProjectPath: "backup/demo"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	}, &bookkeepingGitLabClient{})

	if err := svc.DispatchGitLabIntegration(context.Background(), repo.FullName, "/tmp/demo.git", []ForgejoRefChange{{
		Ref: "refs/heads/example-team/app-fixture", After: pushedSHA,
	}}); err != nil {
		t.Fatalf("DispatchGitLabIntegration: %v", err)
	}
	if pushes != 1 {
		t.Fatalf("pushes=%d, want 1", pushes)
	}
	gitLab := loadProjection(t, svc, pr.ID, ProjectionProviderGitLab)
	if gitLab.State != ProjectionStateOpen || gitLab.LastSyncedSHA != pushedSHA {
		t.Fatalf("GitLab projection after push: %#v", gitLab)
	}
	forgejo := loadProjection(t, svc, pr.ID, ProjectionProviderForgejo)
	if forgejo.State != ProjectionStateOpen || forgejo.LastSyncedSHA != createSHA {
		t.Fatalf("Forgejo projection must stay at create SHA: %#v", forgejo)
	}
}

func TestDispatchGitLabIntegrationDoesNotAdvanceClosedOrUnrelatedProjections(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pushedSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	repo, openPR := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 97)
	_, closedPR := seedOpenHeadPR(t, svc, "example-team/other", createSHA, 98)
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", closedPR.ID).Updates(map[string]any{"state": db.StateClosed, "merged": true}).Error; err != nil {
		t.Fatalf("close other PR: %v", err)
	}
	seedProjection(t, svc, openPR, ProjectionProviderGitLab, "backup/demo", 12, ProjectionStateClosed, createSHA)
	seedProjection(t, svc, closedPR, ProjectionProviderGitLab, "backup/demo", 13, ProjectionStateOpen, createSHA)

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
	openRow := loadProjection(t, svc, openPR.ID, ProjectionProviderGitLab)
	if openRow.State != ProjectionStateClosed || openRow.LastSyncedSHA != createSHA {
		t.Fatalf("closed GitLab projection must not advance: %#v", openRow)
	}
	closedHead := loadProjection(t, svc, closedPR.ID, ProjectionProviderGitLab)
	if closedHead.LastSyncedSHA != createSHA {
		t.Fatalf("unrelated PR GitLab projection advanced: %#v", closedHead)
	}
}

func TestDispatchGitLabIntegrationPushFailureDoesNotAdvanceLastSyncedSHA(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 99)
	seedProjection(t, svc, pr, ProjectionProviderGitLab, "backup/demo", 12, ProjectionStateOpen, createSHA)
	pushErr := errors.New("git push: gitlab unavailable")
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled: true, BaseURL: "http://gitlab.local", Token: "token", MergeAuthority: "forgejo",
		MirrorBranchIncludes: []string{"example-team/*"},
		Repos:                map[string]gitlabintegration.RepoMapping{repo.FullName: {ProjectPath: "backup/demo"}},
	}, func(context.Context, string, string, string, string) error { return pushErr }, &bookkeepingGitLabClient{})

	err := svc.DispatchGitLabIntegration(context.Background(), repo.FullName, "/tmp/demo.git", []ForgejoRefChange{{
		Ref: "refs/heads/example-team/app-fixture", After: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}})
	if err == nil || !strings.Contains(err.Error(), "gitlab unavailable") {
		t.Fatalf("dispatch error=%v", err)
	}
	row := loadProjection(t, svc, pr.ID, ProjectionProviderGitLab)
	if row.LastSyncedSHA != createSHA || row.State != ProjectionStateOpen {
		t.Fatalf("failed PushRef must not rewrite GitLab last_synced_sha: %#v", row)
	}
}

func TestDispatchForgejoIntegrationStillAdvancesOnlyForgejoOpenPRLastSyncedSHA(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pushedSHA := "cccccccccccccccccccccccccccccccccccccccc"
	repo, pr := seedOpenHeadPR(t, svc, "agent/demo", createSHA, 7)
	seedProjection(t, svc, pr, ProjectionProviderForgejo, "forgejo/demo", 42, ProjectionStateOpen, createSHA)
	seedProjection(t, svc, pr, ProjectionProviderGitLab, "backup/demo", 12, ProjectionStateOpen, createSHA)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "backup/demo", 6, ProjectionStateOpen, createSHA)
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "token", DefaultOwner: "example-owner",
		MirrorBranchIncludes: []string{"agent/*"},
	}, &fakeProjectionClient{}, func(ctx context.Context, req forgejointegration.PushRequest) error { return nil })

	if err := svc.DispatchForgejoIntegration(context.Background(), repo.FullName, "/repos/example-owner/demo.git", []ForgejoRefChange{{
		Ref: "refs/heads/agent/demo", After: pushedSHA,
	}}); err != nil {
		t.Fatalf("DispatchForgejoIntegration: %v", err)
	}
	forgejo := loadProjection(t, svc, pr.ID, ProjectionProviderForgejo)
	if forgejo.LastSyncedSHA != pushedSHA || forgejo.State != ProjectionStateOpen {
		t.Fatalf("Forgejo projection after required push: %#v", forgejo)
	}
	gitLab := loadProjection(t, svc, pr.ID, ProjectionProviderGitLab)
	if gitLab.LastSyncedSHA != createSHA || gitLab.State != ProjectionStateOpen {
		t.Fatalf("Forgejo dispatch must not rewrite GitLab last_synced_sha: %#v", gitLab)
	}
	gitHub := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	if gitHub.LastSyncedSHA != createSHA || gitHub.State != ProjectionStateOpen {
		t.Fatalf("Forgejo dispatch must not rewrite GitHub last_synced_sha: %#v", gitHub)
	}
}

func TestCloseGitLabShadowMergeRequestRecordsProjectionClosed(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mergedSHA := "dddddddddddddddddddddddddddddddddddddddd"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 99)
	seedProjection(t, svc, pr, ProjectionProviderGitLab, "backup/demo", 12, ProjectionStateOpen, createSHA)
	seedProjection(t, svc, pr, ProjectionProviderForgejo, "forgejo/demo", 42, ProjectionStateOpen, createSHA)
	client := &bookkeepingGitLabClient{
		handled: true,
		mr:      gitlabintegration.MergeRequest{IID: 12, WebURL: "http://gitlab.local/backup/demo/-/merge_requests/12"},
	}
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled: true, BaseURL: "http://gitlab.local", Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]gitlabintegration.RepoMapping{repo.FullName: {ProjectPath: "backup/demo", TargetBranch: "main"}},
	}, func(context.Context, string, string, string, string) error {
		t.Fatal("shadow close must not merge or push GitLab")
		return nil
	}, client)

	result, handled, err := svc.closeGitLabShadowMergeRequest(context.Background(), forgejointegration.MergedPullRequestEvent{
		RepoFullName: repo.FullName, PRNumber: 42, HeadBranch: pr.HeadRef, BaseBranch: pr.BaseRef,
	}, mergedSHA, pr)
	if err != nil || !handled || client.calls != 1 {
		t.Fatalf("close GitLab shadow: handled=%v err=%v calls=%d result=%#v", handled, err, client.calls, result)
	}
	gitLab := loadProjection(t, svc, pr.ID, ProjectionProviderGitLab)
	if gitLab.State != ProjectionStateClosed || gitLab.State == ProjectionStateMerged || gitLab.LastSyncedSHA != mergedSHA || gitLab.ExternalNumber != 12 {
		t.Fatalf("GitLab projection after shadow close: %#v", gitLab)
	}
	forgejo := loadProjection(t, svc, pr.ID, ProjectionProviderForgejo)
	if forgejo.State != ProjectionStateOpen || forgejo.LastSyncedSHA != createSHA {
		t.Fatalf("Forgejo required projection must be unchanged by GitLab shadow close: %#v", forgejo)
	}
}

func TestCloseGitLabShadowMergeRequestRecordsClosedWhenAlreadyMergedByAncestry(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mergedSHA := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 97)
	seedProjection(t, svc, pr, ProjectionProviderGitLab, "backup/demo", 9, ProjectionStateOpen, createSHA)
	client := &bookkeepingGitLabClient{handled: false}
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled: true, BaseURL: "http://gitlab.local", Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]gitlabintegration.RepoMapping{repo.FullName: {ProjectPath: "backup/demo"}},
	}, nil, client)

	result, handled, err := svc.closeGitLabShadowMergeRequest(context.Background(), forgejointegration.MergedPullRequestEvent{
		RepoFullName: repo.FullName, PRNumber: 41, HeadBranch: pr.HeadRef, BaseBranch: pr.BaseRef,
	}, mergedSHA, pr)
	if err != nil || handled || client.calls != 1 {
		t.Fatalf("ancestry-merged close: handled=%v err=%v calls=%d result=%#v", handled, err, client.calls, result)
	}
	gitLab := loadProjection(t, svc, pr.ID, ProjectionProviderGitLab)
	if gitLab.State != ProjectionStateClosed || gitLab.State == ProjectionStateMerged || gitLab.LastSyncedSHA != mergedSHA || gitLab.ExternalNumber != 9 {
		t.Fatalf("ancestry-merged GitLab row must be AGS-closed with merged SHA: %#v", gitLab)
	}
}

func TestCloseGitLabShadowMergeRequestFailureDoesNotRewriteProjection(t *testing.T) {
	svc := setupProjectionStateService(t)
	createSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	repo, pr := seedOpenHeadPR(t, svc, "example-team/app-fixture", createSHA, 99)
	seedProjection(t, svc, pr, ProjectionProviderGitLab, "backup/demo", 12, ProjectionStateOpen, createSHA)
	closeErr := errors.New("gitlab close transport failed")
	client := &bookkeepingGitLabClient{err: closeErr}
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled: true, BaseURL: "http://gitlab.local", Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]gitlabintegration.RepoMapping{repo.FullName: {ProjectPath: "backup/demo"}},
	}, nil, client)

	_, handled, err := svc.closeGitLabShadowMergeRequest(context.Background(), forgejointegration.MergedPullRequestEvent{
		RepoFullName: repo.FullName, PRNumber: 42, HeadBranch: pr.HeadRef, BaseBranch: pr.BaseRef,
	}, "ffffffffffffffffffffffffffffffffffffffff", pr)
	if err == nil || !errors.Is(err, closeErr) || handled {
		t.Fatalf("expected close error, handled=%v err=%v", handled, err)
	}
	gitLab := loadProjection(t, svc, pr.ID, ProjectionProviderGitLab)
	if gitLab.State != ProjectionStateOpen || gitLab.LastSyncedSHA != createSHA {
		t.Fatalf("failed close must not upsert GitLab projection: %#v", gitLab)
	}
}
