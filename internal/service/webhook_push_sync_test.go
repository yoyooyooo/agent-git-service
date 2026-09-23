package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
)

// TestMergePRRecord_RefreshesStackedOpenPRHead verifies that merging a child PR
// into an intermediate branch refreshes the open parent PR whose head_ref is
// that intermediate branch (stacked / wave PR metadata lag).
func TestMergePRRecord_RefreshesStackedOpenPRHead(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	parent, authCtx, user := setupPRWithRealBranches(t, svc, "stack-user", "stack-repo")
	fullName := parent.Repository.FullName

	wave := parent.HeadRef
	parentSHA := parent.HeadSHA
	if parentSHA == "" {
		t.Fatal("parent PR missing head sha")
	}

	childBranch := "agent/child-stack"
	if err := svc.Git.CreateBranch(authCtx, fullName, childBranch, wave); err != nil {
		t.Fatalf("create child branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(authCtx, fullName, childBranch, "child.txt", "child commit", []byte("child\n")); err != nil {
		t.Fatalf("write child file: %v", err)
	}
	child, err := svc.CreatePR(authCtx, service.CreatePRInput{
		RepoFullName: fullName,
		Title:        "Child stacked PR",
		Body:         "stacks on wave",
		HeadRef:      childBranch,
		BaseRef:      wave,
		AuthorLogin:  user.Login,
	})
	if err != nil {
		t.Fatalf("create child PR: %v", err)
	}

	if err := svc.MergePRRecord(authCtx, &child, "merge", "Merge child into wave"); err != nil {
		t.Fatalf("merge child: %v", err)
	}

	waveTip, err := svc.Git.HeadSHA(authCtx, fullName, wave)
	if err != nil {
		t.Fatalf("wave tip: %v", err)
	}
	if waveTip == parentSHA {
		t.Fatal("expected wave tip to advance after child merge")
	}

	var refreshed db.PullRequest
	if err := svc.DB.First(&refreshed, parent.ID).Error; err != nil {
		t.Fatalf("reload parent PR: %v", err)
	}
	if refreshed.HeadSHA != waveTip {
		t.Fatalf("stacked parent PR head_sha=%s want wave tip %s (stale metadata lag)", refreshed.HeadSHA, waveTip)
	}
}

func TestSyncOpenPRHeadsForBranch_UpdatesMatchingOpenPR(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "sync-user", "sync-repo")
	fullName := pr.Repository.FullName
	oldSHA := pr.HeadSHA

	if _, err := svc.Git.WriteFile(authCtx, fullName, pr.HeadRef, "extra.txt", "advance tip", []byte("extra\n")); err != nil {
		t.Fatalf("advance tip: %v", err)
	}
	tip, err := svc.Git.HeadSHA(authCtx, fullName, pr.HeadRef)
	if err != nil {
		t.Fatalf("tip: %v", err)
	}
	if tip == oldSHA {
		t.Fatal("expected tip to advance")
	}

	var stale db.PullRequest
	if err := svc.DB.First(&stale, pr.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stale.HeadSHA != oldSHA {
		t.Fatalf("precondition: expected stale head %s, got %s", oldSHA, stale.HeadSHA)
	}

	if err := svc.SyncOpenPRHeadsForBranch(authCtx, pr.RepositoryID, fullName, pr.HeadRef); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var refreshed db.PullRequest
	if err := svc.DB.First(&refreshed, pr.ID).Error; err != nil {
		t.Fatalf("reload after sync: %v", err)
	}
	if refreshed.HeadSHA != tip {
		t.Fatalf("head_sha=%s want %s", refreshed.HeadSHA, tip)
	}
}

func TestSyncOpenPRHeadsForBranch_MatchingAndNonMatching(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	matching, authCtx, user := setupPRWithRealBranches(t, svc, "match-user", "match-repo")
	fullName := matching.Repository.FullName
	otherBranch := "agent/other"
	if err := svc.Git.CreateBranch(authCtx, fullName, otherBranch, "main"); err != nil {
		t.Fatalf("create other branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(authCtx, fullName, otherBranch, "other.txt", "other", []byte("other\n")); err != nil {
		t.Fatalf("write other: %v", err)
	}
	nonMatching, err := svc.CreatePR(authCtx, service.CreatePRInput{
		RepoFullName: fullName,
		Title:        "Other PR",
		HeadRef:      otherBranch,
		BaseRef:      "main",
		AuthorLogin:  user.Login,
	})
	if err != nil {
		t.Fatalf("create non-matching PR: %v", err)
	}
	otherSHA := nonMatching.HeadSHA

	if _, err := svc.Git.WriteFile(authCtx, fullName, matching.HeadRef, "extra.txt", "advance matching", []byte("extra\n")); err != nil {
		t.Fatalf("advance matching: %v", err)
	}
	tip, err := svc.Git.HeadSHA(authCtx, fullName, matching.HeadRef)
	if err != nil {
		t.Fatalf("tip: %v", err)
	}
	if err := svc.SyncOpenPRHeadsForBranch(authCtx, matching.RepositoryID, fullName, matching.HeadRef); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var gotMatching, gotOther db.PullRequest
	if err := svc.DB.First(&gotMatching, matching.ID).Error; err != nil {
		t.Fatalf("reload matching: %v", err)
	}
	if err := svc.DB.First(&gotOther, nonMatching.ID).Error; err != nil {
		t.Fatalf("reload other: %v", err)
	}
	if gotMatching.HeadSHA != tip {
		t.Fatalf("matching head_sha=%s want %s", gotMatching.HeadSHA, tip)
	}
	if gotOther.HeadSHA != otherSHA {
		t.Fatalf("non-matching PR was rewritten: got %s want %s", gotOther.HeadSHA, otherSHA)
	}
}

func TestSyncPRHeadAfterPush_SkipsClosedPR(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "closed-sync", "closed-repo")
	oldSHA := pr.HeadSHA
	if _, err := svc.Git.WriteFile(authCtx, pr.Repository.FullName, pr.HeadRef, "extra.txt", "advance", []byte("extra\n")); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{"state": db.StateClosed, "merged": false}).Error; err != nil {
		t.Fatalf("close PR: %v", err)
	}
	updated, err := svc.SyncPRHeadAfterPush(authCtx, pr.ID, pr.Repository.FullName)
	if err != nil {
		t.Fatalf("sync closed PR: %v", err)
	}
	if updated.HeadSHA != oldSHA {
		t.Fatalf("closed PR head rewritten: got %s want %s", updated.HeadSHA, oldSHA)
	}
}

func TestSyncPRHeadAfterPush_HeadSHAFailure(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "headsha-fail", "headsha-repo")
	oldSHA := pr.HeadSHA
	_, err := svc.SyncPRHeadAfterPush(authCtx, pr.ID, "missing/repo")
	if err == nil {
		t.Fatal("expected HeadSHA failure")
	}
	var got db.PullRequest
	if loadErr := svc.DB.First(&got, pr.ID).Error; loadErr != nil {
		t.Fatalf("reload: %v", loadErr)
	}
	if got.HeadSHA != oldSHA {
		t.Fatalf("failed sync rewrote head_sha to %s", got.HeadSHA)
	}
}

func TestSyncPRHeadAfterPush_EnqueueFailureIsVisible(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	t.Cleanup(func() { service.SetTestEnqueueForgejoPullRequestProjection(nil) })
	service.SetTestEnqueueForgejoPullRequestProjection(func(*service.Service, context.Context, db.PullRequest) (db.PullRequestProjectionJob, error) {
		return db.PullRequestProjectionJob{}, errors.New("enqueue boom")
	})

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "enqueue-fail", "enqueue-repo")
	if _, err := svc.Git.WriteFile(authCtx, pr.Repository.FullName, pr.HeadRef, "extra.txt", "advance", []byte("extra\n")); err != nil {
		t.Fatalf("advance: %v", err)
	}
	_, err := svc.SyncPRHeadAfterPush(authCtx, pr.ID, pr.Repository.FullName)
	if err == nil {
		t.Fatal("expected enqueue failure")
	}
}

func TestSyncPRHeadAfterPush_CASDoesNotApplyStaleTip(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "cas-user", "cas-repo")
	fullName := pr.Repository.FullName
	if _, err := svc.Git.WriteFile(authCtx, fullName, pr.HeadRef, "tip1.txt", "tip1", []byte("tip1\n")); err != nil {
		t.Fatalf("write tip1: %v", err)
	}
	tip1, err := svc.Git.HeadSHA(authCtx, fullName, pr.HeadRef)
	if err != nil {
		t.Fatalf("tip1: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		service.SetTestSyncPRHeadAfterTipRead(nil)
		releaseOnce()
	})
	service.SetTestSyncPRHeadAfterTipRead(func() {
		select {
		case <-started:
		default:
			close(started)
			<-release
		}
	})

	errCh := make(chan error, 1)
	go func() {
		_, syncErr := svc.SyncPRHeadAfterPush(authCtx, pr.ID, fullName)
		errCh <- syncErr
	}()
	<-started
	if _, err := svc.Git.WriteFile(authCtx, fullName, pr.HeadRef, "tip2.txt", "tip2", []byte("tip2\n")); err != nil {
		t.Fatalf("write tip2: %v", err)
	}
	tip2, err := svc.Git.HeadSHA(authCtx, fullName, pr.HeadRef)
	if err != nil {
		t.Fatalf("tip2: %v", err)
	}
	if tip2 == tip1 {
		t.Fatal("expected tip2 to differ from tip1")
	}
	if _, err := svc.SyncPRHeadAfterPush(authCtx, pr.ID, fullName); err != nil {
		t.Fatalf("newer sync: %v", err)
	}
	releaseOnce()
	if err := <-errCh; err != nil {
		t.Fatalf("stale sync: %v", err)
	}

	var got db.PullRequest
	if err := svc.DB.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.HeadSHA != tip2 {
		t.Fatalf("lost-update: head_sha=%s want latest tip %s (not stale %s)", got.HeadSHA, tip2, tip1)
	}
}

func TestForgejoMergeWebhook_RefreshesStackedOpenPRHead(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	svc.DisableForgejoProjectionWorker = true

	if err := svc.DB.Create(&db.User{Login: "wave-user", Name: "wave-user", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "wave-user", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	fullName := "wave-user/repo"
	wave := "agent/wave"
	if err := svc.Git.CreateBranch(ctx, fullName, wave, "main"); err != nil {
		t.Fatalf("create wave: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, fullName, wave, "wave.txt", "wave", []byte("wave\n")); err != nil {
		t.Fatalf("write wave: %v", err)
	}
	parent, err := svc.CreatePR(ctx, service.CreatePRInput{
		RepoFullName: fullName, Title: "parent", HeadRef: wave, BaseRef: "main", AuthorLogin: "wave-user",
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	parentSHA := parent.HeadSHA
	childBranch := "agent/child"
	if err := svc.Git.CreateBranch(ctx, fullName, childBranch, wave); err != nil {
		t.Fatalf("create child branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, fullName, childBranch, "child.txt", "child", []byte("child\n")); err != nil {
		t.Fatalf("write child: %v", err)
	}
	child, err := svc.CreatePR(ctx, service.CreatePRInput{
		RepoFullName: fullName, Title: "child", HeadRef: childBranch, BaseRef: wave, AuthorLogin: "wave-user",
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID: child.ID, RepositoryID: child.RepositoryID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "forgejo/repo", ExternalNumber: 42, SourceBranch: childBranch, TargetBranch: wave, State: service.ProjectionStateOpen,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}

	childSHA, err := svc.Git.HeadSHA(ctx, fullName, childBranch)
	if err != nil {
		t.Fatalf("child sha: %v", err)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, fullName)
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	forgejoRoot := t.TempDir()
	forgejoBare := filepath.Join(forgejoRoot, "forgejo", "repo.git")
	if out, err := exec.Command("git", "init", "--bare", forgejoBare).CombinedOutput(); err != nil {
		t.Fatalf("init forgejo bare: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, childSHA+":refs/heads/"+wave).CombinedOutput(); err != nil {
		t.Fatalf("seed forgejo wave: %v %s", err, out)
	}

	agsWave, err := svc.Git.HeadSHA(ctx, fullName, wave)
	if err != nil {
		t.Fatalf("ags wave: %v", err)
	}
	if agsWave != parentSHA {
		t.Fatalf("precondition: AGS wave=%s want parent %s", agsWave, parentSHA)
	}

	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret",
		RepoMap: map[string]forgejointegration.RepoMapping{fullName: {Owner: "forgejo", Repo: "repo", BaseBranch: "main"}},
	}, &projectionCloseForgejoClient{}, nil)

	body := []byte(`{"action":"closed","repository":{"full_name":"forgejo/repo","default_branch":"main"},"pull_request":{"number":42,"merged":true,"html_url":"http://forgejo.local/forgejo/repo/pulls/42","head":{"ref":"agent/child"},"base":{"ref":"agent/wave"}}}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body)
	if err != nil {
		t.Fatalf("HandleForgejoWebhook: %v", err)
	}
	if !result.Handled {
		t.Fatalf("unhandled webhook: %#v", result)
	}

	waveTip, err := svc.Git.HeadSHA(ctx, fullName, wave)
	if err != nil {
		t.Fatalf("wave tip: %v", err)
	}
	if waveTip != childSHA {
		t.Fatalf("AGS wave=%s want child %s", waveTip, childSHA)
	}
	var refreshed db.PullRequest
	if err := svc.DB.First(&refreshed, parent.ID).Error; err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if refreshed.HeadSHA != waveTip {
		t.Fatalf("stacked parent head_sha=%s want wave tip %s", refreshed.HeadSHA, waveTip)
	}
}
