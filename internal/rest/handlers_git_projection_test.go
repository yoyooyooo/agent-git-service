package rest_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestNativeGitAPIProjectsBranchLifecycle(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	compatSeedRepo(t, h, "native-ref-projection")
	full, branch := "testuser/native-ref-projection", "agent/native"
	head, err := h.Svc.Git.HeadSHA(ctx, full, "main")
	if err != nil {
		t.Fatal(err)
	}
	var pushes []forgejointegration.PushRequest
	h.Svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token",
		MirrorBranchIncludes: []string{"agent/*"},
		RepoMap:              map[string]forgejointegration.RepoMapping{full: {Owner: "forgejo", Repo: "native-ref-projection", BaseBranch: "main"}},
	}, &restPRProjectionClient{headSHA: head}, func(_ context.Context, req forgejointegration.PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})
	w := h.DoRESTJSON(t, "POST", "/api/v3/repos/"+full+"/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": head})
	assertStatusCode(t, w, 201)
	if len(pushes) != 1 {
		t.Fatalf("native ref creation projected %d times, want 1", len(pushes))
	}
	w = h.DoRESTJSON(t, "PUT", "/api/v3/repos/"+full+"/contents/note.txt", map[string]any{
		"branch": branch, "message": "advance native branch", "content": base64.StdEncoding.EncodeToString([]byte("native write\n")),
	})
	assertStatusCode(t, w, 201)
	if len(pushes) != 2 {
		t.Fatalf("native content update projected %d times, want 2", len(pushes))
	}
	tip, err := h.Svc.Git.HeadSHA(ctx, full, branch)
	if err != nil || tip == head {
		t.Fatalf("native branch did not advance: tip=%s err=%v", tip, err)
	}
	w = h.DoRESTJSON(t, "PATCH", "/api/v3/repos/"+full+"/git/refs/heads/"+branch, map[string]any{"sha": tip})
	assertStatusCode(t, w, 200)
	if len(pushes) != 3 {
		t.Fatalf("native ref update projected %d times, want 3", len(pushes))
	}
	w = h.DoRESTNoAuth(t, "DELETE", "/api/v3/repos/"+full+"/git/refs/heads/"+branch)
	if w.Code != 401 && w.Code != 403 {
		t.Fatalf("unauthorized native delete status=%d", w.Code)
	}
	if len(pushes) != 3 {
		t.Fatal("unauthorized deletion reached the provider")
	}
	w = h.DoRESTJSON(t, "DELETE", "/api/v3/repos/"+full+"/git/refs/heads/"+branch, nil)
	assertStatusCode(t, w, 204)
	if len(pushes) != 4 || pushes[3].Refspec != ":refs/heads/"+branch {
		t.Fatalf("native delete did not project its exact ref: %#v", pushes)
	}
	if _, err := h.Svc.Git.LookupRef(ctx, full, "refs/heads/"+branch); err != gitstore.ErrRefNotFound {
		t.Fatalf("native branch still exists: %v", err)
	}
	w = h.DoRESTJSON(t, "POST", "/api/v3/repos/"+full+"/git/refs", map[string]any{"ref": "refs/locks/lease", "sha": head})
	assertStatusCode(t, w, 201)
	w = h.DoRESTJSON(t, "DELETE", "/api/v3/repos/"+full+"/git/refs/locks/lease", nil)
	assertStatusCode(t, w, 204)
	if len(pushes) != 4 {
		t.Fatal("custom ref namespace was unexpectedly mirrored as a branch")
	}
}

func TestNativeGitAPIProjectionFailureKeepsNativeSuccessAndRecordsDrift(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	compatSeedRepo(t, h, "native-ref-failure")
	full, branch := "testuser/native-ref-failure", "agent/native"
	head, err := h.Svc.Git.HeadSHA(ctx, full, "main")
	if err != nil {
		t.Fatal(err)
	}
	h.Svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token", MirrorBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{full: {Owner: "forgejo", Repo: "native-ref-failure", BaseBranch: "main"}},
	}, &restPRProjectionClient{headSHA: head}, func(context.Context, forgejointegration.PushRequest) error {
		return fmt.Errorf("test provider unavailable")
	})
	w := h.DoRESTJSON(t, "POST", "/api/v3/repos/"+full+"/git/refs", map[string]any{"ref": "refs/heads/" + branch, "sha": head})
	assertStatusCode(t, w, 201)
	if tip, err := h.Svc.Git.HeadSHA(ctx, full, branch); err != nil || tip != head {
		t.Fatalf("provider failure rolled back native success: tip=%s err=%v", tip, err)
	}
	var state db.ProjectionRefState
	if err := h.DB.Where("ref = ? AND provider = ?", "refs/heads/"+branch, service.ProjectionProviderForgejo).First(&state).Error; err != nil {
		t.Fatalf("provider failure was not durably recorded: %v", err)
	}
	if state.Status != service.ProjectionStatusActive {
		t.Fatalf("provider failure status=%s", state.Status)
	}
}
