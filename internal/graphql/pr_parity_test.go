package graphql

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestOfficialGHPRListStatesAndMergeCommit(t *testing.T) {
	database, err := db.Init("sqlite:" + filepath.Join(t.TempDir(), "parity.db") + "?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	actor := db.User{Login: "fixture", Type: db.TypeUser, Status: db.UserStatusActive}
	if err = database.Create(&actor).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{OwnerID: actor.ID, Name: "project", FullName: "fixture/project", DefaultBranch: "main"}
	if err = database.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	for i, state := range []string{db.StateOpen, db.StateClosed, db.StateClosed} {
		pr := db.PullRequest{RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: actor.ID, Number: i + 1, Title: "fixture", State: state, HeadRef: "feature", BaseRef: "main", HeadSHA: head, Merged: i == 2}
		if pr.Merged {
			pr.MergeCommitSHA = head
		}
		if err = database.Create(&pr).Error; err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(&service.Service{DB: database, BaseURL: "https://primary.example.test"})
	ctx := service.ContextWithUser(context.Background(), actor)
	for _, tc := range []struct {
		name, query string
		vars        map[string]any
		numbers     []int
	}{
		{"official gh merged variable", `query($owner:String!,$repo:String!,$state:[PullRequestState!]){repository(owner:$owner,name:$repo){pullRequests(states:$state){nodes{number state mergeCommit{oid}}}}}`, map[string]any{"owner": "fixture", "repo": "project", "state": []string{"MERGED"}}, []int{3}},
		{"closed excludes merged", `query($owner:String!,$repo:String!,$state:[PullRequestState!]){repository(owner:$owner,name:$repo){pullRequests(states:$state){nodes{number state mergeCommit{oid}}}}}`, map[string]any{"owner": "fixture", "repo": "project", "state": []string{"CLOSED"}}, []int{2}},
		{"open inline", `query($owner:String!,$repo:String!){repository(owner:$owner,name:$repo){pullRequests(states:[OPEN]){nodes{number state mergeCommit{oid}}}}}`, map[string]any{"owner": "fixture", "repo": "project"}, []int{1}},
		{"all states", `query($owner:String!,$repo:String!,$state:[PullRequestState!]){repository(owner:$owner,name:$repo){pullRequests(states:$state){nodes{number state mergeCommit{oid}}}}}`, map[string]any{"owner": "fixture", "repo": "project", "state": []string{"OPEN", "CLOSED", "MERGED"}}, []int{1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{"query": tc.query, "variables": tc.vars})
			request := httptest.NewRequest("POST", "/api/graphql", strings.NewReader(string(payload))).WithContext(ctx)
			response := httptest.NewRecorder()
			server.Handler(response, request)
			var result struct {
				Data struct {
					Repository struct {
						PullRequests struct {
							Nodes []struct {
								Number      int
								State       string
								MergeCommit *struct {
									OID string `json:"oid"`
								}
							}
						}
					}
				}
				Errors []any
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || len(result.Errors) > 0 {
				t.Fatalf("response=%s err=%v", response.Body.String(), err)
			}
			nodes := result.Data.Repository.PullRequests.Nodes
			if len(nodes) != len(tc.numbers) {
				t.Fatalf("wrong state selection: %s", response.Body.String())
			}
			seen := map[int]bool{}
			for _, node := range nodes {
				seen[node.Number] = true
				if node.Number == 3 && (node.MergeCommit == nil || node.MergeCommit.OID != head) {
					t.Fatalf("missing merge SHA: %s", response.Body.String())
				}
				if node.Number != 3 && node.MergeCommit != nil {
					t.Fatal("unmerged PR has merge commit")
				}
			}
			for _, number := range tc.numbers {
				if !seen[number] {
					t.Fatalf("missing PR %d: %s", number, response.Body.String())
				}
			}
		})
	}
	intro := TypeFields("PullRequest")["fields"].([]any)
	found := false
	for _, field := range intro {
		if field.(map[string]any)["name"] == "mergeCommit" {
			found = true
		}
	}
	if !found {
		t.Fatal("mergeCommit missing from client feature discovery")
	}
}
