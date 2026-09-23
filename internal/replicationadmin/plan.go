package replicationadmin

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

const PeerPlanVersion = "ags.replication.peer-plan.v1"

type PeerGrant struct {
	EdgeID     string                            `json:"edge_id"`
	SPKISHA256 string                            `json:"spki_sha256"`
	Stores     []edgeprotocol.RepositoryIdentity `json:"stores"`
}

type ReadBinding struct {
	Repository string                          `json:"repository"`
	Identity   edgeprotocol.RepositoryIdentity `json:"identity"`
}

// PeerPlan is a credential-free configuration fragment. Applied is always
// false: generating it neither edits runtime config nor grants node access.
// Certificates/CA private keys are supplied by the deployment's PKI, not AGS.
type PeerPlan struct {
	Version              string        `json:"version"`
	Applied              bool          `json:"applied"`
	Peer                 PeerGrant     `json:"peer"`
	Bindings             []ReadBinding `json:"bindings"`
	CertificateNotBefore time.Time     `json:"certificate_not_before"`
	CertificateNotAfter  time.Time     `json:"certificate_not_after"`
}

func BuildPeerPlan(edgeID string, certificatePEM, caPEM []byte, registrations []edgeprotocol.Registration, now time.Time) (PeerPlan, error) {
	probe := edgeprotocol.RepositoryIdentity{AuthorityID: edgeID, StoreID: "peer", RepositoryID: 1, Kind: "repo"}
	if probe.Validate() != nil || len(registrations) == 0 || len(registrations) > 256 || now.IsZero() {
		return PeerPlan{}, errors.New("peer plan needs a valid node and bounded registered repositories")
	}
	chain, err := certificates(certificatePEM)
	if err != nil {
		return PeerPlan{}, err
	}
	roots, err := certificates(caPEM)
	if err != nil {
		return PeerPlan{}, err
	}
	leaf := chain[0]
	explicitClient := false
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageClientAuth {
			explicitClient = true
		}
	}
	if leaf.IsCA || !explicitClient {
		return PeerPlan{}, errors.New("peer requires an explicit client-auth leaf certificate")
	}
	pool, intermediate := x509.NewCertPool(), x509.NewCertPool()
	for _, root := range roots {
		if !root.IsCA || !root.BasicConstraintsValid {
			return PeerPlan{}, errors.New("peer trust file must contain CA certificates only")
		}
		pool.AddCert(root)
	}
	for _, issuer := range chain[1:] {
		intermediate.AddCert(issuer)
	}
	// Verify defaults to serverAuth; clientAuth must be requested explicitly.
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediate, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return PeerPlan{}, errors.New("peer certificate is not currently trusted for client authentication")
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	plan := PeerPlan{Version: PeerPlanVersion, Peer: PeerGrant{EdgeID: edgeID, SPKISHA256: hex.EncodeToString(sum[:])}, CertificateNotBefore: leaf.NotBefore.UTC(), CertificateNotAfter: leaf.NotAfter.UTC()}
	seen := make(map[edgeprotocol.RepositoryIdentity]bool)
	names := make(map[string]bool)
	authority := ""
	for _, registered := range registrations {
		if registered.Validate() != nil || registered.Identity == nil {
			return PeerPlan{}, errors.New("peer plan requires successfully registered repository receipts")
		}
		if authority == "" {
			authority = registered.AuthorityID
		}
		if registered.AuthorityID != authority || names[registered.Repository] {
			return PeerPlan{}, errors.New("ambiguous or mixed-authority peer plan")
		}
		names[registered.Repository] = true
		identity := *registered.Identity
		if !seen[identity] {
			plan.Peer.Stores = append(plan.Peer.Stores, identity)
			seen[identity] = true
		}
		plan.Bindings = append(plan.Bindings, ReadBinding{Repository: registered.Repository, Identity: identity})
	}
	return plan, nil
}

func certificates(data []byte) ([]*x509.Certificate, error) {
	if len(data) == 0 || len(data) > 1<<20 {
		return nil, errors.New("invalid certificate file size")
	}
	var result []*x509.Certificate
	for len(bytes.TrimSpace(data)) > 0 {
		data = bytes.TrimSpace(data)
		if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("certificate file contains unexpected material")
		}
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(result) == 16 {
			return nil, errors.New("invalid bounded certificate chain")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid certificate")
		}
		result, data = append(result, cert), rest
	}
	if len(result) == 0 {
		return nil, errors.New("certificate file is empty")
	}
	return result, nil
}
