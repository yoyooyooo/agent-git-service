package logging

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestGitHTTPCompletionClassification(t *testing.T) {
	for _, tc := range []struct {
		name, category, want       string
		status, n                  int
		writeErr                   error
		cancel, authorized, denied bool
	}{
		{name: "normal", authorized: true, status: 200, n: 4, want: "completed"},
		{name: "challenge", status: 401, want: "authorization_failed"},
		{name: "hidden", status: 404, want: "authorization_failed"},
		{name: "revalidation", authorized: true, denied: true, status: 403, want: "authorization_failed"},
		{name: "storage missing", authorized: true, status: 404, want: "request_rejected"},
		{name: "start", category: "start_failed", status: 500, n: 20, want: "cgi_start_failed"},
		{name: "headers", category: "headers_failed", status: 200, want: "failed_before_first_byte"},
		{name: "lookup", category: "backend_unavailable", status: 500, want: "failed_before_first_byte"},
		{name: "copy before body", category: "copy_failed", status: 200, writeErr: errors.New("sink"), want: "failed_before_first_byte"},
		{name: "copy mid body", category: "copy_failed", status: 200, n: 4, writeErr: errors.New("sink"), want: "cgi_copy_failed"},
		{name: "pipe", category: "copy_failed", status: 200, n: 4, writeErr: syscall.EPIPE, want: "client_disconnected"},
		{name: "reset", status: 200, n: 4, writeErr: syscall.ECONNRESET, want: "client_disconnected"},
		{name: "closed", status: 200, n: 4, writeErr: net.ErrClosed, want: "client_disconnected"},
		{name: "write timeout", status: 200, n: 4, writeErr: os.ErrDeadlineExceeded, want: "write_timeout"},
		{name: "write timeout before body", status: 200, writeErr: os.ErrDeadlineExceeded, want: "write_timeout"},
		{name: "write timeout cancels context", status: 200, n: 4, writeErr: &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}, cancel: true, want: "write_timeout"},
		{name: "wrapped deadline", status: 200, n: 4, writeErr: fmt.Errorf("wrapped: %w", context.DeadlineExceeded), want: "write_timeout"},
		{name: "cancel", status: 200, n: 4, cancel: true, want: "client_disconnected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, trace := WithGitHTTPTrace(context.Background(), "fixture/repo", "git-upload-pack")
			if tc.authorized {
				trace.Authorized("canonical/repo")
			}
			if tc.denied {
				trace.AuthorizationFailed()
			}
			if tc.category != "" {
				trace.CGIFailure(tc.category)
			}
			trace.Wrote(tc.n, tc.writeErr)
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, _ := trace.Completion(ctx, tc.status)
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestGitHTTPCompletionTimeoutAndExitPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, want, category      string
		deadline, cancel, timeout bool
		exit                      int
		failed                    bool
	}{
		{name: "normal exit", exit: 0, want: "completed"},
		{name: "exit 7", exit: 7, failed: true, want: "cgi_exit_failed", category: "exit_failed"},
		{name: "signal", exit: -1, failed: true, want: "cgi_exit_failed", category: "exit_failed"},
		{name: "copy cleanup kill", exit: -1, failed: true, want: "cgi_copy_failed", category: "copy_failed"},
		{name: "header failure plus exit", exit: 7, failed: true, want: "failed_before_first_byte", category: "headers_failed"},
		{name: "write timeout plus cleanup kill", exit: -1, failed: true, timeout: true, cancel: true, want: "write_timeout", category: "copy_failed"},
		{name: "context deadline", exit: 0, deadline: true, want: "request_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, trace := WithGitHTTPTrace(context.Background(), "fixture/repo", "git-upload-pack")
			if tc.category != "exit_failed" && tc.category != "" {
				trace.CGIFailure(tc.category)
			}
			trace.Wrote(11, nil)
			if tc.timeout {
				trace.Wrote(0, os.ErrDeadlineExceeded)
				// Preserve timeout even if a later write has a different error.
				trace.Wrote(0, syscall.EPIPE)
			}
			if tc.deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			trace.CGIExited(tc.exit, tc.failed)
			got, fields := trace.Completion(ctx, 200)
			if got != tc.want {
				t.Fatalf("outcome=%s want=%s", got, tc.want)
			}
			values := map[string]any{}
			for i := 0; i < len(fields); i += 2 {
				values[fields[i].(string)] = fields[i+1]
			}
			if values["cgi_exit_code"] != tc.exit || values["cgi_error"] != tc.category {
				t.Fatalf("fields=%v", values)
			}
		})
	}
}
