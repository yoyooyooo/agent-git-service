package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/stretchr/testify/require"
)

func TestLifecycleReviewAsyncExactConfirmationBarrier(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(fmt.Sprint(drift), func(t *testing.T) {
			svc := setupProjectionStateService(t)
			var repo db.Repository
			require.NoError(t, svc.DB.First(&repo).Error)
			pr := db.PullRequest{Number: 803, RepositoryID: repo.ID, Repository: repo, State: db.StateOpen, HeadRef: "feature", BaseRef: "main", HeadSHA: "abc1234", LifecycleGeneration: 1, ForgejoReopenPending: true}
			require.NoError(t, svc.DB.Create(&pr).Error)
			binding := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, ExternalRepo: "forgejo/repo", ExternalNumber: 42, State: ProjectionStateClosed}
			require.NoError(t, svc.DB.Create(&binding).Error)
			started, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/repos/forgejo/repo/pulls/42" {
					t.Errorf("unexpected provider operation: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 400)
					return
				}
				close(started)
				<-release
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"number":42,"state":"open","merged":false,"updated_at":"2001-01-01T00:00:02Z","head":{"ref":"feature","sha":"abc1234"},"base":{"ref":"main"}}`)
			}))
			defer server.Close()
			svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: server.URL, RepoMap: map[string]forgejointegration.RepoMapping{repo.FullName: {Owner: "forgejo", Repo: "repo"}}}, forgejointegration.NewHTTPClient(server.URL, "fixture-only"), nil)
			job := seedGenericProjectionJob(t, svc, pr, ForgejoProjectionPhaseQueued)
			claim, err := svc.claimGenericForgejoProjectionJob(context.Background(), job)
			require.NoError(t, err)
			ctx := contextWithGenericProjectionClaim(context.Background(), claim)
			require.NoError(t, svc.updateForgejoProjectionJobPhase(ctx, job.ID, ForgejoProjectionPhaseRecordingProjection, nil))
			done := make(chan error, 1)
			go func() {
				done <- svc.completeGenericForgejoPullRequestProjection(ctx, job.ID, repo.FullName, pr.HeadSHA, pr, forgejointegration.PullRequestResult{ExternalRepo: "forgejo/repo", Number: 42})
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("async completed without required exact open confirmation: %v", err)
			}
			if drift {
				require.NoError(t, svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{"state": db.StateClosed, "lifecycle_generation": 2}).Error)
			}
			close(release)
			err = <-done
			if drift {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			var current db.PullRequest
			require.NoError(t, svc.DB.First(&current, pr.ID).Error)
			require.NoError(t, svc.DB.First(&binding, binding.ID).Error)
			require.NoError(t, svc.DB.First(&job, job.ID).Error)
			if drift {
				require.True(t, current.ForgejoReopenPending)
				require.Nil(t, current.ForgejoLifecycleAt)
				require.Equal(t, ProjectionStateClosed, binding.State)
				require.Equal(t, ForgejoProjectionPhaseRecordingProjection, job.Phase)
			} else {
				require.False(t, current.ForgejoReopenPending)
				require.NotNil(t, current.ForgejoLifecycleAt)
				require.Equal(t, ProjectionStateOpen, binding.State)
				require.Equal(t, ForgejoProjectionPhaseProjected, job.Phase)
			}
		})
	}
}
