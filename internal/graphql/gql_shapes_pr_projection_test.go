package graphql

import (
	"context"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/stretchr/testify/require"
)

func TestPRGQLExternalProjections(t *testing.T) {
	server := setupTestServer(t)
	require.NoError(t, server.Svc.DB.AutoMigrate(&db.PullRequestProjection{}))

	var human db.User
	require.NoError(t, server.Svc.DB.Where("login = ?", "tester").First(&human).Error)
	repo := db.Repository{Name: "demo", FullName: "tester/demo", OwnerID: human.ID, Owner: human, DefaultBranch: "main"}
	require.NoError(t, server.Svc.DB.Create(&repo).Error)
	pr := db.PullRequest{
		ID: 42, Number: 8, Title: "projected", State: db.StateOpen, Author: human, AuthorID: human.ID,
		RepositoryID: repo.ID, Repository: repo, HeadRepositoryID: repo.ID, HeadRepository: repo,
		HeadRef: "feature", BaseRef: "main",
	}
	require.NoError(t, server.Svc.DB.Create(&pr).Error)
	require.NoError(t, server.Svc.UpsertPullRequestProjection(context.Background(), db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "tester/demo", ExternalNumber: 23,
		ExternalURL:  "http://forgejo.local/tester/demo/pulls/23",
		SourceBranch: "feature", TargetBranch: "main", State: service.ProjectionStateOpen,
	}))

	omitted := server.prGQL(context.Background(), pr, "url number")
	if rows, _ := omitted["externalProjections"].([]map[string]any); len(rows) != 0 {
		t.Fatalf("unrequested externalProjections=%#v", omitted["externalProjections"])
	}

	shape := server.prGQL(context.Background(), pr, "externalProjections")
	rows, _ := shape["externalProjections"].([]map[string]any)
	if len(rows) != 1 || rows[0]["provider"] != "forgejo" || rows[0]["externalNumber"] != 23 {
		t.Fatalf("externalProjections=%#v", shape["externalProjections"])
	}
	if rows[0]["externalUrl"] != "http://forgejo.local/tester/demo/pulls/23" {
		t.Fatalf("externalUrl=%#v", rows[0]["externalUrl"])
	}
}
