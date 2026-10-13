package forgejointegration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAstraAuthorityRepairDenialMakesZeroProviderWrites(t *testing.T) {
	for _, denyAt := range []int{1, 2} {
		name := "before_backup"
		if denyAt == 2 {
			name = "after_backup_before_branch"
		}
		t.Run(name, func(t *testing.T) {
			const oldSHA = "1111111111111111111111111111111111111111"
			const newSHA = "2222222222222222222222222222222222222222"
			client := &fakeClient{pullRequestHead: oldSHA}
			backups, pushes, admissions := 0, 0, 0
			integration := New(Config{Enabled: true, BaseURL: "http://forgejo.local", Token: "fixture-only", DefaultOwner: "example-owner", AutoPullRequest: true, MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"}}, client, func(context.Context, PushRequest) error { pushes++; return nil })
			integration.remoteRef = func(context.Context, string, string, string) (string, error) { return oldSHA, nil }
			integration.backupTag = func(context.Context, string, string, string, string) error {
				if admissions != 1 {
					t.Fatal("backup write preceded admission")
				}
				backups++
				return nil
			}
			denied := errors.New("generation expired or superseded")
			_, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
				RepoFullName: "example-owner/demo", RepoPath: "/fixture", Head: "agent/demo", Base: "main", HeadSHA: newSHA,
				RewriteLease: &PullRequestRewriteLease{ExternalRepo: "example-owner/demo", ExternalNumber: 1, BackupDivergedObservedRef: true},
				BeforeWrite: func(context.Context) error {
					admissions++
					if admissions == denyAt {
						return denied
					}
					return nil
				},
			})
			if !errors.Is(err, denied) || !strings.Contains(err.Error(), "provider admission denied") {
				t.Fatalf("admission denial not returned: %v", err)
			}
			if backups != denyAt-1 || pushes != 0 || admissions != denyAt {
				t.Fatalf("denied worker made unexpected provider writes: backup tags=%d branch pushes=%d admissions=%d err=%v", backups, pushes, admissions, err)
			}
		})
	}
}
