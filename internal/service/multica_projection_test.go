package service

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
)

func TestMulticaIssueRefForPRUsesRepoWorkspaceMapping(t *testing.T) {
	svc := &Service{MulticaProjection: multicaprojection.New(multicaprojection.Config{
		Enabled: true,
		Repos: map[string]multicaprojection.WorkspaceConfig{
			"example-owner/personal": {Workspace: "workspace-beta", WorkspaceID: "ws-workspace-beta"},
		},
		Workspace:   "workspace-alpha",
		WorkspaceID: "ws-humanity",
	})}

	got := svc.multicaIssueRefForPR(context.Background(), "example-owner/personal", db.PullRequest{Title: "Smoke", Body: "Multica: HAK-12"})
	want := multicaprojection.IssueRef{Workspace: "workspace-beta", WorkspaceID: "ws-workspace-beta", IssueKey: "HAK-12", URL: "https://multica.ai/workspace-beta/issues/HAK-12"}
	if got != want {
		t.Fatalf("multicaIssueRefForPR() = %#v, want %#v", got, want)
	}
}

func TestMulticaIssueRefForPRPrefersExplicitWorkspaceMarker(t *testing.T) {
	svc := &Service{MulticaProjection: multicaprojection.New(multicaprojection.Config{
		Enabled: true,
		Repos: map[string]multicaprojection.WorkspaceConfig{
			"example-owner/personal": {Workspace: "workspace-beta", WorkspaceID: "ws-workspace-beta"},
		},
		Workspaces: map[string]multicaprojection.WorkspaceConfig{
			"workspace-alpha": {Workspace: "workspace-alpha", WorkspaceID: "ws-humanity"},
		},
	})}

	got := svc.multicaIssueRefForPR(context.Background(), "example-owner/personal", db.PullRequest{Body: "Multica: workspace-alpha/HUM-66"})
	want := multicaprojection.IssueRef{Workspace: "workspace-alpha", WorkspaceID: "ws-humanity", IssueKey: "HUM-66", URL: "https://multica.ai/workspace-alpha/issues/HUM-66"}
	if got != want {
		t.Fatalf("multicaIssueRefForPR() = %#v, want %#v", got, want)
	}
}
