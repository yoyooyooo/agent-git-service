package githubintegration

import (
	"context"
	"errors"
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
	// Closing now reconciles the exact PR without retry-unsafe comments. Its
	// attributed body remains intact, and authority is checked before the write.
	body := client.ensureInput.Body
	checks := 0
	req := CloseShadowPullRequestRequest{RepoFullName: "ags/demo", HeadBranch: "agent/topic", BaseBranch: "main", PRNumber: 3, ExternalNumber: 8, BeforeWrite: func(context.Context) error {
		checks++
		return nil
	}}
	res, handled, err := integration.CloseShadowPullRequest(ctx, req)
	if err != nil || !handled || res.Number != 8 || client.closeNumber != 8 || checks != 1 || client.closeNote != "" || client.ensureInput.Body != body {
		t.Fatalf("exact close lost attribution or authority: result=%+v handled=%v checks=%d client=%+v err=%v", res, handled, checks, client, err)
	}
	client.closeNumber = 0
	req.BeforeWrite = func(context.Context) error { return context.Canceled }
	if _, _, err := integration.CloseShadowPullRequest(ctx, req); !errors.Is(err, context.Canceled) || client.closeNumber != 0 || client.closeNote != "" {
		t.Fatalf("denied close mutated provider: client=%+v err=%v", client, err)
	}
}

func TestGitHubEmptyMappingPreservesText(t *testing.T) {
	for _, principals := range []map[string]attribution.Principal{nil, {}} {
		integration := NewWithClient(Config{Attribution: attribution.Config{Principals: principals}}, nil, &fakeAPIClient{})
		ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: 7, Login: "alice"})
		if got := integration.attributedText(ctx, "original body"); got != "original body" {
			t.Fatal(got)
		}
	}
}
