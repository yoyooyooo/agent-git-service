package service

import (
	"context"
	"errors"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
)

func TestGitLabMirrorFactsRequireRepositoryRead(t *testing.T) {
	svc := setupProjectionStateService(t)
	if err := svc.DB.AutoMigrate(&db.Collaborator{}, &db.TeamMember{}, &db.TeamRepository{}, &db.OrganizationMember{}); err != nil {
		t.Fatal(err)
	}
	var repo db.Repository
	if err := svc.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&repo).Update("private", true).Error; err != nil {
		t.Fatal(err)
	}
	reader := db.User{Login: "reader", Type: db.TypeUser}
	outsider := db.User{Login: "outsider", Type: db.TypeUser}
	for _, user := range []*db.User{&reader, &outsider} {
		if err := svc.DB.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.DB.Create(&db.Collaborator{RepositoryID: repo.ID, UserID: reader.ID, Permission: "read"}).Error; err != nil {
		t.Fatal(err)
	}
	svc.GitLabIntegration = gitlabintegration.New(gitlabintegration.Config{
		Enabled: true, BaseURL: "https://gitlab.example.test", MergeAuthority: "forgejo", Token: "fixture-token",
	}, func(context.Context, string, string, string, string) error {
		t.Fatal("read invoked provider write")
		return nil
	})
	for _, ctx := range []context.Context{ContextWithAnonRequest(t.Context()), ContextWithUser(t.Context(), outsider)} {
		if _, err := svc.GetGitLabMirrorFacts(ctx, repo.FullName, "refs/heads/main"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("private facts not hidden: %v", err)
		}
	}
	ctx := ContextWithUser(t.Context(), reader)
	facts, err := svc.GetGitLabMirrorFacts(ctx, repo.FullName, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if facts.Schema != "ags.gitlab-mirror-facts.v1" || facts.Repository != repo.FullName || facts.ConfigurationSource != "running_integration" || facts.GitLabRefReadback != "not_claimed" || facts.Target == nil {
		t.Fatalf("unexpected facts: %+v", facts)
	}
	if _, err := svc.GetGitLabMirrorFacts(ctx, "missing/repo", "refs/heads/main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing repo: %v", err)
	}
	for _, ref := range []string{"", "main", "refs/heads/main~1", "refs/heads/a b"} {
		if _, err := svc.GetGitLabMirrorFacts(ctx, repo.FullName, ref); !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid ref accepted: %v", err)
		}
	}
	svc.GitLabIntegration = nil
	facts, err = svc.GetGitLabMirrorFacts(ctx, repo.FullName, "refs/heads/main")
	if err != nil || facts.Configuration != nil || facts.Target != nil {
		t.Fatalf("missing integration facts: %+v %v", facts, err)
	}
}
