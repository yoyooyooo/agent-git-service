#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || {
    echo "missing dependency: $cmd" >&2
    exit 1
  }
}

require_git_http_backend() {
  local backend
  backend="$(git --exec-path)/git-http-backend"
  if [[ ! -x "$backend" ]]; then
    echo "git-http-backend not found at $backend" >&2
    exit 1
  fi
}

require_go_tests() {
  local package="$1"
  shift
  local listed test_name
  listed="$(go test "$package" -list '^Test')"
  for test_name in "$@"; do
    if ! grep -Fqx "$test_name" <<<"$listed"; then
      echo "integration test selector does not exist in $package: $test_name" >&2
      exit 1
    fi
  done
}

require_cmd git
require_cmd go
require_git_http_backend

cd "$ROOT_DIR"

REST_RUN='^(TestHostRewrite_|TestOAuth_|TestAuth_|TestRepoLifecycle|TestIssueLifecycle|TestPRLifecycle|TestAccessGrantRoutesBootstrapWithoutAGSProfileAndRemainSecretSafe|TestAccessGrantOpenAPIPathsAndSchemasAreClosed|TestForgejoRebaseProjectionIncidentAcceptance|TestGraphQLWriteRESTRead_StateParity|TestRESTWriteGraphQLRead_StateParity|TestOrganizationInvitationHandlers_FullFlow|TestTeamShare_)'
REST_HARDCUT_TESTS=(
  TestAccessGrantRoutesBootstrapWithoutAGSProfileAndRemainSecretSafe
  TestAccessGrantOpenAPIPathsAndSchemasAreClosed
)
GRAPHQL_RUN='^(TestGraphQLAuth_|TestGraphQL_RepositoryQuery|TestGraphQL_IssueQuery|TestGraphQL_PRQuery|TestGraphQL_CreateIssueMutation|TestGraphQL_MergePRMutation|TestGraphQL_MergePRMutation_FailureCases)$'
FORGEJO_REWRITE_RUN='^Test(GitPushArgsUseExactForceWithLeaseWithoutForcedRefspec|GitPushRejectsLeaseCombinedWithUnconditionalForcedRefspec|SanitizeProjectionErrorSummaryRedactsCredentialsForAnyURLScheme|EnsurePullRequestLeaseRewrite(VerifiesRemoteAndPullRequestHead|RejectsConcurrentRemoteChangeBeforePush|RejectsWrongMappedPullRequest|FailsWhenPullRequestAPINeverConverges|RejectsBaseBranch)|RepositoryAuthority.*|HTTPClientRepositoryAuthorityRetrofit|HTTPClientGetPullRequest(ReadsExactNumber|ReturnsNotFound))$'
READY_AUTHORITY_RUN='^TestReadyz_(RequiredProjectionAlertingMissing|ForgejoAuthorityPolicyDrift|ForgejoAuthorityExplicitOptOut|ProjectionAlertingExplicitOptOut)'
NOTIFICATION_OUTBOUND_RUN='^TestFeishuOutboundDispatcher(ClassifiesFrequencyLimitAsRetryable|DeliversSuccessfulText)$'
SERVICE_PROJECTION_TESTS=(
  TestFindPullRequestByForgejoProjectionDoesNotAssumeMatchingNumbers
  TestForgejoActionRebaseLabelRebasesAGSPRAndRefreshesProjection
  TestForgejoActionRebaseLabelRejectsConcurrentRemoteChangeWithLease
  TestForgejoActionRebaseLabelTerminallyDeniesBaseMoveAfterProjection
  TestForgejoActionRebaseConflictLeavesAGSAndForgejoHeadsUnchanged
  TestForgejoActionRebaseJobClaimUsesDatabaseCAS
  TestGetProjectionStatusIncludesForgejoProjectionJobs
  TestForgejoActionProjectionPresentationFailurePreservesDurableFailure
  TestCommitForgejoActionIntentConcurrentCallersLeaveOneActive
  TestCommitForgejoActionIntentExpiresOldBeforeCreatingNew
  TestCommitForgejoActionIntentSameKeyRejectsChangedForgejoCoordinates
  TestConsumeForgejoActionIntentDuplicateActiveFailsClosed
  TestSameForgejoActionIntentFactsRejectsEveryExternalCoordinateChange
  TestExactActionLiveDriftDeniesBeforeProviderMutation
  TestAccessGrantPRMergePersistsUnknownBeforePostAndNeverRepeatsIt
  TestAccessGrantPRMergeRequiresCanonicalCallerOwnedInvocationID
  TestAccessGrantPRMergeCompletedRequestIsIdempotent
  TestAccessGrantPRMergeConcurrentSameEffectMakesOneProviderPost
  TestAccessGrantPRMergeUseTimeDriftMakesZeroProviderWrites
  TestDelegatedSessionUseTimeAcceptsOnlyAccessGrantTransportMode
  TestDelegatedEffectBoundaryReceiptReadbackIsOriginatingSessionScoped
  TestActionIntentReceiptBoundaryStatusMatrix
  TestRecoveryQuarantinesHistoricalDelegatedDispatchWithoutInventingReceipt
  TestRecoveryQuarantinesNewProtocolMissingReceiptWithoutProviderCall
  TestDispatchingIntentWebhookCanConsumeBeforeInitialProviderCallReturns
  TestDispatchingIntentEarlyWebhookDoesNotLoseBoundJob
  TestRecoveryDispatchWebhookCanConsumeBeforeProviderCallReturns
  TestInitialDispatchAcknowledgementAcceptsWebhookAdvanceAfterSuccessfulCAS
  TestRecoveryDispatchAcknowledgementAcceptsWebhookAdvanceAfterSuccessfulCAS
  TestRecoveryRetriesUnknownDispatchOutcomeWithoutReturningToPlanned
  TestRecoveryExpiresDispatchingIntentWithoutProviderRetry
  TestOldRebaseGenerationCannotUseCompleteOrDenyNewIntent
  TestHistoricalUnboundRebaseJobFailsClosedWithoutSelectingIntent
  TestPostJobProviderDenialAtomicallyTerminalizesExactIntentAndJob
  TestStaleActionDenialTargetsOldIntentNotReplacementJob
  TestTerminalDenialRejectsAllLatePhaseWriters
  TestTerminalDenialWinsConcurrentRebaseCompletion
  TestRebaseCompletionCannotResurrectTerminalDenial
  TestGenericProjectionEnqueueCannotReplaceActionGeneration
  TestGenericProjectionManualRetryAdvancesGenerationAndRejectsActiveWorker
  TestGenericProjectionTakeoverRejectsLateWorkerGeneration
  TestGitLabRebaseWritesRequireFreshExactActionBinding
  TestSuccessCommentConcurrentPublishWritesProviderOnce
  TestSuccessCommentPreWriteCrashSchedulesLeaseExpiryAndRecovers
  TestSuccessCommentProviderWriteCrashRecoversByReadbackWithoutDuplicate
  TestDispatchPullRequestIntegrationsReturnsErrorAndLeavesPartialStateAfterForgejoBranchPush
  TestDispatchPullRequestIntegrationsKeepsOptionalGitLabFailureSeparate
  TestScanForgejoPullRequestIntegrityFailsClosedWithoutExactConfirmation
  TestScanForgejoPullRequestIntegrityRechecksStaleBulkCandidates
  TestScanForgejoPullRequestIntegrityFindsMissingAndStateDrift
  TestRecordForgejoProjectionFailureImmediatelyCreatesDurableOutbound
  TestRecordProjectionFailureWithoutAlertTargetReturnsExplicitOutcomeAndKeepsDrift
  TestProjectionFailureAndOutboundIntentRollbackTogetherWhenIntentPersistenceFails
  TestProjectionFailureWithTargetButNoDispatcherReturnsExplicitOutcomeAndKeepsPendingIntent
  TestProjectionDriftRetryReachesDeliveredAndMaxAttemptsDeadLetters
  TestProjectionAlertRedactsCredentialBearingSummaryAndURL
  TestResolveUnnotifiedProjectionDriftDoesNotEmitResolvedOutbound
  TestNewProjectionDriftGenerationIsNotThrottledByPreviousAlert
  TestProjectionDriftResolvedAndNewGenerationEachCreateOneOutbound
)
SERVICE_PROJECTION_RUN="^($(IFS='|'; echo "${SERVICE_PROJECTION_TESTS[*]}"))$"

echo "==> go test -count=1 ./internal/db -run migration focused selectors"
go test -count=1 ./internal/db -run '^(TestMigrateAddsSafeDefaultsForLegacyProjectionJobs|TestMigratePullRequestActionBoundaryReceiptIsAdditiveRepeatableAndDoesNotBackfill)$'

echo "==> go test -count=1 ./internal/testharness"
go test -count=1 ./internal/testharness

echo "==> go test -count=1 ./internal/router"
go test -count=1 ./internal/router

echo "==> go test -count=1 ./internal/githttp/..."
go test -count=1 ./internal/githttp/...

echo "==> go test -count=1 ./internal/forgejointegration -run ${FORGEJO_REWRITE_RUN}"
go test -count=1 ./internal/forgejointegration -run "$FORGEJO_REWRITE_RUN"

echo "==> validate focused ./internal/service integration test selectors"
require_go_tests ./internal/service "${SERVICE_PROJECTION_TESTS[@]}"

echo "==> go test -count=1 ./internal/service -run ${SERVICE_PROJECTION_RUN}"
go test -count=1 ./internal/service -run "$SERVICE_PROJECTION_RUN"

echo "==> go test -count=1 ./server -run ${READY_AUTHORITY_RUN}"
go test -count=1 ./server -run "$READY_AUTHORITY_RUN"

echo "==> go test -count=1 ./cmd/forgejo-authority"
go test -count=1 ./cmd/forgejo-authority

echo "==> go test -count=1 ./config ./internal/integrations"
go test -count=1 ./config ./internal/integrations

echo "==> go test -count=1 ./internal/notifications -run ${NOTIFICATION_OUTBOUND_RUN}"
go test -count=1 ./internal/notifications -run "$NOTIFICATION_OUTBOUND_RUN"

echo "==> validate focused ./internal/rest hard-cut test selectors"
require_go_tests ./internal/rest "${REST_HARDCUT_TESTS[@]}"

echo "==> go test -count=1 ./internal/rest -run ${REST_RUN}"
go test -count=1 ./internal/rest -run "$REST_RUN"

echo "==> go test -count=1 ./internal/graphql -run ${GRAPHQL_RUN}"
go test -count=1 ./internal/graphql -run "$GRAPHQL_RUN"

echo "Integration tests passed."
