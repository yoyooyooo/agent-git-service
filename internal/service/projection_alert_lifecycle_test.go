package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

func setupProjectionAlertLifecycle(t *testing.T, state string, merged bool) (*Service, db.PullRequest, db.PullRequestProjection, *fakePullRequestListClient) {
	t.Helper()
	svc := setupProjectionStateService(t)
	svc.DisableForgejoProjectionWorker = true
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{RepositoryID: repo.ID, Number: 1, Title: "lifecycle", State: state, Merged: merged,
		HeadRef: "agent/reused", HeadSHA: strings.Repeat("a", 40), BaseRef: "main"}
	if merged {
		pr.MergeCommitSHA = strings.Repeat("b", 40)
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	projection := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
		ExternalRepo: "forgejo/demo", ExternalNumber: 42, SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef,
		State: state, LastSyncedSHA: pr.HeadSHA}
	if err := svc.DB.Create(&projection).Error; err != nil {
		t.Fatal(err)
	}
	row := forgejointegration.PullRequestSnapshot{Number: 42, State: state, Merged: merged, HeadRef: pr.HeadRef,
		HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, MergeCommitSHA: pr.MergeCommitSHA}
	client := &fakePullRequestListClient{rows: []forgejointegration.PullRequestSnapshot{row}, exactRows: map[int]forgejointegration.PullRequestSnapshot{42: row}}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
	}, client, nil)
	return svc, pr, projection, client
}

func TestHistoricalProjectionUsesLifecycleFactsInsteadOfReusedBranchHead(t *testing.T) {
	for _, merged := range []bool{false, true} {
		name := "closed-unmerged"
		if merged {
			name = "merged"
		}
		t.Run(name, func(t *testing.T) {
			svc, pr, _, client := setupProjectionAlertLifecycle(t, db.StateClosed, merged)
			row := client.rows[0]
			row.HeadSHA = strings.Repeat("c", 40)
			client.rows[0], client.exactRows[42] = row, row
			ctx := context.Background()
			if err := svc.recordPullRequestIntegrityFailure(ctx, "example-owner/demo", pullRequestProjectionIntegrityRef(pr.Number), pr,
				forgejointegration.ProjectionFailureSHADrift, pr.HeadSHA, row.HeadSHA, "historical branch head differs", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			recorded, err := svc.scanForgejoPullRequestIntegrity(ctx, "example-owner/demo")
			if err != nil || recorded != 0 {
				t.Fatalf("historical scan recorded=%d err=%v; a reused live branch is not a terminal PR head", recorded, err)
			}
			var state db.ProjectionRefState
			if err := svc.DB.Where("ref = ?", pullRequestProjectionIntegrityRef(pr.Number)).First(&state).Error; err != nil {
				t.Fatal(err)
			}
			if state.Status != ProjectionStatusResolved {
				t.Fatalf("stale historical head alert remains %s", state.Status)
			}
			var jobs int64
			if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Count(&jobs).Error; err != nil || jobs != 0 {
				t.Fatalf("historical audit queued %d jobs, err=%v", jobs, err)
			}
		})
	}
}

func TestHistoricalProjectionStillDetectsDifferentMergeCommit(t *testing.T) {
	svc, _, _, client := setupProjectionAlertLifecycle(t, db.StateClosed, true)
	row := client.rows[0]
	row.MergeCommitSHA = strings.Repeat("d", 40)
	client.rows[0], client.exactRows[42] = row, row
	recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
	if err != nil || recorded != 1 {
		t.Fatalf("different terminal merge must remain drift, recorded=%d err=%v", recorded, err)
	}
	if len(client.exactCalls) != 1 {
		t.Fatalf("merge mismatch was not exactly confirmed: %v", client.exactCalls)
	}
}

func TestProjectionAuditReconcilesOnlyExactlyObservedBaseMetadata(t *testing.T) {
	for _, state := range []string{db.StateOpen, db.StateClosed} {
		t.Run(state, func(t *testing.T) {
			svc, pr, projection, client := setupProjectionAlertLifecycle(t, state, state == db.StateClosed)
			if err := svc.DB.Model(&projection).Update("target_branch", "agent/old-stack").Error; err != nil {
				t.Fatal(err)
			}
			recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
			if err != nil || recorded != 0 {
				t.Fatalf("actual provider base already equals AGS: recorded=%d err=%v", recorded, err)
			}
			if len(client.exactCalls) != 1 {
				t.Fatalf("metadata repair did not require an exact provider read: %v", client.exactCalls)
			}
			if err := svc.DB.First(&projection, projection.ID).Error; err != nil {
				t.Fatal(err)
			}
			if projection.TargetBranch != pr.BaseRef || projection.ExternalNumber != 42 || projection.LastSyncedSHA != pr.HeadSHA {
				t.Fatalf("unexpected repaired projection: %#v", projection)
			}
		})
	}
}

func TestProjectionEnqueueDoesNotCreateWorkForTerminalPR(t *testing.T) {
	svc, pr, _, _ := setupProjectionAlertLifecycle(t, db.StateClosed, true)
	// The caller may have loaded the PR before the merge. Admission must reload
	// its lifecycle rather than trusting that stale caller snapshot.
	pr.State, pr.Merged = db.StateOpen, false
	job, err := svc.EnqueueForgejoPullRequestProjection(context.Background(), pr)
	if err != nil || job.ID != 0 {
		t.Fatalf("terminal PR created projection work: job_id=%d err=%v", job.ID, err)
	}
	var jobs int64
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Count(&jobs).Error; err != nil || jobs != 0 {
		t.Fatalf("terminal PR has %d queued jobs: %v", jobs, err)
	}
}

type projectionAuditHookClient struct {
	*fakePullRequestListClient
	onExact func() error
}

func (f *projectionAuditHookClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (forgejointegration.PullRequestSnapshot, bool, error) {
	row, found, err := f.fakePullRequestListClient.GetPullRequest(ctx, owner, repo, number)
	if err == nil && f.onExact != nil {
		err = f.onExact()
	}
	return row, found, err
}

func TestProjectionBaseReconciliationRejectsStaleOrUnavailableEvidence(t *testing.T) {
	for _, kind := range []string{"native-base-changed", "mapping-changed", "provider-unavailable"} {
		t.Run(kind, func(t *testing.T) {
			svc, pr, projection, client := setupProjectionAlertLifecycle(t, db.StateOpen, false)
			if err := svc.DB.Model(&projection).Update("target_branch", "agent/old-stack").Error; err != nil {
				t.Fatal(err)
			}
			hook := &projectionAuditHookClient{fakePullRequestListClient: client, onExact: func() error {
				switch kind {
				case "native-base-changed":
					return svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("base_ref", "release").Error
				case "mapping-changed":
					return svc.DB.Model(&db.PullRequestProjection{}).Where("id = ?", projection.ID).Update("external_number", 43).Error
				default:
					return context.DeadlineExceeded
				}
			}}
			svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
				Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token", AutoPullRequest: true,
				MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"},
				RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
			}, hook, nil)
			_, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
			if err == nil {
				t.Fatal("stale/unavailable evidence was accepted")
			}
			if kind != "provider-unavailable" && !errors.Is(err, ErrInvalidState) {
				t.Fatalf("stale compare-and-swap returned %v", err)
			}
			if err := svc.DB.First(&projection, projection.ID).Error; err != nil {
				t.Fatal(err)
			}
			if projection.TargetBranch != "agent/old-stack" {
				t.Fatalf("stale observation changed base to %s", projection.TargetBranch)
			}
		})
	}
}

func TestProjectionAuditDoesNotHealRealCoordinateOrRetainedHeadDrift(t *testing.T) {
	for _, kind := range []string{"wrong-base", "case-distinct-base", "wrong-head", "wrong-pull-ref", "retained-head-drift"} {
		t.Run(kind, func(t *testing.T) {
			svc, _, projection, client := setupProjectionAlertLifecycle(t, db.StateClosed, false)
			row := client.rows[0]
			switch kind {
			case "wrong-base":
				row.BaseRef = "release"
			case "case-distinct-base":
				row.BaseRef = "MAIN"
			case "wrong-head":
				row.HeadRef = "agent/unrelated"
			case "wrong-pull-ref":
				row.HeadRef = "refs/pull/43/head"
			case "retained-head-drift":
				row.HeadRef, row.HeadSHA = "refs/pull/42/head", strings.Repeat("c", 40)
			}
			client.rows[0], client.exactRows[42] = row, row
			recorded, err := svc.scanForgejoPullRequestIntegrity(context.Background(), "example-owner/demo")
			if err != nil || recorded != 1 {
				t.Fatalf("real historical mismatch suppressed: recorded=%d err=%v", recorded, err)
			}
			var jobs int64
			if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Count(&jobs).Error; err != nil || jobs != 0 {
				t.Fatalf("historical mismatch queued %d writes: %v", jobs, err)
			}
			var after db.PullRequestProjection
			if err := svc.DB.First(&after, projection.ID).Error; err != nil || after.TargetBranch != projection.TargetBranch || after.LastSyncedSHA != projection.LastSyncedSHA {
				t.Fatalf("mismatch rewrote mapping: %#v err=%v", after, err)
			}
		})
	}
}
