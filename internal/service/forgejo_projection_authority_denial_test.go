package service

import (
	"errors"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
)

func TestAuthorityBoundaryReceiptPayloadRejectsSecretShapes(t *testing.T) {
	for _, encoded := range []string{
		`{"session_token":"value"}`,
		`{"subject":"ags_sess_forbidden"}`,
		`{"subject":"-----BEGIN PRIVATE KEY-----"}`,
		`{"subject":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhZ2VudCJ9.signature"}`,
	} {
		if err := validateSecretSafeReceiptJSON([]byte(encoded)); !errors.Is(err, ErrAuthorityBoundaryReceiptSecret) {
			t.Fatalf("secret-shaped payload accepted: %s err=%v", encoded, err)
		}
	}
}

func TestTerminalProviderAdmissionDenialAlsoTerminalDeniesOriginatingIntent(t *testing.T) {
	svc, pr, _ := setupIntentTestService(t)
	sessionID := "session-provider-admission-denied"
	intent := db.PullRequestActionIntent{
		ID: "intent-provider-admission-denied", IdempotencyKey: "provider-admission-denied", Action: "pr.rebase", State: ForgejoActionIntentRunning,
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AGSPRNumber: pr.Number, Repository: pr.Repository.FullName,
		ForgejoRepo: "example-owner/demo", ForgejoPRNumber: 42, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
		ExpectedHeadSHA: pr.HeadSHA, ExpectedBaseSHA: pr.BaseSHA, ExpectedLabels: "[]", PostLabels: "[]",
		PrincipalID: pr.AuthorID, AgentSessionID: &sessionID, ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	if err := svc.DB.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	intentID := intent.ID
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AgentSessionID: &sessionID, ActionIntentID: &intentID,
		Provider: ProjectionProviderForgejo, Trigger: ForgejoProjectionTriggerActionRebase, ActionGeneration: 1, RepoFullName: pr.Repository.FullName,
		AGSPRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA, Phase: ForgejoProjectionPhasePushingRef,
	}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	failure := newProjectionJobFailure("refs/heads/"+pr.HeadRef, pr.HeadSHA, delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged), false)
	if failure == nil || !errors.Is(failure.err, ErrDelegatedSessionUseTimeDenied) {
		t.Fatalf("failure=%#v", failure)
	}
	svc.markForgejoProjectionJobFailed(contextWithForgejoActionJob(t.Context(), job), job.ID, ForgejoProjectionPhaseFailedTerminal, nil, failure)
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || intent.FailureCode != DelegatedDenialAuthoritySnapshotChanged || intent.FinishedAt == nil {
		t.Fatalf("intent not terminal denied: %#v", intent)
	}
}
