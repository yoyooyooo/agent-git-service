package middleware

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	applog "github.com/ngaut/agent-git-service/internal/logging"
)

func TestRequestLogging_ClientClosedStatusLogsAtInfo(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	slog.SetDefault(logger)
	t.Cleanup(func() {
		slog.SetDefault(prev)
	})

	handler := RequestLogging()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusClientClosedRequest)
		_, _ = w.Write([]byte(`{"message":"Client Closed Request"}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v3/repos/acme/demo/issues", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	logLine := buf.String()
	if !strings.Contains(logLine, "level=INFO") {
		t.Fatalf("expected INFO level log, got %q", logLine)
	}
	if strings.Contains(logLine, "level=WARN") || strings.Contains(logLine, "level=ERROR") {
		t.Fatalf("did not expect warning/error log, got %q", logLine)
	}
	if !strings.Contains(logLine, "status=499") {
		t.Fatalf("expected status=499 in log, got %q", logLine)
	}
}

func TestGitRequestLoggingDoesNotCaptureProtocolBody(t *testing.T) {
	_, trace := applog.WithGitHTTPTrace(context.Background(), "fixture/repo", "git-upload-pack")
	writer := &captureResponseWriter{
		WrapResponseWriter: chimiddleware.NewWrapResponseWriter(httptest.NewRecorder(), 1),
		gitTrace:           trace,
	}
	if _, err := writer.Write(bytes.Repeat([]byte{'x'}, 128<<10)); err != nil {
		t.Fatal(err)
	}
	if writer.body.Len() != 0 {
		t.Fatal("Git response retained in diagnostic buffer")
	}
	_, fields := trace.Completion(context.Background(), 200)
	if len(fields) == 0 {
		t.Fatal("missing completion fields")
	}
}

func TestGitRequestLoggingRouteRecognition(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		git          bool
	}{
		{"GET", "/fixture/repo.git/info/refs?service=git-upload-pack", true},
		{"POST", "/fixture/repo.git/git-upload-pack", true},
		{"POST", "/fixture/repo.git/git-receive-pack", true},
		{"GET", "/fixture/repo.git/unknown", false},
		{"GET", "/api/v3/repos/fixture/repo.git/info/refs", false},
		{"POST", "/fixture/repo/git-upload-pack", false},
	} {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		_, _, got := gitHTTPRoute(request)
		if got != tc.git {
			t.Errorf("%s %s: got %v", tc.method, tc.path, got)
		}
	}
}
