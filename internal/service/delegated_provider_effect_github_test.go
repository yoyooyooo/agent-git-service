package service

import (
	"context"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
)

type actionGitHubClient struct{ ensureCalls int }

func (c *actionGitHubClient) EnsurePullRequest(context.Context, string, githubintegration.PullRequestInput) (githubintegration.PullRequest, error) {
	c.ensureCalls++
	return githubintegration.PullRequest{Number: 8, HTMLURL: "https://github.com/example-org/project-kit/pull/8"}, nil
}

func (c *actionGitHubClient) ClosePullRequestForBranch(context.Context, string, string, string, string) (githubintegration.PullRequest, bool, error) {
	return githubintegration.PullRequest{}, false, nil
}

func TestGitHubRebaseWritesRequireFreshExactActionBinding(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drift func(t *testing.T, svc *Service, intent db.PullRequestActionIntent, job db.PullRequestProjectionJob)
	}{
		{
			name: "intent expired after dispatch admission",
			drift: func(t *testing.T, svc *Service, intent db.PullRequestActionIntent, _ db.PullRequestProjectionJob) {
				if err := svc.DB.Model(&db.PullRequestActionIntent{}).Where("id = ?", intent.ID).Update("expires_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "intent denied after dispatch admission",
			drift: func(t *testing.T, svc *Service, intent db.PullRequestActionIntent, _ db.PullRequestProjectionJob) {
				if err := svc.DB.Model(&db.PullRequestActionIntent{}).Where("id = ?", intent.ID).Update("state", ForgejoActionIntentDenied).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "job rebound to another generation",
			drift: func(t *testing.T, svc *Service, _ db.PullRequestActionIntent, job db.PullRequestProjectionJob) {
				other := "other-intent"
				if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", job.ID).Updates(map[string]any{
					"action_intent_id": other, "action_generation": job.ActionGeneration + 1,
				}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, pr, projection := setupIntentTestService(t)
			intent := seedDispatchedIntent(t, svc, pr, projection)
			job := db.PullRequestProjectionJob{
				PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
				Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
				HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhasePushingRef,
			}
			ctx := bindTestActionJob(t, svc, intent, &job)
			if err := svc.DB.Create(&job).Error; err != nil {
				t.Fatal(err)
			}

			client := &actionGitHubClient{}
			pushes := 0
			svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
				Enabled: true, Token: "token", MergeAuthority: "forgejo",
				Repos: map[string]githubintegration.RepoMapping{"example-owner/demo": {Owner: "example-org", Repo: "project-kit"}},
			}, func(context.Context, string, string, string, string) error {
				pushes++
				return nil
			}, client)
			tc.drift(t, svc, intent, job)

			if _, handled, err := svc.ensureGitHubShadowPullRequest(ctx, "example-owner/demo", "/tmp/demo.git", pr.HeadSHA, pr); err == nil || handled {
				t.Fatalf("GitHub rebase drift was not rejected: handled=%v err=%v", handled, err)
			}
			if pushes != 0 || client.ensureCalls != 0 {
				t.Fatalf("GitHub provider calls after exact binding drift: pushes=%d ensures=%d", pushes, client.ensureCalls)
			}
		})
	}
}

func TestUnboundHistoricalGitHubRebaseFailsClosed(t *testing.T) {
	svc, pr, _ := setupIntentTestService(t)
	client := &actionGitHubClient{}
	pushes := 0
	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled: true, Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]githubintegration.RepoMapping{"example-owner/demo": {Owner: "example-org", Repo: "project-kit"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	}, client)
	ctx := ContextWithDelegatedSession(context.Background(), db.DelegatedAgentSession{ID: "historical-rebase-session"})
	if _, handled, err := svc.ensureGitHubShadowPullRequest(ctx, "example-owner/demo", "/tmp/demo.git", pr.HeadSHA, pr); err == nil || handled {
		t.Fatalf("unbound historical rebase was not rejected: handled=%v err=%v", handled, err)
	}
	if pushes != 0 || client.ensureCalls != 0 {
		t.Fatalf("provider calls for unbound rebase: pushes=%d ensures=%d", pushes, client.ensureCalls)
	}
}
