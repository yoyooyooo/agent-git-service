package notifications

import (
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/service"
)

func TestProjectionDriftFeishuTitleSplitsGitLab(t *testing.T) {
	if got := projectionDriftFeishuTitle(service.ProjectionProviderForgejo, false); got != "AGS Forgejo projection drift" {
		t.Fatalf("forgejo title=%q", got)
	}
	if got := projectionDriftFeishuTitle(service.ProjectionProviderGitLab, false); got != "AGS GitLab projection drift" {
		t.Fatalf("gitlab title=%q", got)
	}
	if got := projectionDriftFeishuTitle(service.ProjectionProviderGitLab, true); got != "AGS GitLab projection drift resolved" {
		t.Fatalf("gitlab resolved title=%q", got)
	}
	if got := projectionDriftFeishuTitle(service.ProjectionProviderGitHub, false); got != "AGS GitHub projection drift" {
		t.Fatalf("github title=%q", got)
	}
	if got := projectionDriftFeishuTitle(service.ProjectionProviderGitHub, true); got != "AGS GitHub projection drift resolved" {
		t.Fatalf("github resolved title=%q", got)
	}
	text := renderProjectionDriftText(service.ProjectionDriftNotification{
		Provider:     service.ProjectionProviderGitLab,
		RepoFullName: "example-team/shipping-fixture",
		Ref:          "refs/heads/sync/upstream-resolve/4bbcd043-conflict",
		Type:         "non_fast_forward_blocked",
		ErrorSummary: "non_fast_forward",
	})
	if !strings.HasPrefix(text, "AGS GitLab projection drift\n") {
		t.Fatalf("gitlab feishu text title: %q", text)
	}
	if strings.Contains(text, "Forgejo projection drift") {
		t.Fatalf("gitlab feishu text reused Forgejo title: %q", text)
	}
}
