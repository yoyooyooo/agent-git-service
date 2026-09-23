package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

type presentationFailureForgejoClient struct{}

func (presentationFailureForgejoClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}

func (presentationFailureForgejoClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}

func (presentationFailureForgejoClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}

func (presentationFailureForgejoClient) AddIssueLabels(ctx context.Context, owner, repo string, issueNumber int, labels []string) error {
	return errors.New("label presentation unavailable")
}

func (presentationFailureForgejoClient) RemoveIssueLabel(ctx context.Context, owner, repo string, issueNumber int, label string) error {
	return errors.New("label presentation unavailable")
}

func (presentationFailureForgejoClient) CreateIssueComment(ctx context.Context, owner, repo string, issueNumber int, body string) error {
	return errors.New("comment presentation unavailable")
}

func TestGitLSRemoteSHAForRefIgnoresWarningsAndOtherRefs(t *testing.T) {
	output := "warning: redirected to canonical endpoint\n1111111111111111111111111111111111111111\trefs/heads/other\n2222222222222222222222222222222222222222\trefs/heads/main\n"
	sha, ok := gitLSRemoteSHAForRef(output, "refs/heads/main")
	if !ok || sha != "2222222222222222222222222222222222222222" {
		t.Fatalf("sha=%q ok=%v", sha, ok)
	}
	if sha, ok := gitLSRemoteSHAForRef(output, "refs/heads/missing"); ok || sha != "" {
		t.Fatalf("missing sha=%q ok=%v", sha, ok)
	}
}

func TestForgejoWebhookSignatureHeaderPrefersSHA256(t *testing.T) {
	headers := map[string][]string{
		"X-Hub-Signature":     {"sha1=old"},
		"X-Hub-Signature-256": {"sha256=new"},
	}
	if got := forgejoWebhookSignatureFromHeaders(headers); got != "sha256=new" {
		t.Fatalf("signature=%q", got)
	}
}

func TestForgejoActionProjectionPresentationFailurePreservesDurableFailure(t *testing.T) {
	svc := setupProjectionStateService(t)
	ctx := context.Background()
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatalf("load repo: %v", err)
	}
	pr := db.PullRequest{
		Number: 146, RepositoryID: repo.ID, Repository: repo, Title: "projection incident",
		State: db.StateOpen, HeadRef: "chore/sync-repo-flow-integrity-caca953c", HeadSHA: "a32d9c4774cd069c07bade74a7a0dda124a3a5d5", BaseRef: "main",
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create PR: %v", err)
	}
	if err := svc.DB.Create(&db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
		ExternalRepo: "example-team/shipping-fixture", ExternalNumber: 132,
		SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: ProjectionStateOpen,
		LastSyncedSHA: "3eb75932af7c91c3644ac999a80acb3dfcc064ad",
	}).Error; err != nil {
		t.Fatalf("create projection: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "token",
	}, presentationFailureForgejoClient{}, func(ctx context.Context, req forgejointegration.PushRequest) error { return nil })
	event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: "example-team/shipping-fixture", PRNumber: 132}
	providerErr := &forgejointegration.ProjectionError{
		Type: forgejointegration.ProjectionFailureNonFastForward, Ref: "refs/heads/" + pr.HeadRef, Branch: pr.HeadRef,
		ExpectedSHA: pr.HeadSHA, ActualSHA: "3eb75932af7c91c3644ac999a80acb3dfcc064ad", ErrorSummary: "remote head differs",
	}
	failure, err := svc.recordForgejoActionProjectionFailure(ctx, event, pr, pr.HeadSHA, PullRequestProviderResult{
		Provider: ProjectionProviderForgejo, Required: true, Attempted: true, DesiredSHA: pr.HeadSHA,
		ObservedSHA: providerErr.ActualSHA, Err: providerErr,
	}, "push")
	if err != nil {
		t.Fatalf("record durable failure: %v", err)
	}
	svc.markForgejoActionProjectionFailed(ctx, event, pr, failure, true)

	var eventCount int64
	if err := svc.DB.Model(&db.ProjectionEvent{}).Where("provider = ? AND repository_id = ? AND ref = ?", ProjectionProviderForgejo, repo.ID, providerErr.Ref).Count(&eventCount).Error; err != nil {
		t.Fatalf("count events: %v", err)
	}
	var state db.ProjectionRefState
	if err := svc.DB.Where("provider = ? AND repository_id = ? AND ref = ? AND status = ?", ProjectionProviderForgejo, repo.ID, providerErr.Ref, ProjectionStatusActive).First(&state).Error; err != nil {
		t.Fatalf("load active state: %v", err)
	}
	if eventCount != 1 || state.AGSSHA != "a32d9c4774cd069c07bade74a7a0dda124a3a5d5" || state.ExternalSHA != "3eb75932af7c91c3644ac999a80acb3dfcc064ad" {
		t.Fatalf("presentation failure changed durable facts: events=%d state=%#v", eventCount, state)
	}
}
