package server

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/middleware"
	"github.com/ngaut/agent-git-service/internal/replication"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

// Public aliases let embedders describe exact peer bindings without importing
// internal packages. They do not expose the primary business DB or its Service.
type ReplicationIdentity = edgeprotocol.RepositoryIdentity
type ReplicationPeer = replication.PeerGrant
type ReplicationRetention = snapshotstore.RetentionPolicy
type ReplicationRetentionStats = snapshotstore.RetentionStats

type ReplicationOptions struct {
	AuthorityID          string
	SnapshotRoot         string
	ExportPolicyRevision string
	ExportPrefixes       []string
	Peers                []ReplicationPeer
	MaxPackBytes         int64
	MinFreeBytes         uint64
	PlanTTL              time.Duration
	RequestTimeout       time.Duration
	Concurrency          int
	Retention            ReplicationRetention
}

// ReplicationEndpoint belongs to THIS primary process and Store. It must not
// run as a separate sidecar pointed at the same live Git directory: that would
// bypass the in-process capture barrier. Serve Handler on a separately trusted
// mTLS listener, never mount it under the public Edge proxy.
// No endpoint is automatically installed on the existing primary listeners.
type ReplicationEndpoint struct {
	handler      http.Handler
	retained     *snapshotstore.Store
	cancel       context.CancelFunc
	done         chan struct{}
	mu           sync.Mutex
	retentionErr error
}

func (s *Server) NewReplicationEndpoint(cfg ReplicationOptions) (*ReplicationEndpoint, error) {
	if s == nil || s.deps == nil || s.deps.SvcDeps == nil || s.deps.Store == nil || s.deps.SvcDeps.Git != s.deps.Store {
		return nil, errors.New("replication requires the owning primary runtime")
	}
	if s.deps.Cfg.ReplicationAuthorityID != "" && s.deps.Cfg.ReplicationAuthorityID != cfg.AuthorityID {
		return nil, errors.New("replication endpoint authority differs from operator registration authority")
	}
	if s.cfg.ControlPlaneDSN != "" || s.deps.Options.authenticator != nil {
		return nil, errors.New("replication currently requires single-DB native credential authentication")
	}
	if cfg.PlanTTL == 0 {
		cfg.PlanTTL = time.Minute
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = time.Minute
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	retained, err := snapshotstore.Open(cfg.SnapshotRoot, cfg.MaxPackBytes)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*ReplicationEndpoint, error) { _ = retained.Close(); return nil, err }
	if cfg.Retention == (ReplicationRetention{}) {
		cfg.Retention = snapshotstore.DefaultRetentionPolicy()
	}
	// Cover the maximum Edge negotiation window plus preparation validity.
	if cfg.Retention.MinAge < 10*time.Minute+cfg.PlanTTL {
		return fail(errors.New("primary retention protection is shorter than read negotiation window"))
	}
	if err := retained.ConfigureRetention(cfg.Retention); err != nil {
		return fail(err)
	}
	if err := retained.ConfigureHeadroom(cfg.MinFreeBytes); err != nil {
		return fail(err)
	}
	authority, err := service.NewPrimaryReadAuthority(s.deps.SvcDeps, retained, cfg.AuthorityID, cfg.ExportPolicyRevision, cfg.ExportPrefixes, cfg.PlanTTL)
	if err != nil {
		return fail(err)
	}
	reads, err := replication.NewReadHandler(authority, middleware.OptionalTokenAuth(s.deps.SvcDeps), cfg.Peers, cfg.RequestTimeout, cfg.Concurrency)
	if err != nil {
		return fail(err)
	}
	exports, err := replication.NewExportHandler(retained, cfg.Peers, cfg.Concurrency)
	if err != nil {
		return fail(err)
	}
	warm, err := replication.NewWarmHandler(authority, cfg.AuthorityID, cfg.Peers)
	if err != nil {
		return fail(err)
	}
	// Exact dispatch only; no ServeMux path cleaning/redirect normalization.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case edgeprotocol.NodeStatusPath, edgeprotocol.WarmPath:
			warm.ServeHTTP(w, r)
		case edgeprotocol.PreparePath, edgeprotocol.RevalidatePath:
			reads.ServeHTTP(w, r)
		case edgeprotocol.ExportPath, edgeprotocol.TransferPath:
			exports.ServeHTTP(w, r)
		default:
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
		}
	})
	ctx, cancel := context.WithCancel(s.deps.SvcDeps.ServerCtx())
	endpoint := &ReplicationEndpoint{handler: handler, retained: retained, cancel: cancel, done: make(chan struct{})}
	go endpoint.maintain(ctx)
	return endpoint, nil
}

func (e *ReplicationEndpoint) Handler() http.Handler { return e.handler }

// Close after stopping/joining the peer listener. Active read leases/imports
// make Close fail rather than release process ownership prematurely.
func (e *ReplicationEndpoint) Close() error {
	e.cancel()
	<-e.done
	return e.retained.Close()
}

// RetentionError exposes the last completed background pass; it is not a
// primary-health or repository-freshness assertion.
func (e *ReplicationEndpoint) RetentionError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.retentionErr
}
func (e *ReplicationEndpoint) maintain(ctx context.Context) {
	defer close(e.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass, cancel := context.WithTimeout(ctx, time.Minute)
			_, err := e.retained.Prune(pass)
			cancel()
			e.mu.Lock()
			e.retentionErr = err
			e.mu.Unlock()
		}
	}
}
