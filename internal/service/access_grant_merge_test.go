package service_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/gorm"
)

type accessGrantMergeProvider struct {
	mu                  sync.Mutex
	database            *gorm.DB
	snapshot            forgejointegration.PullRequestSnapshot
	remoteSHA           string
	runs                []forgejointegration.WorkflowRun
	authority           forgejointegration.RepositoryAuthorityState
	mergeMode           string
	hideMergedReads     int
	mergeableAfterReads int
	snapshotReads       int
	mergeCalls          int
	dispatchObserved    bool
}

func (*accessGrantMergeProvider) EnsureRepository(context.Context, string, string, bool) error {
	return nil
}

func (*accessGrantMergeProvider) EnsurePullRequest(context.Context, forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}

func (*accessGrantMergeProvider) UpdatePullRequestState(context.Context, string, string, int, string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}

func (p *accessGrantMergeProvider) GetPullRequest(context.Context, string, string, int) (forgejointegration.PullRequestSnapshot, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshotReads++
	if !p.snapshot.Mergeable && p.mergeableAfterReads > 0 && p.snapshotReads >= p.mergeableAfterReads {
		p.snapshot.Mergeable = true
	}
	snapshot := p.snapshot
	if p.mergeMode == "lost_response" && snapshot.Merged && p.hideMergedReads > 0 {
		p.hideMergedReads--
		snapshot.State = "open"
		snapshot.Merged = false
		snapshot.MergeCommitSHA = ""
	}
	return snapshot, snapshot.Number > 0, nil
}

func (p *accessGrantMergeProvider) MergePullRequest(_ context.Context, _, _ string, _ int, request forgejointegration.PullRequestMergeRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mergeCalls++
	var count int64
	if p.database != nil && p.database.Model(&db.AccessGrantInvocation{}).
		Where("operation = ? AND state = ? AND provider_attempt = ? AND provider_outcome = ?", "pr.merge", service.ForgejoActionIntentDispatching, "attempted", "outcome_unknown").
		Count(&count).Error == nil && count == 1 {
		p.dispatchObserved = true
	}
	if request.ExpectedHead != p.snapshot.HeadSHA || request.Method != "rebase" || request.DeleteBranch {
		return errors.New("noncanonical merge payload")
	}
	if p.mergeMode == "success" {
		p.snapshot.State = "closed"
		p.snapshot.Merged = true
		p.snapshot.MergeCommitSHA = p.snapshot.HeadSHA
		return nil
	}
	if p.mergeMode == "lost_response" {
		// The provider committed the merge, but the caller observed only a
		// transport error. Recovery must discover this through the exact GET.
		p.snapshot.State = "closed"
		p.snapshot.Merged = true
		p.snapshot.MergeCommitSHA = p.snapshot.HeadSHA
		// Integration performs six bounded readbacks after a provider error;
		// hide those observations to model a genuinely lost response.
		p.hideMergedReads = 6
		return errors.New("simulated lost provider response")
	}
	return errors.New("simulated provider transport failure")
}

func (p *accessGrantMergeProvider) ListWorkflowRuns(context.Context, string, string, int) ([]forgejointegration.WorkflowRun, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]forgejointegration.WorkflowRun(nil), p.runs...), nil
}

func (p *accessGrantMergeProvider) InspectRepositoryAuthority(context.Context, string, string, string, string, string) (forgejointegration.RepositoryAuthorityState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.authority, nil
}

func (*accessGrantMergeProvider) ApplyRepositoryAuthority(context.Context, string, string, string, string, string, string, forgejointegration.RepositoryAuthorityState) error {
	return nil
}

func TestAccessGrantPRMergePersistsUnknownBeforePostAndNeverRepeatsIt(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "unknown")
	receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ID != input.InvocationID || receipt.State != service.ForgejoActionIntentRecovery || receipt.ProviderAttempt != "attempted" || receipt.ProviderOutcome != "outcome_unknown" || receipt.ProviderMerged {
		t.Fatalf("receipt=%#v", receipt)
	}
	provider.mu.Lock()
	calls, dispatchObserved := provider.mergeCalls, provider.dispatchObserved
	provider.mu.Unlock()
	if calls != 1 || !dispatchObserved {
		t.Fatalf("provider calls=%d dispatch_observed=%v", calls, dispatchObserved)
	}

	// Repeating the effect endpoint after the unknown boundary is GET-only.
	repeated, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil || repeated.State != service.ForgejoActionIntentRecovery {
		t.Fatalf("repeated=%#v err=%v", repeated, err)
	}
	provider.mu.Lock()
	calls = provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("unknown provider POST repeated: %d", calls)
	}

	// A renewed grant cannot claim and repeat the same globally exact effect.
	renewed, err := svc.RenewAccessGrant(context.Background(), grantToken, service.AccessGrantRenewInput{ExecutionContext: accessGrantExecutionContextInput()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ExecuteAccessGrantPRMerge(context.Background(), renewed.GrantToken, input); !errors.Is(err, service.ErrAccessGrantDenied) || errors.Is(err, service.ErrAccessGrantConflict) {
		t.Fatalf("renewed grant must not receive a post-dispatch conflict: %v", err)
	}
	provider.mu.Lock()
	provider.snapshot.State = "closed"
	provider.snapshot.Merged = true
	provider.snapshot.MergeCommitSHA = provider.snapshot.HeadSHA
	provider.mu.Unlock()

	// A renewed descendant and the revoked originating token both retain the
	// same GET-only recovery locator; neither path can dispatch another POST.
	reconciled, err := svc.GetAccessGrantInvocation(context.Background(), renewed.GrantToken, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.State != service.ForgejoActionIntentCompleted || !reconciled.ProviderMerged || reconciled.ProviderOutcome != "reconciled_after_error" || !strings.EqualFold(reconciled.ProviderMergeSHA, input.ExpectedHeadSHA) {
		t.Fatalf("reconciled=%#v", reconciled)
	}
	fromOrigin, err := svc.GetAccessGrantInvocation(context.Background(), grantToken, receipt.ID)
	if err != nil || fromOrigin.ID != reconciled.ID || fromOrigin.ProviderOutcome != reconciled.ProviderOutcome {
		t.Fatalf("originating readback=%#v err=%v", fromOrigin, err)
	}
	if _, err := svc.GetAccessGrantInvocation(context.Background(), grantToken, strings.ToUpper(receipt.ID)); !errors.Is(err, service.ErrAccessGrantDenied) {
		t.Fatalf("noncanonical invocation locator readback err=%v", err)
	}
	provider.mu.Lock()
	calls = provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("GET recovery emitted provider POST: %d", calls)
	}
}

func TestAccessGrantPRMergeLostResponseRecoversSameGrantByGetOnly(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "lost_response")
	initial, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil || initial.State != service.ForgejoActionIntentRecovery || initial.ProviderOutcome != "outcome_unknown" {
		t.Fatalf("initial=%#v err=%v", initial, err)
	}

	recovered, err := svc.GetAccessGrantInvocation(context.Background(), grantToken, initial.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != service.ForgejoActionIntentCompleted || !recovered.ProviderMerged ||
		recovered.ProviderOutcome != "reconciled_after_error" || recovered.ProviderMergeSHA != input.ExpectedHeadSHA {
		t.Fatalf("recovered=%#v", recovered)
	}

	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("lost-response recovery emitted provider POSTs=%d", calls)
	}
}

func TestAccessGrantPRMergeRequiresCanonicalCallerOwnedInvocationID(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "unknown")
	for _, invalid := range []string{"", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", "not-a-uuid"} {
		candidate := input
		candidate.InvocationID = invalid
		if _, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, candidate); !errors.Is(err, service.ErrValidation) {
			t.Fatalf("invocation_id=%q err=%v", invalid, err)
		}
	}
	first, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil || first.ID != input.InvocationID {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	changedLocator := input
	changedLocator.InvocationID = uuid.NewString()
	if _, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, changedLocator); !errors.Is(err, service.ErrValidation) || errors.Is(err, service.ErrAccessGrantConflict) {
		t.Fatalf("dispatched effect with a different locator must not return conflict: %v", err)
	}
	changedFacts := input
	changedFacts.ProviderPRNumber++
	if _, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, changedFacts); !errors.Is(err, service.ErrValidation) || errors.Is(err, service.ErrAccessGrantConflict) {
		t.Fatalf("dispatched invocation with different exact facts must not return conflict: %v", err)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider writes=%d", calls)
	}
}

func TestAccessGrantPRMergeConcurrentSameEffectMakesOneProviderPost(t *testing.T) {
	for _, mode := range []string{"success", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			svc, grantToken, input, provider := setupAccessGrantMerge(t, mode)
			const callers = 8
			start := make(chan struct{})
			receipts := make(chan service.AccessGrantInvocationReceipt, callers)
			errs := make(chan error, callers)
			var wg sync.WaitGroup
			for range callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
					if err != nil {
						errs <- err
						return
					}
					receipts <- receipt
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			close(receipts)
			for err := range errs {
				t.Fatalf("concurrent exact-effect call err=%v", err)
			}
			count := 0
			for receipt := range receipts {
				count++
				if receipt.ID != input.InvocationID || receipt.GrantID == "" {
					t.Fatalf("concurrent receipt=%#v", receipt)
				}
			}
			if count != callers {
				t.Fatalf("concurrent receipts=%d want=%d", count, callers)
			}
			provider.mu.Lock()
			calls := provider.mergeCalls
			provider.mu.Unlock()
			if calls != 1 {
				t.Fatalf("concurrent provider POSTs=%d", calls)
			}
		})
	}
}

func TestAccessGrantPRMergeCompletedRequestIsIdempotent(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
	first, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != service.ForgejoActionIntentCompleted || !first.ProviderMerged || first.ProviderOutcome != "confirmed" || first.ProviderMergeSHA != input.ExpectedHeadSHA {
		t.Fatalf("first=%#v", first)
	}
	second, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil || second.ID != first.ID || second.State != service.ForgejoActionIntentCompleted {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("completed provider POST repeated: %d", calls)
	}
}

func TestAccessGrantPRMergePreDispatchMethodConflictIsTerminalAndGETtable(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
	input.MergeMethod = "fast-forward-only"

	receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ID != input.InvocationID || receipt.State != service.ForgejoActionIntentConflict ||
		receipt.AuthorizationOutcome != "denied" || receipt.ProviderAttempt != "not_attempted" ||
		receipt.ProviderOutcome != "not_attempted" || receipt.ProviderMerged || receipt.FinishedAt == nil ||
		receipt.DenialCode != "provider_merge_method_base_drift" || receipt.ProviderRepository != "forgejo/demo" ||
		receipt.EffectMethod != input.MergeMethod {
		t.Fatalf("terminal pre-dispatch receipt=%#v", receipt)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("pre-dispatch conflict emitted provider writes=%d", calls)
	}

	readback, err := svc.GetAccessGrantInvocation(context.Background(), grantToken, input.InvocationID)
	if err != nil || readback.ID != receipt.ID || readback.State != service.ForgejoActionIntentConflict ||
		readback.ProviderAttempt != "not_attempted" || readback.DenialCode != receipt.DenialCode {
		t.Fatalf("terminal GET readback=%#v err=%v", readback, err)
	}
	repeated, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil || repeated.ID != receipt.ID || repeated.State != service.ForgejoActionIntentConflict {
		t.Fatalf("repeated terminal request=%#v err=%v", repeated, err)
	}
	provider.mu.Lock()
	calls = provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("terminal conflict repeated provider writes=%d", calls)
	}
}

func TestAccessGrantPRMergeUsesCurrentBaseAfterMonotonicRollForward(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
	repoPath, err := svc.Git.GetRepoPath(context.Background(), accessGrantTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	creationBase := provider.snapshot.BaseSHA
	tree := strings.TrimSpace(runAccessGrantMergeGit(t, repoPath, "rev-parse", creationBase+"^{tree}"))
	currentBase := strings.TrimSpace(runAccessGrantMergeGit(t, repoPath, "commit-tree", tree, "-p", creationBase, "-m", "base advanced"))
	rebasedHead := strings.TrimSpace(runAccessGrantMergeGit(t, repoPath, "commit-tree", tree, "-p", currentBase, "-m", "rebased head"))
	runAccessGrantMergeGit(t, repoPath, "update-ref", "refs/heads/main", currentBase, creationBase)
	if err := svc.DB.Model(&db.PullRequest{}).Where("number = ?", input.AGSPRNumber).Update("head_sha", rebasedHead).Error; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.snapshot.HeadSHA = rebasedHead
	provider.snapshot.BaseSHA = currentBase
	provider.remoteSHA = currentBase
	provider.runs[0].HeadSHA = rebasedHead
	provider.mu.Unlock()
	input.ExpectedHeadSHA = rebasedHead

	receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != service.ForgejoActionIntentCompleted || !receipt.ProviderMerged || receipt.ProviderMergeSHA != rebasedHead {
		t.Fatalf("receipt=%#v", receipt)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider writes=%d", calls)
	}
}

func TestAccessGrantPRMergeRejectsHeadThatDoesNotContainCurrentBase(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
	repoPath, err := svc.Git.GetRepoPath(context.Background(), accessGrantTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	creationBase := provider.snapshot.BaseSHA
	tree := strings.TrimSpace(runAccessGrantMergeGit(t, repoPath, "rev-parse", creationBase+"^{tree}"))
	currentBase := strings.TrimSpace(runAccessGrantMergeGit(t, repoPath, "commit-tree", tree, "-p", creationBase, "-m", "base advanced"))
	runAccessGrantMergeGit(t, repoPath, "update-ref", "refs/heads/main", currentBase, creationBase)
	provider.mu.Lock()
	provider.snapshot.BaseSHA = currentBase
	provider.remoteSHA = currentBase
	provider.mu.Unlock()

	receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil || receipt.State != service.ForgejoActionIntentConflict ||
		receipt.ProviderAttempt != "not_attempted" || receipt.DenialCode != "ags_head_does_not_contain_current_base" {
		t.Fatalf("stale-head terminal receipt=%#v err=%v", receipt, err)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider writes=%d", calls)
	}
	readback, err := svc.GetAccessGrantInvocation(context.Background(), grantToken, input.InvocationID)
	if err != nil || readback.ID != receipt.ID || readback.DenialCode != receipt.DenialCode {
		t.Fatalf("stale-head GET readback=%#v err=%v", readback, err)
	}
}

func runAccessGrantMergeGit(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	gitArgs := []string{"-C", repoPath, "-c", "user.name=access-grant-merge-test", "-c", "user.email=access-grant-merge-test@localhost"}
	cmd := exec.Command("git", append(gitArgs, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestAccessGrantPRMergeUseTimeDriftMakesZeroProviderWrites(t *testing.T) {
	for _, tc := range []struct {
		name             string
		terminalConflict bool
		mutate           func(*accessGrantMergeProvider)
	}{
		{name: "provider head", terminalConflict: true, mutate: func(p *accessGrantMergeProvider) { p.snapshot.HeadSHA = strings.Repeat("b", 40) }},
		{name: "provider base", terminalConflict: true, mutate: func(p *accessGrantMergeProvider) { p.snapshot.BaseSHA = strings.Repeat("c", 40) }},
		{name: "review mergeability", terminalConflict: true, mutate: func(p *accessGrantMergeProvider) { p.snapshot.Mergeable = false }},
		{name: "ci", mutate: func(p *accessGrantMergeProvider) { p.runs[0].Status = "failure" }},
		{name: "protection force", mutate: func(p *accessGrantMergeProvider) { p.authority.ForcePushBlocked = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
			provider.mu.Lock()
			tc.mutate(provider)
			provider.mu.Unlock()
			receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
			if tc.terminalConflict {
				if err != nil || receipt.State != service.ForgejoActionIntentConflict || receipt.ProviderAttempt != "not_attempted" || receipt.DenialCode != "provider_pr_drift" {
					t.Fatalf("terminal use-time conflict=%#v err=%v", receipt, err)
				}
			} else if err == nil {
				t.Fatalf("use-time denial unexpectedly authorized merge: %#v", receipt)
			}
			provider.mu.Lock()
			calls := provider.mergeCalls
			provider.mu.Unlock()
			if calls != 0 {
				t.Fatalf("provider writes=%d", calls)
			}
		})
	}
}

func TestAccessGrantPRMergeAllowsUnsuccessfulCIWhenRepoDisablesRequireCI(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
	provider.mu.Lock()
	provider.runs[0].Status = "failure"
	provider.mu.Unlock()
	if _, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input); err == nil {
		t.Fatal("expected exact-head CI denial by default")
	}
	svc.AccessGrantRequireCI = func(string) bool { return false }
	input.InvocationID = uuid.NewString()
	receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil || !receipt.ProviderMerged || receipt.ProviderMergeSHA != input.ExpectedHeadSHA {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}

func TestAccessGrantPRMergeWaitsForExactProviderMergeability(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
	provider.mu.Lock()
	provider.snapshot.Mergeable = false
	provider.mergeableAfterReads = 3
	provider.mu.Unlock()

	receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.ProviderMerged || receipt.ProviderMergeSHA != input.ExpectedHeadSHA {
		t.Fatalf("receipt=%#v", receipt)
	}
	provider.mu.Lock()
	reads, calls := provider.snapshotReads, provider.mergeCalls
	provider.mu.Unlock()
	if reads < 3 || calls != 1 {
		t.Fatalf("snapshot reads=%d merge calls=%d", reads, calls)
	}
}

func TestAccessGrantPRMergeUsesAuthoritativeNonDefaultPRBase(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
	var repository db.Repository
	if err := svc.DB.First(&repository, "full_name = ?", "example-owner/demo").Error; err != nil {
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
	if err := svc.DB.First(&pr, "repository_id = ? AND number = ?", repository.ID, input.AGSPRNumber).Error; err != nil {
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
	input.InvocationID = uuid.NewString()

	receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.ProviderMerged || receipt.ProviderMergeSHA != input.ExpectedHeadSHA {
		t.Fatalf("receipt=%#v", receipt)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider merge calls=%d", calls)
	}
}

func TestAccessGrantPRMergeRequiresIndependentCurrentHeadApproval(t *testing.T) {
	svc, grantToken, input, provider := setupAccessGrantMerge(t, "success")
	var pr db.PullRequest
	if err := svc.DB.First(&pr, "number = ?", input.AGSPRNumber).Error; err != nil {
		t.Fatal(err)
	}
	protectBranch(t, svc, pr.RepositoryID, pr.BaseRef, "", `{"required_approving_review_count":1}`, true)
	if _, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input); !errors.Is(err, service.ErrInvalidState) {
		t.Fatalf("merge without independent approval err=%v", err)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("provider writes before approval=%d", calls)
	}
	if _, err := svc.AddPRReview(context.Background(), pr.ID, "independent-reviewer", "APPROVE", "current head approved", pr.HeadSHA); err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.ExecuteAccessGrantPRMerge(context.Background(), grantToken, input)
	if err != nil || !receipt.ProviderMerged || receipt.ProviderMergeSHA != input.ExpectedHeadSHA {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	provider.mu.Lock()
	calls = provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider writes after approval=%d", calls)
	}
}

func setupAccessGrantMerge(t *testing.T, mergeMode string) (*service.Service, string, service.AccessGrantPRMergeInput, *accessGrantMergeProvider) {
	t.Helper()
	svc, _, repository := setupAccessGrantService(t)
	if err := svc.Git.Init(context.Background(), repository.FullName, "main", true); err != nil {
		t.Fatal(err)
	}
	baseSHA, err := svc.Git.HeadSHA(context.Background(), repository.FullName, "main")
	if err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{
		Number: 7, RepositoryID: repository.ID, HeadRepositoryID: repository.ID, AuthorID: repository.OwnerID,
		Title: "access grant merge", State: db.StateOpen, HeadRef: "feature/access-grant",
		HeadSHA: baseSHA, BaseRef: "main", BaseSHA: baseSHA,
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	projection := db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repository.ID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "forgejo/demo", ExternalNumber: 9, SourceBranch: pr.HeadRef,
		TargetBranch: pr.BaseRef, State: service.ProjectionStateOpen, LastSyncedSHA: pr.HeadSHA,
	}
	if err := svc.DB.Create(&projection).Error; err != nil {
		t.Fatal(err)
	}
	bootstrap, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	mergeInput := accessGrantIssueInput(bootstrap.Grant.Actor.Login, "", []string{"pr.merge"})
	mergeInput.AccessRole = "maintainer"
	elevated, err := svc.IssueAccessGrant(context.Background(), mergeInput)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(elevated.Grant.EffectiveOperations, "pr.merge") {
		t.Fatalf("merge operation missing: %#v", elevated.Grant)
	}

	provider := &accessGrantMergeProvider{
		database: svc.DB, mergeMode: mergeMode, remoteSHA: pr.BaseSHA,
		snapshot: forgejointegration.PullRequestSnapshot{
			Number: 9, State: "open", Merged: false, Mergeable: true,
			HeadRef: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef, BaseSHA: pr.BaseSHA,
		},
		runs: []forgejointegration.WorkflowRun{{
			ID: 100, Name: "CI Success", WorkflowID: "ci-success", Status: "success", HeadBranch: "#9", HeadSHA: pr.HeadSHA,
		}},
		authority: forgejointegration.RepositoryAuthorityState{
			BaseBranchProtected: true, DirectPushBlocked: true, ForcePushBlocked: true,
			IntegrationBotCollaborator: true, IntegrationBotAuthorized: true, IntegrationBotMergeAuthorized: true,
		},
	}
	enabled := true
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.invalid", Token: "server-owned-token",
		AuthorityPolicyEnabled: true, WebhookURL: "http://ags.invalid/webhook", IntegrationBot: "ags-bot",
		RepoMap: map[string]forgejointegration.RepoMapping{
			repository.FullName: {Owner: "forgejo", Repo: "demo", Enabled: &enabled, BaseBranch: "main", DelegatedMergeMethod: "rebase"},
		},
	}, provider, func(context.Context, forgejointegration.PushRequest) error { return nil }, func(context.Context, string, string, string) (string, error) {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return provider.remoteSHA, nil
	})
	return svc, elevated.GrantToken, service.AccessGrantPRMergeInput{
		InvocationID: uuid.NewString(), AGSPRNumber: pr.Number, ProviderPRNumber: projection.ExternalNumber,
		ExpectedHeadSHA: pr.HeadSHA, MergeMethod: "rebase",
	}, provider
}
