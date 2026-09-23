package rest_test

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestPutRepoContentsRefreshesStackedOpenPRHead(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	repo, err := h.Svc.CreateRepo(ctx, service.CreateRepoInput{
		OwnerLogin:    h.User.Login,
		Name:          "contents-stack",
		DefaultBranch: "main",
		AddReadme:     true,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	fullName := repo.FullName
	wave := "feature"
	if err := h.Svc.Git.CreateBranch(ctx, fullName, wave, "main"); err != nil {
		t.Fatalf("create wave: %v", err)
	}
	if _, err := h.Svc.Git.WriteFile(ctx, fullName, wave, "hello.txt", "add test file", []byte("hello world\n")); err != nil {
		t.Fatalf("write wave: %v", err)
	}
	authCtx := service.ContextWithUser(ctx, h.User)
	parent, err := h.Svc.CreatePR(authCtx, service.CreatePRInput{
		RepoFullName: fullName,
		Title:        "parent",
		HeadRef:      wave,
		BaseRef:      "main",
		AuthorLogin:  h.User.Login,
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	oldSHA := parent.HeadSHA

	w := h.DoRESTJSON(t, "PUT", "/api/v3/repos/"+fullName+"/contents/stack.txt", map[string]any{
		"message": "advance wave via contents",
		"content": base64.StdEncoding.EncodeToString([]byte("stacked\n")),
		"branch":  wave,
	})
	assertStatusCode(t, w, 201)

	tip, err := h.Svc.Git.HeadSHA(ctx, fullName, wave)
	if err != nil {
		t.Fatalf("tip: %v", err)
	}
	if tip == oldSHA {
		t.Fatal("expected wave tip to advance")
	}
	var refreshed db.PullRequest
	if err := h.DB.First(&refreshed, parent.ID).Error; err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if refreshed.HeadSHA != tip {
		t.Fatalf("contents writer did not refresh open PR head: got %s want %s", refreshed.HeadSHA, tip)
	}
}
