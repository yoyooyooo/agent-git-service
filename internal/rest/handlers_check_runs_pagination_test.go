package rest_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/middleware"
	"github.com/ngaut/agent-git-service/internal/rest"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCheckRunsPaginationAcrossCIBackends(t *testing.T) {
	for _, kind := range []string{"native", "github-actions"} {
		t.Run(kind, func(t *testing.T) {
			database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "checks.db")), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := database.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { sqlDB.Close() })
			if err := database.AutoMigrate(&db.User{}, &db.Token{}, &db.Repository{}, &db.Label{}, &db.AgentBinding{}, &db.BranchProtection{}, &db.Collaborator{}, &db.TeamMember{}, &db.TeamRepository{}, &db.OrganizationMember{}, &db.Workflow{}, &db.WorkflowRun{}, &db.WorkflowRunJob{}, &db.CIResource{}); err != nil {
				t.Fatal(err)
			}
			owner := db.User{Login: "fixture-owner", Type: db.TypeUser}
			if err := database.Create(&owner).Error; err != nil {
				t.Fatal(err)
			}
			if err := database.Create(&db.Token{UserID: owner.ID, Value: "fixture-native-token"}).Error; err != nil {
				t.Fatal(err)
			}
			repo := db.Repository{OwnerID: owner.ID, Name: "repo", FullName: "fixture-owner/repo", Private: true}
			if err := database.Create(&repo).Error; err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("d", 40)
			svc := &service.Service{DB: database}
			if kind == "native" {
				for n := 1; n <= 3; n++ {
					workflow := db.Workflow{RepositoryID: repo.ID, Name: fmt.Sprintf("workflow-%d", n), Path: fmt.Sprintf(".github/workflows/%d.yml", n), State: db.WorkflowActive}
					if err := database.Create(&workflow).Error; err != nil {
						t.Fatal(err)
					}
					run := db.WorkflowRun{RepositoryID: repo.ID, WorkflowID: workflow.ID, HeadSHA: sha, Status: db.RunCompleted, Conclusion: db.ConclusionSuccess}
					if err := database.Create(&run).Error; err != nil {
						t.Fatal(err)
					}
					if err := database.Create(&db.WorkflowRunJob{RunID: run.ID, Name: fmt.Sprintf("check-%d", n), Status: db.RunCompleted, Conclusion: db.ConclusionSuccess}).Error; err != nil {
						t.Fatal(err)
					}
				}
			} else {
				provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixture-provider-token" {
						t.Error("wrong provider request")
					}
					if r.URL.Path == "/repos/ci/project/actions/runs" {
						runs := []any{}
						for n := 1; n <= 3; n++ {
							runs = append(runs, map[string]any{"id": n, "workflow_id": n, "name": fmt.Sprintf("workflow-%d", n), "head_sha": sha, "head_branch": "feature", "status": "completed", "conclusion": "success", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"})
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 3, "workflow_runs": runs})
						return
					}
					selector := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/ci/project/actions/runs/"), "/jobs")
					n, err := strconv.Atoi(selector)
					if err != nil || n < 1 || n > 3 {
						t.Error("unexpected provider path")
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "jobs": []any{map[string]any{"id": 10 + n, "run_id": n, "head_sha": sha, "name": fmt.Sprintf("check-%d", n), "status": "completed", "conclusion": "success"}}})
				}))
				t.Cleanup(provider.Close)
				config := cibackend.BackendConfig{Kind: kind, URL: provider.URL, TokenFile: "fixture", AllowHTTP: true}
				backend, err := cibackend.NewHTTP(config, "fixture-provider-token")
				if err != nil {
					t.Fatal(err)
				}
				required := []string{}
				svc.CI, err = cibackend.WithBackends(cibackend.Config{Backends: map[string]cibackend.BackendConfig{"builder": config}, Repositories: map[string]cibackend.Binding{repo.FullName: {Backend: "builder", Repository: "ci/project", RequiredChecks: &required}}}, map[string]cibackend.Backend{"builder": backend})
				if err != nil {
					t.Fatal(err)
				}
			}
			deps := &rest.Deps{Svc: svc}
			mux := chi.NewRouter()
			mux.Use(middleware.TokenAuth(svc))
			mux.Get("/api/v3/repos/{owner}/{repo}/commits/{ref}/check-runs", deps.CI(deps.ListCheckRunsForRef, "checks-for-ref"))
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			svc.PublicAPIBaseURL = server.URL
			path := "/api/v3/repos/" + repo.FullName + "/commits/" + sha + "/check-runs"
			type pageBody struct {
				Total  int              `json:"total_count"`
				Checks []map[string]any `json:"check_runs"`
			}
			read := func(query string) (pageBody, *httptest.ResponseRecorder) {
				t.Helper()
				r := httptest.NewRequest(http.MethodGet, path+query, nil)
				r.Header.Set("Authorization", "token fixture-native-token")
				out := httptest.NewRecorder()
				mux.ServeHTTP(out, r)
				var got pageBody
				if err := json.Unmarshal(out.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				return got, out
			}
			seen := map[float64]bool{}
			for page, size := range []int{2, 1, 0} {
				body, response := read(fmt.Sprintf("?per_page=2&page=%d", page+1))
				if response.Code != 200 || body.Total != 3 || body.Checks == nil || len(body.Checks) != size {
					t.Fatalf("page %d: %d %s", page+1, response.Code, response.Body)
				}
				if (page == 0) != strings.Contains(response.Header().Get("Link"), `rel="next"`) {
					t.Fatalf("wrong continuation header: %s", response.Header())
				}
				for _, check := range body.Checks {
					id := check["id"].(float64)
					if seen[id] {
						t.Fatalf("check ID repeated across pages: %.0f", id)
					}
					seen[id] = true
				}
			}
			body, response := read("?per_page=100&page=2")
			if response.Code != 200 || body.Total != 3 || len(body.Checks) != 0 || body.Checks == nil {
				t.Fatalf("out-of-range page repeated checks: %s", response.Body)
			}
			if _, response := read("?page=0"); response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("invalid pagination: %d", response.Code)
			}
			gh, err := exec.LookPath("gh")
			if err != nil {
				t.Log("official gh unavailable; HTTP contract verified")
				return
			}
			cmd := exec.CommandContext(t.Context(), gh, "api", server.URL+path+"?per_page=2", "--paginate", "--slurp")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GH_CONFIG_DIR=" + t.TempDir(), "GH_ENTERPRISE_TOKEN=fixture-native-token", "GH_PROMPT_DISABLED=1", "NO_COLOR=1"}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("gh checks pagination: %v %s", err, output)
			}
			var pages []pageBody
			if err := json.Unmarshal(output, &pages); err != nil {
				t.Fatal(err)
			}
			if len(pages) != 2 || pages[0].Total != 3 || pages[1].Total != 3 || len(pages[0].Checks)+len(pages[1].Checks) != 3 {
				t.Fatalf("gh pagination incomplete: %s", output)
			}
		})
	}
}
