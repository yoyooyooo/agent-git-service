package service_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/middleware"
	"github.com/ngaut/agent-git-service/internal/replication"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

func TestReplicationControlPreservesAccessGrantTransportScopeAndRevoke(t *testing.T) {
	svc, executor, repo := setupAccessGrantService(t)
	ownerCtx := service.ContextWithUser(context.Background(), executor)
	if err := svc.Git.Init(ownerCtx, repo.FullName, "main", true); err != nil {
		t.Fatal(err)
	}
	identity, err := svc.ProvisionReplicationIdentity(ownerCtx, "primary-a", repo.FullName, "repo")
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshotstore.Open(t.TempDir()+"/retained", 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authority, err := service.NewPrimaryReadAuthority(svc, store, "primary-a", "p1", []string{"refs/heads/"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// This unit fixture represents the already-verified TLS layer. Real mTLS
	// handshake/key/store rejection is covered in edge/control integration.
	cert := &x509.Certificate{RawSubjectPublicKeyInfo: []byte("test-only-verified-peer"), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	handler, err := replication.NewReadHandler(authority, middleware.OptionalTokenAuth(svc), []replication.PeerGrant{{EdgeID: "edge-1", SPKISHA256: replication.CertificateKeyID(cert), Stores: []edgeprotocol.RepositoryIdentity{identity}}}, time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "https://peer.test"+path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.TLS = &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	issued, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	transport, err := svc.IssueAccessGrantTransportSession(context.Background(), issued.GrantToken, service.AccessGrantOperationInput{Operation: "git.read", Constraints: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	request := edgeprotocol.PrepareRead{Version: edgeprotocol.PrepareVersion, RequestID: "grant-read-1", Repository: repo.FullName, Identity: identity, Phase: "discover"}
	response := call(edgeprotocol.PreparePath, transport.SessionToken, request)
	if response.Code != 200 {
		t.Fatalf("git.read rejected: %d %s", response.Code, response.Body.String())
	}
	var plan edgeprotocol.ReadPlan
	if err := edgeprotocol.DecodeControl(response.Body, &plan); err != nil {
		t.Fatal(err)
	}
	if response := call(edgeprotocol.RevalidatePath, transport.SessionToken, plan); response.Code != 200 {
		t.Fatalf("valid revalidation: %d %s", response.Code, response.Body.String())
	}
	wrongScope, err := svc.IssueAccessGrantTransportSession(context.Background(), issued.GrantToken, service.AccessGrantOperationInput{Operation: "repo.read", Constraints: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if response := call(edgeprotocol.PreparePath, wrongScope.SessionToken, request); response.Code == 200 {
		t.Fatal("repo.read was widened to Git transport")
	}
	if _, err := svc.RevokeAccessGrant(context.Background(), issued.GrantToken, "replication test"); err != nil {
		t.Fatal(err)
	}
	if response := call(edgeprotocol.RevalidatePath, transport.SessionToken, plan); response.Code == 200 {
		t.Fatal("revoked parent grant still admitted")
	}
}

func TestReplicationPolicyChangeInvalidatesPlanEvenWithSameLabel(t *testing.T) {
	svc, executor, repo := setupAccessGrantService(t)
	ctx := service.ContextWithUser(context.Background(), executor)
	if err := svc.Git.Init(ctx, repo.FullName, "main", true); err != nil {
		t.Fatal(err)
	}
	identity, err := svc.ProvisionReplicationIdentity(ctx, "primary-a", repo.FullName, "repo")
	if err != nil {
		t.Fatal(err)
	}
	store, err := snapshotstore.Open(t.TempDir()+"/retained", 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := service.NewPrimaryReadAuthority(svc, store, "primary-a", "same-label", []string{"refs/heads/", "refs/tags/"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := first.PrepareRead(ctx, "edge-1", edgeprotocol.PrepareRead{Version: edgeprotocol.PrepareVersion, RequestID: "p1", Repository: repo.FullName, Identity: identity, Phase: "discover"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.NewPrimaryReadAuthority(svc, store, "primary-a", "same-label", []string{"refs/heads/"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.RevalidateRead(ctx, "edge-1", plan); !errors.Is(err, edgeprotocol.ErrReadDenied) {
		t.Fatalf("old export policy stayed authorized: %v", err)
	}
	// Ordering is not a policy change.
	reordered, err := service.NewPrimaryReadAuthority(svc, store, "primary-a", "same-label", []string{"refs/tags/", "refs/heads/"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := reordered.RevalidateRead(ctx, "edge-1", plan); err != nil {
		t.Fatal(err)
	}
}
