package replicationadmin

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test peer CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func testLeaf(t *testing.T, ca *x509.Certificate, key *ecdsa.PrivateKey, usage x509.ExtKeyUsage, offset time.Duration) []byte {
	t.Helper()
	leaf, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(offset)
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "same-node-name"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &leaf.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func TestPeerPlanVerifiesClientCertificateAndExactBindings(t *testing.T) {
	ca, key, caPEM := testCA(t)
	cert := testLeaf(t, ca, key, x509.ExtKeyUsageClientAuth, 0)
	registration := sampleRegistration()
	plan, err := BuildPeerPlan("edge-1", cert, caPEM, []edgeprotocol.Registration{registration}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applied || len(plan.Peer.SPKISHA256) != 64 || plan.Peer.Stores[0] != *registration.Identity || plan.Bindings[0].Identity != *registration.Identity || plan.Bindings[0].Repository != registration.Repository {
		t.Fatal("wrong enrollment plan")
	}
	rotated, err := BuildPeerPlan("edge-1", testLeaf(t, ca, key, x509.ExtKeyUsageClientAuth, 0), caPEM, []edgeprotocol.Registration{registration}, time.Now())
	if err != nil || rotated.Peer.SPKISHA256 == plan.Peer.SPKISHA256 {
		t.Fatal("same subject was treated as same key", err)
	}
	_, _, otherCA := testCA(t)
	for _, tc := range []struct {
		name     string
		cert, ca []byte
	}{
		{"untrusted", cert, otherCA}, {"expired", testLeaf(t, ca, key, x509.ExtKeyUsageClientAuth, -2*time.Hour), caPEM}, {"future", testLeaf(t, ca, key, x509.ExtKeyUsageClientAuth, 2*time.Hour), caPEM},
		{"server-only", testLeaf(t, ca, key, x509.ExtKeyUsageServerAuth, 0), caPEM}, {"CA-as-node", caPEM, caPEM}, {"whitespace", []byte(" \n"), caPEM}, {"private-material", append(append([]byte{}, cert...), []byte("-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----")...), caPEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildPeerPlan("edge-1", tc.cert, tc.ca, []edgeprotocol.Registration{registration}, time.Now()); err == nil {
				t.Fatal("unsafe certificate admitted")
			}
		})
	}
	unregistered := registration
	unregistered.Identity = nil
	mixed := sampleRegistration()
	mixed.Repository = "owner/other"
	mixed.AuthorityID = "other"
	mixed.Identity.AuthorityID = "other"
	for _, rows := range [][]edgeprotocol.Registration{{unregistered}, {registration, registration}, {registration, mixed}} {
		if _, err := BuildPeerPlan("edge-1", cert, caPEM, rows, time.Now()); err == nil {
			t.Fatal("unsafe registration set admitted")
		}
	}
}
func TestPeerPlanCommandProducesOnlyUnappliedConfiguration(t *testing.T) {
	ca, key, caPEM := testCA(t)
	cert := testLeaf(t, ca, key, x509.ExtKeyUsageClientAuth, 0)
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	registered, _ := json.Marshal(sampleRegistration())
	registrationPath := write("registered.json", registered)
	args := []string{"peer-plan", "--edge-id", "edge-1", "--certificate", write("node.pem", cert), "--ca-file", write("ca.pem", caPEM), "--registration", registrationPath}
	var out bytes.Buffer
	if err := Run(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "PRIVATE KEY") || strings.Contains(out.String(), "CERTIFICATE") {
		t.Fatal("plan embedded certificate/key material")
	}
	var plan PeerPlan
	if err := edgeprotocol.DecodeControl(&out, &plan); err != nil || plan.Version != PeerPlanVersion || plan.Applied || len(plan.Bindings) != 1 {
		t.Fatal("invalid plan", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 3 {
		t.Fatal("command wrote configuration or credentials", err)
	}
}
