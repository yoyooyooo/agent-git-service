// Package replication provides the primary's peer export and read-control
// adapters. The optional owning-primary factory assembles them without changing
// default listeners. This export endpoint only transmits retained views: it
// never captures Git or grants end-user access. Read preparation is handled
// separately, with original-user authentication in addition to peer identity.
package replication

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

// PeerGrant is operator-owned. mTLS chain validation AND a registered public
// key AND an exact authority/store/repository/kind grant are required. There is
// no hostname, common-name, header, same-name repository or wildcard fallback.
type PeerGrant struct {
	EdgeID     string
	SPKISHA256 string
	Stores     []edgeprotocol.RepositoryIdentity
}

type ExportHandler struct {
	store *snapshotstore.Store
	peers map[string]PeerGrant
	slots chan struct{}
}

func CertificateKeyID(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func NewExportHandler(store *snapshotstore.Store, grants []PeerGrant, concurrency int) (*ExportHandler, error) {
	if store == nil || concurrency < 1 || concurrency > 16 {
		return nil, errors.New("export store and bounded concurrency are required")
	}
	peers, err := makePeerRegistry(grants)
	if err != nil {
		return nil, err
	}
	return &ExportHandler{store: store, peers: peers, slots: make(chan struct{}, concurrency)}, nil
}

func (h *ExportHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.TLS == nil || !r.TLS.HandshakeComplete || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		exportError(w, http.StatusUnauthorized, "verified_peer_required")
		return
	}
	peer, registered := requestPeer(r, h.peers)
	if !registered {
		exportError(w, http.StatusForbidden, "peer_not_registered")
		return
	}
	if r.Method != http.MethodPost || (r.URL.Path != edgeprotocol.ExportPath && r.URL.Path != edgeprotocol.TransferPath) || r.URL.RawQuery != "" || r.URL.RawPath != "" {
		exportError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Content-Encoding") != "" {
		exportError(w, http.StatusBadRequest, "invalid_replication_envelope")
		return
	}
	var descriptor edgeprotocol.RepositorySnapshot
	var requestedBase *edgeprotocol.RepositorySnapshot
	var err error
	if r.URL.Path == edgeprotocol.TransferPath {
		request, decodeErr := edgeprotocol.DecodeTransferRequest(r.Body)
		descriptor, requestedBase, err = request.Target, request.Base, decodeErr
	} else {
		descriptor, err = edgeprotocol.DecodeSnapshot(r.Body)
	}
	if err != nil {
		exportError(w, http.StatusBadRequest, "invalid_snapshot_descriptor")
		return
	}
	if !peerAllows(peer, descriptor.Identity) || requestedBase != nil && !peerAllows(peer, requestedBase.Identity) {
		exportError(w, http.StatusForbidden, "store_not_granted")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		exportError(w, http.StatusServiceUnavailable, "export_capacity_exceeded")
		return
	}
	view, err := h.store.Acquire(r.Context(), descriptor)
	if errors.Is(err, snapshotstore.ErrMissing) {
		exportError(w, http.StatusNotFound, "snapshot_not_retained")
		return
	}
	if err != nil {
		exportError(w, http.StatusServiceUnavailable, "snapshot_unavailable")
		return
	}
	defer view.Release()
	if r.URL.Path == edgeprotocol.TransferPath {
		var base *snapshotstore.Lease
		if requestedBase != nil {
			base, err = h.store.Acquire(r.Context(), *requestedBase)
			if err != nil && !errors.Is(err, snapshotstore.ErrMissing) {
				exportError(w, http.StatusServiceUnavailable, "base_unavailable")
				return
			}
			if base != nil {
				defer base.Release()
			}
		}
		header := edgeprotocol.TransferHeader{Version: edgeprotocol.TransferVersion, Manifest: view.Manifest()}
		if base != nil {
			descriptor := base.Snapshot()
			header.Base = &descriptor
		}
		w.Header().Set("Content-Type", edgeprotocol.TransferContentType)
		if edgeprotocol.WriteTransferHeader(w, header) != nil {
			panic(http.ErrAbortHandler)
		}
		if base == nil {
			err = view.WritePack(r.Context(), w)
		} else {
			err = view.WriteIncrementalPack(r.Context(), base, w)
		}
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	w.Header().Set("Content-Type", edgeprotocol.ExportContentType)
	if err := edgeprotocol.WriteExportHeader(w, view.Manifest()); err != nil {
		panic(http.ErrAbortHandler)
	}
	if err := view.WritePack(r.Context(), w); err != nil {
		// A partial pack must terminate abnormally, not end as a successful
		// empty or truncated 200 response. Consumer also verifies pack/closure.
		panic(http.ErrAbortHandler)
	}
}

func exportError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}
