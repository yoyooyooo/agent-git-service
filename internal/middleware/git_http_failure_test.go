package middleware_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/ngaut/agent-git-service/internal/gitbackend"
	applog "github.com/ngaut/agent-git-service/internal/logging"
	"github.com/ngaut/agent-git-service/internal/middleware"
)

func failureBackendHandler(dir, service string, done chan<- struct{}) http.Handler {
	return chimiddleware.RequestID(middleware.RequestLogging()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if done != nil {
			defer close(done)
		}
		applog.GitHTTPTraceFromContext(r.Context()).Authorized("fixture/repo")
		if err := gitbackend.Serve(w, r, gitbackend.Request{ProjectRoot: dir, Repository: "fixture/repo", Service: service, AllowReceive: service == gitbackend.ReceivePack}); err != nil {
			panic(err)
		}
	})))
}

func TestGitHTTPDiagnosticsCGIExit(t *testing.T) {
	for _, service := range []string{gitbackend.UploadPack, gitbackend.ReceivePack} {
		for _, mode := range []string{"exit-7", "signal"} {
			t.Run(service+"/"+mode, func(t *testing.T) {
				logs := diagnosticLog(t)
				dir := fixtureBackend(t, mode)
				w := httptest.NewRecorder()
				failureBackendHandler(dir, service, nil).ServeHTTP(w, httptest.NewRequest("POST", "/fixture/repo.git/"+service, strings.NewReader("0000")))
				assertChildReaped(t, dir)
				const body = "0008NAK\nPACKpartial"
				if w.Code != 200 || w.Body.String() != body {
					t.Fatalf("protocol changed: status=%d body=%q", w.Code, w.Body.String())
				}
				e := completion(t, logs, "cgi_exit_failed")
				code := float64(7)
				if mode == "signal" {
					code = -1
				}
				if e["level"] != "ERROR" || e["cgi_exit_code"] != code || e["cgi_error"] != "exit_failed" || e["response_bytes"] != float64(len(body)) || e["first_byte_ms"].(float64) < 0 {
					t.Fatalf("incorrect exit diagnostics: %v", e)
				}
			})
		}
	}
}

type deadlineWriter struct {
	header  http.Header
	written int
	status  int
	cancel  context.CancelFunc
}

func (w *deadlineWriter) Header() http.Header { return w.header }
func (w *deadlineWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.WriteHeader(200)
	if w.written > 0 {
		w.cancel()
		return 0, os.ErrDeadlineExceeded
	}
	n := min(len(p), 11)
	w.written += n
	// A successful prefix and a deadline in the same Write must both survive.
	w.cancel()
	return n, os.ErrDeadlineExceeded
}

func TestGitHTTPDiagnosticsInjectedWriteTimeout(t *testing.T) {
	for _, service := range []string{gitbackend.UploadPack, gitbackend.ReceivePack} {
		for _, cancelled := range []bool{false, true} {
			name := service + "/active-context"
			if cancelled {
				name = service + "/cancelled-context"
			}
			t.Run(name, func(t *testing.T) {
				logs := diagnosticLog(t)
				dir := fixtureBackend(t, "timeout-stream")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				writeCancel := func() {}
				if cancelled {
					writeCancel = cancel
				}
				w := &deadlineWriter{header: make(http.Header), cancel: writeCancel}
				failureBackendHandler(dir, service, nil).ServeHTTP(w, httptest.NewRequest("POST", "/fixture/repo.git/"+service, strings.NewReader("0000")).WithContext(ctx))
				assertChildReaped(t, dir)
				e := completion(t, logs, "write_timeout")
				if w.status != 200 || w.written != 11 || e["response_bytes"] != float64(11) || e["level"] != "ERROR" || e["cgi_error"] != "copy_failed" {
					t.Fatalf("incorrect write timeout diagnostics: %v", e)
				}
			})
		}
	}
}

func TestGitHTTPDiagnosticsRealWriteTimeout(t *testing.T) {
	for _, service := range []string{gitbackend.UploadPack, gitbackend.ReceivePack} {
		t.Run(service, func(t *testing.T) {
			logs := diagnosticLog(t)
			dir := fixtureBackend(t, "timeout-stream")
			done := make(chan struct{})
			server := httptest.NewUnstartedServer(failureBackendHandler(dir, service, done))
			server.Config.WriteTimeout = 1500 * time.Millisecond
			server.Start()
			defer server.Close()
			client := server.Client()
			client.Timeout = 10 * time.Second
			resp, err := client.Post(server.URL+"/fixture/repo.git/"+service, "application/x-"+service+"-request", strings.NewReader("0000"))
			if err != nil {
				t.Fatal(err)
			}
			n, readErr := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			await(t, done)
			waitForCompletion(t, logs)
			assertChildReaped(t, dir)
			if resp.StatusCode != 200 || n == 0 || !errors.Is(readErr, io.ErrUnexpectedEOF) {
				t.Fatalf("status=%d bytes=%d read error=%v", resp.StatusCode, n, readErr)
			}
			e := completion(t, logs, "write_timeout")
			if e["level"] != "ERROR" || e["cgi_error"] != "copy_failed" || e["first_byte_ms"].(float64) < 0 {
				t.Fatalf("incorrect timeout: %v", e)
			}
		})
	}
}

func TestGitHTTPDiagnosticsContextDeadline(t *testing.T) {
	logs := diagnosticLog(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	h := chimiddleware.RequestID(middleware.RequestLogging()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("PACKpartial")) })))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/fixture/repo.git/git-upload-pack", nil).WithContext(ctx))
	e := completion(t, logs, "request_timeout")
	if w.Code != 200 || w.Body.String() != "PACKpartial" || e["level"] != "ERROR" {
		t.Fatalf("deadline: %v", e)
	}
}
