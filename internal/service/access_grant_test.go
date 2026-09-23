package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

const (
	accessGrantTestSource    = "multica-mini"
	accessGrantTestWorkspace = "11111111-1111-4111-8111-111111111111"
	accessGrantTestAgent     = "22222222-2222-4222-8222-222222222222"
	accessGrantTestTask      = "33333333-3333-4333-8333-333333333333"
	accessGrantTestRun       = "44444444-4444-4444-8444-444444444444"
	accessGrantTestRepo      = "example-owner/demo"
)

type staticAccessGrantContextPuller struct {
	result executioncontext.PullResult
}

func (p *staticAccessGrantContextPuller) Pull(_ context.Context, request executioncontext.PullRequest) (executioncontext.PullResult, error) {
	if request.SourceToken != "current-task-token" || request.Locator.WorkspaceID != accessGrantTestWorkspace ||
		request.Locator.AgentID != accessGrantTestAgent || request.Locator.TaskID != accessGrantTestTask {
		return executioncontext.PullResult{}, executioncontext.ErrSourceCredentialRejected
	}
	return p.result, nil
}

func TestAccessGrantIssueCreatesCredentialFreeCanonicalActorAndFailSoftDefaults(t *testing.T) {
	svc, executor, _ := setupAccessGrantService(t)
	result, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("unbound-agent", "unknown-class", []string{"repo.admin", "ci.read"}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Schema != service.AccessGrantIssueSchema || !strings.HasPrefix(result.GrantToken, "ags_grant_") {
		t.Fatalf("result=%#v", result)
	}
	grant := result.Grant
	if grant.Actor.ID == 0 || grant.Actor.UserKind != db.UserKindAgent || grant.Executor.ID != executor.ID || grant.Actor.ID == grant.Executor.ID {
		t.Fatalf("actor/executor separation failed: %#v", grant)
	}
	if grant.AgentSelectorOutcome != "ignored_unbound" || grant.PolicyClassOutcome != "ignored_unbound" ||
		!containsString(grant.Warnings, "agent_selector_ignored_unbound") || !containsString(grant.Warnings, "policy_class_ignored") ||
		!containsString(grant.Warnings, "operation_ignored_outside_envelope") {
		t.Fatalf("fail-soft receipt=%#v", grant)
	}
	if containsString(grant.EffectiveOperations, "repo.admin") || containsString(grant.EffectiveOperations, "pr.merge") ||
		!containsString(grant.EffectiveOperations, "git.push") || !containsString(grant.EffectiveOperations, "pr.create") ||
		!containsString(grant.EffectiveOperations, "pr.comment") || !containsString(grant.EffectiveOperations, "review.write") {
		t.Fatalf("effective operations=%v", grant.EffectiveOperations)
	}

	var stored db.AccessGrant
	if err := svc.DB.First(&stored, "id = ?", grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CredentialHash == result.GrantToken || strings.Contains(stored.CredentialPrefix, result.GrantToken) {
		t.Fatal("plaintext grant credential was persisted")
	}
	var tokens, collaborators int64
	if err := svc.DB.Model(&db.Token{}).Where("user_id = ?", grant.Actor.ID).Count(&tokens).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.Collaborator{}).Where("user_id = ?", grant.Actor.ID).Count(&collaborators).Error; err != nil {
		t.Fatal(err)
	}
	if tokens != 0 || collaborators != 0 {
		t.Fatalf("JIT actor received durable credentials/grants: tokens=%d collaborators=%d", tokens, collaborators)
	}
	selected, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput(accessGrantTestAgent, "", nil))
	if err != nil || selected.Grant.AgentSelectorOutcome != "accepted" || selected.Grant.Actor.ID != grant.Actor.ID {
		t.Fatalf("source-bound external agent selector result=%#v err=%v", selected.Grant, err)
	}
	secretIntent, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("mat_selector_secret", "ags_sess_policy_secret", []string{"mat_operation_secret"}))
	if err != nil {
		t.Fatal(err)
	}
	if secretIntent.Grant.AgentSelectorOutcome != "ignored_invalid" || secretIntent.Grant.PolicyClassOutcome != "ignored_invalid" ||
		!containsString(secretIntent.Grant.Warnings, "agent_selector_ignored_invalid") ||
		!containsString(secretIntent.Grant.Warnings, "policy_class_ignored_invalid") ||
		!containsString(secretIntent.Grant.Warnings, "operation_ignored_invalid") {
		t.Fatalf("secret-shaped optional intent did not fail soft: %#v", secretIntent.Grant)
	}
	var secretStored db.AccessGrant
	if err := svc.DB.First(&secretStored, "id = ?", secretIntent.Grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	persisted, _ := json.Marshal(secretStored)
	for _, secret := range []string{"mat_selector_secret", "ags_sess_policy_secret", "mat_operation_secret"} {
		if strings.Contains(string(persisted), secret) {
			t.Fatalf("secret-shaped optional intent persisted: %s", secret)
		}
	}
	if _, err := svc.AuthorizeAccessGrantOperation(context.Background(), secretIntent.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}}); err != nil {
		t.Fatalf("fail-soft secret intent produced unusable grant: %v", err)
	}
}

func TestAccessGrantReceiptMarshalsEmptyCollectionsAsArrays(t *testing.T) {
	svc, _, _ := setupAccessGrantService(t)
	issued, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(issued.Grant)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"effective_policy_classes", "requested_operations", "effective_operations", "warnings"} {
		value, ok := wire[field].([]any)
		if !ok {
			t.Fatalf("%s must be a JSON array, got %T", field, wire[field])
		}
		if field == "requested_operations" && len(value) != 0 {
			t.Fatalf("%s=%v, want an empty array", field, value)
		}
		if field == "warnings" {
			if len(value) != 1 || value[0] != "collaboration_passthrough_transport" {
				t.Fatalf("warnings=%v, want only collaboration_passthrough_transport", value)
			}
		}
	}
	if wire["association_status"] != "linked" || wire["identity_kind"] != "temporary_agent" || wire["collaboration_mode"] != "passthrough_transport" {
		t.Fatalf("association fields=%#v", wire)
	}
}

func TestAccessGrantIssueContinuesWhenExecutionContextUnavailable(t *testing.T) {
	svc, _, _ := setupAccessGrantService(t)
	svc.ExecutionContextPuller = nil
	issued, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if issued.Grant.AssociationStatus != "provisional" {
		t.Fatalf("unavailable CEC must be provisional, got %#v", issued.Grant.AssociationStatus)
	}
	if !containsString(issued.Grant.Warnings, "execution_context_unverified") {
		t.Fatalf("warnings=%v", issued.Grant.Warnings)
	}
	if !containsString(issued.Grant.EffectiveOperations, "git.push") || !containsString(issued.Grant.EffectiveOperations, "pr.create") {
		t.Fatalf("provisional association stripped operations: %v", issued.Grant.EffectiveOperations)
	}
	svc.ExecutionContextPuller = unavailableAccessGrantContextPuller{}
	again, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if again.Grant.AssociationStatus != "provisional" {
		t.Fatalf("source unavailable must be provisional, got %#v", again.Grant.AssociationStatus)
	}
}

type unavailableAccessGrantContextPuller struct{}

func (unavailableAccessGrantContextPuller) Pull(context.Context, executioncontext.PullRequest) (executioncontext.PullResult, error) {
	return executioncontext.PullResult{}, executioncontext.ErrSourceUnavailable
}

func TestAccessGrantAssociationIsObservabilityOnly(t *testing.T) {
	svc, _, _ := setupAccessGrantService(t)
	issued, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("unbound-agent", "unknown-class", []string{"ci.read"}))
	if err != nil {
		t.Fatal(err)
	}
	if issued.Grant.AssociationStatus != "conflict" {
		t.Fatalf("unbound selector should be conflict association, got %#v", issued.Grant)
	}
	if !containsString(issued.Grant.EffectiveOperations, "git.push") || !containsString(issued.Grant.EffectiveOperations, "pr.create") {
		t.Fatalf("conflict association must not strip collaboration operations: %v", issued.Grant.EffectiveOperations)
	}
	if _, err := svc.AuthorizeAccessGrantOperation(context.Background(), issued.GrantToken, service.AccessGrantOperationInput{Operation: "git.push", Constraints: map[string]any{}}); err != nil {
		t.Fatalf("conflict association blocked collaboration authorize: %v", err)
	}
}

func TestAccessGrantMergeRoleIsTheSingleExtraAgentCapability(t *testing.T) {
	svc, executor, _ := setupAccessGrantService(t)
	ordinary, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", []string{"pr.merge"}))
	if err != nil {
		t.Fatal(err)
	}
	if containsString(ordinary.Grant.EffectiveOperations, "pr.merge") {
		t.Fatalf("ordinary Agent must not receive merge: %#v", ordinary.Grant)
	}

	roleInput := accessGrantIssueInput(ordinary.Grant.Actor.Login, "", []string{"pr.merge", "git.force_push", "repo.admin"})
	roleInput.AccessRole = "maintainer"
	elevated, err := svc.IssueAccessGrant(context.Background(), roleInput)
	if err != nil {
		t.Fatal(err)
	}
	if elevated.Grant.PolicyClassOutcome != "accepted_role" ||
		elevated.Grant.Executor.ID != executor.ID ||
		!containsString(elevated.Grant.EffectiveOperations, "pr.merge") ||
		containsString(elevated.Grant.EffectiveOperations, "git.force_push") ||
		containsString(elevated.Grant.EffectiveOperations, "repo.admin") {
		t.Fatalf("role merge receipt=%#v", elevated.Grant)
	}

	adminInput := roleInput
	adminInput.AccessRole = "admin"
	admin, err := svc.IssueAccessGrant(context.Background(), adminInput)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(admin.Grant.EffectiveOperations, "pr.merge") || containsString(admin.Grant.EffectiveOperations, "repo.admin") {
		t.Fatalf("admin role should only add merge: %#v", admin.Grant)
	}
}

func TestDelegatedSessionUseTimeAcceptsOnlyAccessGrantTransportMode(t *testing.T) {
	svc, _, repository := setupAccessGrantService(t)
	issued, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	transport, err := svc.IssueAccessGrantTransportSession(context.Background(), issued.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveDelegatedSessionCredential(context.Background(), transport.SessionToken); err != nil {
		t.Fatalf("Access Grant transport credential was rejected: %v", err)
	}
	if err := svc.DB.Model(&db.DelegatedAgentSession{}).Where("id = ?", transport.Session.ID).Update("credential_mode", "delegated_session").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolveDelegatedSessionCredential(context.Background(), transport.SessionToken); !errors.Is(err, service.ErrDelegatedSessionCredentialInvalid) {
		t.Fatalf("retired credential mode err=%v", err)
	}
	var principal db.User
	if err := svc.DB.First(&principal, "id = ?", transport.Session.Principal.ID).Error; err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithDelegatedSession(service.ContextWithUser(context.Background(), principal), db.DelegatedAgentSession{ID: transport.Session.ID})
	if _, err := svc.RevalidateDelegatedSession(ctx, repository.ID, "repo.read", "", nil); service.DelegatedSessionDenialReason(err) != service.DelegatedDenialCredentialModeInvalid {
		t.Fatalf("retired credential mode denial=%q err=%v", service.DelegatedSessionDenialReason(err), err)
	}
}

func TestAccessGrantAuthorizeRenewRevokeAndPolicyDrift(t *testing.T) {
	svc, executor, _ := setupAccessGrantService(t)
	issued, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := svc.AuthorizeAccessGrantOperation(context.Background(), issued.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Schema != service.AccessGrantInvocationSchema || invocation.State != "completed" || invocation.ProviderAttempt != "not_attempted" || invocation.ProviderOutcome != "not_applicable" {
		t.Fatalf("invocation=%#v", invocation)
	}
	var stored db.AccessGrantInvocation
	if err := svc.DB.First(&stored, "id = ?", invocation.ID).Error; err != nil || stored.GrantID != issued.Grant.ID {
		t.Fatalf("stored invocation=%#v err=%v", stored, err)
	}
	readback, err := svc.GetAccessGrantInvocation(context.Background(), issued.GrantToken, invocation.ID)
	if err != nil || readback.ID != invocation.ID || readback.Operation != "repo.read" {
		t.Fatalf("generic invocation readback=%#v err=%v", readback, err)
	}
	transport, err := svc.IssueAccessGrantTransportSession(context.Background(), issued.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}

	renewed, err := svc.RenewAccessGrant(context.Background(), issued.GrantToken, service.AccessGrantRenewInput{ExecutionContext: accessGrantExecutionContextInput()})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Grant.RenewedFromGrantID != issued.Grant.ID || renewed.GrantToken == issued.GrantToken {
		t.Fatalf("renewed=%#v", renewed)
	}
	var renewedTransport db.DelegatedAgentSession
	if err := svc.DB.First(&renewedTransport, "id = ?", transport.Session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if renewedTransport.RevokedAt == nil || renewedTransport.RevocationReason != "access_grant_renewed" {
		t.Fatalf("renewal did not revoke derived transport Session: %#v", renewedTransport)
	}
	if _, err := svc.AuthorizeAccessGrantOperation(context.Background(), issued.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}}); !errors.Is(err, service.ErrAccessGrantCredentialInvalid) {
		t.Fatalf("old renewed credential err=%v", err)
	}
	if _, err := svc.RevokeAccessGrant(context.Background(), renewed.GrantToken, "test complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthorizeAccessGrantOperation(context.Background(), renewed.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}}); !errors.Is(err, service.ErrAccessGrantCredentialInvalid) {
		t.Fatalf("revoked credential err=%v", err)
	}

	expired, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Exec("UPDATE access_grants SET expires_at = ? WHERE id = ?", time.Now().UTC().Add(-time.Minute), expired.Grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AuthorizeAccessGrantOperation(context.Background(), expired.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}}); !errors.Is(err, service.ErrAccessGrantCredentialInvalid) {
		t.Fatalf("expired credential err=%v", err)
	}

	fresh, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	// Packaging-only policy revision drift must not revoke collaboration transport
	// authorize when the live envelope still covers frozen collaboration ops.
	svc.PrincipalSessions = accessGrantAuthority(t, executor.ID, "v2")
	if _, err := svc.AuthorizeAccessGrantOperation(context.Background(), fresh.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}}); err != nil {
		t.Fatalf("policy packaging drift blocked collaboration authorize: %v", err)
	}
	// Renewal still requires exact authority packaging continuity.
	if _, err := svc.RenewAccessGrant(context.Background(), fresh.GrantToken, service.AccessGrantRenewInput{ExecutionContext: accessGrantExecutionContextInput()}); !errors.Is(err, service.ErrAccessGrantConflict) {
		t.Fatalf("policy-drift renewal err=%v", err)
	}
}

func setupAccessGrantService(t *testing.T) (*service.Service, db.User, db.Repository) {
	t.Helper()
	svc, cleanup := testharness.NewService(t, testharness.ServiceConfig{})
	t.Cleanup(cleanup)
	executor := db.User{Login: "runtime-default-executor", Name: "Runtime Executor", Type: db.TypeUser, Status: "active", UserKind: db.UserKindAgent}
	if err := svc.DB.Create(&executor).Error; err != nil {
		t.Fatal(err)
	}
	repository := db.Repository{Name: "demo", FullName: accessGrantTestRepo, OwnerID: executor.ID, DefaultBranch: "main", Visibility: "private", Private: true}
	if err := svc.DB.Create(&repository).Error; err != nil {
		t.Fatal(err)
	}
	svc.ExecutionContextPuller = &staticAccessGrantContextPuller{result: accessGrantPullResult(t)}
	svc.PrincipalSessions = accessGrantAuthority(t, executor.ID, "v1")
	return svc, executor, repository
}

func accessGrantAuthority(t *testing.T, defaultPrincipalID uint, revision string) *sessionauthority.Set {
	t.Helper()
	defaultOperations := []string{"ci.read", "git.push", "git.read", "pr.create", "pr.rebase", "pr.read", "repo.read", "review.read"}
	config := sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: accessGrantTestSource, Issuer: "multica", KeyIDs: []string{"source-key"}, Status: "active", TrustRevision: "trust-" + revision}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: sessionauthority.DefaultDynamicPolicyClass, Status: "active", PolicyRevision: "default-" + revision, Operations: defaultOperations}},
		TeamBindings: []sessionauthority.TeamBinding{{
			ID: "default-team", IssuerInstanceID: accessGrantTestSource, WorkspaceID: accessGrantTestWorkspace,
			TeamIdentityID: "default-team", PolicyClass: sessionauthority.DefaultDynamicPolicyClass,
			PrincipalID: defaultPrincipalID, Status: "active", BindingRevision: "default-binding-" + revision, EpochFloor: 1,
		}},
		Resources: []sessionauthority.ResourcePolicy{{
			ID: "demo", Target: "primary-a", Service: "ags", Repository: accessGrantTestRepo,
			Status: "active", MaxSessionTTL: "30m", PolicyRevision: "resource-" + revision,
		}},
	}
	authority, err := sessionauthority.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func accessGrantIssueInput(agentID, policyClass string, operations []string) service.AccessGrantIssueInput {
	input := service.AccessGrantIssueInput{
		ExecutionContext: accessGrantExecutionContextInput(), Repository: accessGrantTestRepo,
		AgentSelector: agentID, PolicyClass: policyClass, AdditionalOperations: operations,
	}
	return input
}

func accessGrantExecutionContextInput() service.ExecutionContextIntakeInput {
	return service.ExecutionContextIntakeInput{
		SourceInstanceID: accessGrantTestSource,
		Locator:          executioncontext.Locator{WorkspaceID: accessGrantTestWorkspace, AgentID: accessGrantTestAgent, TaskID: accessGrantTestTask},
		SourceToken:      "current-task-token",
	}
}

func accessGrantPullResult(t *testing.T) executioncontext.PullResult {
	t.Helper()
	observed := time.Now().UTC().Format(time.RFC3339Nano)
	current := executioncontext.CurrentContext{
		Schema: executioncontext.MulticaCurrentExecutionContextSchema, ObservedAt: observed,
		Workspace:   executioncontext.Workspace{ID: accessGrantTestWorkspace, Name: "Mini", Slug: "primary-a"},
		Agent:       executioncontext.Agent{ID: accessGrantTestAgent, Name: "fixture-maintainer", Status: "working"},
		Task:        executioncontext.Task{ID: accessGrantTestTask, Status: "running", Attempt: 1, MaxAttempts: 2},
		Run:         executioncontext.Run{ID: accessGrantTestRun, TaskID: accessGrantTestTask, Status: "running", Attempt: 1, MaxAttempts: 2},
		Attribution: &executioncontext.Attribution{Source: "direct_human", Precise: true},
	}
	body, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	return executioncontext.PullResult{
		SourceRef: executioncontext.SourceRef{
			Schema: executioncontext.SourceRefSchema, SourceInstanceID: accessGrantTestSource, Adapter: executioncontext.AdapterMulticaCurrentExecutionContextV1,
			WorkspaceID: accessGrantTestWorkspace, WorkspaceRef: "primary-a", AgentID: accessGrantTestAgent,
			TaskID: accessGrantTestTask, RunID: accessGrantTestRun, ObservedAt: observed,
		},
		Context: current, ContextJSON: body, ContextDigest: "sha256:" + hex.EncodeToString(digest[:]),
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
