package forgejointegration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnsurePullRequestRetriesKilledPushAfterRemoteRefCreatedAtExpectedSHA(t *testing.T) {
	const (
		repoFullName = "example-owner/demo"
		repoPath     = "/repos/example-owner/demo.git"
		branch       = "agent/large-binary"
		ref          = "refs/heads/" + branch
		expectedSHA  = "b487eae1a5ecb7a6578ba449c6ec7ea19b9cf0bf"
	)
	client := &fakeClient{pullRequestHead: expectedSHA}
	var remoteBranchSHA string
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		remoteBranchSHA = expectedSHA
		return errors.New("git push: signal: killed\nremote: Create a new pull request for 'agent/large-binary'")
	})
	integration.remoteRef = func(ctx context.Context, repoPath, remoteURL, gotRef string) (string, error) {
		if gotRef != ref {
			t.Fatalf("remote ref read ref=%q, want %q", gotRef, ref)
		}
		return remoteBranchSHA, nil
	}

	res, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: repoFullName,
		RepoPath:     repoPath,
		Head:         branch,
		Base:         "main",
		Title:        "Add large deck assets",
		Body:         "body",
		HeadSHA:      expectedSHA,
	})
	if err != nil {
		t.Fatalf("EnsurePullRequest should resume when remote branch already exists at expected SHA: %v", err)
	}
	if remoteBranchSHA != expectedSHA {
		t.Fatalf("remote branch SHA=%q, want expected AGS head %q", remoteBranchSHA, expectedSHA)
	}
	if res.Number != 1 || len(client.ensuredPRs) != 1 {
		t.Fatalf("projection did not continue to PR ensure: result=%#v ensured=%v", res, client.ensuredPRs)
	}
}

func TestEnsurePullRequestCanceledRequestContextLeavesPartialRemoteRefUnresumed(t *testing.T) {
	const (
		branch      = "agent/request-context-large-binary"
		ref         = "refs/heads/" + branch
		expectedSHA = "b487eae1a5ecb7a6578ba449c6ec7ea19b9cf0bf"
	)
	client := &fakeClient{pullRequestHead: expectedSHA}
	var remoteBranchSHA string
	var remoteRefRead bool
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		remoteBranchSHA = expectedSHA
		return errors.New("git push: signal: killed\nunknown_projection_error refs/heads/agent/request-context-large-binary")
	})
	integration.remoteRef = func(ctx context.Context, repoPath, remoteURL, gotRef string) (string, error) {
		remoteRefRead = true
		if gotRef != ref {
			t.Fatalf("remote ref read ref=%q, want %q", gotRef, ref)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
			return remoteBranchSHA, nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := integration.EnsurePullRequest(ctx, PullRequestSyncRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Head:         branch,
		Base:         "main",
		Title:        "Add large deck assets",
		Body:         "body",
		HeadSHA:      expectedSHA,
	})
	if err == nil {
		t.Fatal("expected canceled request-context projection to fail before PR ensure")
	}
	if !strings.Contains(err.Error(), "git push: signal: killed") {
		t.Fatalf("error lost killed-push evidence: %v", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("projection error leaked token: %v", err)
	}
	if remoteBranchSHA != expectedSHA || !remoteRefRead {
		t.Fatalf("test did not simulate remote ref created then verification blocked: remoteSHA=%q remoteRefRead=%v", remoteBranchSHA, remoteRefRead)
	}
	if len(client.ensuredPRs) != 0 {
		t.Fatalf("canceled request context must not continue to Forgejo PR ensure: %v", client.ensuredPRs)
	}
}

func TestEnsurePullRequestRejectsDifferentRemoteSHAAfterKilledPush(t *testing.T) {
	const (
		branch      = "agent/drift-large-binary"
		ref         = "refs/heads/" + branch
		expectedSHA = "b487eae1a5ecb7a6578ba449c6ec7ea19b9cf0bf"
		actualSHA   = "d487eae1a5ecb7a6578ba449c6ec7ea19b9cf0bf"
	)
	client := &fakeClient{pullRequestHead: expectedSHA}
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		return errors.New("git push: signal: killed\nremote: error: cannot lock ref 'refs/heads/agent/drift-large-binary': reference already exists")
	})
	integration.remoteRef = func(ctx context.Context, repoPath, remoteURL, gotRef string) (string, error) {
		if gotRef != ref {
			t.Fatalf("remote ref read ref=%q, want %q", gotRef, ref)
		}
		return actualSHA, nil
	}

	_, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Head:         branch,
		Base:         "main",
		Title:        "Add large deck assets",
		Body:         "body",
		HeadSHA:      expectedSHA,
	})
	if err == nil {
		t.Fatal("expected drift error when Forgejo branch exists at a different SHA")
	}
	if !strings.Contains(err.Error(), "git push: signal: killed") || !strings.Contains(err.Error(), "reference already exists") {
		t.Fatalf("error lost killed-push/lock-ref evidence: %v", err)
	}
	classified := ClassifyProjectionError(ref, expectedSHA, err)
	if classified.Type != ProjectionFailureNonFastForward {
		t.Fatalf("classified conflict type=%q, want %q; error=%v", classified.Type, ProjectionFailureNonFastForward, err)
	}
	if strings.Contains(classified.ErrorSummary, "secret-token") {
		t.Fatalf("classified error leaked token: %q", classified.ErrorSummary)
	}
	if len(client.ensuredPRs) != 0 {
		t.Fatalf("drift must not continue to Forgejo PR ensure: %v", client.ensuredPRs)
	}
}
