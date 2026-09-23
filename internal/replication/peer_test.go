package replication

import (
	"crypto/tls"
	"crypto/x509"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPeerCertificateExpiryIsCheckedOnEveryRequest(t *testing.T) {
	now := time.Now()
	leaf := &x509.Certificate{RawSubjectPublicKeyInfo: []byte("peer-test-key"), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	issuer := &x509.Certificate{NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	grants := map[string]PeerGrant{CertificateKeyID(leaf): {EdgeID: "edge-1"}}
	r := httptest.NewRequest("POST", "https://peer.test/", nil)
	r.TLS = &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf, issuer}}}
	if _, ok := requestPeer(r, grants); !ok {
		t.Fatal("valid registered peer rejected")
	}
	// Same request connection state and key, but a now-expired certificate:
	// the original verified-chain marker must not override its current lifetime.
	leaf.NotAfter = now.Add(-time.Second)
	if _, ok := requestPeer(r, grants); ok {
		t.Fatal("expired leaf remained authorized")
	}
	leaf.NotAfter = now.Add(time.Hour)
	issuer.NotAfter = now.Add(-time.Second)
	if _, ok := requestPeer(r, grants); ok {
		t.Fatal("expired issuer remained authorized")
	}
	issuer.NotAfter = now.Add(time.Hour)
	leaf.NotBefore = now.Add(time.Hour)
	if _, ok := requestPeer(r, grants); ok {
		t.Fatal("not-yet-valid peer remained authorized")
	}
	leaf.NotBefore = now.Add(-time.Hour)
	delete(grants, CertificateKeyID(leaf))
	if _, ok := requestPeer(r, grants); ok {
		t.Fatal("removed key remained authorized")
	}
}
