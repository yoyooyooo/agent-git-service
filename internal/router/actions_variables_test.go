package router_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

// This fixture uses the production router, real isolated TiDB storage, and
// Forgejo 12's list/detail protocol. List data is deliberately not a value.
type variablesHTTPFixture struct {
	svc        *service.Service
	handler    http.Handler
	repo       db.Repository
	nativeRepo db.Repository
	org        db.User
	owner      db.User
	provider   *httptest.Server
	config     cibackend.Config
	names      []string
	values     map[string]string
	calls      atomic.Int64
	mode       atomic.Value
}

func newVariablesHTTPFixture(t *testing.T) *variablesHTTPFixture {
	t.Helper()
	svc, handler := setupRouterTest(t)
	f := &variablesHTTPFixture{
		svc: svc, handler: handler, values: map[string]string{"EMPTY": ""},
		names: []string{"EMPTY"},
	}
	f.mode.Store("")
	for i := 0; i < 165; i++ {
		name := fmt.Sprintf("VAR_%03d", i)
		f.names = append(f.names, name)
		f.values[name] = fmt.Sprintf("provider-value-%03d", i)
	}
	f.owner = db.User{Login: "variable-owner", Type: db.TypeUser}
	reader := db.User{Login: "variable-reader", Type: db.TypeUser}
	writer := db.User{Login: "variable-writer", Type: db.TypeUser}
	other := db.User{Login: "other-owner", Type: db.TypeUser}
	f.org = db.User{Login: "variable-org", Type: db.TypeOrganization}
	for _, user := range []*db.User{&f.owner, &reader, &writer, &other, &f.org} {
		if err := svc.DB.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, token := range []db.Token{
		{UserID: f.owner.ID, Value: "variables-owner-token"},
		{UserID: reader.ID, Value: "variables-reader-token"},
		{UserID: writer.ID, Value: "variables-writer-token"},
	} {
		if err := svc.DB.Create(&token).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.repo = db.Repository{OwnerID: f.owner.ID, Name: "public-project", FullName: "variable-owner/public-project", DefaultBranch: "main"}
	f.nativeRepo = db.Repository{OwnerID: f.owner.ID, Name: "native-project", FullName: "variable-owner/native-project", DefaultBranch: "main"}
	privateRepo := db.Repository{OwnerID: other.ID, Name: "private-project", FullName: "other-owner/private-project", DefaultBranch: "main", Private: true, Visibility: "private"}
	for _, repo := range []*db.Repository{&f.repo, &f.nativeRepo, &privateRepo} {
		if err := svc.DB.Create(repo).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.DB.Create(&db.Collaborator{RepositoryID: f.repo.ID, UserID: writer.ID, Permission: "write"}).Error; err != nil {
		t.Fatal(err)
	}
	// Deliberately conflicting local rows catch fallback and wrong-scope reads.
	for _, variable := range []db.Variable{
		{OwnerID: f.owner.ID, RepositoryID: &f.repo.ID, Name: "VAR_000", Value: "native-must-not-be-returned"},
		{OwnerID: f.owner.ID, RepositoryID: &f.repo.ID, Environment: "production", Name: "NATIVE_SENTINEL", Value: "native-env-sentinel"},
		{OwnerID: f.owner.ID, RepositoryID: &f.nativeRepo.ID, Name: "NATIVE_ONLY", Value: "native-value"},
		{OwnerID: f.owner.ID, RepositoryID: &f.nativeRepo.ID, Environment: "production", Name: "NATIVE_ENV", Value: "native-env-value"},
		{OwnerID: f.org.ID, Name: "ORG_ONLY", Value: "native-org-value"},
	} {
		if err := svc.DB.Create(&variable).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.provider = httptest.NewServer(http.HandlerFunc(f.serveProvider))
	t.Cleanup(f.provider.Close)
	tokenFile := filepath.Join(t.TempDir(), "provider-token")
	if err := os.WriteFile(tokenFile, []byte("synthetic-provider-token"), 0600); err != nil {
		t.Fatal(err)
	}
	f.config = cibackend.Config{
		Default: "native",
		Backends: map[string]cibackend.BackendConfig{
			"forgejo": {Kind: "forgejo", URL: f.provider.URL, TokenFile: tokenFile, AllowHTTP: true},
		},
		Repositories: map[string]cibackend.Binding{
			f.repo.FullName: {Backend: "forgejo", Repository: "ci/project"},
		},
	}
	f.installRegistry(t, f.config)
	svc.BaseURL = "https://ags.example.test"
	return f
}

func (f *variablesHTTPFixture) installRegistry(t *testing.T, config cibackend.Config) {
	t.Helper()
	registry, err := cibackend.New(config, "")
	if err != nil {
		t.Fatal(err)
	}
	f.svc.CI = registry
}

func (f *variablesHTTPFixture) serveProvider(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet || r.Header.Get("Authorization") != "token synthetic-provider-token" {
		http.Error(w, "fixture expected authenticated read", http.StatusForbidden)
		return
	}
	mode := f.mode.Load().(string)
	if mode == "provider-forbidden" {
		http.Error(w, "synthetic-provider-error-body", http.StatusForbidden)
		return
	}
	if mode == "provider-unauthorized" {
		http.Error(w, "synthetic-provider-error-body", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/api/v1/repos/ci/project" {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "full_name": "ci/project"})
		return
	}
	if r.URL.Path == "/api/v1/settings/api" {
		_ = json.NewEncoder(w).Encode(map[string]int{"max_response_items": 50, "default_paging_num": 30})
		return
	}
	const prefix = "/api/v1/repos/ci/project/actions/variables"
	if r.URL.Path == prefix {
		page, errPage := strconv.Atoi(r.URL.Query().Get("page"))
		limit, errLimit := strconv.Atoi(r.URL.Query().Get("limit"))
		if errPage != nil || errLimit != nil || page < 1 || limit < 1 {
			http.Error(w, "explicit valid pagination required", http.StatusBadRequest)
			return
		}
		if limit > 50 {
			limit = 50
		}
		total := len(f.names)
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		if mode == "invalid-total" {
			w.Header().Set("X-Total-Count", "invalid")
		}
		start := (page - 1) * limit
		if start > total {
			start = total
		}
		end := start + limit
		if end > total {
			end = total
		}
		rows := make([]map[string]any, 0, end-start)
		for _, name := range f.names[start:end] {
			rows = append(rows, map[string]any{"name": name, "data": "", "owner_id": 0, "repo_id": 42})
		}
		_ = json.NewEncoder(w).Encode(rows)
		return
	}
	if strings.HasPrefix(r.URL.Path, prefix+"/") {
		name := strings.ToUpper(strings.TrimPrefix(r.URL.Path, prefix+"/"))
		value, ok := f.values[name]
		if !ok || (mode == "deleted-detail" && name == "VAR_001") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "synthetic missing variable"})
			return
		}
		row := map[string]any{"name": name, "data": value, "owner_id": 0, "repo_id": 42}
		if mode == "missing-data" {
			delete(row, "data")
		}
		if mode == "wrong-scope" {
			row["repo_id"] = 99
		}
		if mode == "null-data" {
			row["data"] = nil
		}
		_ = json.NewEncoder(w).Encode(row)
		return
	}
	http.Error(w, "unexpected provider path", http.StatusNotFound)
}

func (f *variablesHTTPFixture) request(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, f.svc.BaseURL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	result := httptest.NewRecorder()
	f.handler.ServeHTTP(result, req)
	return result
}

func TestActionsVariablesForgejoHTTP(t *testing.T) {
	f := newVariablesHTTPFixture(t)
	base := "/api/v3/repos/" + f.repo.FullName + "/actions/variables"
	const owner = "variables-owner-token"
	t.Run("detail uses binding and real value", func(t *testing.T) {
		response := f.request(t, http.MethodGet, base+"/var_000", owner, "")
		if response.Code != http.StatusOK {
			t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
		}
		var variable map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &variable); err != nil {
			t.Fatal(err)
		}
		if variable["name"] != "VAR_000" || variable["value"] != f.values["VAR_000"] {
			t.Fatalf("detail did not use provider value: %#v", variable)
		}
		if _, present := variable["created_at"]; present {
			t.Fatal("provider has no creation date; one was fabricated")
		}
		if _, present := variable["updated_at"]; present {
			t.Fatal("provider has no update date; one was fabricated")
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("variable response may be cached")
		}
	})
	t.Run("real empty differs from missing", func(t *testing.T) {
		response := f.request(t, http.MethodGet, base+"/EMPTY", owner, "")
		var variable map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &variable); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || variable["value"] != "" {
			t.Fatalf("real empty variable response=%d %s", response.Code, response.Body.String())
		}
		missing := f.request(t, http.MethodGet, base+"/NOT_DEFINED", owner, "")
		if missing.Code != http.StatusNotFound {
			t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
		}
	})
	t.Run("pagination windows and hydration", func(t *testing.T) {
		for _, page := range []struct{ page, size, start, end int }{
			{1, 100, 0, 100}, {2, 75, 75, 150}, {3, 75, 150, 166}, {20, 100, 166, 166},
		} {
			path := fmt.Sprintf("%s?page=%d&per_page=%d", base, page.page, page.size)
			response := f.request(t, http.MethodGet, path, owner, "")
			if response.Code != http.StatusOK {
				t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
			}
			var result struct {
				Total     int                            `json:"total_count"`
				Variables []struct{ Name, Value string } `json:"variables"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Total != len(f.names) || len(result.Variables) != page.end-page.start {
				t.Fatalf("page=%d size=%d total=%d rows=%d", page.page, page.size, result.Total, len(result.Variables))
			}
			for i, variable := range result.Variables {
				want := f.names[page.start+i]
				if variable.Name != want || variable.Value != f.values[want] {
					t.Fatalf("page=%d row=%d got=%#v want=%s", page.page, i, variable, want)
				}
			}
			link := response.Header().Get("Link")
			if page.end < len(f.names) && (!strings.Contains(link, "https://ags.example.test/api/v3/") || !strings.Contains(link, `rel="next"`)) {
				t.Fatalf("missing AGS next link: %q", link)
			}
			if strings.Contains(link, f.provider.URL) {
				t.Fatal("provider address leaked into Link")
			}
		}
	})
	t.Run("public readers and writer do not inherit provider privilege", func(t *testing.T) {
		for _, token := range []string{"variables-reader-token", "variables-writer-token"} {
			before := f.calls.Load()
			for _, path := range []string{base, base + "/VAR_000"} {
				response := f.request(t, http.MethodGet, path, token, "")
				if response.Code != http.StatusNotFound {
					t.Fatalf("unauthorized caller status=%d body=%s", response.Code, response.Body.String())
				}
			}
			if f.calls.Load() != before {
				t.Fatal("unauthorized caller reached provider")
			}
		}
	})
	t.Run("pagination links remain usable beyond page one thousand", func(t *testing.T) {
		originalNames, originalValues := f.names, f.values
		defer func() { f.names, f.values = originalNames, originalValues }()
		f.names = append([]string(nil), originalNames...)
		f.values = make(map[string]string, 1001)
		for name, value := range originalValues {
			f.values[name] = value
		}
		for i := 0; len(f.names) < 1001; i++ {
			name := fmt.Sprintf("ZZZ_%03d", i)
			f.names = append(f.names, name)
			f.values[name] = "provider-" + name
		}
		response := f.request(t, http.MethodGet, base+"?page=1000&per_page=1", owner, "")
		if response.Code != http.StatusOK {
			t.Fatalf("page 1000 status=%d body=%s", response.Code, response.Body.String())
		}
		var next string
		for _, part := range strings.Split(response.Header().Get("Link"), ",") {
			if strings.Contains(part, `rel="next"`) {
				next = strings.Trim(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]), "<>")
			}
		}
		link, err := url.Parse(next)
		if err != nil || link.Host != "ags.example.test" || link.Path != base || link.Query().Get("page") != "1001" {
			t.Fatalf("page 1000 did not publish an AGS page 1001 link: %q", next)
		}
		response = f.request(t, http.MethodGet, link.RequestURI(), owner, "")
		var result struct {
			Total     int                            `json:"total_count"`
			Variables []struct{ Name, Value string } `json:"variables"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || result.Total != 1001 || len(result.Variables) != 1 || result.Variables[0].Name != f.names[1000] || result.Variables[0].Value != f.values[f.names[1000]] {
			t.Fatalf("published next page could not be followed: status=%d body=%s", response.Code, response.Body.String())
		}
	})
	t.Run("cross repository and anonymous are refused before provider", func(t *testing.T) {
		before := f.calls.Load()
		response := f.request(t, http.MethodGet, "/api/v3/repos/other-owner/private-project/actions/variables/VAR_000", owner, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("cross repository response=%d", response.Code)
		}
		response = f.request(t, http.MethodGet, base, "", "")
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous response=%d", response.Code)
		}
		if f.calls.Load() != before {
			t.Fatal("unauthorized request reached provider")
		}
	})
	t.Run("provider failures never return native data or partial success", func(t *testing.T) {
		for _, mode := range []string{"provider-forbidden", "provider-unauthorized", "missing-data", "null-data", "wrong-scope", "invalid-total", "deleted-detail"} {
			t.Run(mode, func(t *testing.T) {
				f.mode.Store(mode)
				defer f.mode.Store("")
				path := base + "?per_page=5"
				response := f.request(t, http.MethodGet, path, owner, "")
				if response.Code != http.StatusBadGateway {
					t.Fatalf("failure became status=%d body=%s", response.Code, response.Body.String())
				}
				for _, forbidden := range []string{"provider-value", "native-must-not", "synthetic-provider-error-body", "synthetic-provider-token"} {
					if strings.Contains(response.Body.String(), forbidden) {
						t.Fatalf("unsafe error response contains %q", forbidden)
					}
				}
			})
		}
	})
	t.Run("direct service requires an authenticated administrator", func(t *testing.T) {
		var reader db.User
		if err := f.svc.DB.Where("login = ?", "variable-reader").First(&reader).Error; err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			ctx  context.Context
			want error
		}{
			{context.Background(), service.ErrUnauthorized},
			{service.ContextWithUser(context.Background(), db.User{}), service.ErrUnauthorized},
			{service.ContextWithUser(context.Background(), reader), service.ErrNotFound},
		} {
			before := f.calls.Load()
			if _, err := f.svc.CIRepositoryVariables(tc.ctx, f.repo.FullName, 1, 30); !errors.Is(err, tc.want) {
				t.Fatalf("direct list bypassed caller permission: got %v, want %v", err, tc.want)
			}
			if _, err := f.svc.CIRepositoryVariable(tc.ctx, f.repo.FullName, "VAR_000"); !errors.Is(err, tc.want) {
				t.Fatalf("direct get bypassed caller permission: got %v, want %v", err, tc.want)
			}
			if f.calls.Load() != before {
				t.Fatal("denied direct service caller reached provider")
			}
		}
	})
	t.Run("external writes remain unsupported", func(t *testing.T) {
		for _, tc := range []struct{ method, suffix, body string }{
			{http.MethodPost, "", `{"name":"NEW","value":"synthetic-write"}`},
			{http.MethodPatch, "/VAR_000", `{"value":"synthetic-write"}`},
			{http.MethodDelete, "/VAR_000", ""},
		} {
			response := f.request(t, tc.method, base+tc.suffix, owner, tc.body)
			if response.Code != http.StatusNotImplemented {
				t.Fatalf("%s returned %d: %s", tc.method, response.Code, response.Body.String())
			}
		}
	})
}

func TestActionsVariablesRejectWrongNativeScope(t *testing.T) {
	f := newVariablesHTTPFixture(t)
	const owner = "variables-owner-token"
	var before int64
	if err := f.svc.DB.Model(&db.Variable{}).Count(&before).Error; err != nil {
		t.Fatal(err)
	}
	bases := []string{
		"/api/v3/repos/" + f.repo.FullName + "/environments/production/variables",
		fmt.Sprintf("/api/v3/repositories/%d/environments/production/variables", f.repo.ID),
		"/api/v3/orgs/" + f.org.Login + "/actions/variables",
	}
	for _, base := range bases {
		for _, tc := range []struct{ method, suffix, body string }{
			{http.MethodGet, "", ""},
			{http.MethodGet, "/NATIVE_SENTINEL", ""},
			{http.MethodPost, "", `{"name":"MUST_NOT_WRITE","value":"synthetic-write"}`},
			{http.MethodPatch, "/NATIVE_SENTINEL", `{"value":"synthetic-write"}`},
			{http.MethodDelete, "/NATIVE_SENTINEL", ""},
		} {
			response := f.request(t, tc.method, base+tc.suffix, owner, tc.body)
			if response.Code != http.StatusNotImplemented {
				t.Errorf("%s %s response=%d %s", tc.method, base+tc.suffix, response.Code, response.Body.String())
			}
		}
	}
	var after int64
	if err := f.svc.DB.Model(&db.Variable{}).Count(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after != before || f.calls.Load() != 0 {
		t.Fatalf("unsupported scope touched storage/provider: before=%d after=%d calls=%d", before, after, f.calls.Load())
	}
	ctx := service.ContextWithUser(context.Background(), f.owner)
	if _, err := f.svc.ListVariables(ctx, f.repo.ID, ""); !errors.Is(err, cibackend.ErrUnsupported) {
		t.Fatalf("direct native list bypassed backend selection: %v", err)
	}
	if _, err := f.svc.GetVariable(ctx, f.repo.ID, "production", "NATIVE_SENTINEL"); !errors.Is(err, cibackend.ErrUnsupported) {
		t.Fatalf("direct native get bypassed backend selection: %v", err)
	}
	if _, err := f.svc.CreateVariable(ctx, f.owner.ID, &f.repo.ID, "", "MUST_NOT_WRITE", "synthetic-write"); !errors.Is(err, cibackend.ErrUnsupported) {
		t.Fatalf("direct native create bypassed backend selection: %v", err)
	}
	if _, err := f.svc.ListOrgVariables(ctx, f.org.ID); !errors.Is(err, cibackend.ErrUnsupported) {
		t.Fatalf("direct org list bypassed source guard: %v", err)
	}

	// A native repo remains usable even while another repo selects Forgejo.
	nativeBase := "/api/v3/repos/" + f.nativeRepo.FullName
	for _, suffix := range []string{"/actions/variables/NATIVE_ONLY", "/environments/production/variables/NATIVE_ENV"} {
		response := f.request(t, http.MethodGet, nativeBase+suffix, "variables-reader-token", "")
		if response.Code != http.StatusOK {
			t.Fatalf("existing native read permission changed: %d %s", response.Code, response.Body.String())
		}
	}
	// Declaring an unused provider must not make a purely native registry mixed.
	pureNative := f.config
	pureNative.Repositories = nil
	f.installRegistry(t, pureNative)
	response := f.request(t, http.MethodGet, "/api/v3/orgs/"+f.org.Login+"/actions/variables/ORG_ONLY", "test-token", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "native-org-value") {
		t.Fatalf("pure native org path changed: %d %s", response.Code, response.Body.String())
	}
}

// Run explicitly with AGS_VARIABLES_GH_BINARY pointing to an unmodified official
// gh. The temporary Unix socket transport prevents any request leaving this
// fixture; no workstation host configuration, credentials, or production DB is
// inherited. A missing binary is a skip, never evidence of CLI compatibility.
func TestActionsVariablesOfficialGH(t *testing.T) {
	binary := os.Getenv("AGS_VARIABLES_GH_BINARY")
	if binary == "" {
		t.Skip("set AGS_VARIABLES_GH_BINARY to run real official gh acceptance")
	}
	resolved, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	f := newVariablesHTTPFixture(t)
	f.svc.BaseURL = "https://ags.variables.example.test"
	// Keep the socket name below the macOS sockaddr_un path limit.
	socketDir, err := os.MkdirTemp("/tmp", "ags-vars-gh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "http.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: f.handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte("http_unix_socket: "+socketPath+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "hosts.yml"), []byte("ags.variables.example.test:\n    user: variable-owner\n    oauth_token: variables-owner-token\n    git_protocol: https\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, resolved, args...)
		command.Dir = configDir
		command.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + configDir,
			"GH_CONFIG_DIR=" + configDir,
			"GH_HOST=ags.variables.example.test",
			"GH_PROMPT_DISABLED=1",
			"GH_NO_UPDATE_NOTIFIER=1",
			"GH_NO_EXTENSION_UPDATE_NOTIFIER=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"NO_COLOR=1",
		}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("official gh %v failed: %v\n%s", args, err, output)
		}
		return output
	}
	t.Logf("client: %s", strings.TrimSpace(string(run("--version"))))
	var single struct{ Name, Value string }
	if err := json.Unmarshal(run("variable", "get", "VAR_000", "-R", f.repo.FullName, "--json", "name,value"), &single); err != nil {
		t.Fatal(err)
	}
	if single.Name != "VAR_000" || single.Value != f.values["VAR_000"] {
		t.Fatal("official gh single get did not receive the provider value")
	}
	var listed []struct{ Name, Value string }
	if err := json.Unmarshal(run("variable", "list", "-R", f.repo.FullName, "--json", "name,value"), &listed); err != nil {
		t.Fatal(err)
	}
	actual := make(map[string]string, len(listed))
	for _, variable := range listed {
		if _, duplicate := actual[variable.Name]; duplicate {
			t.Fatalf("official gh listed duplicate %s", variable.Name)
		}
		actual[variable.Name] = variable.Value
	}
	if !reflect.DeepEqual(actual, f.values) {
		t.Fatalf("official gh list incomplete or wrong: got %d, want %d", len(actual), len(f.values))
	}
	path := "repos/" + f.repo.FullName + "/actions/variables?per_page=100"
	names := strings.Fields(string(run("api", "--paginate", path, "--jq", ".variables[].name")))
	if !reflect.DeepEqual(names, f.names) {
		t.Fatalf("official gh API pagination lost records: got %d, want %d", len(names), len(f.names))
	}
	empty := string(run("variable", "get", "EMPTY", "-R", f.repo.FullName))
	if strings.TrimSpace(empty) != "" {
		t.Fatal("official gh empty variable was changed")
	}
}
