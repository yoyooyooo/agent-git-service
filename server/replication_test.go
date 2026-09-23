package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/replication"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

func TestReplicationEndpointUsesOwningPrimaryAndIsNotPublic(t *testing.T) {
	root := t.TempDir()
	srv, err := New(config.Config{DBdsn: "file:" + filepath.Join(root, "primary.db"), GitRepoDir: filepath.Join(root, "repos"), BaseURL: "http://ags.test", ListenMode: "production", Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	svc := srv.deps.SvcDeps
	owner := db.User{Login: "replication-owner", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := svc.DB.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithUser(context.Background(), owner)
	repo, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: owner.Login, Name: "live", Private: true, AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := svc.ProvisionReplicationIdentity(ctx, "primary", repo.FullName, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Create(&db.Token{UserID: owner.ID, Value: "server-control-test"}).Error; err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{RawSubjectPublicKeyInfo: []byte("endpoint-test-peer"), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	cfg := ReplicationOptions{AuthorityID: "primary", SnapshotRoot: filepath.Join(root, "retained"), ExportPolicyRevision: "v1", ExportPrefixes: []string{"refs/heads/"}, Peers: []ReplicationPeer{{EdgeID: "edge-1", SPKISHA256: replication.CertificateKeyID(cert), Stores: []ReplicationIdentity{identity}}}, MaxPackBytes: 16 << 20}
	endpoint, err := srv.NewReplicationEndpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := endpoint.Close(); err != nil {
			t.Error(err)
		}
	}()
	request := edgeprotocol.PrepareRead{Version: edgeprotocol.PrepareVersion, RequestID: "rpc-1", Repository: repo.FullName, Identity: identity, Phase: "discover"}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	makeRequest := func() *http.Request {
		r := httptest.NewRequest("POST", "https://ags.test"+edgeprotocol.PreparePath, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer server-control-test")
		r.Header.Set("Content-Type", "application/json")
		r.TLS = &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		return r
	}
	public := httptest.NewRecorder()
	srv.Handler().ServeHTTP(public, makeRequest())
	if public.Code != http.StatusNotFound {
		t.Fatalf("control leaked to public primary routes: %d", public.Code)
	}
	peer := httptest.NewRecorder()
	endpoint.Handler().ServeHTTP(peer, makeRequest())
	if peer.Code != http.StatusOK {
		t.Fatalf("peer read preparation failed: %d %s", peer.Code, peer.Body.String())
	}
	var plan edgeprotocol.ReadPlan
	if err := edgeprotocol.DecodeControl(peer.Body, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Snapshot.Identity != identity {
		t.Fatal("wrong primary storage identity")
	}
	withoutTLS := makeRequest()
	withoutTLS.TLS = nil
	denied := httptest.NewRecorder()
	endpoint.Handler().ServeHTTP(denied, withoutTLS)
	if denied.Code != 403 {
		t.Fatalf("peer endpoint accepted ordinary user token: %d", denied.Code)
	}
	// Closing the endpoint releases its snapshot owner independently of the
	// primary. A failed constructor must release that lock as well.
	if err := endpoint.Close(); err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.ExportPrefixes = nil
	if _, err := srv.NewReplicationEndpoint(bad); err == nil {
		t.Fatal("invalid policy admitted")
	}
	reopened, err := snapshotstore.Open(cfg.SnapshotRoot, cfg.MaxPackBytes)
	if err != nil {
		t.Fatal("failed construction leaked store lock", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
