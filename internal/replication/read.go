package replication

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

// ReadAuthority is implemented by the primary service. The original-user
// middleware must be the SAME token/Session resolver as native Git, without
// widening the delegated REST route allowlist. Neither peer nor user alone
// can pass this handler. The control plane never exports filesystem paths.
type ReadAuthority interface {
	PrepareRead(context.Context, string, edgeprotocol.PrepareRead) (edgeprotocol.ReadPlan, error)
	RevalidateRead(context.Context, string, edgeprotocol.ReadPlan) error
}

type ReadHandler struct {
	authority    ReadAuthority
	authenticate func(http.Handler) http.Handler
	peers        map[string]PeerGrant
	slots        chan struct{}
	timeout      time.Duration
}

func NewReadHandler(authority ReadAuthority, originalAuth func(http.Handler) http.Handler, peers []PeerGrant, timeout time.Duration, concurrency int) (*ReadHandler, error) {
	if authority == nil || originalAuth == nil || timeout < time.Second || timeout > 5*time.Minute || concurrency < 1 || concurrency > 16 {
		return nil, errors.New("primary read authority, original authentication and bounded execution are required")
	}
	registry, err := makePeerRegistry(peers)
	if err != nil {
		return nil, err
	}
	return &ReadHandler{authority: authority, authenticate: originalAuth, peers: registry, slots: make(chan struct{}, concurrency), timeout: timeout}, nil
}

func (h *ReadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	peer, ok := requestPeer(r, h.peers)
	if !ok {
		exportError(w, http.StatusForbidden, "registered_peer_required")
		return
	}
	if r.Method != http.MethodPost || r.URL.RawPath != "" || r.URL.RawQuery != "" || (r.URL.Path != edgeprotocol.PreparePath && r.URL.Path != edgeprotocol.RevalidatePath) {
		exportError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Content-Encoding") != "" {
		exportError(w, http.StatusBadRequest, "invalid_read_envelope")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	r = r.WithContext(ctx)
	var request edgeprotocol.PrepareRead
	var plan edgeprotocol.ReadPlan
	if r.URL.Path == edgeprotocol.PreparePath {
		if edgeprotocol.DecodeControl(r.Body, &request) != nil || request.Validate() != nil {
			exportError(w, 400, "invalid_prepare")
			return
		}
		if !peerAllows(peer, request.Identity) {
			exportError(w, 403, "store_not_granted")
			return
		}
	} else {
		if edgeprotocol.DecodeControl(r.Body, &plan) != nil || plan.ValidateFor(peer.EdgeID, time.Now().UTC()) != nil {
			exportError(w, 400, "invalid_read_plan")
			return
		}
		if !peerAllows(peer, plan.Snapshot.Identity) {
			exportError(w, 403, "store_not_granted")
			return
		}
	}
	// Do not trust any caller-provided forwarding/identity hints. Only the
	// original Authorization is passed to the existing token resolver.
	clean := r.Clone(ctx)
	clean.Header = make(http.Header)
	clean.Header.Set("Authorization", r.Header.Get("Authorization"))
	finish := http.HandlerFunc(func(w http.ResponseWriter, authenticated *http.Request) {
		if err := h.authority.RevalidateRead(authenticated.Context(), peer.EdgeID, plan); err != nil {
			readError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(plan)
	})
	if r.URL.Path == edgeprotocol.RevalidatePath {
		h.authenticate(http.HandlerFunc(func(w http.ResponseWriter, authenticated *http.Request) {
			select {
			case h.slots <- struct{}{}:
				defer func() { <-h.slots }()
			default:
				exportError(w, http.StatusServiceUnavailable, "read_revalidate_busy")
				return
			}
			finish.ServeHTTP(w, authenticated)
		})).ServeHTTP(w, clean)
		return
	}
	h.authenticate(http.HandlerFunc(func(w http.ResponseWriter, authenticated *http.Request) {
		// Authentication precedes potentially expensive waits/capture. Queue
		// overflow is explicit rather than allowing an unbounded waiter set.
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		default:
			exportError(w, http.StatusServiceUnavailable, "read_prepare_busy")
			return
		}
		var err error
		plan, err = h.authority.PrepareRead(authenticated.Context(), peer.EdgeID, request)
		if err != nil {
			readError(w, err)
			return
		}
		// Resolve the ORIGINAL token afresh after capture. Do not reuse the
		// first authenticated context/cache if a credential was revoked while
		// waiting. This also catches native token deletion, not only grants.
		h.authenticate(finish).ServeHTTP(w, clean)
	})).ServeHTTP(w, clean)
}

func readError(w http.ResponseWriter, err error) {
	if errors.Is(err, edgeprotocol.ErrReadDenied) {
		exportError(w, http.StatusForbidden, "read_denied")
		return
	}
	exportError(w, http.StatusServiceUnavailable, "read_unavailable")
}
