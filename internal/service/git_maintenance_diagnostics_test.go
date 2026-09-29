package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/gitstore"
)

func TestGitMaintenanceIdentifiesMissingSchemaWithoutDriverText(t *testing.T) {
	svc, repo, _ := gitMaintenanceDatabase(t)
	if err := svc.DB.Migrator().DropTable(&db.PagesBuild{}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.GitMaintenanceRoots(context.Background(), repo)
	var failure *gitstore.RootInventoryError
	if !errors.As(err, &failure) || failure.Code != "query_failed" || failure.Source != "pages_builds.commit_sha" {
		t.Fatalf("schema failure not classified: %v", err)
	}
}

func TestGitMaintenanceIdentifiesInvalidOIDSourceWithoutContent(t *testing.T) {
	svc, repo, owner := gitMaintenanceDatabase(t)
	pr := db.PullRequest{Number: 1, RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: owner.ID, Title: "old invalid identity", HeadRef: "topic", BaseRef: "main", HeadSHA: "not-an-object-id"}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	_, err := svc.GitMaintenanceRoots(context.Background(), repo)
	var failure *gitstore.RootInventoryError
	if !errors.As(err, &failure) || failure.Code != "invalid_oid" || failure.Source != "pull_requests.head_sha" {
		t.Fatalf("invalid identity not classified: %v", err)
	}
	if err.Error() != "application object inventory failed" {
		t.Fatal("record content leaked into error")
	}
}
