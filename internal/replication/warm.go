package replication

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

type WarmingAuthority interface {
	WarmSnapshot(context.Context, edgeprotocol.RepositoryIdentity) (edgeprotocol.RepositorySnapshot, error)
}

type WarmHandler struct {
	authority WarmingAuthority
	id        string
	peers     map[string]PeerGrant
	slot      chan struct{}
}

func NewWarmHandler(authority WarmingAuthority, id string, grants []PeerGrant) (*WarmHandler, error) {
	probe := edgeprotocol.RepositoryIdentity{AuthorityID: id, StoreID: "warm", RepositoryID: 1, Kind: "repo"}
	if authority == nil || probe.Validate() != nil {
		return nil, errors.New("warm authority required")
	}
	peers, err := makePeerRegistry(grants)
	if err != nil {
		return nil, err
	}
	return &WarmHandler{authority: authority, id: id, peers: peers, slot: make(chan struct{}, 1)}, nil
}
func (h *WarmHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	peer, ok := requestPeer(r, h.peers)
	if !ok {
		exportError(w, 403, "registered_peer_required")
		return
	}
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Content-Encoding") != "" {
		exportError(w, 400, "invalid_node_envelope")
		return
	}
	if r.URL.Path == edgeprotocol.NodeStatusPath && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(edgeprotocol.NodeStatus{Version: edgeprotocol.NodeVersion, EdgeID: peer.EdgeID, AuthorityID: h.id, Stores: len(peer.Stores)})
		return
	}
	if r.URL.Path != edgeprotocol.WarmPath || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
		exportError(w, 404, "not_found")
		return
	}
	var req edgeprotocol.WarmRequest
	if edgeprotocol.DecodeControl(r.Body, &req) != nil || req.Version != edgeprotocol.NodeVersion || req.Identity.Validate() != nil {
		exportError(w, 400, "invalid_warm_request")
		return
	}
	if req.Identity.AuthorityID != h.id || !peerAllows(peer, req.Identity) {
		exportError(w, 403, "store_not_granted")
		return
	}
	select {
	case h.slot <- struct{}{}:
		defer func() { <-h.slot }()
	default:
		exportError(w, 503, "warm_busy")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	snapshot, err := h.authority.WarmSnapshot(ctx, req.Identity)
	if err != nil {
		readError(w, err)
		return
	}
	if snapshot.Validate() != nil || snapshot.Identity != req.Identity {
		exportError(w, 503, "invalid_warm_result")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(edgeprotocol.WarmResponse{Version: edgeprotocol.NodeVersion, Snapshot: snapshot, ObservedAt: time.Now().UTC()})
}
