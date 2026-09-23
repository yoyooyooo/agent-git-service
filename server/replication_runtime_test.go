package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/replication"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

type replicationRuntimeFixture struct {
	root     string
	cfg      config.Config
	file     ReplicationFileConfig
	path     string
	identity ReplicationIdentity
	pool     *x509.CertPool
	client   tls.Certificate
}

func runtimeCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, client bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "replication-test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if client {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return cert, certPEM, keyPEM
}

func newReplicationRuntimeFixture(t *testing.T) *replicationRuntimeFixture {
	t.Helper()
	f := &replicationRuntimeFixture{root: t.TempDir()}
	public := allocateLoopbackAddr(t)
	_, port, _ := net.SplitHostPort(public)
	f.cfg = config.Config{DBdsn: "file:" + filepath.Join(f.root, "primary.db"), GitRepoDir: filepath.Join(f.root, "repos"), BaseURL: "http://" + public, Port: port, ListenMode: "production", Environment: "production"}
	seed, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner := db.User{Login: "peer-owner", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := seed.deps.DB.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithUser(context.Background(), owner)
	repo, err := seed.deps.SvcDeps.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: owner.Login, Name: "project", Private: true, AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	f.identity, err = seed.deps.SvcDeps.ProvisionReplicationIdentity(ctx, "runtime-primary", repo.FullName, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.deps.DB.Create(&db.Token{UserID: owner.ID, Value: "runtime-original-test-only"}).Error; err != nil {
		t.Fatal(err)
	}
	cleanupFailedBootstrap(seed.deps)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "runtime-test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f.pool = x509.NewCertPool()
	f.pool.AddCert(ca)
	f.client, _, _ = runtimeCertificate(t, ca, key, true)
	_, serverPEM, serverKey := runtimeCertificate(t, ca, key, false)
	for name, data := range map[string][]byte{"ca.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "server.pem": serverPEM, "server-key.pem": serverKey} {
		if err := os.WriteFile(filepath.Join(f.root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	clientLeaf, err := x509.ParseCertificate(f.client.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	f.file = ReplicationFileConfig{Version: ReplicationConfigVersion, AuthorityID: "runtime-primary", ListenAddr: allocateLoopbackAddr(t), ServerName: "127.0.0.1", CAFile: "ca.pem", CertificateFile: "server.pem", PrivateKeyFile: "server-key.pem", SnapshotRoot: "retained", ManagedWritesOnly: true, MinFreeBytes: 1, ExportPolicyRevision: "test-v1", ExportPrefixes: []string{"refs/heads/", "refs/tags/"}, MaxPackBytes: 16 << 20, Peers: []ReplicationFilePeer{{EdgeID: "edge-1", SPKISHA256: replication.CertificateKeyID(clientLeaf), Stores: []ReplicationIdentity{f.identity}}}}
	f.path = filepath.Join(f.root, "replication.json")
	f.save(t)
	f.cfg.ReplicationConfigFile = f.path
	return f
}
func (f *replicationRuntimeFixture) save(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *replicationRuntimeFixture) peer(t *testing.T) *edge.PeerClient {
	t.Helper()
	peer, err := edge.NewPeerClient(edge.PeerClientConfig{URL: "https://" + f.file.ListenAddr, RootCAs: f.pool, Certificate: f.client, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.CloseIdleConnections)
	return peer
}
func (f *replicationRuntimeFixture) request() edgeprotocol.PrepareRead {
	return edgeprotocol.PrepareRead{Version: edgeprotocol.PrepareVersion, RequestID: "listener-test", Repository: "peer-owner/project", Identity: f.identity, Phase: "discover"}
}

func TestConfiguredPrimaryReplicationListenerLifecycle(t *testing.T) {
	f := newReplicationRuntimeFixture(t)
	srv, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupFailedBootstrap(srv.deps) })
	if len(srv.deps.Servers) != 2 || srv.deps.Replication == nil {
		t.Fatal("peer not attached to primary")
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	peer := f.peer(t)
	plan, err := peer.PrepareRead(context.Background(), "edge-1", "Bearer runtime-original-test-only", f.request())
	if err != nil || plan.Snapshot.Identity != f.identity {
		t.Fatalf("real mTLS primary read failed: %v", err)
	}
	if _, err := peer.PrepareRead(context.Background(), "edge-1", "Bearer invalid-test-token", f.request()); err == nil {
		t.Fatal("node certificate replaced user authority")
	}
	plain := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.pool, MinVersion: tls.VersionTLS13}}, Timeout: 2 * time.Second}
	defer plain.CloseIdleConnections()
	if resp, err := plain.Get("https://" + f.file.ListenAddr + edgeprotocol.PreparePath); err == nil {
		resp.Body.Close()
		t.Fatal("peer listener accepted missing client certificate")
	}
	public := httptest.NewRecorder()
	srv.Handler().ServeHTTP(public, httptest.NewRequest("POST", f.cfg.BaseURL+edgeprotocol.PreparePath, nil))
	if public.Code != 404 {
		t.Fatalf("private control exposed publicly: %d", public.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", f.file.ListenAddr)
	if err != nil {
		t.Fatal("peer listener leaked", err)
	}
	ln.Close()
	reopened, err := snapshotstore.Open(filepath.Join(f.root, "retained"), 0)
	if err != nil {
		t.Fatal("retained ownership leaked", err)
	}
	reopened.Close()
}

func TestConfiguredReplicationFailsClosedAndReleasesOwnership(t *testing.T) {
	for _, name := range []string{"stale identity", "headroom", "overlap", "occupied port"} {
		t.Run(name, func(t *testing.T) {
			f := newReplicationRuntimeFixture(t)
			switch name {
			case "stale identity":
				f.file.Peers[0].Stores[0].StoreID = strings.Repeat("a", 32) + "-repo"
			case "headroom":
				f.file.MinFreeBytes = 1 << 60
			case "overlap":
				f.file.SnapshotRoot = "repos/retained"
			}
			f.save(t)
			var occupied net.Listener
			if name == "occupied port" {
				var err error
				occupied, err = net.Listen("tcp", f.file.ListenAddr)
				if err != nil {
					t.Fatal(err)
				}
				defer occupied.Close()
			}
			srv, err := New(f.cfg)
			if name == "occupied port" {
				if err != nil {
					t.Fatal(err)
				}
				if err := srv.Start(); err == nil {
					cleanupFailedBootstrap(srv.deps)
					t.Fatal("partial listener start succeeded")
				}
				if srv.started || len(srv.listeners) != 0 {
					t.Fatal("partial listeners retained")
				}
				cleanupFailedBootstrap(srv.deps)
			} else if err == nil {
				cleanupFailedBootstrap(srv.deps)
				t.Fatal("unsafe replication configuration accepted")
			}
			root := filepath.Join(f.root, f.file.SnapshotRoot)
			store, err := snapshotstore.Open(root, 0)
			if err != nil {
				t.Fatal("failed construction leaked snapshot lock", err)
			}
			store.Close()
			ln, err := net.Listen("tcp", strings.TrimPrefix(f.cfg.BaseURL, "http://"))
			if err != nil {
				t.Fatal("public listener leaked", err)
			}
			ln.Close()
		})
	}
}
