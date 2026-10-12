package forgejointegration

import (
	"context"
	"strings"
	"testing"
)

func TestEnsurePullRequestAuthorityRepairBacksUpDivergedObservedRefBeforeLease(t *testing.T) {
	const (
		divergedSHA = "3333333333333333333333333333333333333333"
		newSHA      = "2222222222222222222222222222222222222222"
	)
	client := &fakeClient{pullRequestHead: divergedSHA}
	var pushes []PushRequest
	var backups []string
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token",
		DefaultOwner: "example-owner", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		client.pullRequestHead = newSHA
		return nil
	})
	integration.backupTag = func(ctx context.Context, repoPath, remoteURL, tag, sha string) error {
		backups = append(backups, tag+"@"+sha)
		return nil
	}
	branchReads := 0
	integration.remoteRef = func(ctx context.Context, repoPath, remoteURL, ref string) (string, error) {
		if strings.HasPrefix(ref, "refs/tags/") {
			return divergedSHA, nil
		}
		branchReads++
		// Observe, back up, then confirm the same tip before the lease push.
		if branchReads <= 2 {
			return divergedSHA, nil
		}
		return newSHA, nil
	}
	res, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git",
		Head: "agent/demo", Base: "main", Title: "title", HeadSHA: newSHA,
		RewriteLease: &PullRequestRewriteLease{
			ExternalRepo: "example-owner/demo", ExternalNumber: 1, BackupDivergedObservedRef: true,
		},
	})
	if err != nil {
		t.Fatalf("authority repair: %v", err)
	}
	wantTag := "ags-projection-backup/agent/demo/" + divergedSHA
	if len(backups) != 1 || backups[0] != wantTag+"@"+divergedSHA {
		t.Fatalf("backup=%v", backups)
	}
	if len(pushes) != 1 || pushes[0].ForceWithLeaseSHA != divergedSHA || pushes[0].ForceWithLeaseRef != "refs/heads/agent/demo" || strings.HasPrefix(pushes[0].Refspec, "+") {
		t.Fatalf("lease push=%#v", pushes)
	}
	if res.HeadSHA != newSHA {
		t.Fatalf("result=%#v", res)
	}
}
