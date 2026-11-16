package forgejointegration

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// countingAuthorityClient records every provider call per Forgejo repository.
// It embeds the existing fake only for the non-authority Client methods.
type countingAuthorityClient struct {
	*authorityPolicyFakeClient
	mu       sync.Mutex
	states   map[string]RepositoryAuthorityState
	inspects map[string]int
	applies  map[string]int
}

func newCountingAuthorityClient(drifted ...string) *countingAuthorityClient {
	c := &countingAuthorityClient{authorityPolicyFakeClient: &authorityPolicyFakeClient{}, states: map[string]RepositoryAuthorityState{}, inspects: map[string]int{}, applies: map[string]int{}}
	for _, repo := range drifted {
		c.states[repo] = RepositoryAuthorityState{AllowRebaseUpdate: true}
	}
	return c
}

func (c *countingAuthorityClient) InspectRepositoryAuthority(_ context.Context, owner, repo, _, _, _ string) (RepositoryAuthorityState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := owner + "/" + repo
	c.inspects[key]++
	if state, ok := c.states[key]; ok {
		return state, nil
	}
	return convergedAuthorityState(), nil
}

func (c *countingAuthorityClient) ApplyRepositoryAuthority(_ context.Context, owner, repo, _, _, _, _ string, _ RepositoryAuthorityState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := owner + "/" + repo
	c.applies[key]++
	c.states[key] = convergedAuthorityState()
	return nil
}

func (c *countingAuthorityClient) calls(repo string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inspects[repo] + c.applies[repo]
}

func selectionIntegration(client *countingAuthorityClient) *Integration {
	disabled := false
	integration := New(Config{
		Enabled: true, WebhookSecret: "secret", AuthorityPolicyEnabled: true,
		WebhookURL: "https://ags.example/api/v3/integrations/forgejo/webhook", IntegrationBot: "ags-bot",
		RepoMap: map[string]RepoMapping{
			"jackie/lumi":   {Owner: "jackie", Repo: "lumi", BaseBranch: "main"},
			"jackie/other":  {Owner: "jackie", Repo: "other", BaseBranch: "main"},
			"acme/widget":   {Owner: "ci", Repo: "widget", BaseBranch: "main"},
			"jackie/paused": {Owner: "jackie", Repo: "paused", BaseBranch: "main", Enabled: &disabled},
		},
	}, client, func(context.Context, PushRequest) error { return nil })
	return integration
}

func TestSelectedRepositoryAuthorityMakesZeroCallsForOtherRepositories(t *testing.T) {
	client := newCountingAuthorityClient("jackie/lumi", "jackie/other", "ci/widget")
	integration := selectionIntegration(client)
	ctx := context.Background()

	plans, err := integration.PlanSelectedRepositoryAuthority(ctx, []string{"jackie/lumi"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plans) != 1 || plans[0].ForgejoRepo != "jackie/lumi" || plans[0].Converged {
		t.Fatalf("unexpected selected plan: %#v", plans)
	}
	verified, err := integration.ApplySelectedRepositoryAuthority(ctx, []string{"jackie/lumi"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(verified) != 1 || !verified[0].Converged || client.applies["jackie/lumi"] != 1 {
		t.Fatalf("unexpected apply result: %#v applies=%v", verified, client.applies)
	}
	if _, err := integration.VerifySelectedRepositoryAuthority(ctx, []string{"jackie/lumi"}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, other := range []string{"jackie/other", "ci/widget", "jackie/paused"} {
		if calls := client.calls(other); calls != 0 {
			t.Fatalf("unselected repository %s received %d provider calls", other, calls)
		}
	}
	// The unselected drifted repositories still fail the unfiltered verify,
	// proving the selection, not convergence, kept them untouched.
	if _, err := integration.VerifyRepositoryAuthority(ctx); err == nil {
		t.Fatal("unfiltered verify ignored drift in unselected repositories")
	}
}

func TestSelectedRepositoryAuthorityMatchesAGSOrForgejoNameAndRepeats(t *testing.T) {
	client := newCountingAuthorityClient()
	integration := selectionIntegration(client)
	plans, err := integration.PlanSelectedRepositoryAuthority(context.Background(), []string{"acme/widget", "jackie/lumi", "ci/widget"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	got := make([]string, 0, len(plans))
	for _, plan := range plans {
		got = append(got, plan.ForgejoRepo)
	}
	if strings.Join(got, ",") != "ci/widget,jackie/lumi" {
		t.Fatalf("selection should dedupe and keep default order, got %v", got)
	}
	if client.calls("jackie/other") != 0 {
		t.Fatalf("unselected repository was inspected")
	}
}

func TestSelectedRepositoryAuthorityRejectsUnknownSelectorsBeforeProviderCalls(t *testing.T) {
	for _, selector := range []string{"jackie/missing", "jackie/paused", "lumi", "jackie/lumi/extra", "/lumi", " "} {
		client := newCountingAuthorityClient()
		integration := selectionIntegration(client)
		for name, run := range map[string]func(context.Context, []string) ([]RepositoryAuthorityPlan, error){
			"plan": integration.PlanSelectedRepositoryAuthority, "apply": integration.ApplySelectedRepositoryAuthority, "verify": integration.VerifySelectedRepositoryAuthority,
		} {
			if _, err := run(context.Background(), []string{"jackie/lumi", selector}); err == nil {
				t.Fatalf("%s accepted selector %q", name, selector)
			}
		}
		if len(client.inspects) != 0 || len(client.applies) != 0 {
			t.Fatalf("selector %q reached the provider: inspects=%v applies=%v", selector, client.inspects, client.applies)
		}
	}
}

func TestSelectedRepositoryAuthorityEmptySelectionKeepsDefaultBehavior(t *testing.T) {
	client := newCountingAuthorityClient()
	integration := selectionIntegration(client)
	all, err := integration.PlanRepositoryAuthority(context.Background())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	selected, err := integration.PlanSelectedRepositoryAuthority(context.Background(), nil)
	if err != nil {
		t.Fatalf("selected plan: %v", err)
	}
	if len(all) != 3 || len(selected) != 3 {
		t.Fatalf("default should cover every enabled mapping: all=%d selected=%d", len(all), len(selected))
	}
	if client.calls("jackie/paused") != 0 {
		t.Fatalf("disabled mapping was inspected")
	}
}

func TestSelectedRepositoryAuthorityRejectsAmbiguousSelector(t *testing.T) {
	client := newCountingAuthorityClient()
	integration := New(Config{
		Enabled: true, WebhookSecret: "secret", AuthorityPolicyEnabled: true,
		WebhookURL: "https://ags.example/hook", IntegrationBot: "ags-bot",
		RepoMap: map[string]RepoMapping{
			"jackie/a": {Owner: "jackie", Repo: "b"},
			"jackie/b": {Owner: "ci", Repo: "b"},
		},
	}, client, func(context.Context, PushRequest) error { return nil })
	if _, err := integration.PlanSelectedRepositoryAuthority(context.Background(), []string{"jackie/b"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous selector failure, got %v", err)
	}
	if len(client.inspects) != 0 {
		t.Fatalf("ambiguous selector reached the provider: %v", client.inspects)
	}
}
