package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newProviderInventoryFixture(t *testing.T, provider http.HandlerFunc) (*Service, context.Context, db.Repository) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "inventory.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := database.AutoMigrate(&db.User{}, &db.Repository{}, &db.Collaborator{}, &db.TeamMember{}, &db.TeamRepository{}, &db.OrganizationMember{}, &db.Label{}, &db.PullRequest{}, &db.PullRequestProjection{}); err != nil {
		t.Fatal(err)
	}
	owner := db.User{Login: "native-owner", Type: db.TypeUser, UserKind: db.UserKindHuman}
	if err := database.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{OwnerID: owner.ID, Name: "demo", FullName: "native-owner/demo", Private: true}
	if err := database.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{RepositoryID: repo.ID, Number: 7, State: "open", Title: "Current projection"}
	if err := database.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	projection := db.PullRequestProjection{RepositoryID: repo.ID, PullRequestID: pr.ID, Provider: ProjectionProviderForgejo, ExternalRepo: "provider-owner/mirror", ExternalNumber: 11, State: "open"}
	if err := database.Create(&projection).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)
	svc := &Service{DB: database, ForgejoIntegration: forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: server.URL,
		RepoMap: map[string]forgejointegration.RepoMapping{repo.FullName: {Owner: "provider-owner", Repo: "mirror"}},
	}, forgejointegration.NewHTTPClient(server.URL, "fixture-provider-token"), nil)}
	return svc, ContextWithUser(t.Context(), owner), repo
}

func inventoryProviderPull(number int, state string) map[string]any {
	return map[string]any{
		"number": number, "state": state, "merged": false, "title": fmt.Sprintf("Provider pull %d", number),
		"html_url": fmt.Sprintf("https://forgejo.example.test/provider-owner/mirror/pulls/%d", number),
		"head":     map[string]string{"ref": "feature", "sha": strings.Repeat("a", 40)},
		"base":     map[string]string{"ref": "main", "sha": strings.Repeat("b", 40)},
	}
}

func TestProviderInventoryIncludesUnmappedAndContinuesShortPagesWithoutWrites(t *testing.T) {
	var pages []int
	var exact []int
	pulls := []map[string]any{inventoryProviderPull(3, "closed"), inventoryProviderPull(11, "open"), inventoryProviderPull(12, "closed")}
	svc, ctx, repo := newProviderInventoryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "token fixture-provider-token" {
			t.Error("inventory must only use the configured provider read client")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path == "/api/v1/repos/provider-owner/mirror/pulls" {
			if r.URL.Query().Get("state") != "all" || r.URL.Query().Get("sort") != "oldest" || r.URL.Query().Get("limit") != "50" {
				t.Errorf("unexpected provider query: %s", r.URL.RawQuery)
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			pages = append(pages, page)
			start := min((page-1)*2, len(pulls)) // Provider caps a requested 50 to 2.
			_ = json.NewEncoder(w).Encode(pulls[start:min(start+2, len(pulls))])
			return
		}
		number, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/provider-owner/mirror/pulls/"))
		exact = append(exact, number)
		for _, pull := range pulls {
			if pull["number"] == number {
				_ = json.NewEncoder(w).Encode(pull)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	writes := 0
	for _, register := range []func(string, func(*gorm.DB)) error{
		svc.DB.Callback().Create().Before("gorm:create").Register,
		svc.DB.Callback().Update().Before("gorm:update").Register,
		svc.DB.Callback().Delete().Before("gorm:delete").Register,
	} {
		if err := register("inventory:no_writes", func(*gorm.DB) { writes++ }); err != nil {
			t.Fatal(err)
		}
	}
	var collected []ProviderInventoryPullRequest
	for page := 1; ; {
		result, err := svc.ListProviderPullRequests(ctx, repo.FullName, "all", page, 50)
		if err != nil {
			t.Fatal(err)
		}
		if result.ExternalRepo != "provider-owner/mirror" || result.Repository != repo.FullName || result.ObservedAt.IsZero() {
			t.Fatalf("wrong inventory identity: %+v", result)
		}
		collected = append(collected, result.PullRequests...)
		if result.NextPage == nil {
			if result.PullRequests == nil || len(result.PullRequests) != 0 {
				t.Fatal("only an empty array may terminate provider pagination")
			}
			break
		}
		page = *result.NextPage
	}
	if !reflect.DeepEqual(pages, []int{1, 2, 3}) || len(collected) != 3 {
		t.Fatalf("incomplete pagination: pages=%v rows=%d", pages, len(collected))
	}
	if collected[0].AGSPRNumber != nil || collected[0].MappingStatus != "unmapped" || collected[1].AGSPRNumber == nil || *collected[1].AGSPRNumber != 7 || collected[1].MappingStatus != "current" {
		t.Fatalf("provider numbers must not be guessed as AGS numbers: %+v", collected)
	}
	orphan, err := svc.ReadProviderPullRequest(ctx, repo.FullName, 3)
	if err != nil || orphan.PullRequest.AGSPRNumber != nil || orphan.PullRequest.State != "closed" {
		t.Fatalf("old provider projection is not independently readable: %+v %v", orphan, err)
	}
	current, err := svc.ReadProviderPullRequest(ctx, repo.FullName, 11)
	if err != nil || current.PullRequest.AGSPRNumber == nil || *current.PullRequest.AGSPRNumber != 7 {
		t.Fatalf("current provider mapping: %+v %v", current, err)
	}
	if _, err := svc.ReadProviderPullRequest(ctx, repo.FullName, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing provider pull: %v", err)
	}
	if !reflect.DeepEqual(exact, []int{3, 11, 99}) || writes != 0 {
		t.Fatalf("unexpected provider calls or native mutations: exact=%v writes=%d", exact, writes)
	}
}

func TestProviderInventoryRequiresCurrentNativeReadWithoutDelegation(t *testing.T) {
	calls := 0
	svc, ownerCtx, repo := newProviderInventoryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("[]"))
	})
	outsider := db.User{Login: "outsider", Type: db.TypeUser, UserKind: db.UserKindHuman}
	if err := svc.DB.Create(&outsider).Error; err != nil {
		t.Fatal(err)
	}
	outsiderCtx := ContextWithUser(t.Context(), outsider)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"internal without identity", t.Context(), ErrUnauthorized},
		{"anonymous", ContextWithAnonRequest(t.Context()), ErrUnauthorized},
		{"no repository grant", outsiderCtx, ErrNotFound},
		{"PR scoped session", ContextWithDelegatedSession(ownerCtx, db.DelegatedAgentSession{ID: "single-pr", OperationName: "pr.read"}), ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.ListProviderPullRequests(tc.ctx, repo.FullName, "all", 1, 50); !errors.Is(err, tc.want) {
				t.Fatalf("list permission: got %v want %v", err, tc.want)
			}
			if _, err := svc.ReadProviderPullRequest(tc.ctx, repo.FullName, 3); !errors.Is(err, tc.want) {
				t.Fatalf("exact permission: got %v want %v", err, tc.want)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("denied requests reached the provider: %d", calls)
	}
	grant := db.Collaborator{RepositoryID: repo.ID, UserID: outsider.ID, Permission: "read"}
	if err := svc.DB.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListProviderPullRequests(outsiderCtx, repo.FullName, "all", 1, 50); err != nil || calls != 1 {
		t.Fatalf("native read grant rejected: %v calls=%d", err, calls)
	}
	if err := svc.DB.Delete(&grant).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListProviderPullRequests(outsiderCtx, repo.FullName, "all", 1, 50); !errors.Is(err, ErrNotFound) || calls != 1 {
		t.Fatalf("revoked grant reused: %v calls=%d", err, calls)
	}
	svc.ForgejoIntegration = nil
	if _, err := svc.ListProviderPullRequests(ownerCtx, repo.FullName, "all", 1, 50); !errors.Is(err, ErrProviderInventoryNotConfigured) {
		t.Fatalf("missing integration must not appear as an empty repository: %v", err)
	}
}

func TestProviderInventoryFailsClosedOnInvalidInputAndProviderObservations(t *testing.T) {
	calls := 0
	body := "[]"
	status := http.StatusOK
	svc, ctx, repo := newProviderInventoryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	for _, tc := range []struct {
		state      string
		page, size int
	}{
		{"unknown", 1, 50}, {"all", 0, 50}, {"all", 10001, 50}, {"all", 1, 0}, {"all", 1, 101},
	} {
		if _, err := svc.ListProviderPullRequests(ctx, repo.FullName, tc.state, tc.page, tc.size); !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid pagination/state accepted: %+v %v", tc, err)
		}
	}
	if _, err := svc.ReadProviderPullRequest(ctx, repo.FullName, 0); !errors.Is(err, ErrValidation) || calls != 0 {
		t.Fatalf("invalid input reached provider: %v calls=%d", err, calls)
	}
	for _, invalid := range []string{
		"null", "{}", `[null]`, `[{"number":3,"state":"closed"},{"number":3,"state":"closed"}]`,
		`[{"number":3,"state":"unknown"}]`, `[{"number":3,"state":"closed","html_url":"https://secret@example.test/pull/3"}]`,
	} {
		body = invalid
		if _, err := svc.ListProviderPullRequests(ctx, repo.FullName, "all", 1, 50); !errors.Is(err, ErrProviderInventoryUnavailable) {
			t.Fatalf("invalid provider observation accepted: %v", err)
		}
	}
	body = `[{"number":3,"state":"closed"}]`
	if _, err := svc.ListProviderPullRequests(ctx, repo.FullName, "open", 1, 50); !errors.Is(err, ErrProviderInventoryUnavailable) {
		t.Fatalf("ignored state filter accepted: %v", err)
	}
	if _, err := svc.ListProviderPullRequests(ctx, repo.FullName, "all", ProviderInventoryMaxPage, 50); !errors.Is(err, ErrProviderInventoryUnavailable) {
		t.Fatalf("exhausted bound must fail, not claim completion: %v", err)
	}
	body = `{"number":11,"state":"open"}`
	if _, err := svc.ReadProviderPullRequest(ctx, repo.FullName, 3); !errors.Is(err, ErrProviderInventoryUnavailable) {
		t.Fatalf("wrong exact number accepted: %v", err)
	}
	status = http.StatusServiceUnavailable
	body = "provider credentials must not be reflected"
	if _, err := svc.ListProviderPullRequests(ctx, repo.FullName, "all", 1, 50); !errors.Is(err, ErrProviderInventoryUnavailable) {
		t.Fatalf("provider failure accepted: %v", err)
	}
}

func TestProviderInventoryDoesNotBackfillHumanAgentGrants(t *testing.T) {
	calls := 0
	svc, _, repo := newProviderInventoryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte("[]"))
	})
	if err := svc.DB.AutoMigrate(&db.AgentBinding{}); err != nil {
		t.Fatal(err)
	}
	human := db.User{Login: "bound-human", Type: db.TypeUser, UserKind: db.UserKindHuman}
	agent := db.User{Login: "bound-agent", Type: db.TypeUser, UserKind: db.UserKindAgent}
	for _, user := range []*db.User{&human, &agent} {
		if err := svc.DB.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.DB.Model(&repo).Update("owner_id", agent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Create(&db.AgentBinding{HumanUserID: human.ID, AgentUserID: agent.ID}).Error; err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithUser(t.Context(), human)
	if _, err := svc.ListProviderPullRequests(ctx, repo.FullName, "all", 1, 50); !errors.Is(err, ErrNotFound) || calls != 0 {
		t.Fatalf("legacy binding silently gained native authority: %v calls=%d", err, calls)
	}
	var grants int64
	if err := svc.DB.Model(&db.Collaborator{}).Count(&grants).Error; err != nil || grants != 0 {
		t.Fatalf("provider observation created a grant: %d %v", grants, err)
	}
}
