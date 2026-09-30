package forgejointegration

import (
	"context"
	"errors"
	"testing"
)

func TestRewriteBranchWithLeaseUsesExactOldHeadAndIsIdempotent(t *testing.T) {
	enabled := true
	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "2222222222222222222222222222222222222222"
	current := oldSHA
	pushes := 0
	integration := NewWithGitCapabilities(Config{
		Enabled: true,
		BaseURL: "https://forgejo.example",
		RepoMap: map[string]RepoMapping{
			"owner/repo": {Owner: "mirror", Repo: "repo", Enabled: &enabled},
		},
	}, nil, func(ctx context.Context, req PushRequest) error {
		pushes++
		if req.Refspec != "refs/heads/main:refs/heads/main" || req.ForceWithLeaseRef != "refs/heads/main" || req.ForceWithLeaseSHA != oldSHA {
			t.Fatalf("unsafe lease push: %+v", req)
		}
		current = newSHA
		return nil
	}, func(context.Context, string, string, string) (string, error) {
		return current, nil
	})
	handled, err := integration.RewriteBranchWithLease(context.Background(), "owner/repo", "/tmp/repo.git", "main", oldSHA, newSHA)
	if err != nil || !handled || pushes != 1 || current != newSHA {
		t.Fatalf("rewrite: handled=%v pushes=%d current=%s err=%v", handled, pushes, current, err)
	}
	handled, err = integration.RewriteBranchWithLease(context.Background(), "owner/repo", "/tmp/repo.git", "main", oldSHA, newSHA)
	if err != nil || !handled || pushes != 1 {
		t.Fatalf("idempotent rewrite: handled=%v pushes=%d err=%v", handled, pushes, err)
	}
	current = "3333333333333333333333333333333333333333"
	if _, err = integration.RewriteBranchWithLease(context.Background(), "owner/repo", "/tmp/repo.git", "main", oldSHA, newSHA); err == nil {
		t.Fatal("drifted provider head was overwritten")
	}
}

func TestRewriteBranchWithLeasePropagatesRemoteReadFailure(t *testing.T) {
	enabled := true
	integration := NewWithGitCapabilities(Config{
		Enabled: true,
		BaseURL: "https://forgejo.example",
		RepoMap: map[string]RepoMapping{"owner/repo": {Owner: "mirror", Repo: "repo", Enabled: &enabled}},
	}, nil, func(context.Context, PushRequest) error {
		t.Fatal("push called after failed read")
		return nil
	}, func(context.Context, string, string, string) (string, error) {
		return "", errors.New("provider unavailable")
	})
	handled, err := integration.RewriteBranchWithLease(context.Background(), "owner/repo", "/tmp/repo.git", "main",
		"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222")
	if err == nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
}
