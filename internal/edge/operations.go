package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/buildinfo"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

func operationError(err error) string {
	if err == nil {
		return ""
	}
	var control *ReadControlError
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, snapshotstore.ErrHeadroom):
		return "filesystem_headroom_exhausted"
	case errors.Is(err, snapshotstore.ErrBudget):
		return "retention_budget_exhausted"
	case errors.Is(err, ErrSnapshotQueueFull):
		return "sync_queue_full"
	case errors.As(err, &control):
		if control.Status == 401 || control.Status == 403 {
			return "node_or_store_denied"
		}
		return "primary_control_unavailable"
	default:
		return "synchronization_unavailable"
	}
}

type OperationsHealth struct {
	Ready                bool      `json:"ready"`
	CheckedAt            time.Time `json:"checked_at"`
	Primary              string    `json:"primary"`
	Peer                 string    `json:"peer"`
	Capacity             string    `json:"capacity"`
	AvailableBytes       uint64    `json:"available_bytes"`
	MinimumBytes         uint64    `json:"minimum_bytes"`
	CertificateExpiresAt time.Time `json:"certificate_expires_at"`
	CertificateWarning   bool      `json:"certificate_expiring_soon"`
	Reason               string    `json:"reason"`
	Replicas             string    `json:"replicas"`
}

// Operations probes fixed primary and peer origins. It never uses a user token,
// changes node grants, fabricates repository freshness or retries mutations.
// A health result expires so a blocked/dead monitor cannot remain green.
type Operations struct {
	mu        sync.Mutex
	health    OperationsHealth
	resources *ReadResources
	cfg       config.EdgeConfig
	client    *http.Client
	cancel    context.CancelFunc
	done      chan struct{}
}

func StartOperations(ctx context.Context, cfg config.EdgeConfig, r *ReadResources) (*Operations, error) {
	if r == nil {
		return nil, errors.New("operations require configured read resources")
	}
	normal, err := config.NormalizeEdge(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	o := &Operations{cfg: normal, resources: r, cancel: cancel, done: make(chan struct{}), client: &http.Client{
		Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second},
		Timeout:   8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, health: OperationsHealth{Reason: "checks_pending"}}
	go func() {
		defer close(o.done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			o.check(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return o, nil
}
func (o *Operations) check(ctx context.Context) {
	h := OperationsHealth{CheckedAt: time.Now().UTC(), Primary: "unavailable", Peer: "unavailable", Capacity: "unknown", MinimumBytes: o.resources.minimumFree, CertificateExpiresAt: o.resources.certificateExpiry, Reason: "dependencies_unavailable"}
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	canonical, _ := url.Parse(o.cfg.CanonicalURL)
	req, err := http.NewRequestWithContext(call, http.MethodGet, o.cfg.PrimaryURL+"/readyz", nil)
	if err == nil {
		req.Host = canonical.Host
		res, e := o.client.Do(req)
		if e == nil {
			if res.StatusCode == 200 {
				h.Primary = "ok"
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 16<<10))
			res.Body.Close()
		}
	}
	status, err := o.resources.peer.NodeStatus(call, o.cfg.ID)
	if err == nil {
		h.Peer = "ok"
		for _, b := range o.resources.Runtime.cfg.Bindings {
			if b.Identity.AuthorityID != status.AuthorityID {
				h.Peer = "identity_mismatch"
			}
		}
	}
	h.AvailableBytes, err = snapshotstore.FilesystemAvailable(o.resources.cacheRoot)
	if err == nil {
		h.Capacity = "ok"
		if h.AvailableBytes < h.MinimumBytes {
			h.Capacity = "insufficient"
		}
	}
	h.CertificateWarning = time.Until(h.CertificateExpiresAt) < 7*24*time.Hour
	if !time.Now().Before(h.CertificateExpiresAt) {
		h.Peer = "certificate_expired"
	}
	if o.resources.mirror.Stats().RetentionError != "" {
		h.Capacity = "retention_error"
	}
	h.Replicas = "prewarm_disabled"
	if o.resources.prewarmer != nil {
		h.Replicas = "ok"
		for _, state := range o.resources.prewarmer.Snapshot() {
			if state.State == "failed" {
				h.Replicas = "degraded"
				break
			}
			if state.State != "synchronized" {
				h.Replicas = "warming"
			}
		}
	}
	h.Ready = h.Primary == "ok" && h.Peer == "ok" && h.Capacity == "ok" && h.Replicas != "degraded"
	if h.Ready {
		h.Reason = "operational"
	}
	o.mu.Lock()
	o.health = h
	o.mu.Unlock()
}
func (o *Operations) Snapshot() OperationsHealth {
	o.mu.Lock()
	h := o.health
	o.mu.Unlock()
	if h.CheckedAt.IsZero() || time.Since(h.CheckedAt) > 90*time.Second {
		h.Ready = false
		h.Reason = "checks_stale"
	}
	return h
}
func (o *Operations) Close() { o.cancel(); <-o.done; o.client.CloseIdleConnections() }
func WithOperations(o *Operations) Option {
	return func(s *Server) error {
		if o == nil || o.cfg.ID != s.cfg.ID {
			return errors.New("invalid operations binding")
		}
		s.operations = o
		return nil
	}
}

// DiagnosticsHandler must only be mounted on the separate loopback listener.
// Socket peer is checked directly; forwarded headers cannot authorize access.
// Nothing under this handler is mounted on the public Edge ingress.
func (s *Server) DiagnosticsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		diagnosticHost := r.Host
		if name, _, splitErr := net.SplitHostPort(r.Host); splitErr == nil {
			diagnosticHost = name
		}
		hostIP := net.ParseIP(diagnosticHost)
		if diagnosticHost != "localhost" && (hostIP == nil || !hostIP.IsLoopback()) {
			http.NotFound(w, r)
			return
		}
		if err != nil || ip == nil || !ip.IsLoopback() {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/metrics" {
			s.writeMetrics(w)
			return
		}
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		result := map[string]any{"version": "ags.edge.diagnostics.v1", "edge_id": s.cfg.ID, "source_revision": edgeSourceRevision(), "build": buildinfo.Current("ags-edge"), "unbound_reads": s.cfg.UnboundReads, "telemetry": s.telemetry.Snapshot()}
		if s.operations != nil {
			result["health"] = s.operations.Snapshot()
			resources := s.operations.resources
			result["mirror"] = resources.mirror.Stats()
			if resources.prewarmer != nil {
				result["prewarm"] = resources.prewarmer.Snapshot()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
}
func edgeSourceRevision() string {
	// Verified release archives deliberately omit Go's optional VCS metadata.
	// Prefer their exact compiled identity; retain VCS fallback for local builds.
	if revision := buildinfo.Revision; len(revision) == 40 && strings.Trim(revision, "0123456789abcdef") == "" {
		return revision
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}
func (s *Server) writeMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	snapshot := s.telemetry.Snapshot()
	fmt.Fprintf(w, "ags_edge_requests_total %d\nags_edge_request_failures_total %d\n", snapshot.Requests, snapshot.Failures)
	for _, route := range []string{"rejected", "local_snapshot", "primary_unbound", "primary_write", "primary_other"} {
		fmt.Fprintf(w, "ags_edge_route_requests_total{route=%q} %d\n", route, snapshot.Routes[route])
	}
	if s.operations != nil {
		h := s.operations.Snapshot()
		ready := 0
		if h.Ready {
			ready = 1
		}
		m := s.operations.resources.mirror.Stats()
		fmt.Fprintf(w, "ags_edge_ready %d\nags_edge_free_bytes %d\nags_edge_certificate_expiry_seconds %d\nags_edge_full_transfers_total %d\nags_edge_incremental_transfers_total %d\nags_edge_transfer_bytes_total %d\n", ready, h.AvailableBytes, h.CertificateExpiresAt.Unix(), m.FullTransfers, m.IncrementalTransfers, m.TransferBytes)
	}
}
