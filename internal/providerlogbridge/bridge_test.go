package providerlogbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/ngaut/agent-git-service/internal/providerlogprotocol"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const (
	fixtureOwner = "example-team"
	fixtureRepo  = "pipeline-fixture"
	fixtureSHA   = "1111111111111111111111111111111111111111"
	fixtureRef   = "fix/log-fixture"
	fixturePeer  = "192.0.2.10:43100"
)

func setupBridge(t *testing.T, maxBytes int64) (*Handler, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/forgejo.db"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE repository (id integer primary key, owner_name text, lower_name text)`,
		`CREATE TABLE action_run (id integer primary key, repo_id integer, "index" integer, ref text, commit_sha text, event text)`,
		`CREATE TABLE action_run_job (id integer primary key, run_id integer, repo_id integer, task_id integer, name text, commit_sha text)`,
		`CREATE TABLE action_task (id integer primary key, repo_id integer, commit_sha text, log_filename text, log_in_storage boolean, log_expired boolean)`,
		`INSERT INTO repository VALUES (24, 'example-team', 'pipeline-fixture')`,
		`INSERT INTO action_run VALUES (4001, 24, 1194, 'refs/pull/98/head', '` + fixtureSHA + `', 'pull_request')`,
		`INSERT INTO action_run_job VALUES (6763, 4001, 24, 6949, 'Backend Smoke', '` + fixtureSHA + `')`,
		`INSERT INTO action_task VALUES (6949, 24, '` + fixtureSHA + `', 'example-team/pipeline-fixture/25/6949.log', true, false)`,
		`INSERT INTO action_run VALUES (4002, 24, 1195, 'refs/pull/99/head', '` + fixtureSHA + `', 'pull_request')`,
		`INSERT INTO action_run_job VALUES (6764, 4002, 24, 6950, 'Other PR', '` + fixtureSHA + `')`,
		`INSERT INTO action_task VALUES (6950, 24, '` + fixtureSHA + `', 'example-team/pipeline-fixture/25/6950.log', true, false)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	root := t.TempDir()
	path := filepath.Join(root, fixtureOwner, fixtureRepo, "25", "6949.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("checkout git fetch timed out after 15m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{DB: db, ActionsLogDir: root, ServiceToken: "bridge-secret", MaxBytes: maxBytes, AllowedCIDRs: []string{"192.0.2.0/24", "127.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	return h, root
}

func request(t *testing.T, h *Handler, owner, repo, task string, token string) *httptest.ResponseRecorder {
	t.Helper()
	return requestBinding(t, h, owner, repo, task, token, "98", fixtureRef, fixtureSHA)
}

func requestBinding(t *testing.T, h *Handler, owner, repo, task, token, providerPR, headRef, headSHA string) *httptest.ResponseRecorder {
	t.Helper()
	return requestBindingContext(t, t.Context(), h, owner, repo, task, token, providerPR, headRef, headSHA)
}

func requestBindingContext(t *testing.T, ctx context.Context, h *Handler, owner, repo, task, token, providerPR, headRef, headSHA string) *httptest.ResponseRecorder {
	t.Helper()
	return requestBindingContextWithPeer(t, ctx, h, owner, repo, task, token, providerPR, headRef, headSHA, fixturePeer, nil)
}

func requestBindingContextWithPeer(t *testing.T, ctx context.Context, h *Handler, owner, repo, task, token, providerPR, headRef, headSHA, remoteAddr string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(CaptureSocketPeer)
	r.Use(chimiddleware.RealIP)
	r.Get("/api/internal/provider-logs/repos/{owner}/{repo}/tasks/{task}", func(w http.ResponseWriter, req *http.Request) {
		if chi.URLParam(req, "owner") == "" || chi.URLParam(req, "repo") == "" || chi.URLParam(req, "task") == "" {
			t.Fatalf("missing chi params owner=%q repo=%q task=%q", chi.URLParam(req, "owner"), chi.URLParam(req, "repo"), chi.URLParam(req, "task"))
		}
		h.ServeHTTP(w, req)
	})
	query := url.Values{"provider_pr": {providerPR}, "head_ref": {headRef}, "head_sha": {headSHA}}
	req := httptest.NewRequest(http.MethodGet, "/api/internal/provider-logs/repos/"+owner+"/"+repo+"/tasks/"+task+"?"+query.Encode(), nil).WithContext(ctx)
	req.RemoteAddr = remoteAddr
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandlerSocketPeerCaptureCannotBeForgedByRealIPHeaders(t *testing.T) {
	h, _ := setupBridge(t, 1024)
	for _, header := range []string{"True-Client-IP", "X-Real-IP", "X-Forwarded-For"} {
		t.Run("disallowed_socket_"+header, func(t *testing.T) {
			got := requestBindingContextWithPeer(t, t.Context(), h, fixtureOwner, fixtureRepo, "6949", "bridge-secret", "98", fixtureRef, fixtureSHA, "203.0.113.10:43100", http.Header{header: {"192.0.2.10"}})
			if got.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
		})
		t.Run("allowed_socket_"+header, func(t *testing.T) {
			got := requestBindingContextWithPeer(t, t.Context(), h, fixtureOwner, fixtureRepo, "6949", "bridge-secret", "98", fixtureRef, fixtureSHA, fixturePeer, http.Header{header: {"203.0.113.10"}})
			if got.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
}

func TestHandlerRejectsMissingSocketPeerCapture(t *testing.T) {
	h, _ := setupBridge(t, 1024)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = fixturePeer
	req.Header.Set("Authorization", "Bearer bridge-secret")
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestHandlerReturnsExactDatabaseBoundTaskLog(t *testing.T) {
	h, _ := setupBridge(t, 1024)
	if binding, status, code, err := h.binding(t.Context(), fixtureOwner, fixtureRepo, 6949); err != nil {
		t.Fatalf("binding status=%d code=%s binding=%#v err=%v", status, code, binding, err)
	}
	w := request(t, h, fixtureOwner, fixtureRepo, "6949", "bridge-secret")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, expected := range []string{
		`"schema":"ags.internal-provider-log.v1"`, `"run_number":1194`, `"provider_pr":98`,
		`"provider_ref":"refs/pull/98/head"`, `"event":"pull_request"`, "checkout git fetch timed out after 15m",
	} {
		if !strings.Contains(w.Body.String(), expected) {
			t.Fatalf("body missing %q: %s", expected, w.Body.String())
		}
	}
}

func TestHandlerRejectsTaskWithWrongProviderPRRefOrSHA(t *testing.T) {
	h, _ := setupBridge(t, 1024)
	assertMismatch := func(t *testing.T, task, providerPR, headRef, headSHA string) {
		t.Helper()
		got := requestBinding(t, h, fixtureOwner, fixtureRepo, task, "bridge-secret", providerPR, headRef, headSHA)
		if got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), `"schema":"ags.internal-provider-log-error.v1"`) || !strings.Contains(got.Body.String(), `"code":"binding_mismatch"`) {
			t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
		}
	}
	t.Run("provider_pr", func(t *testing.T) { assertMismatch(t, "6949", "99", fixtureRef, fixtureSHA) })
	t.Run("same_sha_other_pr_task", func(t *testing.T) { assertMismatch(t, "6950", "98", fixtureRef, fixtureSHA) })
	t.Run("head_ref", func(t *testing.T) {
		if err := h.db.Exec(`UPDATE action_run SET ref = 'refs/heads/other/branch', event = 'workflow_dispatch' WHERE id = 4001`).Error; err != nil {
			t.Fatal(err)
		}
		assertMismatch(t, "6949", "98", fixtureRef, fixtureSHA)
		if err := h.db.Exec(`UPDATE action_run SET ref = 'refs/pull/98/head', event = 'pull_request' WHERE id = 4001`).Error; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("head_sha", func(t *testing.T) {
		assertMismatch(t, "6949", "98", fixtureRef, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	})
}

func TestHandlerRejectsEachCommitBindingMismatchBeforeReadingLog(t *testing.T) {
	for _, test := range []struct{ name, table string }{
		{name: "action_run", table: "action_run"}, {name: "action_run_job", table: "action_run_job"}, {name: "action_task", table: "action_task"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _ := setupBridge(t, 1024)
			if err := h.db.Exec(`UPDATE ` + test.table + ` SET commit_sha = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' WHERE ` + map[string]string{
				"action_run": "id = 4001", "action_run_job": "id = 6763", "action_task": "id = 6949",
			}[test.table]).Error; err != nil {
				t.Fatal(err)
			}
			// A missing path proves binding rejection happens before any file read.
			if err := h.db.Exec(`UPDATE action_task SET log_filename = 'missing.log' WHERE id = 6949`).Error; err != nil {
				t.Fatal(err)
			}
			got := request(t, h, fixtureOwner, fixtureRepo, "6949", "bridge-secret")
			if got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), `"code":"binding_mismatch"`) {
				t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
			}
		})
	}
}

func TestNewRejectsDecodedLimitAboveProtocolMaximum(t *testing.T) {
	h, root := setupBridge(t, 0)
	_, err := New(Config{DB: h.db, ActionsLogDir: root, ServiceToken: "bridge-secret", MaxBytes: providerlogprotocol.DecodedTextMaxBytes + 1, AllowedCIDRs: []string{"127.0.0.0/8"}})
	if err == nil || !strings.Contains(err.Error(), "decoded protocol maximum") {
		t.Fatalf("oversized config err=%v", err)
	}
}

func TestHandlerReturnsExactWorkflowDispatchHeadRefBinding(t *testing.T) {
	h, _ := setupBridge(t, 1024)
	if err := h.db.Exec(`UPDATE action_run SET ref = 'refs/heads/fix/log-fixture', event = 'workflow_dispatch' WHERE id = 4001`).Error; err != nil {
		t.Fatal(err)
	}
	got := request(t, h, fixtureOwner, fixtureRepo, "6949", "bridge-secret")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"head_ref":"fix/log-fixture"`) || !strings.Contains(got.Body.String(), `"event":"workflow_dispatch"`) {
		t.Fatalf("status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestHandlerFailsClosedForAuthAndRepo(t *testing.T) {
	h, _ := setupBridge(t, 1024)
	if got := request(t, h, fixtureOwner, fixtureRepo, "6949", "wrong"); got.Code != http.StatusUnauthorized {
		t.Fatalf("auth status=%d", got.Code)
	}
	if got := request(t, h, "other", fixtureRepo, "6949", "bridge-secret"); got.Code != http.StatusNotFound {
		t.Fatalf("repo status=%d", got.Code)
	}
}

func TestHandlerRejectsDatabaseBoundSymlinkLog(t *testing.T) {
	h, root := setupBridge(t, 1024)
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, fixtureOwner, fixtureRepo, "25", "6950.log")); err != nil {
		t.Fatal(err)
	}
	got := requestBinding(t, h, fixtureOwner, fixtureRepo, "6950", "bridge-secret", "99", "other-pr", fixtureSHA)
	if got.Code != http.StatusConflict || !strings.Contains(got.Body.String(), `"code":"unsafe_path"`) {
		t.Fatalf("symlink status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestHandlerRejectsOversizePlainLog(t *testing.T) {
	h, _ := setupBridge(t, 8)
	got := request(t, h, fixtureOwner, fixtureRepo, "6949", "bridge-secret")
	if got.Code != http.StatusRequestEntityTooLarge || !strings.Contains(got.Body.String(), `"code":"log_too_large"`) {
		t.Fatalf("size status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestHandlerRejectsHighlyCompressedOversizeLogAndReapsDecoder(t *testing.T) {
	const maxBytes = int64(1024)
	h, root := setupBridge(t, maxBytes)
	raw := filepath.Join(t.TempDir(), "large.log")
	compressed := filepath.Join(root, fixtureOwner, fixtureRepo, "25", "6949.log.zst")
	if err := os.WriteFile(raw, []byte(strings.Repeat("a", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("zstd", "-q", "-f", raw, "-o", compressed).CombinedOutput(); err != nil {
		t.Fatalf("compress fixture: %v: %s", err, output)
	}
	if info, err := os.Stat(compressed); err != nil {
		t.Fatal(err)
	} else if info.Size() > maxBytes {
		t.Fatalf("compressed fixture size=%d exceeds max=%d", info.Size(), maxBytes)
	}
	if err := h.db.Exec(`UPDATE action_task SET log_filename = 'example-team/pipeline-fixture/25/6949.log.zst' WHERE id = 6949`).Error; err != nil {
		t.Fatal(err)
	}
	productionCommand := h.zstdCommand
	commands := make(chan *exec.Cmd, 1)
	h.zstdCommand = func(ctx context.Context, path string) *exec.Cmd {
		cmd := productionCommand(ctx, path)
		commands <- cmd
		return cmd
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() { responses <- request(t, h, fixtureOwner, fixtureRepo, "6949", "bridge-secret") }()
	var got *httptest.ResponseRecorder
	select {
	case got = <-responses:
	case <-time.After(3 * time.Second):
		t.Fatal("oversize compressed log request did not terminate")
	}
	cmd := <-commands
	if got.Code != http.StatusRequestEntityTooLarge || !strings.Contains(got.Body.String(), `"code":"log_too_large"`) {
		t.Fatalf("compressed size status=%d body=%s", got.Code, got.Body.String())
	}
	if cmd.ProcessState == nil {
		t.Fatal("zstd process was not reaped before response")
	}
}

func TestHandlerCancellationTerminatesAndReapsDecoder(t *testing.T) {
	h, root := setupBridge(t, 1024)
	compressed := filepath.Join(root, fixtureOwner, fixtureRepo, "25", "6949.log.zst")
	if err := os.WriteFile(compressed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Exec(`UPDATE action_task SET log_filename = 'example-team/pipeline-fixture/25/6949.log.zst' WHERE id = 6949`).Error; err != nil {
		t.Fatal(err)
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyReader.Close()
	defer readyWriter.Close()
	commands := make(chan *exec.Cmd, 1)
	h.zstdCommand = func(ctx context.Context, _ string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "sh", "-c", "printf x >&3; exec sleep 60")
		cmd.ExtraFiles = []*os.File{readyWriter}
		commands <- cmd
		return cmd
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- requestBindingContext(t, ctx, h, fixtureOwner, fixtureRepo, "6949", "bridge-secret", "98", fixtureRef, fixtureSHA)
	}()
	cmd := <-commands
	ready := make(chan error, 1)
	go func() { var signal [1]byte; _, err := readyReader.Read(signal[:]); ready <- err }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("decoder readiness: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("decoder did not start")
	}
	cancel()
	var got *httptest.ResponseRecorder
	select {
	case got = <-responses:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled decoder request did not terminate")
	}
	if got.Code != http.StatusBadGateway || !strings.Contains(got.Body.String(), `"code":"decode_failed"`) {
		t.Fatalf("canceled status=%d body=%s", got.Code, got.Body.String())
	}
	if cmd.ProcessState == nil {
		t.Fatal("canceled decoder process was not reaped before response")
	}
}
