package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
)

func TestGitHubProjectionStaleOpenSnapshotCannotSupersedeClose(t *testing.T) {
	svc, pr, _, _ := retryGitHubService(t)
	openSnapshot := pr
	closeGitHubFact(t, svc, &pr, false)
	ctx := context.Background()
	old, err := svc.enqueueGitHubProjection(ctx, pr, githubProjectionClose)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.enqueueGitHubProjection(ctx, openSnapshot, githubProjectionEnsure); !errors.Is(err, errGitHubProjectionPending) {
		t.Fatalf("stale reopen admitted: %v", err)
	}
	current := githubJob(t, svc, pr)
	if current.Trigger != githubProjectionClose || current.Phase != old.Phase || current.Attempt != old.Attempt || !current.NextRunAt.Equal(*old.NextRunAt) {
		t.Fatalf("stale reopen changed close: %+v", current)
	}
}

func TestGitHubProjectionReopenSupersedesClose(t *testing.T) {
	for _, path := range []string{"dispatch", "watch"} {
		for _, previous := range []string{"completed missing", "pending", "claimed", "exhausted", "terminal shadow", "terminal shadow without mapping"} {
			t.Run(path+"/"+previous, func(t *testing.T) {
				svc, pr, client, pushes := retryGitHubService(t)
				ctx := context.Background()
				closeGitHubFact(t, svc, &pr, false)
				old, err := svc.enqueueGitHubProjection(ctx, pr, githubProjectionClose)
				if err != nil {
					t.Fatal(err)
				}
				terminal := previous == "terminal shadow" || previous == "terminal shadow without mapping"
				switch previous {
				case "completed missing", "terminal shadow", "terminal shadow without mapping":
					if err := svc.runGitHubProjectionAttempt(ctx, old, ""); err != nil {
						t.Fatal(err)
					}
				case "claimed":
					var ok bool
					old, ok, err = svc.claimGitHubProjection(ctx, old)
					if err != nil || !ok {
						t.Fatalf("claim=%v err=%v", ok, err)
					}
				case "exhausted":
					if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", old.ID).Updates(map[string]any{"phase": ForgejoProjectionPhaseFailedTerminal, "attempt": githubProjectionMaxAttempts}).Error; err != nil {
						t.Fatal(err)
					}
				}
				if terminal {
					client.exists = true
					client.remote = githubintegration.PullRequest{Number: 8, State: "closed"}
					if previous == "terminal shadow" {
						seedProjection(t, svc, pr, ProjectionProviderGitHub, "fixture/repo", 8, ProjectionStateClosed, pr.HeadSHA)
					}
				}
				// Only the authoritative AGS fact reopens; GitHub never grants this state.
				if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{"state": db.StateOpen, "closed_at": nil}).Error; err != nil {
					t.Fatal(err)
				}
				pr.State, pr.ClosedAt = db.StateOpen, nil
				store, err := gitstore.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				svc.Git = store
				if path == "dispatch" {
					if _, handled, err := svc.ensureGitHubShadowPullRequest(ctx, "example-owner/demo", "/fixture.git", pr.HeadSHA, pr); err != nil || !handled {
						t.Fatalf("handled=%v err=%v", handled, err)
					}
				} else if _, err := svc.ReconcileGitHubPullRequestStates(ctx, 10); err != nil {
					t.Fatal(err)
				}
				job := githubJob(t, svc, pr)
				if job.Trigger != githubProjectionEnsure || job.Phase != ForgejoProjectionPhaseProjected || job.Attempt != 1 || job.LastError != "" || job.LastErrorType != "" {
					t.Fatalf("reopen did not replace close: %+v", job)
				}
				row := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
				wantState, wantWrites := ProjectionStateOpen, 1
				if terminal {
					wantState, wantWrites = ProjectionStateClosed, 0
				}
				if row.State != wantState || row.ExternalNumber != 8 || client.closes != 0 || client.ensures != wantWrites || *pushes != wantWrites {
					t.Fatalf("row=%+v closes=%d ensures=%d pushes=%d", row, client.closes, client.ensures, *pushes)
				}
				current, err := svc.projectionPullRequestFact(ctx, pr.ID)
				if err != nil || current.State != db.StateOpen || current.Merged {
					t.Fatalf("AGS changed: %+v err=%v", current, err)
				}
				if previous == "claimed" {
					var claims int64
					if err := svc.gitHubProjectionClaimScope(ctx, old).Count(&claims).Error; err != nil || claims != 0 {
						t.Fatalf("old close claim survived: count=%d err=%v", claims, err)
					}
					if err := svc.finishGitHubProjection(ctx, old, nil); !errors.Is(err, ErrInvalidState) {
						t.Fatalf("old close retained completion authority: %v", err)
					}
					if err := svc.runGitHubProjectionAttempt(ctx, old, ""); !errors.Is(err, errGitHubProjectionPending) {
						t.Fatalf("old close ran: %v", err)
					}
					if client.closes != 0 {
						t.Fatal("superseded close wrote")
					}
				}
			})
		}
	}
}
