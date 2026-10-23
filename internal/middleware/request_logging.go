// Package middleware provides shared HTTP middleware.
package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	applog "github.com/ngaut/agent-git-service/internal/logging"
)

const (
	maxCapturedErrorBodyBytes  = 4 << 10
	maxLoggedErrorMessageRunes = 512
	statusClientClosedRequest  = 499
)

// RequestLogging attaches request-scoped structured fields and emits a single
// completion log for every HTTP request, including a small error summary when
// the response is non-successful.
func RequestLogging() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := applog.WithRequestContext(r.Context(),
				slog.String("request_id", chimiddleware.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("host", r.Host),
				slog.String("remote_ip", clientIP(r)),
				slog.String("user_agent", r.UserAgent()),
			)
			var gitTrace *applog.GitHTTPTrace
			if repo, operation, ok := gitHTTPRoute(r); ok {
				ctx, gitTrace = applog.WithGitHTTPTrace(ctx, repo, operation)
			}
			r = r.WithContext(ctx)

			start := time.Now()
			ww := &captureResponseWriter{
				WrapResponseWriter: chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor),
				gitTrace:           gitTrace,
			}
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}

			route := "unmatched"
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				if pattern := rctx.RoutePattern(); pattern != "" {
					route = pattern
				}
			}
			applog.AddAttrs(ctx, slog.String("route", route))

			args := []any{
				"status", status,
				"route", route,
				"duration_ms", time.Since(start).Milliseconds(),
				"response_bytes", ww.BytesWritten(),
			}
			outcome := ""
			if gitTrace != nil {
				var fields []any
				outcome, fields = gitTrace.Completion(ctx, status)
				// Git diagnostics never include the protocol/error body.
				args = append([]any{"status", status, "route", route}, fields...)
			} else if msg := ww.errorSummary(); msg != "" {
				args = append(args, "error_message", msg)
			}

			switch {
			case outcome == "cgi_start_failed" || outcome == "failed_before_first_byte" || outcome == "cgi_copy_failed" || outcome == "cgi_exit_failed" || outcome == "write_timeout" || outcome == "request_timeout":
				slog.ErrorContext(ctx, "http request completed", args...)
			case outcome == "client_disconnected":
				slog.WarnContext(ctx, "http request completed", args...)
			case status >= 500:
				slog.ErrorContext(ctx, "http request completed", args...)
			case status == statusClientClosedRequest:
				slog.InfoContext(ctx, "http request completed", args...)
			case status >= 400:
				slog.WarnContext(ctx, "http request completed", args...)
			default:
				slog.InfoContext(ctx, "http request completed", args...)
			}
		})
	}
}

type captureResponseWriter struct {
	chimiddleware.WrapResponseWriter
	body     bytes.Buffer
	gitTrace *applog.GitHTTPTrace
}

func (w *captureResponseWriter) Write(p []byte) (int, error) {
	if w.gitTrace == nil && w.body.Len() < maxCapturedErrorBodyBytes {
		remaining := maxCapturedErrorBodyBytes - w.body.Len()
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = w.body.Write(p[:remaining])
	}
	n, err := w.WrapResponseWriter.Write(p)
	w.gitTrace.Wrote(n, err)
	return n, err
}

// Match only the Git transport surface, including requests rejected by route
// auth before githttp runs. Query strings (and credentials) are never retained.
func gitHTTPRoute(r *http.Request) (string, string, bool) {
	// chi routes against RawPath when present. Split before unescaping so a
	// quoted repository component cannot accidentally look like an API path.
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	action := strings.Join(parts[2:], "/")
	if (r.Method == http.MethodPost && (action == "git-upload-pack" || action == "git-receive-pack")) ||
		(r.Method == http.MethodGet && action == "info/refs") {
		owner, ownerErr := url.PathUnescape(parts[0])
		repo, repoErr := url.PathUnescape(parts[1])
		if ownerErr == nil && repoErr == nil {
			return owner + "/" + strings.TrimSuffix(repo, ".git"), action, true
		}
	}
	return "", "", false
}

func (w *captureResponseWriter) errorSummary() string {
	status := w.Status()
	if status < http.StatusBadRequest {
		return ""
	}

	raw := strings.TrimSpace(w.body.String())
	if raw == "" {
		return ""
	}

	contentType := strings.ToLower(strings.TrimSpace(w.Header().Get("Content-Type")))
	if strings.Contains(contentType, "application/json") {
		var payload map[string]any
		if err := json.Unmarshal(w.body.Bytes(), &payload); err == nil {
			if message := firstString(payload["message"], payload["error"]); message != "" {
				return truncateMessage(message)
			}
		}
	}

	return truncateMessage(raw)
}

func truncateMessage(message string) string {
	runes := []rune(message)
	if len(runes) <= maxLoggedErrorMessageRunes {
		return message
	}
	return string(runes[:maxLoggedErrorMessageRunes]) + "..."
}

func firstString(values ...any) string {
	for _, value := range values {
		if s, ok := value.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func clientIP(r *http.Request) string {
	host := strings.TrimSpace(r.RemoteAddr)
	if host == "" {
		return ""
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		return parsedHost
	}
	return host
}
