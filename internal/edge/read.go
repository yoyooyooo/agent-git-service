package edge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/gitbackend"
)

// ReadAuthority carries the ORIGINAL credential only during each control RPC.
// It is not the node's SnapshotSource and is never stored in a mirror job.
type ReadAuthority interface {
	PrepareRead(context.Context, string, string, edgeprotocol.PrepareRead) (edgeprotocol.ReadPlan, error)
	RevalidateRead(context.Context, string, string, edgeprotocol.ReadPlan) error
}

type ReadBinding struct {
	Repository string                          `json:"repository"`
	Identity   edgeprotocol.RepositoryIdentity `json:"identity"`
}

type ReadRuntimeConfig struct {
	EdgeID       string
	Bindings     []ReadBinding
	Concurrency  int
	RecentViews  int
	RecentWindow time.Duration
}

type recentReadView struct {
	snapshot edgeprotocol.RepositorySnapshot
	seen     time.Time
}

// ReadRuntime composes the real prepare -> ensure -> revalidate -> local Git
// path. It keeps bounded credential-free view hints, NOT sessions, grants or
// cookies. Every HTTP RPC resolves the original user at the primary again.
// No primary Git download proxy is used on any read failure.
type ReadRuntime struct {
	cfg       ReadRuntimeConfig
	authority ReadAuthority
	reader    SnapshotReader
	bindings  map[string]edgeprotocol.RepositoryIdentity
	slots     chan struct{}
	admission chan struct{}
	mu        sync.Mutex
	recent    map[edgeprotocol.RepositoryIdentity][]recentReadView
}

func NewReadRuntime(cfg ReadRuntimeConfig, authority ReadAuthority, reader SnapshotReader) (*ReadRuntime, error) {
	check := edgeprotocol.RepositoryIdentity{AuthorityID: cfg.EdgeID, StoreID: "check", RepositoryID: 1, Kind: "repo"}
	if check.Validate() != nil || authority == nil || reader == nil || len(cfg.Bindings) == 0 || len(cfg.Bindings) > 256 || cfg.Concurrency < 1 || cfg.Concurrency > 32 || cfg.RecentViews < 1 || cfg.RecentViews > 16 || cfg.RecentWindow < time.Second || cfg.RecentWindow > 10*time.Minute {
		return nil, errors.New("read runtime requires explicit bindings and bounded limits")
	}
	rt := &ReadRuntime{cfg: cfg, authority: authority, reader: reader, bindings: make(map[string]edgeprotocol.RepositoryIdentity), slots: make(chan struct{}, cfg.Concurrency), admission: make(chan struct{}, cfg.Concurrency+16), recent: make(map[edgeprotocol.RepositoryIdentity][]recentReadView)}
	for _, binding := range cfg.Bindings {
		req := edgeprotocol.PrepareRead{Version: edgeprotocol.PrepareVersion, RequestID: "validate", Repository: binding.Repository, Identity: binding.Identity, Phase: "discover"}
		if req.Validate() != nil || binding.Identity.Kind != "repo" {
			return nil, errors.New("invalid read binding (live wiki is not enabled)")
		}
		if _, exists := rt.bindings[binding.Repository]; exists {
			return nil, errors.New("duplicate read repository binding")
		}
		rt.bindings[binding.Repository] = binding.Identity
	}
	return rt, nil
}

// Bound is static operator routing, not a permission or freshness check.
func (rt *ReadRuntime) Bound(repository string) bool { _, ok := rt.bindings[repository]; return ok }

func (rt *ReadRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w = readNoStoreWriter{w}
	if deadline, ok := r.Context().Deadline(); ok {
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(deadline)
		defer controller.SetReadDeadline(time.Time{})
	}
	w.Header().Set("Cache-Control", "no-store")
	traceStage(r.Context(), "read_queue")
	select {
	case rt.admission <- struct{}{}:
		defer func() { <-rt.admission }()
	default:
		writeError(w, http.StatusServiceUnavailable, "read_capacity_exceeded")
		return
	}
	select {
	case rt.slots <- struct{}{}:
		defer func() { <-rt.slots }()
	case <-r.Context().Done():
		writeError(w, http.StatusGatewayTimeout, "read_queue_deadline")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		writeError(w, http.StatusBadRequest, "invalid_git_path")
		return
	}
	locator := parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
	identity, bound := rt.bindings[locator]
	if !bound {
		writeError(w, http.StatusNotFound, "repository_not_available")
		return
	}
	if len(r.Header.Values("Authorization")) > 1 || len(r.Header.Get("Authorization")) > 16<<10 {
		writeError(w, http.StatusBadRequest, "invalid_authorization_envelope")
		return
	}
	traceStage(r.Context(), "git_parse")
	rpc, err := parseReadRequest(r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errReadBodyLimit) {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "invalid_git_read_request")
		return
	}
	authorization := r.Header.Get("Authorization")
	plan, view, err := rt.selectView(r.Context(), locator, identity, authorization, rpc)
	if err != nil {
		rt.readError(w, authorization, err)
		return
	}
	defer view.Release()
	// A cold WAN copy may outlive the short read admission. Content is not
	// authority: obtain a NEW decision with the original credential for this
	// exact retained view, never extend timestamps locally or substitute HEAD.
	if !time.Now().UTC().Before(plan.ExpiresAt.Add(-5 * time.Second)) {
		snapshot := plan.Snapshot
		plan, err = rt.prepare(r.Context(), locator, identity, authorization, "fetch", &snapshot)
		if err != nil {
			rt.readError(w, authorization, err)
			return
		}
	}
	// Never start CGI or emit any Git data based on a pre-download decision.
	traceStage(r.Context(), "revalidate")
	if err := rt.authority.RevalidateRead(r.Context(), rt.cfg.EdgeID, authorization, plan); err != nil {
		rt.readError(w, authorization, err)
		return
	}
	if err := plan.ValidateFor(rt.cfg.EdgeID, time.Now().UTC()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "read_plan_expired")
		return
	}
	if rpc.phase == "discover" {
		rt.remember(plan.Snapshot)
	}
	clean := r.Clone(r.Context())
	clean.Header = make(http.Header)
	// CGI receives no cookies, forwarded identity hints, node headers or user
	// credential. Git-Protocol is the already-validated protocol selector.
	if v := r.Header.Get("Git-Protocol"); v != "" {
		clean.Header.Set("Git-Protocol", v)
	}
	if r.Method == http.MethodPost {
		clean.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		clean.Body = io.NopCloser(bytes.NewReader(rpc.body))
		clean.ContentLength = int64(len(rpc.body))
		clean.TransferEncoding = nil
	}
	traceStage(r.Context(), "local_backend")
	backend := gitbackend.Request{ProjectRoot: view.ProjectRoot(), Repository: view.Repository(), Service: gitbackend.UploadPack, Advertise: r.Method == http.MethodGet, IsolatedRead: true}
	if r.Method == http.MethodGet && rpc.version == 2 {
		err = serveReadCapabilities(w, clean, backend)
	} else {
		err = gitbackend.Serve(w, clean, backend)
	}
	if err != nil {
		// After native Git starts it may already have emitted protocol bytes;
		// never append an apparent successful JSON/protocol suffix.
		panic(http.ErrAbortHandler)
	}
}

func (rt *ReadRuntime) prepare(ctx context.Context, locator string, identity edgeprotocol.RepositoryIdentity, authorization, phase string, snapshot *edgeprotocol.RepositorySnapshot) (edgeprotocol.ReadPlan, error) {
	traceStage(ctx, "prepare_"+phase)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return edgeprotocol.ReadPlan{}, err
	}
	req := edgeprotocol.PrepareRead{Version: edgeprotocol.PrepareVersion, RequestID: hex.EncodeToString(nonce[:]), Repository: locator, Identity: identity, Phase: phase, Snapshot: snapshot}
	plan, err := rt.authority.PrepareRead(ctx, rt.cfg.EdgeID, authorization, req)
	if err != nil {
		return plan, err
	}
	if plan.ValidateFor(rt.cfg.EdgeID, time.Now().UTC()) != nil || plan.RequestID != req.RequestID || plan.Phase != phase || plan.Snapshot.Identity != identity || snapshot != nil && plan.Snapshot != *snapshot {
		return edgeprotocol.ReadPlan{}, errors.New("read decision binding mismatch")
	}
	return plan, nil
}

func (rt *ReadRuntime) materialize(ctx context.Context, plan edgeprotocol.ReadPlan, wants []string) (SnapshotView, bool, error) {
	traceStage(ctx, "synchronize")
	view, err := rt.reader.EnsureSnapshot(ctx, plan.Snapshot)
	if err != nil {
		return nil, false, err
	}
	if view == nil {
		return nil, false, errors.New("snapshot reader returned no view")
	}
	if view.Snapshot() != plan.Snapshot || view.Manifest().Validate() != nil || view.Manifest().Snapshot != plan.Snapshot {
		view.Release()
		return nil, false, errors.New("snapshot reader substituted a view")
	}
	contains, err := view.ContainsObjects(ctx, wants)
	if err != nil || !contains {
		view.Release()
		return nil, false, err
	}
	return view, true, nil
}

func (rt *ReadRuntime) selectView(ctx context.Context, locator string, identity edgeprotocol.RepositoryIdentity, authorization string, rpc gitReadRequest) (edgeprotocol.ReadPlan, SnapshotView, error) {
	// A fetch uses its own actual want OIDs and fresh authority over a bounded
	// recent view, not whichever HEAD happened to be discovered most recently
	// by another client. Plans/credentials are never reused between requests.
	if rpc.phase == "fetch" {
		for _, candidate := range rt.candidates(identity) {
			plan, err := rt.prepare(ctx, locator, identity, authorization, "fetch", &candidate)
			if err != nil {
				return edgeprotocol.ReadPlan{}, nil, err // No denial-to-stale fallback.
			}
			view, contains, err := rt.materialize(ctx, plan, rpc.wants)
			if err != nil {
				return edgeprotocol.ReadPlan{}, nil, err
			}
			if contains {
				return plan, view, nil
			}
		}
	}
	// Every discovery is primary-current; never answered by a remembered plan.
	// A restarted/new Edge can fetch an OID present in the current view. If
	// only an expired historical view contained it, return an explicit error.
	plan, err := rt.prepare(ctx, locator, identity, authorization, "discover", nil)
	if err != nil {
		return edgeprotocol.ReadPlan{}, nil, err
	}
	view, contains, err := rt.materialize(ctx, plan, rpc.wants)
	if err != nil {
		return edgeprotocol.ReadPlan{}, nil, err
	}
	if !contains {
		return edgeprotocol.ReadPlan{}, nil, edgeprotocol.ErrReadUnavailable
	}
	if rpc.phase == "fetch" {
		fresh, err := rt.prepare(ctx, locator, identity, authorization, "fetch", &plan.Snapshot)
		if err != nil {
			view.Release()
			return edgeprotocol.ReadPlan{}, nil, err
		}
		plan = fresh
	}
	return plan, view, nil
}

func (rt *ReadRuntime) remember(snapshot edgeprotocol.RepositorySnapshot) {
	now := time.Now()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	views := []recentReadView{{snapshot: snapshot, seen: now}}
	for _, entry := range rt.recent[snapshot.Identity] {
		if len(views) >= rt.cfg.RecentViews {
			break
		}
		if entry.snapshot != snapshot && now.Sub(entry.seen) < rt.cfg.RecentWindow {
			views = append(views, entry)
		}
	}
	rt.recent[snapshot.Identity] = views
}

func (rt *ReadRuntime) candidates(identity edgeprotocol.RepositoryIdentity) []edgeprotocol.RepositorySnapshot {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	now := time.Now()
	var snapshots []edgeprotocol.RepositorySnapshot
	var retained []recentReadView
	for _, entry := range rt.recent[identity] {
		if now.Sub(entry.seen) < rt.cfg.RecentWindow {
			snapshots = append(snapshots, entry.snapshot)
			retained = append(retained, entry)
		}
	}
	rt.recent[identity] = retained
	return snapshots
}

// Keep even native CGI advertisements out of intermediary caches: they are
// produced after per-request authorization and may describe private refs.
type readNoStoreWriter struct{ http.ResponseWriter }

func (w readNoStoreWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w readNoStoreWriter) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.ResponseWriter.WriteHeader(status)
}
func (w readNoStoreWriter) Write(p []byte) (int, error) {
	w.Header().Set("Cache-Control", "no-store")
	return w.ResponseWriter.Write(p)
}
func (w readNoStoreWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (rt *ReadRuntime) readError(w http.ResponseWriter, authorization string, err error) {
	status, code := http.StatusServiceUnavailable, "read_unavailable"
	var control *ReadControlError
	if errors.As(err, &control) {
		switch control.Status {
		case http.StatusUnauthorized:
			status, code = http.StatusUnauthorized, "requires_authentication"
		case http.StatusForbidden:
			status, code = http.StatusForbidden, "read_denied"
			if authorization == "" {
				status, code = http.StatusUnauthorized, "requires_authentication"
			}
		}
	}
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
	}
	writeError(w, status, code)
}
