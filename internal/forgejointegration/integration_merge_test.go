package forgejointegration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type mergeRecoveryClient struct {
	mergeCalls int
	readCalls  int
	snapshot   PullRequestSnapshot
	mergeError error
	readError  error
	missing    bool
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
	if c.mergeError != nil {
		return c.mergeError
	}
	return errors.New("connection reset after provider accepted merge")
}

func (c *mergeRecoveryClient) GetPullRequest(context.Context, string, string, int) (PullRequestSnapshot, bool, error) {
	c.readCalls++
	return c.snapshot, !c.missing, c.readError
}

func TestMergePullRequestClassifiesDefiniteRejectionFromExactReadback(t *testing.T) {
	head := strings.Repeat("a", 40)
	denied := &ProviderHTTPError{Status: http.StatusMethodNotAllowed, Method: http.MethodPost, RouteKind: "pull_merge"}
	for _, tc := range []struct {
		name        string
		mergeError  error
		snapshot    PullRequestSnapshot
		readError   error
		missing     bool
		wantOutcome string
		wantReads   int
	}{
		{name: "405 and same open head", mergeError: denied, snapshot: PullRequestSnapshot{Number: 2, State: "open", HeadSHA: head}, wantOutcome: "rejected", wantReads: 1},
		{name: "wrapped 405", mergeError: fmt.Errorf("provider request: %w", denied), snapshot: PullRequestSnapshot{Number: 2, State: "open", HeadSHA: head}, wantOutcome: "rejected", wantReads: 1},
		{name: "405 after exact merge", mergeError: denied, snapshot: PullRequestSnapshot{Number: 2, State: "closed", Merged: true, HeadSHA: head, MergeCommitSHA: head}, wantOutcome: "reconciled_after_error", wantReads: 1},
		{name: "405 and merged wrong PR number", mergeError: denied, snapshot: PullRequestSnapshot{Number: 3, State: "closed", Merged: true, HeadSHA: head, MergeCommitSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 and merged missing PR number", mergeError: denied, snapshot: PullRequestSnapshot{State: "closed", Merged: true, HeadSHA: head, MergeCommitSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "transport error", mergeError: errors.New("connection reset"), snapshot: PullRequestSnapshot{Number: 2, State: "open", HeadSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "503", mergeError: &ProviderHTTPError{Status: http.StatusServiceUnavailable, Method: http.MethodPost, RouteKind: "pull_merge"}, snapshot: PullRequestSnapshot{Number: 2, State: "open", HeadSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "409", mergeError: &ProviderHTTPError{Status: http.StatusConflict, Method: http.MethodPost, RouteKind: "pull_merge"}, snapshot: PullRequestSnapshot{Number: 2, State: "open", HeadSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "untyped 405", mergeError: errors.New("forgejo POST pull_merge returned status 405"), snapshot: PullRequestSnapshot{Number: 2, State: "open", HeadSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 from another route", mergeError: &ProviderHTTPError{Status: http.StatusMethodNotAllowed, Method: http.MethodPost, RouteKind: "pull_request"}, snapshot: PullRequestSnapshot{Number: 2, State: "open", HeadSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 from GET", mergeError: &ProviderHTTPError{Status: http.StatusMethodNotAllowed, Method: http.MethodGet, RouteKind: "pull_merge"}, snapshot: PullRequestSnapshot{Number: 2, State: "open", HeadSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 and changed head", mergeError: denied, snapshot: PullRequestSnapshot{Number: 2, State: "open", HeadSHA: strings.Repeat("b", 40)}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 and closed unmerged PR", mergeError: denied, snapshot: PullRequestSnapshot{Number: 2, State: "closed", HeadSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 and missing PR number", mergeError: denied, snapshot: PullRequestSnapshot{State: "open", HeadSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 and wrong PR number", mergeError: denied, snapshot: PullRequestSnapshot{Number: 3, State: "open", HeadSHA: head}, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 and unreadable PR", mergeError: denied, readError: errors.New("read unavailable"), wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 and cancelled read", mergeError: denied, readError: context.Canceled, wantOutcome: "outcome_unknown", wantReads: 6},
		{name: "405 and missing PR", mergeError: denied, missing: true, wantOutcome: "outcome_unknown", wantReads: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &mergeRecoveryClient{mergeError: tc.mergeError, snapshot: tc.snapshot, readError: tc.readError, missing: tc.missing}
			integration := New(Config{Enabled: true, RepoMap: map[string]RepoMapping{
				"example-team/app-fixture": {Owner: "example-team", Repo: "app-fixture", BaseBranch: "main"},
			}}, client, nil)
			result, err := integration.MergePullRequest(t.Context(), "example-team/app-fixture", "example-team/app-fixture", 2, PullRequestMergeRequest{Method: "rebase", ExpectedHead: head})
			if result.ProviderOutcome != tc.wantOutcome || (err == nil) != (tc.wantOutcome == "reconciled_after_error") {
				t.Fatalf("result=%#v err=%v, want %s", result, err, tc.wantOutcome)
			}
			if tc.wantOutcome == "rejected" {
				var providerErr *ProviderHTTPError
				if !errors.As(err, &providerErr) || providerErr != denied {
					t.Fatalf("provider rejection lost typed cause: %v", err)
				}
			}
			if client.mergeCalls != 1 || client.readCalls != tc.wantReads {
				t.Fatalf("calls merge=%d read=%d, want merge=1 read=%d", client.mergeCalls, client.readCalls, tc.wantReads)
			}
		})
	}
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
