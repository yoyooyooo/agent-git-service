package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestStandardMergeFastForwardAuthorityAcceptsGHStrategies(t *testing.T) {
	for _, method := range []string{"rebase", "merge"} {
		t.Run(method, func(t *testing.T) {
			svc, _, input, provider := setupAccessGrantMerge(t, "success")
			actor := addHumanMergeActor(t, svc, "standard-ff-"+method)
			var repo db.Repository
			if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
				t.Fatal(err)
			}
			ctx := service.ContextWithUser(context.Background(), actor)
			if err := svc.Git.CreateBranch(ctx, repo.FullName, "feature/standard-ff", "main"); err != nil {
				t.Fatal(err)
			}
			head, err := svc.Git.WriteFile(ctx, repo.FullName, "feature/standard-ff", "ff.txt", "linear", []byte("linear\n"))
			if err != nil {
				t.Fatal(err)
			}
			if err = svc.DB.Model(&db.PullRequest{}).Where("repository_id = ? AND number = ?", repo.ID, input.AGSPRNumber).Updates(map[string]any{"head_ref": "feature/standard-ff", "head_sha": head}).Error; err != nil {
				t.Fatal(err)
			}
			provider.mu.Lock()
			provider.snapshot.HeadRef = "feature/standard-ff"
			provider.snapshot.HeadSHA = head
			provider.snapshot.BaseSHA = head
			provider.snapshot.Mergeable = false
			provider.mu.Unlock()
			bindFastForwardAck(t, svc, provider, repo.FullName, head)
			before, err := svc.Git.HeadSHA(ctx, repo.FullName, "main")
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.MergeStandardPR(ctx, repo.FullName, input.AGSPRNumber, method, "", strings.Repeat("b", 40))
			if !errors.Is(err, service.ErrConflict) {
				t.Fatalf("wrong expected head accepted: %v", err)
			}
			unchanged, _ := svc.Git.HeadSHA(ctx, repo.FullName, "main")
			if unchanged != before {
				t.Fatal("rejected merge changed base")
			}
			pr, err := svc.MergeStandardPR(ctx, repo.FullName, input.AGSPRNumber, method, "", head)
			if err != nil {
				t.Fatal(err)
			}
			base, err := svc.Git.HeadSHA(ctx, repo.FullName, "main")
			if err != nil || base != head || !pr.Merged || pr.MergeCommitSHA != head {
				t.Fatalf("non fast-forward result: base=%s head=%s merged=%v merge=%s err=%v", base, head, pr.Merged, pr.MergeCommitSHA, err)
			}
			provider.mu.Lock()
			calls := provider.mergeCalls
			provider.mu.Unlock()
			if calls != 0 {
				t.Fatalf("already advanced provider received %d merge POSTs", calls)
			}
		})
	}
}

func TestStandardMergeFastForwardAuthorityRejectsDivergedHead(t *testing.T) {
	svc, _, input, provider := setupAccessGrantMerge(t, "success")
	actor := addHumanMergeActor(t, svc, "standard-ff-diverged")
	ctx := service.ContextWithUser(context.Background(), actor)
	const repo = "example-owner/demo"
	if err := svc.Git.CreateBranch(ctx, repo, "feature/diverged", "main"); err != nil {
		t.Fatal(err)
	}
	head, err := svc.Git.WriteFile(ctx, repo, "feature/diverged", "feature.txt", "feature", []byte("feature\n"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := svc.Git.WriteFile(ctx, repo, "main", "base.txt", "base advanced", []byte("base\n"))
	if err != nil {
		t.Fatal(err)
	}
	var repository db.Repository
	if err = svc.DB.First(&repository, "full_name = ?", repo).Error; err != nil {
		t.Fatal(err)
	}
	if err = svc.DB.Model(&db.PullRequest{}).Where("repository_id = ? AND number = ?", repository.ID, input.AGSPRNumber).Updates(map[string]any{"head_ref": "feature/diverged", "head_sha": head}).Error; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.snapshot.HeadRef = "feature/diverged"
	provider.snapshot.HeadSHA = head
	provider.snapshot.BaseSHA = base
	provider.mu.Unlock()
	bindFastForwardAck(t, svc, provider, repo, base)
	for _, method := range []string{"merge", "rebase", "squash"} {
		_, err = svc.MergeStandardPR(ctx, repo, input.AGSPRNumber, method, "", head)
		if !errors.Is(err, service.ErrConflict) {
			t.Fatalf("%s accepted divergent input: %v", method, err)
		}
	}
	got, _ := svc.Git.HeadSHA(ctx, repo, "main")
	if got != base {
		t.Fatal("rejected merge changed base")
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("rejection sent %d provider merges", calls)
	}
}
