package edge_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/gitbackend"
	"github.com/ngaut/agent-git-service/internal/replication"
	"github.com/ngaut/agent-git-service/internal/snapshotstore"
)

type replicaFixture struct {
	snapshot edgeprotocol.Manifest
	primary  *snapshotstore.Store
	url      string
	ca       *x509.CertPool
	client   tls.Certificate
	other    tls.Certificate
	calls    atomic.Int64
}

func replicaGit(t *testing.T, dir, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "http.proxy=", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Replication Test", "GIT_AUTHOR_EMAIL=replication@example.test", "GIT_COMMITTER_NAME=Replication Test", "GIT_COMMITTER_EMAIL=replication@example.test"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func createReplicaFixture(t *testing.T) *replicaFixture {
	t.Helper()
	f := &replicaFixture{}
	root := t.TempDir()
	source := filepath.Join(root, "source.git")
	replicaGit(t, root, "", "init", "--bare", "--template=", source)
	replicaGit(t, source, "", "symbolic-ref", "HEAD", "refs/heads/main")
	parent := ""
	for _, text := range []string{"first", "second"} {
		blob := replicaGit(t, source, text, "hash-object", "-w", "--stdin")
		tree := replicaGit(t, source, "100644 blob "+blob+"\tREADME.md\n", "mktree")
		args := []string{"commit-tree", tree, "-m", text}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		parent = replicaGit(t, source, "", args...)
	}
	replicaGit(t, source, "", "update-ref", "refs/heads/main", parent)
	var err error
	// Offline, quiesced fixture capture. This is NOT an assertion that the live
	// primary mutation guard or original-user ReadPlan endpoint is wired.
	f.snapshot, err = snapshotstore.ObserveManifest(context.Background(), source, edgeprotocol.RepositoryIdentity{AuthorityID: "region-b-primary", StoreID: "repo-incarnation-a", RepositoryID: 9, Kind: "repo"}, "heads-tags-pull-v1", []string{"refs/heads/", "refs/tags/", "refs/pull/"})
	if err != nil {
		t.Fatal(err)
	}
	f.primary, err = snapshotstore.Open(filepath.Join(root, "exports"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.primary.Close(); err != nil {
			t.Errorf("close primary: %v", err)
		}
	})
	if err := f.primary.Install(context.Background(), f.snapshot, func(ctx context.Context, w io.Writer) error {
		return snapshotstore.WritePack(ctx, source, f.snapshot, w)
	}); err != nil {
		t.Fatal(err)
	}
	// Transport can no longer accidentally fall back to the source files.
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	caCert, caKey := newReplicaCA(t)
	f.ca = x509.NewCertPool()
	f.ca.AddCert(caCert)
	serverCert := newReplicaLeaf(t, caCert, caKey, false)
	f.client = newReplicaLeaf(t, caCert, caKey, true)
	f.other = newReplicaLeaf(t, caCert, caKey, true)
	handler, err := replication.NewExportHandler(f.primary, []replication.PeerGrant{{EdgeID: "edge-1", SPKISHA256: replication.CertificateKeyID(f.client.Leaf), Stores: []edgeprotocol.RepositoryIdentity{f.snapshot.Snapshot.Identity}}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.calls.Add(1); handler.ServeHTTP(w, r) }))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: f.ca}
	server.StartTLS()
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

func newReplicaCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Replica Test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func newReplicaLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, client bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "same-name-is-not-identity"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key, Leaf: cert}
}

func (f *replicaFixture) peer(t *testing.T, cert tls.Certificate) *edge.PeerClient {
	t.Helper()
	client, err := edge.NewPeerClient(edge.PeerClientConfig{URL: f.url, RootCAs: f.ca, Certificate: cert, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func newReplicaMirror(t *testing.T, source edge.SnapshotSource) *edge.Mirror {
	t.Helper()
	m, err := edge.NewMirror(context.Background(), edge.MirrorConfig{Root: filepath.Join(t.TempDir(), "mirror"), Concurrency: 2, MaxPending: 16, SyncTimeout: 10 * time.Second}, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("close mirror: %v", err)
		}
	})
	return m
}

func TestPeerExportToMirrorThenNativeGitV0V2(t *testing.T) {
	f := createReplicaFixture(t)
	mirror := newReplicaMirror(t, f.peer(t, f.client))
	var v2 atomic.Bool
	// A fixed, explicitly admitted test view exercises the DATA plane only.
	// The production Server still rejects reads until live authorization and
	// stateless multi-generation selection are implemented.
	ingress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Git-Protocol") == "version=2" {
			v2.Store(true)
		}
		view, err := mirror.EnsureSnapshot(r.Context(), f.snapshot.Snapshot)
		if err != nil {
			t.Errorf("ensure: %v", err)
			http.Error(w, "snapshot unavailable", 503)
			return
		}
		defer view.Release()
		if err := gitbackend.Serve(w, r, gitbackend.Request{ProjectRoot: view.ProjectRoot(), Repository: view.Repository(), Service: gitbackend.UploadPack, Advertise: r.Method == http.MethodGet, IsolatedRead: true}); err != nil {
			t.Errorf("local backend: %v", err)
			http.Error(w, "backend", 500)
		}
	}))
	defer ingress.Close()
	for _, protocol := range []string{"0", "2"} {
		dir := t.TempDir()
		dest := filepath.Join(dir, "clone")
		replicaGit(t, dir, "", "-c", "protocol.version="+protocol, "clone", "--depth=1", ingress.URL+"/team/repo.git", dest)
		if head := replicaGit(t, dest, "", "rev-parse", "HEAD"); head != f.snapshot.Snapshot.HEAD.OID {
			t.Fatal("wrong local HEAD")
		}
		replicaGit(t, dest, "", "-c", "protocol.version="+protocol, "fetch", "--unshallow")
		if count := replicaGit(t, dest, "", "rev-list", "--count", "HEAD"); count != "2" {
			t.Fatalf("history not complete: %s", count)
		}
	}
	if !v2.Load() {
		t.Fatal("v2 was not negotiated")
	}
	if calls := f.calls.Load(); calls != 1 {
		t.Fatalf("multiple Git clones fetched snapshot %d times", calls)
	}
}

func TestPeerExportRejectsWrongKeyAndStoreBinding(t *testing.T) {
	f := createReplicaFixture(t)
	if _, body, err := f.peer(t, f.other).OpenSnapshot(context.Background(), f.snapshot.Snapshot); err == nil {
		body.Close()
		t.Fatal("same CN with different key was authorized")
	}
	valid := f.peer(t, f.client)
	for _, change := range []func(*edgeprotocol.RepositorySnapshot){
		func(s *edgeprotocol.RepositorySnapshot) { s.Identity.AuthorityID = "other-authority" },
		func(s *edgeprotocol.RepositorySnapshot) { s.Identity.StoreID = "same-name-recreated" },
		func(s *edgeprotocol.RepositorySnapshot) { s.Identity.Kind = "wiki" },
		func(s *edgeprotocol.RepositorySnapshot) { s.Identity.RepositoryID++ },
		func(s *edgeprotocol.RepositorySnapshot) { s.SnapshotID = "not-retained" },
	} {
		s := f.snapshot.Snapshot
		change(&s)
		if _, body, err := valid.OpenSnapshot(context.Background(), s); err == nil {
			body.Close()
			t.Fatal("ungranted or absent view accepted")
		}
	}
	if _, body, err := valid.OpenSnapshot(context.Background(), f.snapshot.Snapshot); err != nil {
		t.Fatal(err)
	} else {
		body.Close()
	}
}

type gatedReplicaSource struct {
	source  edge.SnapshotSource
	started chan struct{}
	gate    chan struct{}
	calls   atomic.Int64
	once    sync.Once
}

func (s *gatedReplicaSource) OpenSnapshot(ctx context.Context, r edgeprotocol.RepositorySnapshot) (edgeprotocol.Manifest, io.ReadCloser, error) {
	s.calls.Add(1)
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.gate:
		return s.source.OpenSnapshot(ctx, r)
	case <-ctx.Done():
		return edgeprotocol.Manifest{}, nil, ctx.Err()
	}
}

func TestMirrorCoalescesWaitersAndCallerCancellationIsIndependent(t *testing.T) {
	f := createReplicaFixture(t)
	source := &gatedReplicaSource{source: f.peer(t, f.client), started: make(chan struct{}), gate: make(chan struct{})}
	m := newReplicaMirror(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		v, err := m.EnsureSnapshot(ctx, f.snapshot.Snapshot)
		if v != nil {
			v.Release()
		}
		first <- err
	}()
	<-source.started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller cancellation: %v", err)
	}
	const count = 12
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			v, err := m.EnsureSnapshot(context.Background(), f.snapshot.Snapshot)
			if v != nil {
				v.Release()
			}
			results <- err
		}()
	}
	close(source.gate)
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Fatalf("shared sync lost after caller cancel: %v", err)
		}
	}
	if source.calls.Load() != 1 || f.calls.Load() != 1 {
		t.Fatalf("not coalesced: source=%d network=%d", source.calls.Load(), f.calls.Load())
	}
	v, err := m.EnsureSnapshot(context.Background(), f.snapshot.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Evict(f.snapshot.Snapshot); !errors.Is(err, snapshotstore.ErrPinned) {
		t.Fatalf("evicted pinned snapshot: %v", err)
	}
	v.Release()
	if err := m.Evict(f.snapshot.Snapshot); err != nil {
		t.Fatal(err)
	}
}

type hangingReplicaSource struct {
	manifest edgeprotocol.Manifest
	opened   chan struct{}
	writer   *io.PipeWriter
}

func (s *hangingReplicaSource) OpenSnapshot(context.Context, edgeprotocol.RepositorySnapshot) (edgeprotocol.Manifest, io.ReadCloser, error) {
	r, w := io.Pipe()
	s.writer = w
	close(s.opened)
	return s.manifest, r, nil
}

func TestMirrorCloseInterruptsBlockedPackAndJoinsProducer(t *testing.T) {
	f := createReplicaFixture(t)
	source := &hangingReplicaSource{manifest: f.snapshot, opened: make(chan struct{})}
	m := newReplicaMirror(t, source)
	result := make(chan error, 1)
	go func() {
		v, err := m.EnsureSnapshot(context.Background(), f.snapshot.Snapshot)
		if v != nil {
			v.Release()
		}
		result <- err
	}()
	<-source.opened
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close did not stop blocked import")
	}
	if err := <-result; err == nil {
		t.Fatal("incomplete snapshot reported ready")
	}
	_ = source.writer.Close()
}
