package middleware_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/ngaut/agent-git-service/internal/gitbackend"
	applog "github.com/ngaut/agent-git-service/internal/logging"
	"github.com/ngaut/agent-git-service/internal/middleware"
)

// This subprocess is the CGI child itself (not a mock of net/http/cgi). Using
// the test binary keeps PID/reaping and pipe backpressure assertions portable
// between the supported macOS development and Linux CI environments.
func TestGitCGIHelperProcess(t *testing.T) {
	args := os.Args
	if len(args) < 4 || args[len(args)-3] != "ags-cgi-fixture" {
		return
	}
	dir, mode := args[len(args)-2], args[len(args)-1]
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(2)
	}
	switch mode {
	case "exit-7", "signal":
		fmt.Print("Content-Type: application/x-git-upload-pack-result\r\n\r\n0008NAK\nPACKpartial")
		if mode == "signal" {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		os.Exit(7)
	case "headers":
		fmt.Print("Status: secret-sentinel\r\n\r\n")
	case "stream":
		fmt.Print("Content-Type: application/x-git-upload-pack-result\r\n\r\n0008NAK\n")
		pack, err := os.Open(filepath.Join(dir, "pack"))
		if err != nil {
			os.Exit(3)
		}
		if _, err := io.Copy(os.Stdout, pack); err != nil {
			os.Exit(4)
		}
		_ = pack.Close()
		_ = os.WriteFile(filepath.Join(dir, "produced"), []byte("done"), 0600)
	case "idle":
		for {
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Print("Content-Type: text/plain\r\n\r\nok")
	case "endless", "timeout-stream":
		fmt.Print("Content-Type: application/x-git-upload-pack-result\r\n\r\n0008NAK\nPACK")
		chunk := bytes.Repeat([]byte{'x'}, 32<<10)
		for {
			if mode == "timeout-stream" {
				time.Sleep(5 * time.Millisecond)
			}
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(5)
			}
		}
	}
	os.Exit(0)
}

type safeLog struct {
	sync.Mutex
	bytes.Buffer
}

func (b *safeLog) Write(p []byte) (int, error) { b.Lock(); defer b.Unlock(); return b.Buffer.Write(p) }
func (b *safeLog) snapshot() string            { b.Lock(); defer b.Unlock(); return b.Buffer.String() }

func diagnosticLog(t *testing.T) *safeLog {
	t.Helper()
	var logs safeLog
	previous := slog.Default()
	// Forward a context-enriched record into our synchronized in-memory sink.
	slog.SetDefault(slog.New(&testContextHandler{next: slog.NewJSONHandler(&logs, nil)}))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

type testContextHandler struct{ next slog.Handler }

func (h *testContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}
func (h *testContextHandler) Handle(ctx context.Context, record slog.Record) error {
	record.AddAttrs(applog.SnapshotAttrs(ctx)...)
	return h.next.Handle(ctx, record)
}
func (h *testContextHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &testContextHandler{h.next.WithAttrs(a)}
}
func (h *testContextHandler) WithGroup(g string) slog.Handler {
	return &testContextHandler{h.next.WithGroup(g)}
}

func fixtureBackend(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	git := "#!/bin/sh\nprintf '%s\\n' \"${0%/*}\"\n"
	backend := fmt.Sprintf("#!/bin/sh\nexec %q -test.run '^TestGitCGIHelperProcess$' -- ags-cgi-fixture %q %q\n", binary, dir, mode)
	if mode == "start" {
		backend = "not an executable format\n"
	}
	for name, body := range map[string]string{"git": git, "git-http-backend": backend} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func backendHandler(dir string, finished chan<- struct{}) http.Handler {
	return chimiddleware.RequestID(middleware.RequestLogging()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if finished != nil {
			defer close(finished)
		}
		applog.GitHTTPTraceFromContext(r.Context()).Authorized("fixture/repo")
		if err := gitbackend.Serve(w, r, gitbackend.Request{ProjectRoot: dir, Repository: "fixture/repo", Service: gitbackend.UploadPack}); err != nil {
			http.Error(w, "Internal Server Error", 500)
		}
	})))
}

func completion(t *testing.T, logs *safeLog, outcome string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.snapshot()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid log: %v", err)
		}
		if entry["msg"] == "http request completed" {
			if found != nil {
				t.Fatal("more than one request completion")
			}
			found = entry
		}
	}
	if found == nil || found["git_outcome"] != outcome {
		t.Fatalf("want %s: %s", outcome, logs.snapshot())
	}
	for _, field := range []string{"request_id", "repo", "response_bytes", "duration_ms", "first_byte_ms"} {
		if _, ok := found[field]; !ok {
			t.Errorf("missing %s: %v", field, found)
		}
	}
	if found["repo"] != "fixture/repo" || found["request_id"] == "" {
		t.Fatalf("missing correlation: %v", found)
	}
	if strings.Contains(logs.snapshot(), "secret-sentinel") {
		t.Fatal("credential/child output leaked into logs")
	}
	t.Logf("diagnostic sample: %s", logs.snapshot())
	return found
}

func await(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CGI handler did not return")
	}
}

func assertChildReaped(t *testing.T, dir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("CGI PID %d not reaped: %v", pid, err)
	}
}

func makePack(t *testing.T, dir string) []byte {
	t.Helper()
	repo := filepath.Join(dir, "source.git")
	if out, err := exec.Command("git", "init", "--bare", repo).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	blob := make([]byte, 24<<20)
	_, _ = rand.New(rand.NewSource(42)).Read(blob)
	cmd := exec.Command("git", "--git-dir="+repo, "hash-object", "-w", "--stdin")
	cmd.Stdin = bytes.NewReader(blob)
	oid, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "--git-dir="+repo, "pack-objects", "--stdout")
	cmd.Stdin = bytes.NewReader(oid)
	pack, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(pack) < 20<<20 || string(pack[:4]) != "PACK" {
		t.Fatal("fixture pack unexpectedly small/invalid")
	}
	if err := os.WriteFile(filepath.Join(dir, "pack"), pack, 0600); err != nil {
		t.Fatal(err)
	}
	return pack
}

func TestGitHTTPDiagnosticsLargePackSlowReader(t *testing.T) {
	logs := diagnosticLog(t)
	// Generate with real Git before replacing executable discovery with CGI.
	source := t.TempDir()
	pack := makePack(t, source)
	dir := fixtureBackend(t, "stream")
	if err := os.Rename(filepath.Join(source, "pack"), filepath.Join(dir, "pack")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	server := httptest.NewServer(backendHandler(dir, done))
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL+"/fixture/repo.git/git-upload-pack", strings.NewReader("0000"))
	req.Header.Set("Authorization", "Bearer secret-sentinel")
	req.Header.Set("Cookie", "session=secret-sentinel")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/x-git-upload-pack-result" || response.Header.Get("Content-Encoding") != "" {
		t.Fatalf("protocol headers changed: %v", response)
	}
	prefix := make([]byte, 12)
	if _, err := io.ReadFull(response.Body, prefix); err != nil {
		t.Fatal(err)
	}
	if string(prefix) != "0008NAK\nPACK" {
		t.Fatalf("bad Git prefix: %q", prefix)
	}
	// The producer cannot finish while the reader is paused: a 24 MiB pack
	// exceeds socket/pipe buffers. Seeing the prefix before producer EOF also
	// detects an accidental whole-response memory/disk buffer.
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "produced")); !os.IsNotExist(err) {
		t.Fatal("pack consumed before slow reader drained it")
	}
	hash := sha256.New()
	_, _ = hash.Write(prefix)
	n := int64(len(prefix))
	buf := make([]byte, 64<<10)
	for {
		read, err := response.Body.Read(buf)
		_, _ = hash.Write(buf[:read])
		n += int64(read)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	expected := sha256.New()
	_, _ = expected.Write([]byte("0008NAK\n"))
	_, _ = expected.Write(pack)
	if !bytes.Equal(hash.Sum(nil), expected.Sum(nil)) {
		t.Fatal("stream bytes changed")
	}
	await(t, done)
	// done closes in the handler before the outer logger runs.
	waitForCompletion(t, logs)
	entry := completion(t, logs, "completed")
	if entry["response_bytes"] != float64(n) || entry["first_byte_ms"].(float64) < 0 {
		t.Fatalf("bad metrics: %v", entry)
	}
	assertChildReaped(t, dir)
}

func waitForCompletion(t *testing.T, logs *safeLog) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.snapshot(), "http request completed") {
		if time.Now().After(deadline) {
			t.Fatal("no request completion")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGitHTTPDiagnosticsClientDisconnect(t *testing.T) {
	logs := diagnosticLog(t)
	dir := fixtureBackend(t, "endless")
	done := make(chan struct{})
	server := httptest.NewServer(backendHandler(dir, done))
	defer server.Close()
	response, err := server.Client().Post(server.URL+"/fixture/repo.git/git-upload-pack", "application/x-git-upload-pack-request", strings.NewReader("0000"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	prefix := make([]byte, 64<<10)
	if _, err := io.ReadFull(response.Body, prefix); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	await(t, done)
	waitForCompletion(t, logs)
	entry := completion(t, logs, "client_disconnected")
	if entry["status"] != float64(200) || entry["response_bytes"].(float64) <= 0 || entry["first_byte_ms"].(float64) < 0 {
		t.Fatalf("bad interrupted metrics: %v", entry)
	}
	assertChildReaped(t, dir)
}

type failingWriter struct {
	header  http.Header
	written int
}

func (w *failingWriter) Header() http.Header { return w.header }
func (w *failingWriter) WriteHeader(int)     {}
func (w *failingWriter) Write(p []byte) (int, error) {
	remaining := (64 << 10) - w.written
	if remaining < len(p) {
		w.written += remaining
		return remaining, errors.New("secret-sentinel synthetic copy sink failure")
	}
	w.written += len(p)
	return len(p), nil
}

func TestGitHTTPDiagnosticsCGICopyFailure(t *testing.T) {
	logs := diagnosticLog(t)
	dir := fixtureBackend(t, "endless")
	writer := &failingWriter{header: make(http.Header)}
	req := httptest.NewRequest("POST", "/fixture/repo.git/git-upload-pack", strings.NewReader("0000"))
	backendHandler(dir, nil).ServeHTTP(writer, req)
	entry := completion(t, logs, "cgi_copy_failed")
	if entry["cgi_error"] != "copy_failed" || entry["response_bytes"] != float64(writer.written) {
		t.Fatalf("bad copy metrics: %v", entry)
	}
	assertChildReaped(t, dir)
}

func TestGitHTTPDiagnosticsStartAndPreBodyFailures(t *testing.T) {
	for _, tc := range []struct{ mode, outcome string }{{"start", "cgi_start_failed"}, {"headers", "failed_before_first_byte"}} {
		t.Run(tc.mode, func(t *testing.T) {
			logs := diagnosticLog(t)
			dir := fixtureBackend(t, tc.mode)
			backendHandler(dir, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/fixture/repo.git/git-upload-pack", strings.NewReader("0000")))
			entry := completion(t, logs, tc.outcome)
			if entry["first_byte_ms"] != float64(-1) || entry["response_bytes"] != float64(0) {
				t.Fatalf("unexpected CGI body: %v", entry)
			}
			if tc.mode != "start" {
				assertChildReaped(t, dir)
			}
		})
	}
}

func TestGitHTTPDiagnosticsAuthorizationDenial(t *testing.T) {
	for _, action := range []string{"info/refs?service=git-upload-pack", "git-upload-pack", "git-receive-pack"} {
		t.Run(action, func(t *testing.T) {
			logs := diagnosticLog(t)
			handler := chimiddleware.RequestID(middleware.RequestLogging()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "secret-sentinel", http.StatusUnauthorized)
			})))
			method := "POST"
			if strings.HasPrefix(action, "info/refs") {
				method = "GET"
			}
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, "/fixture/repo.git/"+action, nil))
			completion(t, logs, "authorization_failed")
		})
	}
}

// Context cancellation alone does not kill an idle standard-library CGI child.
// Release it explicitly after measuring, so this test never leaks its fixture.
func TestGitHTTPDiagnosticsIdleCGICancellationBoundary(t *testing.T) {
	logs := diagnosticLog(t)
	dir := fixtureBackend(t, "idle")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	req := httptest.NewRequest("POST", "/fixture/repo.git/git-upload-pack", strings.NewReader("0000")).WithContext(ctx)
	go backendHandler(dir, done).ServeHTTP(httptest.NewRecorder(), req)
	defer func() {
		_ = os.WriteFile(filepath.Join(dir, "release"), []byte("release"), 0600)
		await(t, done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "pid")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("CGI did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
		t.Fatal("idle CGI unexpectedly canceled; update the documented stdlib boundary")
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	await(t, done)
	waitForCompletion(t, logs)
	completion(t, logs, "client_disconnected")
	assertChildReaped(t, dir)
}

func TestGitHTTPDiagnosticsRealGitProtocolRoundTrip(t *testing.T) {
	for _, version := range []string{"0", "2"} {
		t.Run("protocol-"+version, func(t *testing.T) {
			logs := diagnosticLog(t)
			root := t.TempDir()
			repo := filepath.Join(root, "fixture", "repo.git")
			run := func(dir string, args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-c", "http.proxy=", "-c", "protocol.version=" + version}, args...)...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_TRACE_PACKET=1", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return string(out)
			}
			run(root, "init", "--bare", "--initial-branch=main", repo)
			run(root, "--git-dir="+repo, "config", "http.receivepack", "true")
			handler := chimiddleware.RequestID(middleware.RequestLogging()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				service := gitbackend.UploadPack
				advertise := strings.HasSuffix(r.URL.Path, "/info/refs")
				if advertise {
					service = r.URL.Query().Get("service")
				} else if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
					service = gitbackend.ReceivePack
				}
				applog.GitHTTPTraceFromContext(r.Context()).Authorized("fixture/repo")
				if err := gitbackend.Serve(w, r, gitbackend.Request{ProjectRoot: root, Repository: "fixture/repo", Service: service, Advertise: advertise, AllowReceive: service == gitbackend.ReceivePack}); err != nil {
					http.Error(w, "Internal Server Error", 500)
				}
			})))
			server := httptest.NewServer(handler)
			defer server.Close()
			url := server.URL + "/fixture/repo.git"
			client := filepath.Join(root, "client")
			run(root, "clone", url, client)
			run(client, "commit", "--allow-empty", "-m", "stream diagnostic roundtrip")
			run(client, "push", "origin", "HEAD:refs/heads/main")
			refs := run(root, "ls-remote", url)
			if !strings.Contains(refs, "refs/heads/main") {
				t.Fatal("ref missing")
			}
			if version == "2" && !strings.Contains(refs, "version 2") {
				t.Fatal("protocol v2 fell back")
			}
			clone := filepath.Join(root, "second")
			run(root, "clone", url, clone)
			run(clone, "fetch", "origin")
			run(clone, "fsck", "--full")
			if strings.TrimSpace(run(client, "rev-parse", "HEAD")) != strings.TrimSpace(run(clone, "rev-parse", "HEAD")) {
				t.Fatal("cloned ref changed")
			}
			for _, line := range strings.Split(strings.TrimSpace(logs.snapshot()), "\n") {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatal(err)
				}
				if entry["msg"] == "http request completed" && entry["git_outcome"] != "completed" {
					t.Fatalf("unexpected protocol result: %v", entry)
				}
			}
		})
	}
}
