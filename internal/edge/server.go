// Package edge is the independent, database-free AGS Edge runtime.
// Local reads require explicit assembly of ReadRuntime (or its file config).
// The default remains closed, and operational readiness is independently gated.
// Forwarding a download is never treated as a cache hit.
package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/config"
)

const hopHeader = "X-AGS-Edge-Hop"
const readUnavailableReason = "read_path_not_wired"

// Server does not embed the primary server or own business/Git write workers.
type Server struct {
	cfg           config.EdgeConfig
	canonical     *url.URL
	acceptedHosts map[string]bool
	proxy         *httputil.ReverseProxy
	reads         *ReadRuntime
	telemetry     *Telemetry
	operations    *Operations
}

// Option adds explicitly assembled runtime dependencies. Merely supplying a
// mirror is insufficient: local reads require the full original-user path.
type Option func(*Server) error

func WithReadRuntime(reads *ReadRuntime) Option {
	return func(s *Server) error {
		if reads == nil || reads.cfg.EdgeID != s.cfg.ID || s.reads != nil {
			return errors.New("invalid or duplicate Edge read runtime")
		}
		s.reads = reads
		return nil
	}
}

func New(cfg config.EdgeConfig, options ...Option) (*Server, error) {
	cfg, err := config.NormalizeEdge(cfg)
	if err != nil {
		return nil, err
	}
	primary, _ := url.Parse(cfg.PrimaryURL)
	canonical, _ := url.Parse(cfg.CanonicalURL)
	transport := &http.Transport{
		// Do not inherit the machine's proxy environment or Git URL rewrites.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.RequestTimeout,
		// A fresh HTTP/1 connection prevents Transport from retrying mutations
		// on a reused connection, including requests with Idempotency-Key.
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
	}
	s := &Server{cfg: cfg, canonical: canonical, acceptedHosts: map[string]bool{strings.ToLower(canonical.Host): true}, telemetry: newTelemetry()}
	for _, origin := range cfg.CanonicalAliases {
		alias, _ := url.Parse(origin) // NormalizeEdge already validated exact origins.
		s.acceptedHosts[strings.ToLower(alias.Host)] = true
	}
	s.proxy = &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(primary)
			pr.Out.Host = canonical.Host
			// Discard inbound routing/identity assertions. The original user's
			// Authorization is retained; there is no replication credential.
			for key := range pr.Out.Header {
				lower := strings.ToLower(key)
				if strings.HasPrefix(lower, "x-ags-edge-") || strings.HasPrefix(lower, "x-ags-internal-") || strings.HasPrefix(lower, "x-forwarded-") || lower == "forwarded" {
					pr.Out.Header.Del(key)
				}
			}
			pr.Out.Header.Set(hopHeader, cfg.ID)
			if trace, _ := pr.In.Context().Value(traceKey{}).(*requestTrace); trace != nil {
				pr.Out.Header.Set("X-Request-ID", trace.event.ID)
			}
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Host", canonical.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", canonical.Scheme)
		},
		ModifyResponse: func(response *http.Response) error {
			for key := range response.Header {
				if strings.HasPrefix(strings.ToLower(key), "x-ags-edge-") {
					response.Header.Del(key)
				}
			}
			return nil
		},
		ErrorLog: log.New(safeProxyLog{}, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				slog.WarnContext(r.Context(), "edge primary forwarding failed", "edge_id", cfg.ID)
			}
			// No raw upstream URL, user credential, query, or response retry.
			writeError(w, http.StatusBadGateway, "primary_unavailable")
		},
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil Edge option")
		}
		if err := option(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

type safeProxyLog struct{}

func (safeProxyLog) Write(p []byte) (int, error) {
	slog.Warn("edge proxy transport warning")
	return len(p), nil
}

func (s *Server) Handler() http.Handler { return s }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
		s.serveRequest(w, r)
		return
	}
	s.traceRequest(w, r)
}

func (s *Server) serveRequest(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/readyz" {
			if s.operations != nil && s.reads != nil {
				health := s.operations.Snapshot()
				if !health.Ready {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"role": "edge", "ready": health.Ready, "read_path_wired": true, "reason": health.Reason})
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			reason := readUnavailableReason
			if s.reads != nil {
				reason = "read_operational_gates_pending"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"role": "edge", "ready": false, "read_path_wired": s.reads != nil, "reason": reason})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"role": "edge", "live": true})
		}
		return
	}
	if !s.acceptedHosts[strings.ToLower(r.Host)] {
		writeError(w, http.StatusMisdirectedRequest, "canonical_host_required")
		return
	}
	if r.Header.Get(hopHeader) != "" {
		writeError(w, http.StatusLoopDetected, "edge_forwarding_loop")
		return
	}
	if r.Method == http.MethodConnect || r.Method == http.MethodTrace || r.Header.Get("Upgrade") != "" {
		writeError(w, http.StatusMethodNotAllowed, "unsupported_transport")
		return
	}
	// Reject ambiguous paths/queries BEFORE either routing or proxying. In
	// particular, do not let ParseQuery errors select a different Git service
	// in the primary. No query rewriting or best-effort parsing is allowed.
	if r.URL.Path == "" || (path.Clean(r.URL.Path) != r.URL.Path && path.Clean(r.URL.Path)+"/" != r.URL.Path) || strings.ContainsAny(r.URL.Path, "\\\x00\r\n") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") || strings.Contains(strings.ToLower(r.URL.RawPath), "%5c") || len(r.URL.RawQuery) > 16*1024 {
		writeError(w, http.StatusBadRequest, "invalid_request_target")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	if r.URL.Path == "/internal" || strings.HasPrefix(r.URL.Path, "/internal/") || r.URL.Path == "/_ags" || strings.HasPrefix(r.URL.Path, "/_ags/") {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) >= 2 && strings.HasSuffix(parts[1], ".git") {
		if len(parts) < 3 || !isSmartGitPath(parts) {
			if s.cfg.UnboundReads == "primary" {
				s.forward(w, r, "primary_other")
				return
			}
			writeError(w, http.StatusBadRequest, "unsupported_git_request")
			return
		}
		isRead, valid := classifyGit(r.Method, parts, query)
		if !valid {
			writeError(w, http.StatusBadRequest, "unsupported_git_request")
			return
		}
		if isRead {
			locator := parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
			if s.cfg.UnboundReads == "primary" && (s.reads == nil || !s.reads.Bound(locator)) {
				// Choose at ingress for BOTH discovery and RPC. This never
				// reacts to an authentication, cache or synchronization failure.
				s.forward(w, r, "primary_unbound")
				return
			}
			traceRoute(r.Context(), w, "local_snapshot")
			// No init-empty, stale-read or silent download proxy fallback.
			if s.reads == nil {
				writeError(w, http.StatusServiceUnavailable, readUnavailableReason)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
			defer cancel()
			s.reads.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		if !isRead {
			s.forward(w, r, "primary_write")
			return
		}
	}
	// LFS, archive downloads and other non-Smart-HTTP surfaces retain primary
	// behavior; never turn a host-wide rule into a synthetic local 400/404.
	s.forward(w, r, "primary_other")
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, route string) {
	traceRoute(r.Context(), w, route)
	traceStage(r.Context(), "primary_proxy")
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}

func isSmartGitPath(parts []string) bool {
	return parts[2] == "git-upload-pack" || parts[2] == "git-receive-pack" || len(parts) >= 4 && parts[2] == "info" && parts[3] == "refs"
}

func classifyGit(method string, parts []string, query url.Values) (read, valid bool) {
	if parts[0] == "" || strings.TrimSuffix(parts[1], ".git") == "" {
		return false, false
	}
	if len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" && method == http.MethodGet {
		services := query["service"]
		if len(services) != 1 {
			return false, false
		}
		switch services[0] {
		case "git-upload-pack":
			return true, true
		case "git-receive-pack":
			return false, true
		}
	}
	if len(parts) == 3 && method == http.MethodPost && len(query["service"]) == 0 {
		switch parts[2] {
		case "git-upload-pack":
			return true, true
		case "git-receive-pack":
			return false, true
		}
	}
	return false, false
}

func writeError(w http.ResponseWriter, status int, code string) {
	recordFailure(w, code)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": code, "code": code})
}

// Run serves only the supplied listener and drains on cancellation. It does
// not spawn primary workers or persist anything to a repository/database.
func (s *Server) Run(ctx context.Context, listener net.Listener) error {
	httpServer := &http.Server{
		Handler: s, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 * 1024,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			<-done
			return fmt.Errorf("edge shutdown: %w", err)
		}
		<-done
		return nil
	}
}
