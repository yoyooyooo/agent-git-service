package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

// An admitted label can lose its read budget before the first provider write,
// not only after Git rebase. Its error must reach the same local finalizer.
func TestWebhookObservationFailureBeforeFirstProviderWritePreservesIntent(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "unavailable"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			svc, ctx, pr, projection, client := interruptionFixture(t)
			intent := seedDispatchedIntent(t, svc, pr, projection)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			writes, validations := 0, 0
			fixtureReadIntegration(svc, client, func(_ context.Context, ref string) (string, error) {
				return client.remoteRefs[ref], nil
			}, &writes)
			cause := errors.New("fixture unavailable read")
			if canceled {
				cause = context.Canceled
			}
			SetDelegatedProviderWriteHookForTest(svc, func() error {
				validations++
				if canceled {
					cancel()
				}
				return forgejoActionObservationError("fixture pre-write read", cause)
			})
			t.Cleanup(func() { SetDelegatedProviderWriteHookForTest(svc, nil) })
			event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber, HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef, LabelName: forgejointegration.AGSActionRebaseLabel}
			result, err := svc.handleForgejoPullRequestActionLabel(ctx, event)
			if !errors.Is(err, cause) || !IsForgejoActionObservationUnavailable(err) || result.WorkflowStatus != "recovery_needed" {
				t.Fatalf("admitted observation failure was swallowed: result=%+v err=%v", result, err)
			}
			var current db.PullRequestActionIntent
			if err := svc.DB.First(&current, "id = ?", intent.ID).Error; err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			head, err := svc.Git.HeadSHA(context.Background(), pr.Repository.FullName, pr.HeadRef)
			if err != nil || head != pr.HeadSHA || count != 0 || writes != 0 || validations != 1 || client.addCalls+client.removeCalls+client.commentCalls != 0 {
				t.Fatalf("read failure started effects or invented a job: head=%s jobs=%d writes=%d validations=%d err=%v", head, count, writes, validations, err)
			}
			if current.State != ForgejoActionIntentRecovery || current.FailureCode != "action_observation_unavailable" || !current.ExpiresAt.Equal(intent.ExpiresAt) {
				t.Fatalf("same intent lost its interruption or changed expiry: state=%s code=%s", current.State, current.FailureCode)
			}
		})
	}
}
