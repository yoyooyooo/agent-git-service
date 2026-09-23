package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestHumanProviderMergeUsesAGSMappingAndServerExecutor(t *testing.T) {
	svc, _, mergeInput, provider := setupAccessGrantMerge(t, "success")
	human := db.User{
		Login: "human-provider-merge", Name: "Human Provider Merge", Type: db.TypeUser,
		Status: "active", UserKind: db.UserKindHuman, DefaultRepositoryPermission: "none",
	}
	if err := svc.DB.Create(&human).Error; err != nil {
		t.Fatal(err)
	}
	var repository db.Repository
	if err := svc.DB.First(&repository, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Create(&db.Collaborator{RepositoryID: repository.ID, UserID: human.ID, Permission: "admin"}).Error; err != nil {
		t.Fatal(err)
	}

	receipt, err := svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), human), repository.FullName, mergeInput.AGSPRNumber, service.HumanProviderMergeInput{
		ExpectedHeadSHA: mergeInput.ExpectedHeadSHA,
		MergeMethod:     mergeInput.MergeMethod,
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Schema != "ags.provider-merge-receipt.v1" || receipt.ActorID != human.ID || receipt.ActorLogin != human.Login ||
		receipt.AGSPRNumber != mergeInput.AGSPRNumber || receipt.ProviderPRNumber != mergeInput.ProviderPRNumber ||
		receipt.ProviderPRNumber == receipt.AGSPRNumber || !receipt.ProviderMerged || receipt.ProviderMergeSHA != mergeInput.ExpectedHeadSHA ||
		receipt.ProjectionStatus != "pending" {
		t.Fatalf("receipt=%#v", receipt)
	}
	// A retry while the provider webhook is still converging reads the exact
	// divergent provider coordinate and does not send another merge POST.
	repeated, err := svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), human), repository.FullName, mergeInput.AGSPRNumber, service.HumanProviderMergeInput{
		ExpectedHeadSHA: mergeInput.ExpectedHeadSHA,
		MergeMethod:     mergeInput.MergeMethod,
	})
	if err != nil || !repeated.ProviderMerged || repeated.ProjectionStatus != "pending" {
		t.Fatalf("repeated=%#v err=%v", repeated, err)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider merge calls=%d", calls)
	}
}

func TestHumanProviderMergeUsesAuthoritativeNonDefaultPRBase(t *testing.T) {
	svc, _, mergeInput, provider := setupAccessGrantMerge(t, "success")
	human := db.User{
		Login: "human-provider-merge-dynamic-base", Name: "Human Dynamic Base", Type: db.TypeUser,
		Status: "active", UserKind: db.UserKindHuman, DefaultRepositoryPermission: "none",
	}
	if err := svc.DB.Create(&human).Error; err != nil {
		t.Fatal(err)
	}
	var repository db.Repository
	if err := svc.DB.First(&repository, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Create(&db.Collaborator{RepositoryID: repository.ID, UserID: human.ID, Permission: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	const baseRef = "conformance/c3/run/rebase/base"
	if err := svc.Git.CreateBranch(context.Background(), repository.FullName, baseRef, "main"); err != nil {
		t.Fatal(err)
	}
	baseSHA, err := svc.Git.HeadSHA(context.Background(), repository.FullName, baseRef)
	if err != nil {
		t.Fatal(err)
	}
	var pr db.PullRequest
	if err := svc.DB.First(&pr, "repository_id = ? AND number = ?", repository.ID, mergeInput.AGSPRNumber).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&pr).Updates(map[string]any{"base_ref": baseRef, "base_sha": baseSHA}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ?", pr.ID).Update("target_branch", baseRef).Error; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.snapshot.BaseRef = baseRef
	provider.snapshot.BaseSHA = baseSHA
	provider.mu.Unlock()

	receipt, err := svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), human), repository.FullName, mergeInput.AGSPRNumber, service.HumanProviderMergeInput{
		ExpectedHeadSHA: mergeInput.ExpectedHeadSHA,
		MergeMethod:     mergeInput.MergeMethod,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.ProviderMerged || receipt.ProviderMergeSHA != mergeInput.ExpectedHeadSHA || receipt.MergeMethod != "rebase" {
		t.Fatalf("receipt=%#v", receipt)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider merge calls=%d", calls)
	}
}

func TestHumanProviderMergeDoesNotBlockOnPendingCI(t *testing.T) {
	svc, _, mergeInput, provider := setupAccessGrantMerge(t, "success")
	provider.mu.Lock()
	provider.runs = nil
	provider.mu.Unlock()

	human := db.User{
		Login: "human-provider-merge-pending-ci", Name: "Human Pending CI", Type: db.TypeUser,
		Status: "active", UserKind: db.UserKindHuman, DefaultRepositoryPermission: "none",
	}
	if err := svc.DB.Create(&human).Error; err != nil {
		t.Fatal(err)
	}
	var repository db.Repository
	if err := svc.DB.First(&repository, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Create(&db.Collaborator{RepositoryID: repository.ID, UserID: human.ID, Permission: "admin"}).Error; err != nil {
		t.Fatal(err)
	}

	receipt, err := svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), human), repository.FullName, mergeInput.AGSPRNumber, service.HumanProviderMergeInput{
		ExpectedHeadSHA: mergeInput.ExpectedHeadSHA,
		MergeMethod:     mergeInput.MergeMethod,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.ProviderMerged || receipt.ProviderMergeSHA != mergeInput.ExpectedHeadSHA {
		t.Fatalf("receipt=%#v", receipt)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider merge calls=%d", calls)
	}
}

func TestHumanProviderMergeRejectsAgentIdentityBeforeProviderMutation(t *testing.T) {
	svc, _, mergeInput, provider := setupAccessGrantMerge(t, "success")
	agent := db.User{Login: "not-human-merge", Name: "Not Human", Type: db.TypeUser, Status: "active", UserKind: db.UserKindAgent}
	if err := svc.DB.Create(&agent).Error; err != nil {
		t.Fatal(err)
	}
	var repository db.Repository
	if err := svc.DB.First(&repository, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Create(&db.Collaborator{RepositoryID: repository.ID, UserID: agent.ID, Permission: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	_, err := svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), agent), repository.FullName, mergeInput.AGSPRNumber, service.HumanProviderMergeInput{
		ExpectedHeadSHA: mergeInput.ExpectedHeadSHA,
		MergeMethod:     mergeInput.MergeMethod,
	})
	if !errors.Is(err, service.ErrForbidden) {
		t.Fatalf("err=%v", err)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider merge calls=%d", calls)
	}
}
