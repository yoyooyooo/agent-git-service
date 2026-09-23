package service

import (
	"strings"
	"testing"
)

func TestRenderProjectionDriftOutboundTextSplitsGitLabTitle(t *testing.T) {
	forgejo := renderProjectionDriftOutboundText(ProjectionDriftNotification{
		Provider:     ProjectionProviderForgejo,
		RepoFullName: "example-team/shipping-fixture",
		Ref:          "refs/heads/main",
		Type:         "non_fast_forward_blocked",
	}, false)
	if !strings.HasPrefix(forgejo, "AGS Forgejo projection drift detected\n") {
		t.Fatalf("forgejo title: %q", forgejo)
	}

	gitlab := renderProjectionDriftOutboundText(ProjectionDriftNotification{
		Provider:     ProjectionProviderGitLab,
		RepoFullName: "example-team/shipping-fixture",
		Ref:          "refs/heads/sync/upstream-resolve/4bbcd043-conflict",
		Type:         "non_fast_forward_blocked",
		ErrorSummary: "non_fast_forward",
	}, false)
	if !strings.HasPrefix(gitlab, "AGS GitLab projection drift detected\n") {
		t.Fatalf("gitlab title: %q", gitlab)
	}
	if strings.Contains(gitlab, "Forgejo projection drift") {
		t.Fatalf("gitlab alert reused Forgejo title: %q", gitlab)
	}

	resolved := renderProjectionDriftOutboundText(ProjectionDriftNotification{
		Provider:     ProjectionProviderGitLab,
		RepoFullName: "example-team/shipping-fixture",
		Ref:          "refs/heads/sync/upstream-resolve/4bbcd043-conflict",
	}, true)
	if !strings.HasPrefix(resolved, "AGS GitLab projection drift resolved\n") {
		t.Fatalf("gitlab resolved title: %q", resolved)
	}

	github := renderProjectionDriftOutboundText(ProjectionDriftNotification{
		Provider:     ProjectionProviderGitHub,
		RepoFullName: "operator/project-kit",
		Ref:          "refs/heads/main",
		Type:         "non_fast_forward_blocked",
	}, false)
	if !strings.HasPrefix(github, "AGS GitHub projection drift detected\n") {
		t.Fatalf("github title: %q", github)
	}
}
