package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

// managedReplication shares this primary's listeners and lifecycle. Drain tracks
// handlers, not just net/http.Serve's return: canceled pack producers must stop
// before closing the process-owned retained store.
type managedReplication struct {
	endpoint        *ReplicationEndpoint
	server          *http.Server
	cancel          context.CancelFunc
	mu              sync.Mutex
	stopping        bool
	active          int
	drained         chan struct{}
	transferTimeout time.Duration
}

func (s *Server) configureReplication(loaded *loadedReplication) (*managedReplication, error) {
	if loaded == nil {
		return nil, nil
	}
	endpoint, err := s.NewReplicationEndpoint(loaded.options)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*managedReplication, error) { _ = endpoint.Close(); return nil, err }
	ctx, cancel := context.WithTimeout(s.deps.SrvCtx, 30*time.Second)
	defer cancel()
	// Check each actual source as repositories may be separately mounted.
	seen := make(map[edgeprotocol.RepositoryIdentity]bool)
	for _, peer := range loaded.options.Peers {
		for _, identity := range peer.Stores {
			if seen[identity] {
				continue
			}
			seen[identity] = true
			source, err := s.deps.SvcDeps.ReplicationSourcePath(ctx, identity)
			if err != nil {
				return fail(err)
			}
			if _, err := snapshotstore.CheckCaptureFilesystem(source, loaded.options.SnapshotRoot, loaded.file.MinFreeBytes); err != nil {
				return fail(err)
			}
		}
	}
	if len(seen) == 0 {
		return fail(errors.New("replication has no provisioned stores"))
	}
	// Also reject a snapshot root nested in the Git storage tree itself.
	root, err := s.deps.Store.RepoRoot(ctx)
	if err != nil {
		return fail(err)
	}
	report, err := snapshotstore.CheckCaptureFilesystem(root, loaded.options.SnapshotRoot, loaded.file.MinFreeBytes)
	if err != nil {
		return fail(err)
	}
	runtimeCtx, runtimeCancel := context.WithCancel(s.deps.SrvCtx)
	managed := &managedReplication{endpoint: endpoint, cancel: runtimeCancel, drained: make(chan struct{}), transferTimeout: loaded.transferTimeout}
	managed.server = &http.Server{
		Addr: loaded.file.ListenAddr, Handler: managed,
		TLSConfig: loaded.tls, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: loaded.options.RequestTimeout, WriteTimeout: loaded.transferTimeout,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return runtimeCtx },
	}
	slog.Info("replication startup preflight passed", "stores", len(seen), "hardlink_verified", report.HardlinkVerified, "available_bytes", report.AvailableBytes, "minimum_bytes", report.MinimumBytes)
	return managed, nil
}

func (m *managedReplication) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "replication stopping", http.StatusServiceUnavailable)
		return
	}
	m.active++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.active--
		if m.stopping && m.active == 0 {
			close(m.drained)
		}
		m.mu.Unlock()
	}()
	if (r.URL.Path == edgeprotocol.ExportPath || r.URL.Path == edgeprotocol.TransferPath) && m.transferTimeout > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), m.transferTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}
	m.endpoint.Handler().ServeHTTP(w, r)
}

func (m *managedReplication) beginDrain() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.stopping {
		m.stopping = true
		if m.active == 0 {
			close(m.drained)
		}
	}
}

// Close may be retried after a caller deadline. Never release storage ownership
// while a handler may still use it; process exit is safer than concurrent owners.
func (m *managedReplication) close(ctx context.Context) error {
	m.beginDrain()
	m.cancel()
	select {
	case <-m.drained:
		return m.endpoint.Close()
	default:
	}
	select {
	case <-m.drained:
		return m.endpoint.Close()
	case <-ctx.Done():
		return errors.New("replication handlers have not drained")
	}
}
