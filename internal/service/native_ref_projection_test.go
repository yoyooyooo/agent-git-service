package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
)

func TestNativeRefProjectionDeletionRequiresSuccessfulAbsenceRead(t *testing.T) {
	for _, kind := range []string{"absent", "recreated", "read-failed"} {
		t.Run(kind, func(t *testing.T) {
			svc := setupProjectionStateService(t)
			ctx := context.Background()
			store, err := gitstore.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const repo, branch = "example-owner/demo", "agent/deleted"
			if err := store.Init(ctx, repo, "main", true); err != nil {
				t.Fatal(err)
			}
			svc.Git = store
			pushes := 0
			svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
				Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token", MirrorBranchIncludes: []string{"agent/*"},
				RepoMap: map[string]forgejointegration.RepoMapping{repo: {Owner: "forgejo", Repo: "demo", BaseBranch: "main"}},
			}, &fakePullRequestListClient{}, func(_ context.Context, req forgejointegration.PushRequest) error {
				pushes++
				if req.Refspec != ":refs/heads/"+branch {
					t.Fatalf("unexpected refspec %q", req.Refspec)
				}
				return nil
			})
			if kind == "recreated" {
				if err := store.CreateBranch(ctx, repo, branch, "main"); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "read-failed" {
				path, err := store.GetRepoPath(ctx, repo)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(path, "HEAD"), filepath.Join(path, "HEAD.saved")); err != nil {
					t.Fatal(err)
				}
			}
			err = svc.ProjectNativeRefChange(ctx, repo, ForgejoRefChange{Ref: "refs/heads/" + branch, Deleted: true})
			if kind == "absent" {
				if err != nil || pushes != 1 {
					t.Fatalf("verified deletion: pushes=%d err=%v", pushes, err)
				}
			} else if err == nil || pushes != 0 {
				t.Fatalf("unsafe deletion: pushes=%d err=%v", pushes, err)
			}
		})
	}
}
