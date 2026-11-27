package graphql

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestStatusCheckRollupKeepsUnboundForgejoRunQueued(t *testing.T) {
	for _, status := range []string{"waiting", "blocked"} {
		t.Run(status, func(t *testing.T) {
			database, err := db.Init("sqlite:" + filepath.Join(t.TempDir(), "queued.db") + "?_foreign_keys=on")
			if err != nil {
				t.Fatal(err)
			}
			pool, err := database.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = pool.Close() })
			actor := db.User{Login: "fixture", Type: db.TypeUser, Status: db.UserStatusActive}
			if err := database.Create(&actor).Error; err != nil {
				t.Fatal(err)
			}
			repo := db.Repository{OwnerID: actor.ID, Name: "project", FullName: "fixture/project", DefaultBranch: "main", Private: true}
			if err := database.Create(&repo).Error; err != nil {
				t.Fatal(err)
			}
			head := strings.Repeat("a", 40)
			pr := db.PullRequest{RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: actor.ID, Number: 1, Title: "fixture", State: db.StateOpen, HeadRef: "feature", BaseRef: "main", HeadSHA: head}
			if err := database.Create(&pr).Error; err != nil {
				t.Fatal(err)
			}
			pr.Repository = repo
			run := map[string]any{
				"id": 17, "index_in_repo": 9, "workflow_id": "ci.yml", "title": "CI", "prettyref": "feature",
				"commit_sha": head, "event": "push", "status": status,
				"created": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:01:00Z",
				"html_url": "https://ci.example.test/ci/project/actions/runs/9",
			}
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/internal/provider-logs/repos/ci/project/runs":
					http.NotFound(w, r) // A bridge without run listing uses the public run inventory.
				case "/api/v1/repos/ci/project/actions/runs":
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "workflow_runs": []any{run}})
				case "/api/v1/repos/ci/project/actions/runs/17":
					_ = json.NewEncoder(w).Encode(run)
				case "/api/internal/provider-logs/repos/ci/project/runs/17/jobs":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"schema": "ags.internal-provider-jobs.v1", "repo": "ci/project", "run": "17",
						"run_number": 9, "head_sha": head, "jobs": []any{},
					})
				default:
					t.Errorf("unbound rollup attempted unexpected task/log route: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer provider.Close()
			cfg := cibackend.BackendConfig{
				Kind: "forgejo", URL: provider.URL, TokenFile: "fixture", AllowHTTP: true,
				LogBridge: &cibackend.LogBridgeConfig{URL: provider.URL, TokenFile: "fixture", AllowHTTP: true},
			}
			backend, err := cibackend.NewHTTP(cfg, "provider-fixture", "bridge-fixture")
			if err != nil {
				t.Fatal(err)
			}
			required := []string{}
			registry, err := cibackend.WithBackends(cibackend.Config{
				Backends: map[string]cibackend.BackendConfig{"builder": cfg},
				Repositories: map[string]cibackend.Binding{repo.FullName: {
					Backend: "builder", Repository: "ci/project", RequiredChecks: &required,
				}},
			}, map[string]cibackend.Backend{"builder": backend})
			if err != nil {
				t.Fatal(err)
			}
			server := &Server{Svc: &service.Service{DB: database, CI: registry, BaseURL: "https://primary.example.test"}}
			state := &responseErrors{query: `query { repository(owner:"fixture",name:"project") { pullRequest(number:1) { commits(last:1) { nodes { commit { statusCheckRollup { contexts { nodes { ... on CheckRun { name status conclusion } } } } } } } } } }`}
			ctx := service.ContextWithUser(t.Context(), actor)
			ctx = context.WithValue(ctx, responseErrorsKey{}, state)
			body, err := json.Marshal(server.statusCheckRollupGQL(ctx, pr))
			if err != nil || len(state.items) != 0 {
				t.Fatalf("queued rollup failed: errors=%+v marshal=%v", state.items, err)
			}
			var result struct {
				Nodes []struct {
					Commit struct {
						OID               string
						StatusCheckRollup struct {
							Contexts struct {
								CheckRunCount int
								Nodes         []struct{ Name, Status, Conclusion string }
							}
						}
					}
				}
			}
			if err := json.Unmarshal(body, &result); err != nil || len(result.Nodes) != 1 {
				t.Fatalf("invalid rollup: %s error=%v", body, err)
			}
			commit := result.Nodes[0].Commit
			checks := commit.StatusCheckRollup.Contexts
			if commit.OID != head || checks.CheckRunCount != 1 || len(checks.Nodes) != 1 || checks.Nodes[0].Status != "QUEUED" || checks.Nodes[0].Conclusion != "" || checks.Nodes[0].Name == "" {
				t.Fatalf("unbound run was not reported as queued without a conclusion: %s", body)
			}
		})
	}
}
