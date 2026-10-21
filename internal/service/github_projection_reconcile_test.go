package service

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
)

func TestGitHubProjectionReconcileBoundedFairCursor(t *testing.T) {
	svc, first, client, _ := retryGitHubService(t)
	// This first candidate is eligible but missing and has no local Git store;
	// its durable failure must not starve the later closed projection.
	_, second := seedOpenHeadPR(t, svc, "work/later", first.HeadSHA, 92)
	closeGitHubFact(t, svc, &second, false)
	seedProjection(t, svc, second, ProjectionProviderGitHub, "fixture/repo", 8, ProjectionStateOpen, second.HeadSHA)
	if _, err := svc.ReconcileGitHubPullRequestStates(context.Background(), 1); err == nil {
		t.Fatal("expected missing Git store on first candidate")
	}
	if svc.githubProjectionScanCursor != first.ID {
		t.Fatalf("cursor=%d first=%d", svc.githubProjectionScanCursor, first.ID)
	}
	client.exists = true
	client.remote = githubintegration.PullRequest{Number: 8, State: "closed"}
	if _, err := svc.ReconcileGitHubPullRequestStates(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if loadProjection(t, svc, second.ID, ProjectionProviderGitHub).State != ProjectionStateClosed || client.closes != 0 {
		t.Fatal("later candidate starved or terminal shadow written")
	}
	if _, err := svc.ReconcileGitHubPullRequestStates(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if svc.githubProjectionScanCursor != 0 {
		t.Fatal("cursor did not wrap")
	}
}

func TestGitHubProjectionReconcileNeverBackfillsClosedOrExcludedPR(t *testing.T) {
	for _, state := range []string{"closed", "merged", "excluded"} {
		t.Run(state, func(t *testing.T) {
			svc, pr, client, pushes := retryGitHubService(t)
			if state == "excluded" {
				if err := svc.DB.Model(&pr).Update("head_ref", "work/excluded").Error; err != nil {
					t.Fatal(err)
				}
			} else {
				closeGitHubFact(t, svc, &pr, state == "merged")
			}
			if _, err := svc.ReconcileGitHubPullRequestStates(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			var jobs int64
			if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Count(&jobs).Error; err != nil {
				t.Fatal(err)
			}
			if jobs != 0 || len(client.observations) != 0 || client.ensures != 0 || *pushes != 0 {
				t.Fatalf("unwanted backfill: jobs=%d client=%+v", jobs, client)
			}
		})
	}
}

func TestGitHubProjectionReconcileCreatesMissingEligibleShadow(t *testing.T) {
	for _, hasRow := range []bool{false, true} {
		t.Run(map[bool]string{false: "no mapping", true: "remote missing"}[hasRow], func(t *testing.T) {
			svc, pr, client, pushes := retryGitHubService(t)
			if hasRow {
				seedProjection(t, svc, pr, ProjectionProviderGitHub, "fixture/repo", 7, ProjectionStateOpen, pr.HeadSHA)
			}
			store, err := gitstore.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			svc.Git = store
			if _, err := svc.ReconcileGitHubPullRequestStates(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			row := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
			if row.ExternalNumber != 8 || row.State != ProjectionStateOpen || client.ensures != 1 || *pushes != 1 {
				t.Fatalf("row=%+v client=%+v pushes=%d", row, client, *pushes)
			}
			if _, err := svc.ReconcileGitHubPullRequestStates(context.Background(), 10); err != nil {
				t.Fatal(err)
			}
			if client.ensures != 1 {
				t.Fatal("unchanged projection was recreated")
			}
		})
	}
}

func TestGitHubProjectionReconcileClosesLegacyLocallyClosedRow(t *testing.T) {
	svc, pr, client, pushes := retryGitHubService(t)
	closeGitHubFact(t, svc, &pr, true)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "fixture/repo", 8, ProjectionStateClosed, pr.HeadSHA)
	client.exists = true
	client.remote = githubintegration.PullRequest{Number: 8, State: "open"}
	if _, err := svc.ReconcileGitHubPullRequestStates(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var current db.PullRequest
	if err := svc.DB.First(&current, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if client.closes != 1 || client.ensures != 0 || *pushes != 0 || !current.Merged || current.State != db.StateClosed || current.MergeCommitSHA != pr.MergeCommitSHA {
		t.Fatalf("authority or close violated: PR=%+v client=%+v", current, client)
	}
}

func TestGitHubProjectionOpenAGSAndClosedShadowDoesNotReopen(t *testing.T) {
	svc, pr, client, pushes := retryGitHubService(t)
	client.exists = true
	client.remote = githubintegration.PullRequest{Number: 8, State: "closed"}
	if _, err := svc.ReconcileGitHubPullRequestStates(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	row := loadProjection(t, svc, pr.ID, ProjectionProviderGitHub)
	var current db.PullRequest
	if err := svc.DB.First(&current, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.State != ProjectionStateClosed || current.State != db.StateOpen || client.closes != 0 || client.ensures != 0 || *pushes != 0 {
		t.Fatalf("unexpected state changes: projection=%+v PR=%+v client=%+v", row, current, client)
	}
}

func TestGitHubProjectionWatchRepairsTerminalStateForOpenAGS(t *testing.T) {
	svc, pr, client, pushes := retryGitHubService(t)
	seedProjection(t, svc, pr, ProjectionProviderGitHub, "fixture/repo", 8, ProjectionStateOpen, pr.HeadSHA)
	client.exists = true
	client.remote = githubintegration.PullRequest{Number: 8, State: "closed"}
	changed, err := svc.reconcileGitHubPullRequestState(context.Background(), pr.ID)
	if err != nil || !changed || loadProjection(t, svc, pr.ID, ProjectionProviderGitHub).State != ProjectionStateClosed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	current, err := svc.projectionPullRequestFact(context.Background(), pr.ID)
	if err != nil || current.State != db.StateOpen || current.Merged || client.closes != 0 || client.ensures != 0 || *pushes != 0 {
		t.Fatalf("AGS fact or remote changed: pr=%+v client=%+v err=%v", current, client, err)
	}
}
