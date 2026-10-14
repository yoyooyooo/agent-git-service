package forgejointegration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type readinessClient struct {
	authorityPolicyFakeClient
	mu                     sync.Mutex
	active, maximum, calls int
	started                chan struct{}
	release                chan struct{}
	drift                  string
}

func (c *readinessClient) InspectRepositoryAuthority(ctx context.Context, owner, repo, base, hook, bot string) (RepositoryAuthorityState, error) {
	c.mu.Lock()
	c.active++
	c.calls++
	c.maximum = max(c.maximum, c.active)
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.active--; c.mu.Unlock() }()
	select {
	case c.started <- struct{}{}:
	default:
	}
	select {
	case <-c.release:
	case <-ctx.Done():
		return RepositoryAuthorityState{}, ctx.Err()
	}
	state := convergedAuthorityState()
	if repo == c.drift {
		state.ForcePushBlocked = false
	}
	return state, nil
}
func readinessIntegration(c *readinessClient) *Integration {
	mapping := map[string]RepoMapping{}
	for n := range 10 {
		repo := fmt.Sprintf("repo-%02d", n)
		mapping["fixture/"+repo] = RepoMapping{Owner: "fixture", Repo: repo, BaseBranch: "main"}
	}
	return New(Config{Enabled: true, BaseURL: "https://provider.example.test", WebhookSecret: "fixture-secret", AuthorityPolicyEnabled: true, WebhookURL: "https://primary.example.test/hook", IntegrationBot: "fixture-bot", RepoMap: mapping}, c, func(context.Context, PushRequest) error { return nil })
}
func TestReadinessAuthorityBoundedConcurrencyAndAllRepositories(t *testing.T) {
	c := &readinessClient{started: make(chan struct{}, 10), release: make(chan struct{}), drift: "repo-09"}
	i := readinessIntegration(c)
	type result struct {
		plans []RepositoryAuthorityPlan
		err   error
	}
	done := make(chan result, 1)
	go func() { p, e := i.VerifyRepositoryAuthorityReadiness(context.Background()); done <- result{p, e} }()
	for range 4 {
		select {
		case <-c.started:
		case <-time.After(time.Second):
			close(c.release)
			t.Fatal("readiness serialized provider scans")
		}
	}
	close(c.release)
	r := <-done
	if r.err == nil || len(r.plans) != 10 {
		t.Fatal("drift in last repository must fail readiness")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maximum != 4 || c.calls != 10 {
		t.Fatalf("maximum=%d calls=%d", c.maximum, c.calls)
	}
	for n, p := range r.plans {
		if p.AGSRepo != fmt.Sprintf("fixture/repo-%02d", n) {
			t.Fatal("plans must preserve deterministic target order")
		}
	}
}
func TestReadinessAuthorityCancelsBlockedProvider(t *testing.T) {
	c := &readinessClient{started: make(chan struct{}, 10), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := readinessIntegration(c).VerifyRepositoryAuthorityReadiness(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected canceled provider scan, got %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != 0 || c.maximum > 4 {
		t.Fatal("provider workers leaked after cancellation")
	}
}
