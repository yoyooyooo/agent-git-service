package graphql

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestAstraR1MergedFilterBeforeTerminalRowLimit(t *testing.T) {
	database, err := db.Init("sqlite:" + filepath.Join(t.TempDir(), "astra-states.db") + "?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	actor := db.User{Login: "astra-fixture", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := database.Create(&actor).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{OwnerID: actor.ID, Name: "project", FullName: "astra-fixture/project", DefaultBranch: "main"}
	if err := database.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prs := make([]db.PullRequest, 1001)
	for i := range prs {
		prs[i] = db.PullRequest{RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: actor.ID, Number: i + 1, Title: "terminal fixture", State: db.StateClosed, HeadRef: "feature", BaseRef: "main", HeadSHA: head, Merged: i == 0, CreatedAt: old.Add(time.Duration(i) * time.Second)}
		if i == 0 {
			prs[i].MergeCommitSHA = head
		}
	}
	if err := database.CreateInBatches(prs, 100).Error; err != nil {
		t.Fatal(err)
	}
	server := NewServer(&service.Service{DB: database, BaseURL: "https://primary.example.test"})
	ctx := service.ContextWithUser(context.Background(), actor)
	query := `query($owner:String!,$repo:String!,$state:[PullRequestState!]){repository(owner:$owner,name:$repo){pullRequests(states:$state,first:30){nodes{number state}totalCount pageInfo{hasNextPage endCursor}}}}`
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"owner": "astra-fixture", "repo": "project", "state": []string{"MERGED"}}})
	request := httptest.NewRequest("POST", "/api/graphql", strings.NewReader(string(payload))).WithContext(ctx)
	response := httptest.NewRecorder()
	server.Handler(response, request)
	t.Logf("1 older merged PR + 1000 newer closed-unmerged PRs; MERGED response=%s", response.Body.String())
	var result struct {
		Data struct {
			Repository struct {
				PullRequests struct {
					Nodes      []struct{ Number int }
					TotalCount int
					PageInfo   struct {
						HasNextPage bool
						EndCursor   string
					}
				}
			}
		}
		Errors []any
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	nodes := result.Data.Repository.PullRequests.Nodes
	connection := result.Data.Repository.PullRequests
	if response.Code != 200 || len(result.Errors) > 0 || len(nodes) != 1 || nodes[0].Number != 1 || connection.TotalCount != 1 || connection.PageInfo.HasNextPage || connection.PageInfo.EndCursor != "" {
		t.Fatalf("MERGED filter silently loses matching PR #1 behind nonmatching terminal rows: %s", response.Body.String())
	}
}
