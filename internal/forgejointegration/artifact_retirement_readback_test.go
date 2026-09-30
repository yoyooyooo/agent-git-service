package forgejointegration

import (
	"context"
	"testing"
)

func TestRetirementDoesNotTrustSuccessfulPushWithoutExactReadback(t *testing.T) {
	oldSHA := "1111111111111111111111111111111111111111"
	newSHA := "2222222222222222222222222222222222222222"
	reads, pushes := 0, 0
	integration := NewWithGitCapabilities(Config{Enabled: true, BaseURL: "https://forgejo.example", RepoMap: map[string]RepoMapping{"owner/repo": {Owner: "mirror", Repo: "repo"}}}, nil,
		func(context.Context, PushRequest) error { pushes++; return nil },
		func(context.Context, string, string, string) (string, error) { reads++; return oldSHA, nil })
	handled, err := integration.RewriteBranchWithLease(context.Background(), "owner/repo", "/fixture/repo.git", "main", oldSHA, newSHA)
	if !handled || err == nil || pushes != 1 || reads != 2 {
		t.Fatalf("unverified provider write accepted: handled=%v reads=%d pushes=%d err=%v", handled, reads, pushes, err)
	}
}
