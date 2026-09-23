package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"github.com/ngaut/agent-git-service/internal/service"
)

type capturePRMergeNotifier struct {
	notes []service.PullRequestMergedNotification
	err   error
}

func (n *capturePRMergeNotifier) NotifyPullRequestMerged(ctx context.Context, note service.PullRequestMergedNotification) error {
	n.notes = append(n.notes, note)
	return n.err
}

func TestMergePRSendsPullRequestMergedNotification(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	notifier := &capturePRMergeNotifier{}
	svc.PullRequestMergeNotifier = notifier

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "merge-note", "repo")
	merged, err := svc.MergePR(authCtx, pr.Repository.FullName, pr.Number, "merge", "")
	if err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if len(notifier.notes) != 1 {
		t.Fatalf("expected one notification, got %#v", notifier.notes)
	}
	note := notifier.notes[0]
	if note.RepoFullName != pr.Repository.FullName || note.Number != pr.Number || note.Title != pr.Title || note.BaseRef != "main" || note.HeadRef != "feature" {
		t.Fatalf("notification mismatch: %#v", note)
	}
	if note.MergeCommitSHA == "" || note.MergeCommitSHA != merged.MergeCommitSHA || note.MergedByLogin != "merge-note" || note.Source != "ags" || note.URL == "" {
		t.Fatalf("merge facts mismatch: %#v", note)
	}
}

func TestMergePRNotificationErrorDoesNotFailMerge(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	svc.PullRequestMergeNotifier = &capturePRMergeNotifier{err: errors.New("feishu down")}

	pr, authCtx, _ := setupPRWithRealBranches(t, svc, "merge-note-fail", "repo")
	if _, err := svc.MergePR(authCtx, pr.Repository.FullName, pr.Number, "merge", ""); err != nil {
		t.Fatalf("MergePR should ignore notification error, got %v", err)
	}
}

func TestProjectionMergeSendsPullRequestMergedNotificationOnce(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	notifier := &capturePRMergeNotifier{}
	svc.PullRequestMergeNotifier = notifier
	svc.MulticaProjection = multicaprojection.New(multicaprojection.Config{
		Enabled:          true,
		AppURL:           "https://multica.ai",
		DefaultWorkspace: "workspace-alpha",
	})
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "proj-note", Name: "proj-note", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "proj-note", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "proj-note/repo", "agent/projection", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "proj-note/repo", Title: "Projection", Body: "Multica: workspace-alpha/HUM-60", HeadRef: "agent/projection", BaseRef: "main", AuthorLogin: "proj-note"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	for _, projection := range []db.PullRequestProjection{
		{
			PullRequestID:  pr.ID,
			RepositoryID:   pr.RepositoryID,
			Provider:       service.ProjectionProviderForgejo,
			ExternalRepo:   "forgejo/repo",
			ExternalNumber: 42,
			ExternalURL:    "http://forgejo.local/forgejo/repo/pulls/42",
			SourceBranch:   "agent/projection",
			TargetBranch:   "main",
			State:          service.ProjectionStateOpen,
		},
		{
			PullRequestID:  pr.ID,
			RepositoryID:   pr.RepositoryID,
			Provider:       service.ProjectionProviderGitLab,
			ExternalRepo:   "gitlab/repo",
			ExternalNumber: 7,
			ExternalURL:    "http://gitlab.local/gitlab/repo/-/merge_requests/7",
			SourceBranch:   "agent/projection",
			TargetBranch:   "main",
			State:          service.ProjectionStateOpen,
		},
		{
			PullRequestID:  pr.ID,
			RepositoryID:   pr.RepositoryID,
			Provider:       service.ProjectionProviderGitHub,
			ExternalRepo:   "example-org/project-kit",
			ExternalNumber: 8,
			ExternalURL:    "https://github.com/example-org/project-kit/pull/8",
			SourceBranch:   "agent/projection",
			TargetBranch:   "main",
			State:          service.ProjectionStateOpen,
		},
	} {
		if err := svc.UpsertPullRequestProjection(ctx, projection); err != nil {
			t.Fatalf("upsert projection: %v", err)
		}
	}

	mergeSHA := strings.Repeat("c", 40)
	if _, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, mergeSHA, "forgejo"); err != nil {
		t.Fatalf("mark merged: %v", err)
	}
	if _, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, mergeSHA, "forgejo"); err != nil {
		t.Fatalf("mark merged again: %v", err)
	}
	if len(notifier.notes) != 1 {
		t.Fatalf("expected one notification, got %#v", notifier.notes)
	}
	note := notifier.notes[0]
	if note.Source != "projection:forgejo" || note.MergedByLogin != "forgejo" || note.MergeCommitSHA != mergeSHA || note.RepoFullName != "proj-note/repo" || note.ForgejoURL != "http://forgejo.local/forgejo/repo/pulls/42" || note.GitLabURL != "http://gitlab.local/gitlab/repo/-/merge_requests/7" || note.GitHubURL != "https://github.com/example-org/project-kit/pull/8" || note.MulticaIssueKey != "HUM-60" || note.MulticaIssueURL != "https://multica.ai/workspace-alpha/issues/HUM-60" {
		t.Fatalf("projection notification mismatch: %#v", note)
	}
}
