package rest_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githttp"
	"github.com/ngaut/agent-git-service/internal/graphql"
	"github.com/ngaut/agent-git-service/internal/oauth"
	"github.com/ngaut/agent-git-service/internal/rest"
	"github.com/ngaut/agent-git-service/internal/router"
	"github.com/ngaut/agent-git-service/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestProviderInventoryHTTPAndOfficialGHPagination(t *testing.T) {
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "inventory.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := database.AutoMigrate(&db.User{}, &db.Token{}, &db.Repository{}, &db.Collaborator{}, &db.TeamMember{}, &db.TeamRepository{}, &db.OrganizationMember{}, &db.Label{}, &db.PullRequest{}, &db.PullRequestProjection{}); err != nil {
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
	providerCalls := 0
	providerFailure := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "token fixture-provider-token" {
			t.Error("provider request not a configured GET")
		}
		if providerFailure {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("sensitive-provider-error"))
			return
		}
		switch r.URL.Path {
		case "/api/v1/repos/provider-owner/mirror/pulls":
			switch r.URL.Query().Get("page") {
			case "1":
				_, _ = w.Write([]byte(`[{"number":3,"state":"closed","title":"Old projection"}]`))
			case "2":
				_, _ = w.Write([]byte(`[{"number":11,"state":"open","title":"Current projection"}]`))
			case "3":
				_, _ = w.Write([]byte(`[]`))
			default:
				t.Error("unexpected page")
				w.WriteHeader(http.StatusBadRequest)
			}
		case "/api/v1/repos/provider-owner/mirror/pulls/3":
			_, _ = w.Write([]byte(`{"number":3,"state":"closed"}`))
		case "/api/v1/repos/provider-owner/mirror/branches/main":
			_, _ = fmt.Fprintf(w, `{"name":"main","commit":{"id":%q}}`, strings.Repeat("c", 40))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.Close)
	svc := &service.Service{DB: database, BaseURL: "https://ags.example.test", ForgejoIntegration: forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: provider.URL,
		RepoMap: map[string]forgejointegration.RepoMapping{repo.FullName: {Owner: "provider-owner", Repo: "mirror"}},
	}, forgejointegration.NewHTTPClient(provider.URL, "fixture-provider-token"), nil)}
	handlers := &rest.Deps{Svc: svc}
	mux := router.RegisterRoutes(chi.NewRouter(), handlers, githttp.New(nil, svc), graphql.NewServer(svc), oauth.New(svc), "http://console.example.test")
	read := func(path, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.Header.Set("Authorization", "token "+token)
		}
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, r)
		return out
	}
	base := "/api/ext/v1/repos/fixture-owner/repo/provider"
	if out := read(base+"/pulls?state=all", ""); out.Code != http.StatusUnauthorized || providerCalls != 0 {
		t.Fatalf("anonymous inventory: %d calls=%d", out.Code, providerCalls)
	}
	for _, bad := range []string{"?page=invalid", "?page=1&page=2", "?page=0", "?per_page=101", "?state=bad"} {
		if out := read(base+"/pulls"+bad, "fixture-native-token"); out.Code != http.StatusUnprocessableEntity || providerCalls != 0 {
			t.Fatalf("invalid query: %d calls=%d", out.Code, providerCalls)
		}
	}
	for _, path := range []string{base + "/pulls?state=all", base + "/pulls/3", base + "/git/ref?ref=refs%2Fheads%2Fmain"} {
		out := read(path, "fixture-native-token")
		if out.Code != http.StatusOK || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("provider observation: %d %s", out.Code, out.Body)
		}
		if strings.Contains(out.Body.String(), "fixture-provider-token") {
			t.Fatal("provider credential escaped")
		}
	}
	out := read(base+"/pulls?state=all", "fixture-native-token")
	if !strings.Contains(out.Body.String(), `"ags_pr_number":null`) || !strings.Contains(out.Header().Get("Link"), `page=2`) {
		t.Fatalf("unmapped/continuation contract: %s %s", out.Header(), out.Body)
	}
	if out := read(base+"/pulls?state=all&page=3", "fixture-native-token"); out.Header().Get("Link") != "" || !strings.Contains(out.Body.String(), `"next_page":null`) {
		t.Fatal("empty page must terminate")
	}
	if out := read(strings.Replace(base+"/pulls", "/api/ext/v1/", "/api/v3/", 1), "fixture-native-token"); out.Code != http.StatusNotFound {
		t.Fatalf("new API leaked into v3: %d", out.Code)
	}
	providerFailure = true
	if out := read(base+"/pulls?state=all", "fixture-native-token"); out.Code != http.StatusBadGateway || strings.Contains(out.Body.String(), "sensitive-provider-error") {
		t.Fatalf("provider failure contract: %d %s", out.Code, out.Body)
	}
	providerFailure = false
	t.Run("official gh follows every page", func(t *testing.T) {
		gh, err := exec.LookPath("gh")
		if err != nil {
			t.Skip("official gh is not installed; HTTP pagination is covered above")
		}
		server := httptest.NewServer(mux)
		defer server.Close()
		svc.PublicAPIBaseURL = server.URL
		cmd := exec.CommandContext(t.Context(), gh, "api", server.URL+base+"/pulls?state=all", "--paginate", "--slurp")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GH_CONFIG_DIR=" + t.TempDir(), "GH_ENTERPRISE_TOKEN=fixture-native-token", "GH_PROMPT_DISABLED=1", "NO_COLOR=1"}
		body, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gh api pagination: %v %s", err, body)
		}
		var pages []service.ProviderPullRequestInventory
		if err := json.Unmarshal(body, &pages); err != nil {
			t.Fatalf("gh output: %v %s", err, body)
		}
		if len(pages) != 3 || len(pages[0].PullRequests) != 1 || len(pages[1].PullRequests) != 1 || pages[2].NextPage != nil {
			t.Fatalf("gh lost provider pages: %s", body)
		}
	})
}
