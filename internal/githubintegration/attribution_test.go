package githubintegration

import (
	"context"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/attribution"
)

func TestGitHubShadowUsesSharedMappingAndDoesNotSudo(t *testing.T) {
	client := &fakeAPIClient{}
	cfg := Config{Enabled: true, Token: "fixture", MergeAuthority: "forgejo", Repos: map[string]RepoMapping{"ags/demo": {Owner: "team", Repo: "demo"}}, Attribution: attribution.Config{Principals: map[string]attribution.Principal{"alice": {Forgejo: "alice.remote", GitHub: "alice-gh"}}}}
	integration := NewWithClient(cfg, func(context.Context, string, string, string, string) error { return nil }, client)
	ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: 7, Login: "alice"})
	_, _, err := integration.EnsureShadowPullRequest(ctx, ShadowPullRequestRequest{RepoFullName: "ags/demo", RepoPath: t.TempDir(), HeadBranch: "agent/topic", HeadSHA: strings.Repeat("a", 40), BaseBranch: "main", Title: "topic", Body: "body", AGSNumber: 3})
	if err != nil {
		t.Fatal(err)
	}
	// Existing fake captures the complete request; text belongs to AGS, never a
	// GitHub Sudo header or a different user's credential.
	if !strings.HasPrefix(client.ensureInput.Body, "AGS-Actor: alice") || !strings.Contains(client.ensureInput.Body, "github=alice-gh") || !strings.Contains(client.ensureInput.Body, "AGS PR: #3") {
		t.Fatal(client.ensureInput.Body)
	}
	_, _, err = integration.CloseShadowPullRequest(ctx, CloseShadowPullRequestRequest{RepoFullName: "ags/demo", HeadBranch: "agent/topic", BaseBranch: "main", PRNumber: 3})
	if err != nil || !strings.HasPrefix(client.closeNote, "AGS-Actor: alice") || !strings.Contains(client.closeNote, "github=alice-gh") {
		t.Fatal("close attribution missing", err)
	}
}
