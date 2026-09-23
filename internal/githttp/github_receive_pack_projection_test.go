package githttp_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/service"
)

type receivePackGitHubClient struct {
	ensureCalls int
}

func (c *receivePackGitHubClient) EnsurePullRequest(context.Context, string, githubintegration.PullRequestInput) (githubintegration.PullRequest, error) {
	c.ensureCalls++
	return githubintegration.PullRequest{Number: 6, HTMLURL: "https://github.com/backup/demo/pull/6"}, nil
}

func (c *receivePackGitHubClient) ClosePullRequestForBranch(context.Context, string, string, string, string) (githubintegration.PullRequest, bool, error) {
	return githubintegration.PullRequest{}, false, nil
}

// Live mini AGS logged github branch projection pushed on git-receive-pack for
// PR 119, then last_synced_sha stayed stale until a later PATCH /pulls EnsureShadowPR.
// Receive-pack PushRef success must advance the open GitHub row immediately.
func TestGitHTTP_ReceivePackPushRefAdvancesGitHubPRLastSyncedSHAWithoutEnsure(t *testing.T) {
	env := setupTestServer(t, "example-owner", "demo", "main", true)
	env.Svc.DisableForgejoProjectionWorker = true
	if err := env.DB.AutoMigrate(&db.PullRequestProjection{}, &db.ProjectionRefState{}, &db.ProjectionEvent{}); err != nil {
		t.Fatalf("migrate projection tables: %v", err)
	}
	var repo db.Repository
	if err := env.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}

	localDir := filepath.Join(t.TempDir(), "local")
	runGit(t, t.TempDir(), "clone", env.RepoURL, localDir)
	runGit(t, localDir, "checkout", "-b", "example-team/app-fixture")
	if err := os.WriteFile(filepath.Join(localDir, "one.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, localDir, "add", "one.txt")
	runGit(t, localDir, "commit", "-m", "one")
	runGit(t, localDir, "push", "-u", "origin", "example-team/app-fixture")
	env.Svc.Wg.Wait()
	createSHA := strings.TrimSpace(runGit(t, localDir, "rev-parse", "HEAD"))

	pr := db.PullRequest{
		Number: 88, RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: repo.OwnerID,
		Title: "github receive-pack bookkeeping", State: db.StateOpen,
		HeadRef: "example-team/app-fixture", HeadSHA: createSHA, BaseRef: "main",
	}
	if err := env.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create PR: %v", err)
	}
	if err := env.DB.Create(&db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: service.ProjectionProviderGitHub,
		ExternalRepo: "backup/demo", ExternalNumber: 6, ExternalURL: "https://github.com/backup/demo/pull/6",
		SourceBranch: "example-team/app-fixture", TargetBranch: "main", State: service.ProjectionStateOpen, LastSyncedSHA: createSHA,
	}).Error; err != nil {
		t.Fatalf("create GitHub projection: %v", err)
	}

	client := &receivePackGitHubClient{}
	pushes := 0
	env.Svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled: true, Token: "token", MergeAuthority: "forgejo",
		MirrorBranchIncludes: []string{"example-team/*"},
		Repos:                map[string]githubintegration.RepoMapping{repo.FullName: {Owner: "backup", Repo: "demo"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	}, client)

	if err := os.WriteFile(filepath.Join(localDir, "two.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, localDir, "add", "two.txt")
	runGit(t, localDir, "commit", "-m", "two")
	pushedSHA := strings.TrimSpace(runGit(t, localDir, "rev-parse", "HEAD"))
	runGit(t, localDir, "push", "origin", "example-team/app-fixture")
	env.Svc.Wg.Wait()

	if pushes != 1 {
		t.Fatalf("GitHub PushRef calls=%d, want 1", pushes)
	}
	if client.ensureCalls != 0 {
		t.Fatalf("EnsurePullRequest calls=%d, want 0: receive-pack must not wait for metadata EnsureShadowPR", client.ensureCalls)
	}
	var row db.PullRequestProjection
	if err := env.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderGitHub).First(&row).Error; err != nil {
		t.Fatalf("load GitHub projection: %v", err)
	}
	if row.LastSyncedSHA != pushedSHA || row.State != service.ProjectionStateOpen {
		t.Fatalf("GitHub last_synced_sha after receive-pack PushRef stayed stale: %#v want sha=%s", row, pushedSHA)
	}
}
