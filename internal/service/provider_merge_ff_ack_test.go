package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestHumanProviderMergePreflightComparesForgejoRefNotAGSBase(t *testing.T) {
	svc, _, mergeInput, provider := setupAccessGrantMerge(t, "success")
	human := db.User{
		Login: "human-split-base", Name: "Human Split Base", Type: db.TypeUser,
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
	agsBase, err := svc.Git.HeadSHA(context.Background(), repository.FullName, "main")
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("ab", 20)
	provider.mu.Lock()
	provider.snapshot.BaseSHA = other
	provider.mu.Unlock()

	_, err = svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), human), repository.FullName, mergeInput.AGSPRNumber, service.HumanProviderMergeInput{
		ExpectedHeadSHA: mergeInput.ExpectedHeadSHA,
		MergeMethod:     mergeInput.MergeMethod,
	})
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(err.Error(), "ags_base="+agsBase) || !strings.Contains(err.Error(), "pr_base="+other) {
		t.Fatalf("drift error missing sides: %v", err)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider merge calls=%d", calls)
	}
}

func TestHumanProviderMergeFastForwardAckWhenProviderRefEqualsHead(t *testing.T) {
	svc, _, mergeInput, provider := setupAccessGrantMerge(t, "success")
	human := db.User{
		Login: "human-ff-ack", Name: "Human FF Ack", Type: db.TypeUser,
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
	agsBase, err := svc.Git.HeadSHA(context.Background(), repository.FullName, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Git.CreateBranch(context.Background(), repository.FullName, "feature/ff-ack", "main"); err != nil {
		t.Fatal(err)
	}
	headSHA, err := svc.Git.WriteFile(context.Background(), repository.FullName, "feature/ff-ack", "ff.txt", "ff", []byte("ff\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.PullRequest{}).Where("repository_id = ? AND number = ?", repository.ID, mergeInput.AGSPRNumber).
		Updates(map[string]any{"head_ref": "feature/ff-ack", "head_sha": headSHA}).Error; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.snapshot.HeadRef = "feature/ff-ack"
	provider.snapshot.HeadSHA = headSHA
	provider.snapshot.BaseSHA = headSHA
	provider.snapshot.Mergeable = false
	provider.mu.Unlock()
	enabled := true
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.invalid", Token: "server-owned-token",
		AuthorityPolicyEnabled: true, WebhookURL: "http://ags.invalid/webhook", IntegrationBot: "ags-bot",
		RepoMap: map[string]forgejointegration.RepoMapping{
			repository.FullName: {Owner: "forgejo", Repo: "demo", Enabled: &enabled, BaseBranch: "main", DelegatedMergeMethod: "fast-forward-only", FastForwardAck: true},
		},
	}, provider, func(context.Context, forgejointegration.PushRequest) error { return nil }, func(context.Context, string, string, string) (string, error) {
		return headSHA, nil
	})

	receipt, err := svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), human), repository.FullName, mergeInput.AGSPRNumber, service.HumanProviderMergeInput{
		ExpectedHeadSHA: headSHA,
		MergeMethod:     "fast-forward-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ProviderMerged || receipt.Outcome != "source_success" || receipt.ProjectionStatus != "provider_pr_pending" || receipt.ProviderMergeSHA != headSHA {
		t.Fatalf("receipt=%#v", receipt)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider merge POST calls=%d", calls)
	}
	gotMain, err := svc.Git.HeadSHA(context.Background(), repository.FullName, "main")
	if err != nil || gotMain != headSHA {
		t.Fatalf("ags main=%s want %s err=%v", gotMain, headSHA, err)
	}
	var pr db.PullRequest
	if err := svc.DB.First(&pr, "repository_id = ? AND number = ?", repository.ID, mergeInput.AGSPRNumber).Error; err != nil {
		t.Fatal(err)
	}
	if !pr.Merged || pr.MergeCommitSHA != headSHA {
		t.Fatalf("pr=%#v", pr)
	}
	var projection db.PullRequestProjection
	if err := svc.DB.Where("pull_request_id = ?", pr.ID).First(&projection).Error; err != nil {
		t.Fatal(err)
	}
	if projection.State != service.ProjectionStateOpen {
		t.Fatalf("provider projection closed while Forgejo PR still open: %#v", projection)
	}
	if agsBase == headSHA {
		t.Fatal("expected AGS base to advance")
	}
}

func TestHumanProviderMergeFastForwardAckKeepsAdvancedMain(t *testing.T) {
	svc, _, mergeInput, provider := setupAccessGrantMerge(t, "success")
	human := addHumanMergeActor(t, svc, "human-ff-keep")
	var repository db.Repository
	if err := svc.DB.First(&repository, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.Git.CreateBranch(context.Background(), repository.FullName, "feature/ff-keep", "main"); err != nil {
		t.Fatal(err)
	}
	headSHA, err := svc.Git.WriteFile(context.Background(), repository.FullName, "feature/ff-keep", "keep.txt", "keep", []byte("keep\n"))
	if err != nil {
		t.Fatal(err)
	}
	repoPath, err := svc.Git.GetRepoPath(context.Background(), repository.FullName)
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(runAccessGrantMergeGit(t, repoPath, "rev-parse", headSHA+"^{tree}"))
	advanced := strings.TrimSpace(runAccessGrantMergeGit(t, repoPath, "commit-tree", tree, "-p", headSHA, "-m", "main continued"))
	currentMain, err := svc.Git.HeadSHA(context.Background(), repository.FullName, "main")
	if err != nil {
		t.Fatal(err)
	}
	runAccessGrantMergeGit(t, repoPath, "update-ref", "refs/heads/main", advanced, currentMain)
	if err := svc.DB.Model(&db.PullRequest{}).Where("repository_id = ? AND number = ?", repository.ID, mergeInput.AGSPRNumber).
		Updates(map[string]any{"head_ref": "feature/ff-keep", "head_sha": headSHA}).Error; err != nil {
		t.Fatal(err)
	}
	bindFastForwardAck(t, svc, provider, repository.FullName, headSHA)

	receipt, err := svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), human), repository.FullName, mergeInput.AGSPRNumber, service.HumanProviderMergeInput{
		ExpectedHeadSHA: headSHA,
		MergeMethod:     "fast-forward-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ProviderMerged || receipt.Outcome != "source_success" || receipt.ProviderMergeSHA != headSHA {
		t.Fatalf("receipt=%#v", receipt)
	}
	gotMain, err := svc.Git.HeadSHA(context.Background(), repository.FullName, "main")
	if err != nil || gotMain != advanced {
		t.Fatalf("ags main=%s want %s err=%v", gotMain, advanced, err)
	}
}

func TestHumanProviderMergeObserveDoesNotPost(t *testing.T) {
	svc, _, mergeInput, provider := setupAccessGrantMerge(t, "success")
	human := addHumanMergeActor(t, svc, "human-observe")
	var repository db.Repository
	if err := svc.DB.First(&repository, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.ObserveHumanProviderMerge(service.ContextWithUser(context.Background(), human), repository.FullName, mergeInput.AGSPRNumber, mergeInput.ExpectedHeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Outcome != "recovery_needed" || receipt.ProviderMerged {
		t.Fatalf("receipt=%#v", receipt)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider merge POST calls=%d", calls)
	}
}

func TestHumanProviderMergeDoesNotAckWhenFlagDisabled(t *testing.T) {
	svc, _, mergeInput, provider := setupAccessGrantMerge(t, "success")
	human := addHumanMergeActor(t, svc, "human-no-ack")
	var repository db.Repository
	if err := svc.DB.First(&repository, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.snapshot.Mergeable = false
	provider.remoteSHA = mergeInput.ExpectedHeadSHA
	provider.snapshot.BaseSHA = mergeInput.ExpectedHeadSHA
	provider.mu.Unlock()
	_, err := svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), human), repository.FullName, mergeInput.AGSPRNumber, service.HumanProviderMergeInput{
		ExpectedHeadSHA: mergeInput.ExpectedHeadSHA,
		MergeMethod:     mergeInput.MergeMethod,
	})
	if err == nil {
		t.Fatal("expected mergeable denial without fast-forward ack")
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider merge POST calls=%d", calls)
	}
}

func TestAccessGrantPRMergeFastForwardAckWhenProviderRefEqualsHead(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
	var repository db.Repository
	if err := svc.DB.First(&repository, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.snapshot.Mergeable = false
	provider.remoteSHA = input.ExpectedHeadSHA
	provider.snapshot.BaseSHA = input.ExpectedHeadSHA
	provider.mu.Unlock()
	bindFastForwardAck(t, svc, provider, repository.FullName, input.ExpectedHeadSHA)
	input.MergeMethod = "fast-forward-only"

	receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ProviderMerged || receipt.ProviderAttempt != "not_attempted" || receipt.ProviderOutcome != "source_success" || receipt.ProviderMergeSHA != input.ExpectedHeadSHA {
		t.Fatalf("receipt=%#v", receipt)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider merge POST calls=%d", calls)
	}
	var pr db.PullRequest
	if err := svc.DB.First(&pr, "repository_id = ? AND number = ?", repository.ID, input.AGSPRNumber).Error; err != nil {
		t.Fatal(err)
	}
	if !pr.Merged || pr.MergeCommitSHA != input.ExpectedHeadSHA {
		t.Fatalf("pr=%#v", pr)
	}
	var projection db.PullRequestProjection
	if err := svc.DB.Where("pull_request_id = ?", pr.ID).First(&projection).Error; err != nil {
		t.Fatal(err)
	}
	if projection.State != service.ProjectionStateOpen {
		t.Fatalf("provider projection closed while Forgejo PR still open: %#v", projection)
	}
}

func addHumanMergeActor(t *testing.T, svc *service.Service, login string) db.User {
	t.Helper()
	human := db.User{
		Login: login, Name: login, Type: db.TypeUser,
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
	return human
}

func bindFastForwardAck(t *testing.T, svc *service.Service, provider *accessGrantMergeProvider, repository, headSHA string) {
	t.Helper()
	enabled := true
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.invalid", Token: "server-owned-token",
		AuthorityPolicyEnabled: true, WebhookURL: "http://ags.invalid/webhook", IntegrationBot: "ags-bot",
		RepoMap: map[string]forgejointegration.RepoMapping{
			repository: {Owner: "forgejo", Repo: "demo", Enabled: &enabled, BaseBranch: "main", DelegatedMergeMethod: "fast-forward-only", FastForwardAck: true},
		},
	}, provider, func(context.Context, forgejointegration.PushRequest) error { return nil }, func(context.Context, string, string, string) (string, error) {
		return headSHA, nil
	})
}
