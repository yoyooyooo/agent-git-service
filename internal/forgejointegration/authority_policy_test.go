package forgejointegration

import (
	"context"
	"strings"
	"testing"
)

type authorityPolicyFakeClient struct {
	state      RepositoryAuthorityState
	applyCount int
}

func (c *authorityPolicyFakeClient) EnsureRepository(context.Context, string, string, bool) error {
	return nil
}
func (c *authorityPolicyFakeClient) EnsurePullRequest(context.Context, PullRequestRequest) (PullRequestResult, error) {
	return PullRequestResult{}, nil
}
func (c *authorityPolicyFakeClient) UpdatePullRequestState(context.Context, string, string, int, string) (PullRequestResult, error) {
	return PullRequestResult{}, nil
}
func (c *authorityPolicyFakeClient) InspectRepositoryAuthority(context.Context, string, string, string, string, string) (RepositoryAuthorityState, error) {
	return c.state, nil
}
func (c *authorityPolicyFakeClient) ApplyRepositoryAuthority(context.Context, string, string, string, string, string, string, RepositoryAuthorityState) error {
	c.applyCount++
	labels := map[string]bool{}
	for _, label := range authorityWorkflowLabels {
		labels[label] = true
	}
	c.state = RepositoryAuthorityState{
		BaseBranchProtected: true, DirectPushBlocked: true, ForcePushBlocked: true,
		IntegrationBotCollaborator: true, IntegrationBotAuthorized: true, IntegrationBotMergeAuthorized: true,
		Labels: labels, WebhookID: 3, WebhookActive: true, WebhookPullRequests: true, WebhookIssues: true, WebhookDelete: true,
	}
	return nil
}

func TestRepositoryAuthorityPlanApplyVerifyIsIdempotent(t *testing.T) {
	client := &authorityPolicyFakeClient{state: RepositoryAuthorityState{
		AllowRebaseUpdate: true,
		Labels:            map[string]bool{AGSActionRebaseLabel: true},
	}}
	integration := New(Config{
		Enabled: true, WebhookSecret: "secret", AuthorityPolicyEnabled: true,
		WebhookURL: "https://ags.example/api/v3/integrations/forgejo/webhook", IntegrationBot: "ags-bot",
		RepoMap: map[string]RepoMapping{
			"acme/widget": {Owner: "ci", Repo: "widget", BaseBranch: "main"},
		},
	}, client, func(context.Context, PushRequest) error { return nil })

	plan, err := integration.PlanRepositoryAuthority(context.Background())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan) != 1 || plan[0].Converged || len(plan[0].Changes) < 3 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	if !containsAuthorityChange(plan[0].Changes, "disable_allow_rebase_update") || !containsAuthorityChange(plan[0].Changes, "protect_base_for_ags_authority") || !containsAuthorityChange(plan[0].Changes, "grant_integration_bot_write") || !containsAuthorityChange(plan[0].Changes, "configure_ags_webhook") {
		t.Fatalf("plan omitted authority changes: %v", plan[0].Changes)
	}
	if containsAuthorityChange(plan[0].Changes, "authorize_integration_bot") || containsAuthorityChange(plan[0].Changes, "authorize_integration_bot_merge") {
		t.Fatalf("plan still requires closed whitelists: %v", plan[0].Changes)
	}

	verified, err := integration.ApplyRepositoryAuthority(context.Background())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(verified) != 1 || !verified[0].Converged || client.applyCount != 1 {
		t.Fatalf("unexpected verified plan/count: %#v / %d", verified, client.applyCount)
	}
	if _, err := integration.ApplyRepositoryAuthority(context.Background()); err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	if client.applyCount != 1 {
		t.Fatalf("converged apply mutated provider again: %d", client.applyCount)
	}
}

func TestRepositoryAuthorityVerifyRejectsDrift(t *testing.T) {
	client := &authorityPolicyFakeClient{state: convergedAuthorityState()}
	client.state.AllowRebaseUpdate = true
	integration := New(Config{
		Enabled: true, WebhookSecret: "secret", AuthorityPolicyEnabled: true, WebhookURL: "https://ags.example/hook", IntegrationBot: "ags-bot",
		RepoMap: map[string]RepoMapping{"acme/widget": {Owner: "ci", Repo: "widget"}},
	}, client, func(context.Context, PushRequest) error { return nil })

	_, err := integration.VerifyRepositoryAuthority(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disable_allow_rebase_update") {
		t.Fatalf("expected drift verification failure, got %v", err)
	}
}

func TestRepositoryAuthorityRequiresIntegrationBot(t *testing.T) {
	integration := New(Config{
		Enabled: true, WebhookSecret: "secret", AuthorityPolicyEnabled: true, WebhookURL: "https://ags.example/hook",
		RepoMap: map[string]RepoMapping{"acme/widget": {Owner: "ci", Repo: "widget"}},
	}, &authorityPolicyFakeClient{}, func(context.Context, PushRequest) error { return nil })
	if _, err := integration.PlanRepositoryAuthority(context.Background()); err == nil || !strings.Contains(err.Error(), "integration bot") {
		t.Fatalf("expected integration bot failure, got %v", err)
	}
}

func TestPullRequestMergeAuthorityRequiresCollaboratorNotClosedWhitelist(t *testing.T) {
	state := convergedAuthorityState()
	client := &authorityPolicyFakeClient{state: state}
	integration := New(Config{
		Enabled: true, WebhookSecret: "secret", AuthorityPolicyEnabled: true,
		WebhookURL: "https://ags.example/hook", IntegrationBot: "ags-bot",
		RepoMap: map[string]RepoMapping{"acme/widget": {Owner: "ci", Repo: "widget", BaseBranch: "main"}},
	}, client, func(context.Context, PushRequest) error { return nil })

	client.state.IntegrationBotCollaborator = false
	if _, err := integration.VerifyPullRequestMergeAuthority(context.Background(), "acme/widget", "main"); err == nil || !strings.Contains(err.Error(), "lacks the server executor") {
		t.Fatalf("expected missing executor failure, got %v", err)
	}
	client.state.IntegrationBotCollaborator = true
	client.state.IntegrationBotAuthorized = false
	client.state.IntegrationBotMergeAuthorized = false
	client.state.DirectPushBlocked = false
	if _, err := integration.VerifyPullRequestMergeAuthority(context.Background(), "acme/widget", "main"); err != nil {
		t.Fatalf("open whitelist denied merge preflight: %v", err)
	}
}

func TestPullRequestMergeAuthorityAllowsMirroredNonDefaultBaseWithServerExecutor(t *testing.T) {
	client := &authorityPolicyFakeClient{state: RepositoryAuthorityState{
		IntegrationBotCollaborator: true,
	}}
	integration := New(Config{
		Enabled: true, WebhookSecret: "secret", AuthorityPolicyEnabled: true,
		WebhookURL: "https://ags.example/hook", IntegrationBot: "ags-bot",
		MirrorBranchIncludes: []string{"main", "conformance/c3/*/*/*"},
		RepoMap:              map[string]RepoMapping{"acme/widget": {Owner: "ci", Repo: "widget", BaseBranch: "main"}},
	}, client, func(context.Context, PushRequest) error { return nil })

	if _, err := integration.VerifyPullRequestMergeAuthority(context.Background(), "acme/widget", "conformance/c3/run/rebase/base"); err != nil {
		t.Fatalf("mirrored non-default base denied: %v", err)
	}
	if _, err := integration.VerifyPullRequestMergeAuthority(context.Background(), "acme/widget", "private/not-mirrored"); err == nil {
		t.Fatal("non-mirrored base was authorized")
	}
	if _, err := integration.VerifyPullRequestMergeAuthority(context.Background(), "acme/widget", "main"); err == nil {
		t.Fatal("unprotected configured base was authorized")
	}
}

func TestRepositoryAuthorityRejectsSharedForgejoTarget(t *testing.T) {
	integration := New(Config{
		Enabled: true, WebhookSecret: "secret", AuthorityPolicyEnabled: true, WebhookURL: "https://ags.example/hook", IntegrationBot: "ags-bot",
		RepoMap: map[string]RepoMapping{
			"acme/one": {Owner: "ci", Repo: "shared"},
			"acme/two": {Owner: "ci", Repo: "shared"},
		},
	}, &authorityPolicyFakeClient{}, func(context.Context, PushRequest) error { return nil })
	if _, err := integration.PlanRepositoryAuthority(context.Background()); err == nil || !strings.Contains(err.Error(), "mapped from both") {
		t.Fatalf("expected shared target failure, got %v", err)
	}
}

func TestRepositoryAuthorityRequiresExplicitMappedTargets(t *testing.T) {
	integration := New(Config{
		Enabled: true, WebhookSecret: "secret", AuthorityPolicyEnabled: true, WebhookURL: "https://ags.example/hook", IntegrationBot: "ags-bot",
	}, &authorityPolicyFakeClient{}, func(context.Context, PushRequest) error { return nil })
	if _, err := integration.PlanRepositoryAuthority(context.Background()); err == nil || !strings.Contains(err.Error(), "explicit repository mapping") {
		t.Fatalf("expected explicit mapping failure, got %v", err)
	}
}

func convergedAuthorityState() RepositoryAuthorityState {
	labels := map[string]bool{}
	for _, label := range authorityWorkflowLabels {
		labels[label] = true
	}
	return RepositoryAuthorityState{
		BaseBranchProtected: true, DirectPushBlocked: true, ForcePushBlocked: true,
		IntegrationBotCollaborator: true, IntegrationBotAuthorized: true, IntegrationBotMergeAuthorized: true,
		Labels: labels, WebhookID: 3, WebhookActive: true, WebhookPullRequests: true, WebhookIssues: true, WebhookDelete: true,
	}
}

func containsAuthorityChange(changes []string, want string) bool {
	for _, change := range changes {
		if change == want {
			return true
		}
	}
	return false
}
