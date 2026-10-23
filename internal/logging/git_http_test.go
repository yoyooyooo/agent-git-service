package logging

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
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
