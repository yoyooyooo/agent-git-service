package edge_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/gitbackend"
	"github.com/ngaut/agent-git-service/internal/middleware"
	"github.com/ngaut/agent-git-service/internal/replication"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

type controlFixture struct {
	svc      *service.Service
	owner    db.User
	repo     db.Repository
	ctx      context.Context
	identity edgeprotocol.RepositoryIdentity
	peer     *edge.PeerClient
	retained *snapshotstore.Store
	token    string
	peerURL  string
	nodeCert tls.Certificate
	peerCA   *x509.Certificate
}

func newControlFixture(t *testing.T, decorators ...func(replication.ReadAuthority, *service.Service) replication.ReadAuthority) *controlFixture {
	t.Helper()
	svc, cleanup := testharness.NewService(t, testharness.ServiceConfig{})
	t.Cleanup(cleanup)
	owner := db.User{Login: "edge-owner", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := svc.DB.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithUser(context.Background(), owner)
	repo, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: owner.Login, Name: "project", Private: true, AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := svc.ProvisionReplicationIdentity(ctx, "primary-test", repo.FullName, "repo")
	if err != nil {
		t.Fatal(err)
	}
	retained, err := snapshotstore.Open(t.TempDir()+"/retained", 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := retained.Close(); err != nil {
			t.Error(err)
		}
	})
	authority, err := service.NewPrimaryReadAuthority(svc, retained, "primary-test", "export-v1", []string{"refs/heads/", "refs/tags/", "refs/pull/"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ca, caKey := newReplicaCA(t)
	clientCert := newReplicaLeaf(t, ca, caKey, true)
	leaf, err := x509.ParseCertificate(clientCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	grants := []replication.PeerGrant{{EdgeID: "edge-fixture-test", SPKISHA256: replication.CertificateKeyID(leaf), Stores: []edgeprotocol.RepositoryIdentity{identity}}}
	var readAuthority replication.ReadAuthority = authority
	for _, decorate := range decorators {
		readAuthority = decorate(readAuthority, svc)
	}
	control, err := replication.NewReadHandler(readAuthority, middleware.OptionalTokenAuth(svc), grants, 30*time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	exports, err := replication.NewExportHandler(retained, grants, 2)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(edgeprotocol.PreparePath, control)
	mux.Handle(edgeprotocol.RevalidatePath, control)
	mux.Handle(edgeprotocol.ExportPath, exports)
	warm, err := replication.NewWarmHandler(authority, "primary-test", grants)
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle(edgeprotocol.WarmPath, warm)
	mux.Handle(edgeprotocol.NodeStatusPath, warm)
	mux.Handle(edgeprotocol.TransferPath, exports)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	server := httptest.NewUnstartedServer(mux)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{newReplicaLeaf(t, ca, caKey, false)}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	t.Cleanup(server.Close)
	peer, err := edge.NewPeerClient(edge.PeerClientConfig{URL: server.URL, RootCAs: pool, Certificate: clientCert, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.CloseIdleConnections)
	token := "control-original-user-test-only"
	if err := svc.DB.Create(&db.Token{UserID: owner.ID, Value: token}).Error; err != nil {
		t.Fatal(err)
	}
	return &controlFixture{svc: svc, owner: owner, repo: repo, ctx: ctx, identity: identity, peer: peer, retained: retained, token: token, peerURL: server.URL, nodeCert: clientCert, peerCA: ca}
}

func (f *controlFixture) request() edgeprotocol.PrepareRead {
	return edgeprotocol.PrepareRead{Version: edgeprotocol.PrepareVersion, RequestID: "rpc-1", Repository: f.repo.FullName, Identity: f.identity, Phase: "discover"}
}
func (f *controlFixture) prepare(t *testing.T) edgeprotocol.ReadPlan {
	t.Helper()
	plan, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, f.request())
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPrimaryControlRealIdentityAuthorizationAndLocalClone(t *testing.T) {
	f := newControlFixture(t)
	plan := f.prepare(t)
	mirror := newReplicaMirror(t, f.peer)
	view, err := mirror.EnsureSnapshot(context.Background(), plan.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Release()
	if err := f.peer.RevalidateRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, plan); err != nil {
		t.Fatal(err)
	}
	// Fixed admitted view at this test ingress. Packet-stage selection in the
	// production Edge remains a separate gate, not claimed by this test.
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := f.peer.RevalidateRead(r.Context(), "edge-fixture-test", r.Header.Get("Authorization"), plan); err != nil {
			http.Error(w, "denied", 403)
			return
		}
		if err := gitbackend.Serve(w, r, gitbackend.Request{ProjectRoot: view.ProjectRoot(), Repository: view.Repository(), Service: gitbackend.UploadPack, Advertise: r.Method == http.MethodGet, IsolatedRead: true}); err != nil {
			t.Error(err)
		}
	}))
	defer ingress.Close()
	for _, protocol := range []string{"0", "2"} {
		dir := t.TempDir() + "/clone"
		replicaGit(t, "", "", "-c", "http.extraHeader=Authorization: Bearer "+f.token, "-c", "protocol.version="+protocol, "clone", ingress.URL+"/view/repo.git", dir)
		if got := strings.TrimSpace(replicaGit(t, dir, "", "rev-parse", "HEAD")); got != plan.Snapshot.HEAD.OID {
			t.Fatalf("wrong cloned HEAD %s", got)
		}
	}
}

func TestPrimaryControlFreshDiscoveryAndRetainedFetchAreDifferent(t *testing.T) {
	f := newControlFixture(t)
	first := f.prepare(t)
	if _, err := f.svc.Git.WriteFile(f.ctx, f.repo.FullName, "main", "next.txt", "advance", []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	second := f.prepare(t)
	if first.Snapshot == second.Snapshot || first.Snapshot.HEAD.OID == second.Snapshot.HEAD.OID {
		t.Fatal("new discovery was stale")
	}
	request := f.request()
	request.Phase = "fetch"
	request.Snapshot = &first.Snapshot
	old, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, request)
	if err != nil || old.Snapshot != first.Snapshot {
		t.Fatalf("substituted newer view: %v", err)
	}
	if err := f.peer.RevalidateRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, old); err != nil {
		t.Fatal(err)
	}
}

func TestPrimaryControlPeerPermissionDoesNotGrantUserRead(t *testing.T) {
	f := newControlFixture(t)
	for _, credential := range []string{"", "Bearer bogus", "Basic invalid"} {
		if _, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", credential, f.request()); err == nil {
			t.Fatal("node credential bypassed user auth")
		}
	}
	stranger := db.User{Login: "stranger", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := f.svc.DB.Create(&stranger).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DB.Create(&db.Token{UserID: stranger.ID, Value: "stranger-test"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", "Bearer stranger-test", f.request()); err == nil {
		t.Fatal("unrelated user read private repo")
	}
	plan := f.prepare(t)
	if err := f.svc.DB.Where("value = ?", f.token).Delete(&db.Token{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.peer.RevalidateRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, plan); err == nil {
		t.Fatal("deleted token retained authority")
	}
}

func TestPrimaryControlRenameAndSameNameRecreation(t *testing.T) {
	f := newControlFixture(t)
	first := f.prepare(t)
	renamed, err := f.svc.RenameRepo(f.ctx, f.repo.FullName, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := f.svc.ProvisionReplicationIdentity(f.ctx, f.identity.AuthorityID, renamed.FullName, "repo")
	if err != nil || identity != f.identity {
		t.Fatalf("rename changed identity: %v", err)
	}
	if plan := f.prepare(t); plan.Snapshot.Identity != f.identity {
		t.Fatal("alias did not resolve same store")
	}
	if err := f.svc.DeleteRepo(f.ctx, renamed.FullName); err != nil {
		t.Fatal(err)
	}
	if err := f.peer.RevalidateRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, first); err == nil {
		t.Fatal("deleted repo still readable")
	}
	if _, err := f.svc.CreateRepo(f.ctx, service.CreateRepoInput{OwnerLogin: f.owner.Login, Name: "project", Private: true, AutoInit: true}); err != nil {
		t.Fatal(err)
	}
	newIdentity, err := f.svc.ProvisionReplicationIdentity(f.ctx, f.identity.AuthorityID, f.repo.FullName, "repo")
	if err != nil || newIdentity.StoreID == f.identity.StoreID {
		t.Fatalf("reused storage identity: %v", err)
	}
	if _, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, f.request()); err == nil {
		t.Fatal("old grant admitted same-name new repo")
	}
	request := f.request()
	request.Identity = newIdentity
	if _, err := f.peer.PrepareRead(context.Background(), "edge-fixture-test", "Bearer "+f.token, request); err == nil {
		t.Fatal("old peer grant automatically expanded")
	}
}
