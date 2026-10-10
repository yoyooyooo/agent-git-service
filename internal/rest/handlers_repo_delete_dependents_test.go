package rest_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestDeleteRepoHandlerWithCrossRepositoryPRDependents(t *testing.T) {
	h := testharness.New(t)
	ctx := service.ContextWithUser(context.Background(), h.User)
	remove, err := h.Svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: h.User.Login, Name: "remove", DefaultBranch: "main", AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	keep, err := h.Svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: h.User.Login, Name: "keep", DefaultBranch: "main", AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, headID := range []uint{remove.ID, keep.ID} {
		pr := db.PullRequest{RepositoryID: keep.ID, HeadRepositoryID: headID, Number: i + 1, Title: "fixture", AuthorID: h.User.ID, HeadRef: "feature", BaseRef: "main"}
		if err := h.DB.Create(&pr).Error; err != nil {
			t.Fatal(err)
		}
		review := db.PullRequestReview{PullRequestID: pr.ID, AuthorLogin: h.User.Login, State: "APPROVED"}
		job := db.PullRequestProjectionJob{RepositoryID: keep.ID, PullRequestID: pr.ID, Provider: "forgejo", RepoFullName: keep.FullName, AGSPRNumber: pr.Number}
		for _, model := range []any{&review, &job} {
			if err := h.DB.Create(model).Error; err != nil {
				t.Fatal(err)
			}
		}
		for _, model := range []any{
			&db.PullRequestProjectionJobAttempt{JobID: job.ID, Attempt: 1, Phase: "queued", Status: "failed"},
			&db.PRReviewComment{PullRequestID: pr.ID, PullRequestReviewID: &review.ID, AuthorLogin: h.User.Login},
			&db.PullRequestProjection{RepositoryID: keep.ID, PullRequestID: pr.ID, Provider: "forgejo", ExternalRepo: "fixture/keep", ExternalNumber: pr.Number},
			&db.PullRequestMulticaLink{RepositoryID: keep.ID, PullRequestID: pr.ID, Workspace: "fixture", IssueID: "fixture", IssueKey: "fixture", Source: db.MulticaLinkSourceMarker},
		} {
			if err := h.DB.Create(model).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	w := h.DoREST(t, http.MethodDelete, "/api/v3/repos/"+remove.FullName, nil)
	assertStatusCode(t, w, http.StatusNoContent)
	if h.Svc.Git.Exists(ctx, remove.FullName) {
		t.Fatal("deleted repository Git storage remains")
	}
	if !h.Svc.Git.Exists(ctx, keep.FullName) {
		t.Fatal("other repository Git storage was removed")
	}
	for _, model := range []any{&db.PullRequest{}, &db.PullRequestReview{}, &db.PRReviewComment{}, &db.PullRequestProjection{}, &db.PullRequestProjectionJob{}, &db.PullRequestProjectionJobAttempt{}, &db.PullRequestMulticaLink{}} {
		var count int64
		if err := h.DB.Model(model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("%T remaining rows = %d, want 1", model, count)
		}
	}
	w = h.DoREST(t, http.MethodGet, "/api/v3/repos/"+keep.FullName, nil)
	assertStatusCode(t, w, http.StatusOK)
}
