package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
)

// The regression uses migrated SQLite (the affected deployment database), real
// native Git and an explicit provider boundary. No production data or TiDB is
// required; existing TiDB action regressions remain unchanged.
func interruptionFixture(t *testing.T) (*Service, context.Context, db.PullRequest, db.PullRequestProjection, *minimalForgejoClient) {
	t.Helper()
	directory := t.TempDir()
	database, err := db.Init("sqlite:" + filepath.Join(directory, "action.sqlite") + "?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	git, err := gitstore.New(filepath.Join(directory, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{DB: database, Git: git, BaseURL: "https://primary.example.test", SourceRevision: strings.Repeat("a", 40), DisableForgejoProjectionWorker: true}
	actor := db.User{Login: "example-owner", Type: db.TypeUser, Name: "Fixture", UserKind: db.UserKindHuman, Status: db.UserStatusActive}
	if err = database.Create(&actor).Error; err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithUser(context.Background(), actor)
	repo, err := svc.CreateRepo(ctx, CreateRepoInput{OwnerLogin: actor.Login, Name: "demo", AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = git.CreateBranch(ctx, repo.FullName, "agent/intent-test", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err = git.WriteFile(ctx, repo.FullName, "agent/intent-test", "feature.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatal(err)
	}
	if _, err = git.WriteFile(ctx, repo.FullName, "main", "base.txt", "base advanced", []byte("base\n")); err != nil {
		t.Fatal(err)
	}
	pr, err := svc.CreatePR(ctx, CreatePRInput{RepoFullName: repo.FullName, Title: "Interrupted projection", HeadRef: "agent/intent-test", BaseRef: "main", AuthorLogin: actor.Login})
	if err != nil {
		t.Fatal(err)
	}
	pr, err = svc.GetPR(ctx, repo.FullName, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	projection := db.PullRequestProjection{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, ExternalRepo: "forgejo/repo", ExternalNumber: 42, SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: ProjectionStateOpen, LastSyncedSHA: pr.HeadSHA}
	if err = database.Create(&projection).Error; err != nil {
		t.Fatal(err)
	}
	client := &minimalForgejoClient{pr: forgejointegration.PullRequestSnapshot{Number: 42, State: "open", HeadRef: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef}, remoteRefs: map[string]string{pr.HeadRef: pr.HeadSHA, pr.BaseRef: pr.BaseSHA}}
	return svc, ctx, pr, projection, client
}

func fixtureReadIntegration(svc *Service, client *minimalForgejoClient, read func(context.Context, string) (string, error), pushes *int, effects ...func(context.Context, forgejointegration.PushRequest) error) {
	enabled := true
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo.example.test", Token: "fixture-only", AutoPullRequest: true, MirrorBranchIncludes: []string{"main", "agent/*"}, PRBranchIncludes: []string{"agent/*"}, RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "repo", Enabled: &enabled}}}, client, func(ctx context.Context, request forgejointegration.PushRequest) error {
		(*pushes)++
		if len(effects) > 0 {
			return effects[0](ctx, request)
		}
		return nil
	}, func(ctx context.Context, _, _, ref string) (string, error) {
		return read(ctx, strings.TrimPrefix(ref, "refs/heads/"))
	})
}

func TestActionObservationUnavailablePreservesAuthorityAndUnderlyingCause(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, errors.New("private provider failure")} {
		t.Run(cause.Error(), func(t *testing.T) {
			svc, ctx, pr, projection, client := interruptionFixture(t)
			intent := seedDispatchedIntent(t, svc, pr, projection)
			writes := 0
			fixtureReadIntegration(svc, client, func(context.Context, string) (string, error) { return "", cause }, &writes)
			err := svc.revalidateCurrentDelegatedProviderWrite(contextWithForgejoActionIntent(ctx, intent.ID), projection.ExternalRepo, projection.ExternalNumber)
			if !IsForgejoActionObservationUnavailable(err) || !errors.Is(err, cause) || errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
				t.Fatalf("read failure became authority denial: %v", err)
			}
			var current db.PullRequestActionIntent
			if err = svc.DB.First(&current, "id = ?", intent.ID).Error; err != nil {
				t.Fatal(err)
			}
			if current.State != intent.State || writes != 0 || client.addCalls+client.removeCalls+client.commentCalls != 0 {
				t.Fatal("unavailable read mutated authority or provider")
			}
		})
	}
}

func TestActionConcreteHeadDriftStillTerminallyDenies(t *testing.T) {
	svc, ctx, pr, projection, client := interruptionFixture(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	writes := 0
	fixtureReadIntegration(svc, client, func(_ context.Context, ref string) (string, error) {
		if ref == pr.HeadRef {
			return strings.Repeat("f", 40), nil
		}
		return client.remoteRefs[ref], nil
	}, &writes)
	err := svc.revalidateCurrentDelegatedProviderWrite(contextWithForgejoActionIntent(ctx, intent.ID), projection.ExternalRepo, 42)
	if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) || IsForgejoActionObservationUnavailable(err) {
		t.Fatal("true drift was made retryable", err)
	}
	var current db.PullRequestActionIntent
	if err = svc.DB.First(&current, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.State != ForgejoActionIntentDenied || writes != 0 {
		t.Fatal("true drift was not denied before writes")
	}
}

func TestWebhookCanceledAfterNativeRebaseRecordsSameGenerationWithoutProviderWrites(t *testing.T) {
	svc, ctx, pr, projection, client := interruptionFixture(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	writes := 0
	interrupted := false
	fixtureReadIntegration(svc, client, func(call context.Context, ref string) (string, error) {
		var job db.PullRequestProjectionJob
		if err := svc.DB.Where("pull_request_id = ?", pr.ID).First(&job).Error; err == nil && job.Phase == ForgejoProjectionPhasePushingRef {
			interrupted = true
			cancel()
			return "", call.Err()
		}
		return client.remoteRefs[ref], nil
	}, &writes)
	event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: projection.ExternalRepo, PRNumber: 42, HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef, LabelName: forgejointegration.AGSActionRebaseLabel}
	result, err := svc.handleForgejoPullRequestActionLabel(ctx, event)
	if !interrupted || !errors.Is(err, context.Canceled) || result.WorkflowStatus != "recovery_needed" {
		t.Fatalf("interruption not captured: %+v %v", result, err)
	}
	var job db.PullRequestProjectionJob
	if err = svc.DB.Where("pull_request_id = ?", pr.ID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	var current db.PullRequestActionIntent
	if err = svc.DB.First(&current, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	head, err := svc.Git.HeadSHA(context.Background(), pr.Repository.FullName, pr.HeadRef)
	if err != nil {
		t.Fatal(err)
	}
	if head == pr.HeadSHA || head != job.DesiredAGSHeadSHA || head != result.SyncedSHA || job.ExpectedForgejoOldHeadSHA != pr.HeadSHA {
		t.Fatal("native outcome or old lease lost")
	}
	if job.Phase != ForgejoProjectionPhaseFailedRetryable || job.LastErrorType != "action_observation_unavailable" || current.State != ForgejoActionIntentRecovery || !current.ExpiresAt.Equal(intent.ExpiresAt) {
		t.Fatalf("not saved locally without extending authorization: %+v %+v", job, current)
	}
	if writes != 0 || client.remoteRefs[pr.HeadRef] != pr.HeadSHA {
		t.Fatal("cleanup performed provider write")
	}
	summary, found, err := svc.PullRequestProjectionJob(context.Background(), pr.ID)
	if err != nil || !found || summary.Trigger != ForgejoProjectionTriggerActionRebase || summary.Status != "retryable_failed" || summary.NextRepairAction != "call_projection_retry" {
		t.Fatal("PR presentation hid action or claimed a scheduled worker", summary, err)
	}
	// Recover the SAME durable generation through the existing routine. The
	// provider accepts only the saved old-head lease; no new native rebase.
	fixtureReadIntegration(svc, client, func(_ context.Context, ref string) (string, error) { return client.remoteRefs[ref], nil }, &writes, func(call context.Context, request forgejointegration.PushRequest) error {
		if request.ForceWithLeaseRef != "refs/heads/"+pr.HeadRef || request.ForceWithLeaseSHA != pr.HeadSHA {
			return errors.New("incorrect recovery lease")
		}
		actual, err := svc.Git.HeadSHA(call, pr.Repository.FullName, pr.HeadRef)
		if err != nil {
			return err
		}
		if actual != head {
			return errors.New("native rebase was repeated")
		}
		client.remoteRefs[pr.HeadRef] = actual
		client.pr.HeadSHA = actual
		return nil
	})
	fresh, err := svc.GetPR(context.Background(), pr.Repository.FullName, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.resumeForgejoActionRebaseJob(context.Background(), event, fresh, job, ForgejoWebhookResult{})
	if err != nil || recovered.WorkflowStatus != "rebased" || writes != 1 {
		t.Fatalf("same generation failed to recover: %+v %v writes=%d", recovered, err, writes)
	}
	finalHead, err := svc.Git.HeadSHA(context.Background(), pr.Repository.FullName, pr.HeadRef)
	if err != nil || finalHead != head {
		t.Fatal("resume changed native rebase result", err)
	}
	// This is a durable recovery hint, not a new task or a new rebase.
	count := int64(0)
	if err = svc.DB.Model(&db.PullRequestProjectionJob{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("cleanup created another job", count, err)
	}
}

func TestInterruptionCleanupDoesNotResurrectTerminalOrReplacementGeneration(t *testing.T) {
	for _, mode := range []string{"denied", "completed", "new_generation", "expired"} {
		t.Run(mode, func(t *testing.T) {
			svc, ctx, pr, projection, _ := interruptionFixture(t)
			intent := seedDispatchedIntent(t, svc, pr, projection)
			id := intent.ID
			job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo, Trigger: ForgejoProjectionTriggerActionRebase, ActionGeneration: 1, ActionIntentID: &id, AGSPRNumber: pr.Number, ExternalRepo: projection.ExternalRepo, ExternalNumber: 42, Phase: ForgejoProjectionPhasePushingRef, DesiredAGSHeadSHA: pr.HeadSHA, ExpectedForgejoOldHeadSHA: pr.HeadSHA}
			if err := svc.DB.Create(&job).Error; err != nil {
				t.Fatal(err)
			}
			ctx = contextWithForgejoActionJob(ctx, job)
			switch mode {
			case "denied":
				if err := svc.DB.Model(&intent).Update("state", ForgejoActionIntentDenied).Error; err != nil {
					t.Fatal(err)
				}
			case "completed":
				if err := svc.DB.Model(&intent).Update("state", ForgejoActionIntentCompleted).Error; err != nil {
					t.Fatal(err)
				}
			case "new_generation":
				if err := svc.DB.Model(&job).Update("action_generation", 2).Error; err != nil {
					t.Fatal(err)
				}
			case "expired":
				if err := svc.DB.Model(&intent).Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
					t.Fatal(err)
				}
			}
			var before db.PullRequestActionIntent
			if err := svc.DB.First(&before, "id = ?", intent.ID).Error; err != nil {
				t.Fatal(err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			changed, err := svc.recordForgejoActionInterruption(canceled)
			if err != nil {
				t.Fatal(err)
			}
			var after db.PullRequestActionIntent
			if err := svc.DB.First(&after, "id = ?", intent.ID).Error; err != nil {
				t.Fatal(err)
			}
			if mode != "expired" && (changed || after.State != before.State) {
				t.Fatal("late cleanup resurrected terminal/new generation")
			}
			if !after.ExpiresAt.Equal(before.ExpiresAt) {
				t.Fatal("cleanup extended authorization")
			}
			if mode == "expired" && (!changed || after.State != ForgejoActionIntentRecovery) {
				t.Fatal("lost local evidence of expired interrupted action")
			}
		})
	}
}

func TestUnavailableClassificationNeverOverridesKnownDenial(t *testing.T) {
	err := fmt.Errorf("%w: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch), context.DeadlineExceeded)
	if IsForgejoActionObservationUnavailable(err) {
		t.Fatal("known denial made recoverable")
	}
}
