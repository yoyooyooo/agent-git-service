package service

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

const repairConfirmedHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const repairAdvancedHead = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func setupConfirmedAuthorityRepair(t *testing.T) (*Service, db.PullRequest, db.PullRequestProjectionJob) {
	t.Helper()
	svc := setupProjectionStateService(t)
	svc.DisableForgejoProjectionWorker = true
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo.local", Token: "fixture-only"}, &fakeProjectionClient{}, nil)
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{Number: 1, RepositoryID: repo.ID, Title: "repair fixture", State: db.StateOpen, HeadRef: "fix/drift", HeadSHA: repairConfirmedHead, BaseRef: "main"}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, Trigger: ForgejoProjectionTriggerPullRequest, RepoFullName: repo.FullName, AGSPRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: repairConfirmedHead, Phase: ForgejoProjectionPhaseFailedTerminal, Attempt: 4, LastErrorType: forgejointegration.ProjectionFailureNonFastForward, ExternalRepo: "forgejo/demo", ExternalNumber: 2}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RepairForgejoPullRequestProjectionAuthority(context.Background(), repo.FullName, pr.Number, repairConfirmedHead); err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	return svc, pr, job
}

// A confirmation for A must not authorize a repair of B.
func TestAstraRepairConfirmationCannotTransferToNewHead(t *testing.T) {
	svc, pr, job := setupConfirmedAuthorityRepair(t)
	if err := svc.DB.Model(&pr).Update("head_sha", repairAdvancedHead).Error; err != nil {
		t.Fatal(err)
	}
	pr.HeadSHA = repairAdvancedHead
	got, err := svc.EnqueueForgejoPullRequestProjection(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadSHA != repairAdvancedHead || got.Phase != ForgejoProjectionPhaseQueued || got.CorrelationID != "" || got.Attempt <= job.Attempt {
		t.Fatalf("confirmation for %s carried into ordinary requeue: head=%s phase=%s marker=%s attempt=%d", repairConfirmedHead, got.HeadSHA, got.Phase, got.CorrelationID, got.Attempt)
	}
}

func TestAuthorityRepairConfirmationClearedByOrdinaryRetry(t *testing.T) {
	svc, pr, job := setupConfirmedAuthorityRepair(t)
	if _, err := svc.RetryForgejoPullRequestProjection(context.Background(), "example-owner/demo", pr.Number); err != nil {
		t.Fatal(err)
	}
	var got db.PullRequestProjectionJob
	if err := svc.DB.First(&got, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.CorrelationID != "" || got.HeadSHA != repairConfirmedHead || got.Attempt <= job.Attempt {
		t.Fatalf("ordinary retry retained repair authority: %#v", got)
	}
}

func TestAuthorityRepairWriteAdmissionRechecksConfirmedHeadAndGeneration(t *testing.T) {
	for _, change := range []string{"head", "closed", "ordinary_requeue"} {
		t.Run(change, func(t *testing.T) {
			svc, pr, job := setupConfirmedAuthorityRepair(t)
			claimed, err := svc.claimGenericForgejoProjectionJob(context.Background(), job)
			if err != nil {
				t.Fatal(err)
			}
			ctx := contextWithGenericProjectionClaim(context.Background(), claimed)
			if err := svc.updateForgejoProjectionJobPhase(ctx, job.ID, ForgejoProjectionPhasePushingRef, map[string]any{"correlation_id": ""}); err != nil {
				t.Fatal(err)
			}
			if err := svc.revalidateGenericProjectionProviderWrite(ctx, claimed, pr); err != nil {
				t.Fatalf("confirmed current head denied: %v", err)
			}
			switch change {
			case "head":
				if err := svc.DB.Model(&pr).Update("head_sha", repairAdvancedHead).Error; err != nil {
					t.Fatal(err)
				}
			case "closed":
				if err := svc.DB.Model(&pr).Update("state", db.StateClosed).Error; err != nil {
					t.Fatal(err)
				}
			case "ordinary_requeue":
				if _, err := svc.RetryForgejoPullRequestProjection(context.Background(), "example-owner/demo", pr.Number); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.revalidateGenericProjectionProviderWrite(ctx, claimed, pr); err == nil {
				t.Fatal("superseded repair write admitted")
			}
		})
	}
}
