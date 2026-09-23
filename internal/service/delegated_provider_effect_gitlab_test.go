package service

import (
	"context"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
)

type actionGitLabClient struct{ ensureCalls int }

func (c *actionGitLabClient) EnsureMergeRequest(context.Context, string, gitlabintegration.MergeRequestInput) (gitlabintegration.MergeRequest, error) {
	c.ensureCalls++
	return gitlabintegration.MergeRequest{IID: 7, WebURL: "http://gitlab.local/mr/7"}, nil
}

func (c *actionGitLabClient) CloseMergeRequestForBranch(context.Context, string, string, string, string) (gitlabintegration.MergeRequest, bool, error) {
	return gitlabintegration.MergeRequest{}, false, nil
}

func TestGitLabRebaseWritesRequireFreshExactActionBinding(t *testing.T) {
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

			client := &actionGitLabClient{}
			pushes := 0
			svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
				Enabled: true, BaseURL: "http://gitlab.local", Token: "token", MergeAuthority: "forgejo",
				Repos: map[string]gitlabintegration.RepoMapping{"example-owner/demo": {ProjectPath: "backup/demo"}},
			}, func(context.Context, string, string, string, string) error {
				pushes++
				return nil
			}, client)
			tc.drift(t, svc, intent, job)

			if _, handled, err := svc.ensureGitLabShadowMergeRequest(ctx, "example-owner/demo", "/tmp/demo.git", pr.HeadSHA, pr); err == nil || handled {
				t.Fatalf("GitLab rebase drift was not rejected: handled=%v err=%v", handled, err)
			}
			if pushes != 0 || client.ensureCalls != 0 {
				t.Fatalf("GitLab provider calls after exact binding drift: pushes=%d ensures=%d", pushes, client.ensureCalls)
			}
		})
	}
}

func TestUnboundHistoricalGitLabRebaseFailsClosed(t *testing.T) {
	svc, pr, _ := setupIntentTestService(t)
	client := &actionGitLabClient{}
	pushes := 0
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled: true, BaseURL: "http://gitlab.local", Token: "token", MergeAuthority: "forgejo",
		Repos: map[string]gitlabintegration.RepoMapping{"example-owner/demo": {ProjectPath: "backup/demo"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	}, client)
	// A delegated marker whose Session is unavailable still must not fall back
	// to an unbound provider write.
	ctx := ContextWithDelegatedSession(context.Background(), db.DelegatedAgentSession{ID: "historical-rebase-session"})
	if _, handled, err := svc.ensureGitLabShadowMergeRequest(ctx, "example-owner/demo", "/tmp/demo.git", pr.HeadSHA, pr); err == nil || handled {
		t.Fatalf("unbound historical rebase was not rejected: handled=%v err=%v", handled, err)
	}
	if pushes != 0 || client.ensureCalls != 0 {
		t.Fatalf("provider calls for unbound rebase: pushes=%d ensures=%d", pushes, client.ensureCalls)
	}
}
