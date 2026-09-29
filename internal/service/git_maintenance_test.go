package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func gitMaintenanceDatabase(t *testing.T) (*Service, db.Repository, db.User) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "maintenance.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err = db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	owner := db.User{Login: "maintenance-owner", Name: "Maintenance Owner", Type: db.TypeUser}
	if err = database.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{Name: "repo", FullName: "maintenance-owner/repo", OwnerID: owner.ID, DefaultBranch: "main"}
	if err = database.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	return &Service{DB: database}, repo, owner
}

func TestGitMaintenanceRootsUseRealSchemaAndPreserveHistoricalFacts(t *testing.T) {
	svc, repo, owner := gitMaintenanceDatabase(t)
	oid := func(c string) string { return strings.Repeat(c, 40) }
	pr := db.PullRequest{Number: 7, RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: owner.ID, Title: "past merge", State: db.StateClosed, Merged: true, HeadRef: "topic", BaseRef: "main", HeadSHA: oid("a"), BaseSHA: oid("b"), MergeCommitSHA: oid("c")}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	review := db.PullRequestReview{PullRequestID: pr.ID, AuthorLogin: owner.Login, State: "APPROVED", CommitSHA: oid("d")}
	deployment := db.Deployment{RepositoryID: repo.ID, CreatorID: owner.ID, Ref: oid("e"), Task: "deploy"}
	symbolic := db.Deployment{RepositoryID: repo.ID, CreatorID: owner.ID, Ref: "main", Task: "deploy"}
	constraints, _ := json.Marshal(map[string]string{"head_sha": oid("f"), "ref": "topic"})
	invocation := db.AccessGrantInvocation{ID: "maintenance-invocation", GrantID: "fixture-grant", ActorUserID: owner.ID, ExecutorUserID: owner.ID, RepositoryID: repo.ID, Repository: repo.FullName, Operation: "ci.read", ConstraintsJSON: string(constraints), State: "succeeded"}
	for _, row := range []any{&review, &deployment, &symbolic, &invocation} {
		if err := svc.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	roots, err := svc.GitMaintenanceRoots(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{oid("a"), oid("b"), oid("c"), oid("d"), oid("e"), oid("f")}
	sort.Strings(expected)
	if !reflect.DeepEqual(roots, expected) {
		t.Fatalf("root inventory %v want %v", roots, expected)
	}
	var after db.PullRequest
	if err = svc.DB.First(&after, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.HeadSHA != pr.HeadSHA || after.BaseSHA != pr.BaseSHA || after.MergeCommitSHA != pr.MergeCommitSHA || !after.Merged {
		t.Fatal("maintenance rewrote historical PR facts")
	}
}

func TestGitMaintenanceRootQueriesFailClosedOnMissingSchemaOrCancelledRead(t *testing.T) {
	svc, repo, _ := gitMaintenanceDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.GitMaintenanceRoots(ctx, repo); err == nil {
		t.Fatal("cancelled scan treated as empty inventory")
	}
	if err := svc.DB.Migrator().DropTable(&db.PagesBuild{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GitMaintenanceRoots(context.Background(), repo); err == nil {
		t.Fatal("missing schema treated as empty inventory")
	}
}
