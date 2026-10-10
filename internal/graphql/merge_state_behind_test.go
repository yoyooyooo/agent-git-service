package graphql

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestMergeStateStatusBehindWhenHeadOmitsBase(t *testing.T) {
	srv := setupTestServer(t)
	ctx := service.ContextWithUser(context.Background(), db.User{Login: "tester"})
	const full = "tester/behind"
	owner := db.User{Login: "tester", Name: "tester", Type: db.TypeUser}
	if err := srv.Svc.DB.Where("login = ?", "tester").First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{OwnerID: owner.ID, Name: "behind", FullName: full, DefaultBranch: "main"}
	if err := srv.Svc.DB.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	if err := srv.Svc.Git.Init(ctx, full, "main", true); err != nil {
		t.Fatal(err)
	}
	base, err := srv.Svc.Git.HeadSHA(ctx, full, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err = srv.Svc.Git.CreateBranch(ctx, full, "feature", "main"); err != nil {
		t.Fatal(err)
	}
	head, err := srv.Svc.Git.WriteFile(ctx, full, "feature", "feature.txt", "feature", []byte("feature\n"))
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := srv.Svc.Git.WriteFile(ctx, full, "main", "base.txt", "advance", []byte("advance\n"))
	if err != nil {
		t.Fatal(err)
	}
	if advanced == base || advanced == head {
		t.Fatal("fixture did not diverge")
	}
	pr := db.PullRequest{RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: owner.ID, Number: 1, Title: "behind", State: db.StateOpen, HeadRef: "feature", BaseRef: "main", HeadSHA: head, BaseSHA: base}
	if err = srv.Svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){mergeStateStatus mergeable}}}`
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"owner": "tester", "name": "behind", "number": 1}})
	request := httptest.NewRequest("POST", "/api/graphql", strings.NewReader(string(payload))).WithContext(ctx)
	response := httptest.NewRecorder()
	srv.Handler(response, request)
	var result struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					MergeStateStatus string
					Mergeable        string
				}
			}
		}
		Errors []any
	}
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != 200 || len(result.Errors) > 0 {
		t.Fatalf("response=%s err=%v", response.Body.String(), err)
	}
	got := result.Data.Repository.PullRequest
	if got.MergeStateStatus != "BEHIND" {
		t.Fatalf("mergeStateStatus=%s body=%s", got.MergeStateStatus, response.Body.String())
	}
}
