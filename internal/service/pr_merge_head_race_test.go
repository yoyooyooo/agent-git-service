package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
)

// Regression: a push lands after MergeStandardPR checked H but
// before its provider-ref read returns. The standard expected-head contract
// must reject the now-stale H without advancing main or closing the PR.
func TestAstraR1StandardMergeRejectsHeadMovingDuringProviderRead(t *testing.T) {
	for _, method := range []string{"merge", "rebase"} {
		for _, tc := range []struct {
			name                     string
			moveRef, movePR, closePR bool
		}{
			{"source and PR head", true, true, false},
			{"source ref only", true, false, false},
			{"PR head only", false, true, false},
			{"PR closed", false, false, true},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				svc, _, input, provider := setupAccessGrantMerge(t, "success")
				actor := addHumanMergeActor(t, svc, "astra-r1-head-race")
				ctx := service.ContextWithUser(context.Background(), actor)
				const repo = "example-owner/demo"
				const branch = "feature/astra-r1"
				if err := svc.Git.CreateBranch(ctx, repo, branch, "main"); err != nil {
					t.Fatal(err)
				}
				head, err := svc.Git.WriteFile(ctx, repo, branch, "first.txt", "H", []byte("first\n"))
				if err != nil {
					t.Fatal(err)
				}
				var pr db.PullRequest
				if err := svc.DB.First(&pr, "number = ?", input.AGSPRNumber).Error; err != nil {
					t.Fatal(err)
				}
				if err := svc.DB.Model(&pr).Updates(map[string]any{"head_ref": branch, "head_sha": head}).Error; err != nil {
					t.Fatal(err)
				}
				provider.mu.Lock()
				provider.snapshot.HeadRef = branch
				provider.snapshot.HeadSHA = head
				provider.snapshot.BaseSHA = head
				provider.snapshot.Mergeable = false
				provider.mu.Unlock()
				base, err := svc.Git.HeadSHA(ctx, repo, "main")
				if err != nil {
					t.Fatal(err)
				}
				enabled := true
				moved := ""
				svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
					Enabled: true, BaseURL: "http://forgejo.invalid", Token: "server-owned-token",
					AuthorityPolicyEnabled: true, WebhookURL: "http://ags.invalid/webhook", IntegrationBot: "ags-bot",
					RepoMap: map[string]forgejointegration.RepoMapping{
						repo: {Owner: "forgejo", Repo: "demo", Enabled: &enabled, BaseBranch: "main", DelegatedMergeMethod: "fast-forward-only", FastForwardAck: true},
					},
				}, provider, func(context.Context, forgejointegration.PushRequest) error { return nil },
					func(context.Context, string, string, string) (string, error) {
						if moved == "" {
							if tc.moveRef {
								moved, err = svc.Git.WriteFile(ctx, repo, branch, "second.txt", "H2", []byte("second\n"))
								if err != nil {
									return "", err
								}
							} else {
								moved = strings.Repeat("b", 40)
							}
							if tc.movePR {
								if err := svc.DB.Model(&pr).Update("head_sha", moved).Error; err != nil {
									return "", err
								}
							}
							if tc.closePR {
								if err := svc.DB.Model(&pr).Update("state", db.StateClosed).Error; err != nil {
									return "", err
								}
							}
						}
						return head, nil
					})
				result, mergeErr := svc.MergeStandardPR(ctx, repo, input.AGSPRNumber, method, "", head)
				actualBase, err := svc.Git.HeadSHA(ctx, repo, "main")
				if err != nil {
					t.Fatal(err)
				}
				var current db.PullRequest
				if err := svc.DB.First(&current, pr.ID).Error; err != nil {
					t.Fatal(err)
				}
				t.Logf("expected H=%s; moved head H2=%s; base before=%s; base after=%s; returned merged=%v; stored head=%s; stored merged=%v; stored merge SHA=%s; merge error=%v", head, moved, base, actualBase, result.Merged, current.HeadSHA, current.Merged, current.MergeCommitSHA, mergeErr)
				if !errors.Is(mergeErr, service.ErrConflict) || actualBase != base || current.Merged || (!tc.closePR && current.State != db.StateOpen) {
					t.Fatalf("stale expected head accepted: want conflict, unchanged base and open PR")
				}
			})
		}
	}
}

func TestAstraR1StandardMergeRejectsSquashForLinearHead(t *testing.T) {
	svc, _, input, provider := setupAccessGrantMerge(t, "success")
	actor := addHumanMergeActor(t, svc, "astra-r1-squash")
	ctx := service.ContextWithUser(context.Background(), actor)
	const repo = "example-owner/demo"
	const branch = "feature/astra-squash"
	if err := svc.Git.CreateBranch(ctx, repo, branch, "main"); err != nil {
		t.Fatal(err)
	}
	head, err := svc.Git.WriteFile(ctx, repo, branch, "squash.txt", "linear", []byte("linear\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.PullRequest{}).Where("number = ?", input.AGSPRNumber).Updates(map[string]any{"head_ref": branch, "head_sha": head}).Error; err != nil {
		t.Fatal(err)
	}
	bindFastForwardAck(t, svc, provider, repo, head)
	before, err := svc.Git.HeadSHA(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	_, mergeErr := svc.MergeStandardPR(ctx, repo, input.AGSPRNumber, "squash", "", head)
	after, err := svc.Git.HeadSHA(ctx, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(mergeErr, service.ErrConflict) || before != after {
		t.Fatalf("squash err=%v base before=%s after=%s", mergeErr, before, after)
	}
}
