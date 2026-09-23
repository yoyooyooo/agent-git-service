package service

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"github.com/ngaut/agent-git-service/internal/wikicatalog"
	"gorm.io/gorm"
)

// Test helper methods - exported only for testing purposes.
// These methods provide access to internal functions for test validation.

// LoadSecretsForTest exposes loadSecrets for testing.
func (s *Service) LoadSecretsForTest(ctx context.Context, repo db.Repository) map[string]string {
	return s.loadSecrets(ctx, repo)
}

// LoadEnvSecretsForTest exposes loadEnvSecrets for testing.
func (s *Service) LoadEnvSecretsForTest(ctx context.Context, repo db.Repository, env string) map[string]string {
	return s.loadEnvSecrets(ctx, repo, env)
}

// CreateArtifactFromPathForTest exposes createArtifactFromPath for testing.
func (s *Service) CreateArtifactFromPathForTest(ctx context.Context, runID uint, name, basePath string) error {
	return s.createArtifactFromPath(ctx, runID, name, basePath)
}

// CompleteRunForTest exposes completeRun for testing.
func (s *Service) CompleteRunForTest(ctx context.Context, runID uint, conclusion string) {
	s.completeRun(ctx, runID, conclusion)
}

// EnableWorkflowExecForTest enables workflow execution with a host-shell runner.
// Production code always uses the Docker sandbox when WorkflowExecEnabled is set.
func (s *Service) EnableWorkflowExecForTest(timeout time.Duration) {
	s.WorkflowExecEnabled = true
	if timeout > 0 {
		s.WorkflowExecTimeout = timeout
	}
	s.workflowStepRunner = workflowStepRunnerFunc(func(ctx context.Context, req workflowStepRequest) (workflowStepResult, error) {
		cmd := exec.CommandContext(ctx, "bash", "-e", "-c", req.Script)
		cmd.Dir = req.Dir
		cmd.Env = append([]string{
			"HOME=" + workflowTmpMount,
			"PATH=" + workflowExecPath,
			"CI=true",
			"GITHUB_ACTIONS=true",
		}, req.Env...)
		out, err := cmd.CombinedOutput()
		result := workflowStepResult{Output: out}
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				result.ExitCode = exitErr.ExitCode()
			} else {
				result.ExitCode = 1
			}
			if ctx.Err() == context.DeadlineExceeded {
				result.TimedOut = true
				result.ExitCode = 1
			}
			return result, err
		}
		return result, nil
	})
}

// SetWorkflowStepRunnerForTest replaces the workflow runner with a custom test hook.
func (s *Service) SetWorkflowStepRunnerForTest(timeout time.Duration, fn func(ctx context.Context, dir, script string, env []string) ([]byte, int, error)) {
	s.WorkflowExecEnabled = true
	if timeout > 0 {
		s.WorkflowExecTimeout = timeout
	}
	s.workflowStepRunner = workflowStepRunnerFunc(func(ctx context.Context, req workflowStepRequest) (workflowStepResult, error) {
		out, exitCode, err := fn(ctx, req.Dir, req.Script, req.Env)
		result := workflowStepResult{
			Output:   out,
			ExitCode: exitCode,
			TimedOut: ctx.Err() == context.DeadlineExceeded,
		}
		if err != nil {
			return result, err
		}
		if exitCode != 0 {
			return result, fmt.Errorf("workflow test runner exit code %d", exitCode)
		}
		return result, nil
	})
}

// SetWikiGitIngestAfterSnapshotHookForTest installs a test-only hook after
// ingestOneWikiGit snapshots catalog/git state and before it replays any git
// commits.
func (s *Service) SetWikiGitIngestAfterSnapshotHookForTest(fn func(repoFullName string)) {
	s.testWikiGitIngestAfterSnapshot = fn
}

// SetWikiBackgroundGitIngestStartedHookForTest installs a test-only hook fired
// when a repo-scoped background wiki git ingest is claimed and scheduled.
func (s *Service) SetWikiBackgroundGitIngestStartedHookForTest(fn func(repoFullName string)) {
	s.testWikiBackgroundGitIngestStarted = fn
}

// IsPublicRepoForTest exposes isPublicRepo to external-package tests.
func IsPublicRepoForTest(s *Service, ctx context.Context, repoID uint) bool {
	return s.isPublicRepo(ctx, repoID)
}

// SetTestWikiCompactRefUpdateFailureForTest installs a test-only hook that can
// force CompactWikiHistory to fail before the compacted catalog state commits.
func SetTestWikiCompactRefUpdateFailureForTest(s *Service, fn func(repoFullName, commitSHA string) error) {
	s.testWikiCompactRefUpdateFailure = fn
}

// SetTestWikiPostCommitEffectsForTest installs a test-only hook at the start
// of ordered post-commit side-effect processing.
func SetTestWikiPostCommitEffectsForTest(s *Service, fn func(repoFullName string, result wikicatalog.ChangeSetResult)) {
	s.testWikiPostCommitEffects = fn
}

// SetTestWikiPreparedPublishFailureForTest installs a test-only hook that can
// force prepared REST commit publication to fail after the catalog transaction.
func SetTestWikiPreparedPublishFailureForTest(s *Service, fn func(repoFullName, commitSHA string) error) {
	s.testWikiPreparedPublishFailure = fn
}

// SetTestWikiPreparedPersistForTest installs a test-only hook immediately
// before a prepared REST commit's objects are persisted.
func SetTestWikiPreparedPersistForTest(s *Service, fn func(repoFullName, commitSHA string) error) {
	s.testWikiPreparedPersist = fn
}

// SetTestWikiRESTSnapshotForTest installs a test-only hook after a REST writer
// captures its catalog snapshot and before it waits for the repository Git lock.
func SetTestWikiRESTSnapshotForTest(s *Service, fn func(repoFullName string)) {
	s.testWikiRESTSnapshot = fn
}

// SetTestWikiReceivePackIngestFailureForTest installs a test-only hook that can
// force the catalog ingest after receive-pack has updated the Git ref to fail.
func SetTestWikiReceivePackIngestFailureForTest(s *Service, fn func(repoFullName string) error) {
	s.testWikiReceivePackIngestFailure = fn
}

// SetTestWikiGitRepairObligationLoadedForTest installs a hook after a wiki
// repair obligation is loaded and before it is consumed or cleared.
func SetTestWikiGitRepairObligationLoadedForTest(s *Service, fn func(repoFullName string, obligation db.WikiGitRepairObligation)) {
	s.testWikiGitRepairObligationLoaded = fn
}

// SetTestWikiCompactionJobStartedForTest installs a test-only hook fired after
// the async compaction worker marks a job running.
func SetTestWikiCompactionJobStartedForTest(s *Service, fn func(jobID string)) {
	s.testWikiCompactionJobStarted = fn
}

// SetTestWikiCompactionJobContinueForTest installs a test-only hook that can
// block the async compaction worker until tests allow it to proceed.
func SetTestWikiCompactionJobContinueForTest(s *Service, fn func(jobID string)) {
	s.testWikiCompactionJobContinue = fn
}

// SetCreatePRRetryHookForTest installs a test-only hook fired after a retryable
// transaction failure and before CreatePR starts the next attempt.
func SetCreatePRRetryHookForTest(s *Service, fn func(attempt int)) {
	s.testCreatePRRetry = fn
}

// SetCreatePRRefBeforeWriteHookForTest injects drift after the PR row commit
// and before the local refs/pull mutation.
func SetCreatePRRefBeforeWriteHookForTest(s *Service, fn func()) {
	s.testCreatePRRefBeforeWrite = fn
}

// SetAuthorityBoundaryTransactionHookForTest wraps complete receipt
// transactions so tests can inject commit-time conflict results.
func SetAuthorityBoundaryTransactionHookForTest(s *Service, fn func(attempt int, run func(*gorm.DB) error) error) {
	s.testAuthorityBoundaryTransaction = fn
}

// StampDurableActionIntentAuthorityForTest snapshots the current durable
// principal authority onto a direct action-intent fixture.
func StampDurableActionIntentAuthorityForTest(ctx context.Context, s *Service, intent *db.PullRequestActionIntent) error {
	if s == nil || intent == nil {
		return fmt.Errorf("durable action intent fixture is required")
	}
	var principal db.User
	if err := s.DBForCtx(ctx).First(&principal, intent.PrincipalID).Error; err != nil {
		return err
	}
	result, err := s.AuthorizeDurableOperation(ctx, principal, "ags", intent.Repository, "pr.rebase",
		forgejoRebaseDurableConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA))
	if err != nil {
		return err
	}
	intent.TeamIdentityID = result.AuthorizationBasis.TeamIdentityID
	intent.PolicyClass = result.AuthorizationBasis.PolicyClass
	intent.MembershipEpoch = result.AuthorizationBasis.MembershipEpoch
	intent.AuthorityRev = durableActionAuthorityRevision(result)
	return nil
}

// SetDelegatedProviderWriteHookForTest injects a final provider seam result.
func SetDelegatedProviderWriteHookForTest(s *Service, fn func() error) {
	s.testDelegatedProviderWrite = fn
}

// SetForgejoSuccessCommentBeforeWriteHookForTest simulates interruption after
// a durable claim but before the provider write.
func SetForgejoSuccessCommentBeforeWriteHookForTest(s *Service, fn func() error) {
	s.testForgejoSuccessCommentBeforeWrite = fn
}

// SetForgejoSuccessCommentAfterWriteHookForTest simulates interruption after
// the provider accepted a deterministic comment and before AGS readback.
func SetForgejoSuccessCommentAfterWriteHookForTest(s *Service, fn func() error) {
	s.testForgejoSuccessCommentAfterWrite = fn
}

// SetForgejoTerminalDenialBeforeCommitHookForTest pauses the owning denial
// transaction after intent denial and before terminal job convergence.
func SetForgejoTerminalDenialBeforeCommitHookForTest(s *Service, fn func()) {
	s.testForgejoTerminalDenialBeforeCommit = fn
}

// ConfirmDelegatedSessionCommitBoundaryForTest exposes only the final DB-side
// boundary so an expiry that occurs during evaluation can be deterministic.
func ConfirmDelegatedSessionCommitBoundaryForTest(s *Service, ctx context.Context, session db.DelegatedAgentSession, required RepoPermission, compareRevision bool) (RepoPermission, error) {
	return s.confirmDelegatedSessionCommitBoundary(ctx, session, required, compareRevision)
}

// FinalizeCanonicalDelegatedSessionForTest derives the exact persisted
// team-v4 snapshot for a hermetic transport fixture.
func FinalizeCanonicalDelegatedSessionForTest(s *Service, ctx context.Context, session *db.DelegatedAgentSession) error {
	request := sessionauthority.Request{
		Issuer: session.Issuer, IssuerInstanceID: session.IssuerInstanceID, AssertionKeyID: session.AssertionKeyID,
		Subject: session.IssuerSubject, WorkspaceID: session.IssuerWorkspaceID, TeamIdentityID: session.TeamIdentityID,
		PolicyClass: session.PolicyClass, MembershipEpoch: session.MembershipEpoch,
		Target: session.TargetInstance, Service: session.ResourceService, Repository: session.Repository.FullName,
		Operation: session.OperationName, Now: time.Now().UTC(),
	}
	resolved, err := s.PrincipalSessions.Resolve(request)
	if err != nil {
		return err
	}
	permission, err := s.currentRepoAccess(ctx, session.RepositoryID, session.PrincipalUserID)
	if err != nil {
		return err
	}
	revision := nativeGrantRevision(session.PrincipalUserID, session.RepositoryID, permission)
	snapshot, err := principalSessionSnapshotHash(resolved, revision, session.MembershipEpoch, session.GrantedCapabilities, session.OperationConstraints)
	if err != nil {
		return err
	}
	session.ContractRevision = sessionauthority.ContractRevision
	session.TrustRevision = resolved.Issuer.TrustRevision
	session.TeamBindingRevision = resolved.TeamBinding.BindingRevision
	session.PolicyVersion = policyVersion(resolved)
	session.PolicySnapshotHash = snapshot
	session.NativeGrantRevision = revision
	session.ResourcePolicyRevision = resolved.Resource.PolicyRevision
	return s.observeTeamAuthorityEpoch(ctx, resolved, session.MembershipEpoch)
}

// ClaimWikiBackgroundGitIngestForTest exposes background git ingest slot claims for tests.
func (s *Service) ClaimWikiBackgroundGitIngestForTest(ctx context.Context, repoFullName string) bool {
	repo, err := s.LookupRepoIdentity(ctx, repoFullName)
	if err != nil {
		return false
	}
	return s.claimWikiBackgroundGitIngest(s.wikiRepoKey(ctx, repo))
}

// ReleaseWikiBackgroundGitIngestForTest exposes background git ingest cleanup for tests.
func (s *Service) ReleaseWikiBackgroundGitIngestForTest(ctx context.Context, repoFullName string) {
	repo, err := s.LookupRepoIdentity(ctx, repoFullName)
	if err != nil {
		return
	}
	s.releaseWikiBackgroundGitIngest(s.wikiRepoKey(ctx, repo))
}
