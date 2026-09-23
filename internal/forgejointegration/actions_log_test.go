package forgejointegration

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/ngaut/agent-git-service/internal/providerlogbridge"
	"github.com/ngaut/agent-git-service/internal/providerlogprotocol"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestActionsLogCandidatesUseForgejoShardLayout(t *testing.T) {
	dir := t.TempDir()
	got, err := actionsLogCandidates(dir, "operator", "project-kit", 6401)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.HasSuffix(got[0], filepath.Join("operator", "project-kit", "01", "6401.log.zst")) {
		t.Fatalf("6401 candidates=%v", got)
	}
	got, err = actionsLogCandidates(dir, "operator", "agent-git-service", 1017)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got[0], filepath.Join("operator", "agent-git-service", "f9", "1017.log.zst")) {
		t.Fatalf("1017 candidates=%v", got)
	}
}

func TestReadLocalActionsLogPrefersUncompressedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "operator", "project-kit", "01", "6401.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("job failed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs, found, err := readLocalActionsLog(dir, "operator", "project-kit", 6401)
	if err != nil || !found || string(logs) != "job failed\n" {
		t.Fatalf("logs=%q found=%t err=%v", logs, found, err)
	}
	if _, found, err := readLocalActionsLog(dir, "operator", "project-kit", 6400); err != nil || found {
		t.Fatalf("missing run unexpectedly found err=%v", err)
	}
}

func TestActionsLogCandidatesRejectTraversal(t *testing.T) {
	if _, err := actionsLogCandidates(t.TempDir(), "../etc", "project-kit", 1); err == nil {
		t.Fatal("expected owner traversal to fail")
	}
}

func TestWorkflowRunLogsReadsAuthenticatedProviderHostBridge(t *testing.T) {
	head := "75029c1d787a16dacfec1d1e734e730a881ff9cd"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/provider-logs/repos/forgejo/demo-ci/tasks/6949" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer bridge-secret" {
			t.Fatalf("authorization missing")
		}
		if r.URL.Query().Get("provider_pr") != "16" || r.URL.Query().Get("head_ref") != "feature/work" || r.URL.Query().Get("head_sha") != head {
			t.Fatalf("binding query=%q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema":"ags.internal-provider-log.v1","repo":"forgejo/demo-ci","task_id":6949,"run_number":1194,"job_name":"Backend Smoke","head_sha":"` + head + `","provider_pr":16,"provider_ref":"refs/pull/16/head","event":"pull_request","text":"checkout git fetch timed out after 15m\n"}`))
	}))
	defer server.Close()
	client := &fakeClient{
		runs:               []WorkflowRun{{ID: 6949, Name: "Backend Smoke", Status: "failure", HeadBranch: "#16", HeadSHA: head}},
		workflowRunListErr: errors.New("Forgejo workflow run API must not be called before the configured bridge"),
	}
	integration := New(Config{
		Enabled: true, ActionsLogBridgeURL: server.URL, ActionsLogBridgeToken: "bridge-secret",
		RepoMap: map[string]RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci"}},
	}, client, nil)
	logs, supported, err := integration.WorkflowRunLogs(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "feature/work", head, 6949)
	if err != nil || !supported || string(logs) != "checkout git fetch timed out after 15m\n" {
		t.Fatalf("WorkflowRunLogs=%q supported=%t err=%v", logs, supported, err)
	}
	if client.workflowRunListCalls != 0 {
		t.Fatalf("workflow run API calls=%d, want bridge-primary zero", client.workflowRunListCalls)
	}
}

func TestWorkflowRunLogsRoundTripsNearMaximumControlTextThroughRealBridge(t *testing.T) {
	head := "75029c1d787a16dacfec1d1e734e730a881ff9cd"
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/forgejo.db"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
		`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
		`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text)`,
		`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text, log_filename text, log_in_storage boolean, log_expired boolean)`,
		`INSERT INTO repository VALUES (24, 'forgejo', 'demo-ci')`,
		`INSERT INTO action_run VALUES (4001, 24, 1194, 'refs/pull/16/head', '` + head + `', 'pull_request')`,
		`INSERT INTO action_run_job VALUES (6763, 4001, 24, 6949, 'Backend Smoke', '` + head + `')`,
		`INSERT INTO action_task VALUES (6949, 24, '` + head + `', 'forgejo/demo-ci/25/6949.log', true, false)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	root := t.TempDir()
	path := filepath.Join(root, "forgejo", "demo-ci", "25", "6949.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	pattern := []byte{'\x00', '\x01', '\x02', '\n'}
	text := bytes.Repeat(pattern, int((providerlogprotocol.DecodedTextMaxBytes-1024)/int64(len(pattern))))
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
	handler, err := providerlogbridge.New(providerlogbridge.Config{
		DB: db, ActionsLogDir: root, ServiceToken: "bridge-secret", AllowedCIDRs: []string{"127.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Use(providerlogbridge.CaptureSocketPeer)
	router.Use(chimiddleware.RealIP)
	router.Get("/api/internal/provider-logs/repos/{owner}/{repo}/tasks/{task}", handler.ServeHTTP)
	server := httptest.NewServer(router)
	defer server.Close()
	client := &fakeClient{workflowRunListErr: errors.New("provider fallback must not run")}
	integration := New(Config{
		Enabled: true, ActionsLogBridgeURL: server.URL, ActionsLogBridgeToken: "bridge-secret",
		RepoMap: map[string]RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci"}},
	}, client, nil)
	got, supported, err := integration.WorkflowRunLogs(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "feature/work", head, 6949)
	if err != nil || !supported || !bytes.Equal(got, text) {
		t.Fatalf("roundtrip bytes=%d want=%d supported=%t err=%v", len(got), len(text), supported, err)
	}
	if client.workflowRunListCalls != 0 {
		t.Fatalf("workflow run API calls=%d, want zero", client.workflowRunListCalls)
	}
}

func TestWorkflowRunLogsRejectsInvalidBridgeEvidenceWithoutProviderFallback(t *testing.T) {
	head := "75029c1d787a16dacfec1d1e734e730a881ff9cd"
	valid := `{"schema":"ags.internal-provider-log.v1","repo":"forgejo/demo-ci","task_id":6949,"run_number":1194,"job_name":"Backend Smoke","head_sha":"` + head + `","provider_pr":16,"provider_ref":"refs/pull/16/head","event":"pull_request","text":"masked log"}`
	tests := map[string]struct {
		status int
		body   string
	}{
		"non_2xx":      {status: http.StatusBadGateway, body: `{"schema":"ags.internal-provider-log-error.v1","code":"database_unavailable","message":"Provider log bridge is unavailable"}`},
		"invalid_json": {status: http.StatusOK, body: `{`},
		"repo":         {status: http.StatusOK, body: strings.Replace(valid, `"repo":"forgejo/demo-ci"`, `"repo":"other/demo-ci"`, 1)},
		"task":         {status: http.StatusOK, body: strings.Replace(valid, `"task_id":6949`, `"task_id":6950`, 1)},
		"sha":          {status: http.StatusOK, body: strings.Replace(valid, head, strings.Repeat("a", 40), 1)},
		"provider_pr":  {status: http.StatusOK, body: strings.Replace(valid, `"provider_pr":16`, `"provider_pr":17`, 1)},
		"provider_ref": {status: http.StatusOK, body: strings.Replace(valid, `"provider_ref":"refs/pull/16/head"`, `"provider_ref":"refs/pull/17/head"`, 1)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := &fakeClient{workflowRunListErr: errors.New("provider fallback must not run")}
			integration := New(Config{
				Enabled: true, ActionsLogBridgeURL: server.URL, ActionsLogBridgeToken: "bridge-secret",
				RepoMap: map[string]RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci"}},
			}, client, nil)
			_, supported, err := integration.WorkflowRunLogs(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "feature/work", head, 6949)
			var typed *actionsLogBridgeError
			if !supported || err == nil || !errors.As(err, &typed) {
				t.Fatalf("supported=%t err=%T %v", supported, err, err)
			}
			if client.workflowRunListCalls != 0 {
				t.Fatalf("workflow run API calls=%d, want zero", client.workflowRunListCalls)
			}
		})
	}
}

func TestWorkflowRunLogsReadsColocatedFileBeforeHTTP(t *testing.T) {
	head := "75029c1d787a16dacfec1d1e734e730a881ff9cd"
	dir := t.TempDir()
	path := filepath.Join(dir, "forgejo", "demo-ci", "02", "2.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("file log"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{
		runs:    []WorkflowRun{{ID: 2, Name: "build", Status: "failure", HeadBranch: "#16", HeadSHA: head}},
		runLogs: map[int64][]byte{2: []byte("http log")},
	}
	integration := New(Config{
		Enabled:       true,
		ActionsLogDir: dir,
		RepoMap:       map[string]RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci"}},
	}, client, nil)
	logs, supported, err := integration.WorkflowRunLogs(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "feature/work", head, 2)
	if err != nil || !supported || string(logs) != "file log" {
		t.Fatalf("WorkflowRunLogs=%q supported=%t err=%v", logs, supported, err)
	}
}
