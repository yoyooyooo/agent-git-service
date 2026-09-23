package forgejointegration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type mergeRecoveryClient struct {
	mergeCalls int
	readCalls  int
	snapshot   PullRequestSnapshot
}

func (*mergeRecoveryClient) EnsureRepository(context.Context, string, string, bool) error {
	return nil
}

func (*mergeRecoveryClient) EnsurePullRequest(context.Context, PullRequestRequest) (PullRequestResult, error) {
	return PullRequestResult{}, nil
}

func (*mergeRecoveryClient) UpdatePullRequestState(context.Context, string, string, int, string) (PullRequestResult, error) {
	return PullRequestResult{}, nil
}

func (c *mergeRecoveryClient) MergePullRequest(context.Context, string, string, int, PullRequestMergeRequest) error {
	c.mergeCalls++
	return errors.New("connection reset after provider accepted merge")
}

func (c *mergeRecoveryClient) GetPullRequest(context.Context, string, string, int) (PullRequestSnapshot, bool, error) {
	c.readCalls++
	return c.snapshot, true, nil
}

func TestMergePullRequestReconcilesTransportErrorFromExactProviderReadback(t *testing.T) {
	head := strings.Repeat("a", 40)
	client := &mergeRecoveryClient{snapshot: PullRequestSnapshot{
		Number: 2, State: "closed", Merged: true, HeadSHA: head, MergeCommitSHA: head,
	}}
	integration := New(Config{
		Enabled: true,
		RepoMap: map[string]RepoMapping{
			"example-team/app-fixture": {Owner: "example-team", Repo: "app-fixture", BaseBranch: "main"},
		},
	}, client, nil)

	result, err := integration.MergePullRequest(t.Context(), "example-team/app-fixture", "example-team/app-fixture", 2, PullRequestMergeRequest{
		Method: "fast-forward-only", ExpectedHead: head,
	})
	if err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if result.ProviderOutcome != "reconciled_after_error" || !result.Snapshot.Merged {
		t.Fatalf("result=%#v", result)
	}
	if client.mergeCalls != 1 || client.readCalls != 1 {
		t.Fatalf("calls merge=%d read=%d", client.mergeCalls, client.readCalls)
	}
}

func TestMergePullRequestDoesNotConfirmMergedSnapshotAtAnotherHead(t *testing.T) {
	expectedHead := strings.Repeat("a", 40)
	client := &mergeRecoveryClient{snapshot: PullRequestSnapshot{
		Number: 2, State: "closed", Merged: true, HeadSHA: strings.Repeat("b", 40), MergeCommitSHA: strings.Repeat("c", 40),
	}}
	integration := New(Config{
		Enabled: true,
		RepoMap: map[string]RepoMapping{
			"example-team/app-fixture": {Owner: "example-team", Repo: "app-fixture", BaseBranch: "main"},
		},
	}, client, nil)

	result, err := integration.MergePullRequest(t.Context(), "example-team/app-fixture", "example-team/app-fixture", 2, PullRequestMergeRequest{
		Method: "fast-forward-only", ExpectedHead: expectedHead,
	})
	if err == nil || result.ProviderOutcome != "outcome_unknown" {
		t.Fatalf("result=%#v err=%v, want outcome_unknown", result, err)
	}
	if client.mergeCalls != 1 || client.readCalls != 6 {
		t.Fatalf("calls merge=%d read=%d", client.mergeCalls, client.readCalls)
	}
}
