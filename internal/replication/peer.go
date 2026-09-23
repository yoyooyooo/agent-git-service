package replication

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func makePeerRegistry(grants []PeerGrant) (map[string]PeerGrant, error) {
	peers := make(map[string]PeerGrant)
	for _, grant := range grants {
		probe := edgeprotocol.RepositoryIdentity{AuthorityID: grant.EdgeID, StoreID: "peer", RepositoryID: 1, Kind: "repo"}
		if probe.Validate() != nil || len(grant.SPKISHA256) != 64 || strings.Trim(grant.SPKISHA256, "0123456789abcdef") != "" || len(grant.Stores) == 0 {
			return nil, errors.New("invalid explicit peer grant")
		}
		if _, exists := peers[grant.SPKISHA256]; exists {
			return nil, errors.New("ambiguous peer key")
		}
		for _, identity := range grant.Stores {
			if err := identity.Validate(); err != nil {
				return nil, err
			}
		}
		grant.Stores = append([]edgeprotocol.RepositoryIdentity{}, grant.Stores...)
		peers[grant.SPKISHA256] = grant
	}
	return peers, nil
}

func requestPeer(r *http.Request, peers map[string]PeerGrant) (PeerGrant, bool) {
	if r.TLS == nil || !r.TLS.HandshakeComplete || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return PeerGrant{}, false
	}
	// A keep-alive connection can outlive its original TLS handshake. Recheck
	// the verified chain's lifetime for every RPC; expiry cannot be bypassed
	// merely by keeping the old connection busy.
	now := time.Now()
	valid := false
	for _, chain := range r.TLS.VerifiedChains {
		chainValid := len(chain) > 0
		for _, cert := range chain {
			if cert == nil || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
				chainValid = false
				break
			}
		}
		if chainValid {
			valid = true
			break
		}
	}
	if !valid {
		return PeerGrant{}, false
	}
	peer, ok := peers[CertificateKeyID(r.TLS.PeerCertificates[0])]
	return peer, ok
}

func peerAllows(peer PeerGrant, identity edgeprotocol.RepositoryIdentity) bool {
	for _, granted := range peer.Stores {
		if granted == identity {
			return true
		}
	}
	return false
}
