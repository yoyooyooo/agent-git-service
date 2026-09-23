package edge_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func TestReplicationRequiresActualMTLSAndRejectsUserBearer(t *testing.T) {
	f := createReplicaFixture(t)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.ca}}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	data, err := edgeprotocol.EncodeSnapshot(f.snapshot.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, f.url+edgeprotocol.ExportPath, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("TLS accepted a connection without a node certificate")
	}
	transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.ca, Certificates: []tls.Certificate{f.client}}}
	client = &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for _, test := range []struct{ name, path, body, bearer string }{
		{"user credential", edgeprotocol.ExportPath, string(data), "Bearer test-user-secret"},
		{"unknown fields", edgeprotocol.ExportPath, `{"url":"http://arbitrary-target"}`, ""},
		{"duplicate JSON", edgeprotocol.ExportPath, string(data) + string(data), ""},
		{"query routing", edgeprotocol.ExportPath + "?source=http://arbitrary-target", string(data), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, f.url+test.path, strings.NewReader(test.body))
			r.Header.Set("Content-Type", "application/json")
			if test.bearer != "" {
				r.Header.Set("Authorization", test.bearer)
			}
			resp, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode < 400 || strings.Contains(string(body), "test-user-secret") || resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("bad envelope admitted/leaked: status=%d body=%s", resp.StatusCode, body)
			}
		})
	}
}

func TestPeerClientDoesNotFollowRedirectOrUseEnvironmentProxy(t *testing.T) {
	ca, key := newReplicaCA(t)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	cert := newReplicaLeaf(t, ca, key, true)
	serverCert := newReplicaLeaf(t, ca, key, false)
	var diverted atomic.Int64
	rogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { diverted.Add(1); w.WriteHeader(200) }))
	defer rogue.Close()
	source := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, rogue.URL, http.StatusTemporaryRedirect)
	}))
	source.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	source.StartTLS()
	defer source.Close()
	t.Setenv("HTTPS_PROXY", rogue.URL)
	t.Setenv("ALL_PROXY", rogue.URL)
	peer, err := edge.NewPeerClient(edge.PeerClientConfig{URL: source.URL, RootCAs: roots, Certificate: cert, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.CloseIdleConnections()
	f := createReplicaFixture(t)
	if _, body, err := peer.OpenSnapshot(context.Background(), f.snapshot.Snapshot); err == nil {
		body.Close()
		t.Fatal("redirect accepted")
	}
	if diverted.Load() != 0 {
		t.Fatal("node data or credentials followed redirect/proxy")
	}
}

func TestMirrorQueueIsBoundedAndShutdownCancelsQueuedWork(t *testing.T) {
	f := createReplicaFixture(t)
	source := &gatedReplicaSource{source: f.peer(t, f.client), started: make(chan struct{}), gate: make(chan struct{})}
	mirror, err := edge.NewMirror(context.Background(), edge.MirrorConfig{Root: filepath.Join(t.TempDir(), "mirror"), Concurrency: 1, MaxPending: 1, SyncTimeout: 5 * time.Second}, source)
	if err != nil {
		t.Fatal(err)
	}
	defer mirror.Close()
	first := make(chan error, 1)
	go func() {
		v, err := mirror.EnsureSnapshot(context.Background(), f.snapshot.Snapshot)
		if v != nil {
			v.Release()
		}
		first <- err
	}()
	<-source.started
	second, err := edgeprotocol.NewManifest(f.snapshot.Snapshot.Identity, f.snapshot.Snapshot.ObjectFormat, f.snapshot.Snapshot.HEAD, "policy-2", f.snapshot.Refs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mirror.EnsureSnapshot(context.Background(), second.Snapshot); !errors.Is(err, edge.ErrSnapshotQueueFull) {
		t.Fatalf("unbounded queue: %v", err)
	}
	if err := mirror.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err == nil {
		t.Fatal("canceled producer reported a ready snapshot")
	}
}

type wrongReplicaSource struct {
	manifest edgeprotocol.Manifest
	closed   atomic.Bool
}
type trackBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b *trackBody) Close() error { b.closed.Store(true); return nil }
func (s *wrongReplicaSource) OpenSnapshot(context.Context, edgeprotocol.RepositorySnapshot) (edgeprotocol.Manifest, io.ReadCloser, error) {
	return s.manifest, &trackBody{Reader: strings.NewReader("untrusted pack"), closed: &s.closed}, nil
}

func TestMirrorRejectsWrongManifestAndClosesItsBody(t *testing.T) {
	f := createReplicaFixture(t)
	other := f.snapshot.Clone()
	other.Snapshot.Identity.StoreID = "different-incarnation"
	source := &wrongReplicaSource{manifest: other}
	mirror := newReplicaMirror(t, source)
	if v, err := mirror.EnsureSnapshot(context.Background(), f.snapshot.Snapshot); err == nil {
		v.Release()
		t.Fatal("substituted snapshot accepted")
	}
	if !source.closed.Load() {
		t.Fatal("rejected pack stream leaked")
	}
}
