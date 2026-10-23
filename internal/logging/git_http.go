package logging

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"time"
)

type gitHTTPKey struct{}

// GitHTTPTrace records transport facts, not Git ref-level success. It never
// retains response bodies, credentials, or raw CGI diagnostics.
type GitHTTPTrace struct {
	mu                                       sync.Mutex
	start                                    time.Time
	repo, operation                          string
	authorized, backend, authorizationFailed bool
	cgiError                                 string
	bytes                                    int64
	firstByte                                time.Duration
	writeError, disconnected                 bool
}

func WithGitHTTPTrace(ctx context.Context, repo, operation string) (context.Context, *GitHTTPTrace) {
	trace := &GitHTTPTrace{start: time.Now(), repo: repo, operation: operation}
	return context.WithValue(ctx, gitHTTPKey{}, trace), trace
}

func GitHTTPTraceFromContext(ctx context.Context) *GitHTTPTrace {
	trace, _ := ctx.Value(gitHTTPKey{}).(*GitHTTPTrace)
	return trace
}

func (t *GitHTTPTrace) Authorized(repo string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.authorized = true
	t.repo = repo
}

func (t *GitHTTPTrace) AuthorizationFailed() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.authorizationFailed = true
}

func (t *GitHTTPTrace) BackendStarted() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.backend = true
}

// CGIFailure accepts only server-owned categories; callers must not pass the
// standard library's interpolated error (which may contain child output).
func (t *GitHTTPTrace) CGIFailure(category string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cgiError = category
}

func (t *GitHTTPTrace) Wrote(n int, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n > 0 {
		if t.bytes == 0 {
			t.firstByte = time.Since(t.start)
		}
		t.bytes += int64(n)
	}
	if err != nil {
		t.writeError = true
		t.disconnected = errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed)
	}
}

// Completion uses -1 for a body first byte that was never accepted by the
// ResponseWriter. HTTP 200 is deliberately not sufficient for "completed".
func (t *GitHTTPTrace) Completion(ctx context.Context, status int) (string, []any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	outcome := "completed"
	switch {
	case t.cgiError == "start_failed":
		outcome = "cgi_start_failed"
	case t.disconnected || errors.Is(ctx.Err(), context.Canceled):
		outcome = "client_disconnected"
	case t.cgiError == "headers_failed" || t.cgiError == "backend_unavailable":
		outcome = "failed_before_first_byte"
	case (t.cgiError == "copy_failed" || t.writeError) && t.bytes == 0:
		outcome = "failed_before_first_byte"
	case t.cgiError == "copy_failed" || t.writeError:
		outcome = "cgi_copy_failed"
	case ctx.Err() != nil:
		outcome = "request_cancelled"
	case t.authorizationFailed || (!t.authorized && !t.backend && (status == 401 || status == 403 || status == 404)):
		// 404 includes deliberately hidden authorization denials, not a claim
		// that the repository exists.
		outcome = "authorization_failed"
	case status >= 400:
		outcome = "request_rejected"
	}
	first := float64(-1)
	if t.bytes > 0 {
		first = float64(t.firstByte) / float64(time.Millisecond)
	}
	return outcome, []any{
		"repo", t.repo, "git_operation", t.operation, "git_outcome", outcome,
		"response_bytes", t.bytes, "duration_ms", float64(time.Since(t.start)) / float64(time.Millisecond),
		"first_byte_ms", first, "cgi_error", t.cgiError,
	}
}
