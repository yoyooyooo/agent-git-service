package edge_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/edge"
)

func writeReadConfig(t *testing.T, f *controlFixture) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	key, err := x509.MarshalPKCS8PrivateKey(f.nodeCert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.peerCA.Raw}))
	write("node.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.nodeCert.Certificate[0]}))
	write("node-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	cfg := edge.ReadFileConfig{Version: edge.ReadConfigVersion, PeerURL: f.peerURL, CAFile: "ca.pem", CertificateFile: "node.pem", PrivateKeyFile: "node-key.pem", CacheRoot: "cache", Bindings: []edge.ReadBinding{{Repository: f.repo.FullName, Identity: f.identity}}, ReadConcurrency: 4, SyncConcurrency: 2, MaxPending: 8, MaxPackBytes: 16 << 20, SyncTimeout: "30s", PeerTimeout: "30s", RecentViews: 8, RecentWindow: "5m"}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	write("read.json", data)
	return filepath.Join(dir, "read.json")
}

func TestReadConfigurationLoadsRealRuntimeWithoutPrimaryDB(t *testing.T) {
	f := newControlFixture(t)
	path := writeReadConfig(t, f)
	resources, err := edge.OpenReadResources(context.Background(), "edge-fixture-test", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resources.Close(); err != nil {
			t.Error(err)
		}
	}()
	// A deliberately unreachable public upstream proves reads only use the
	// mTLS control/data plane and then local objects, never a download proxy.
	srv, err := edge.New(config.EdgeConfig{ID: "edge-fixture-test", PrimaryURL: "http://127.0.0.1:1", CanonicalURL: "http://ags.test"}, edge.WithReadRuntime(resources.Runtime))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://ags.test/"+f.repo.FullName+".git/info/refs?service=git-upload-pack", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "refs/heads/main") || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("configured read failed: %d %s", rr.Code, rr.Body.String())
	}
	ready := httptest.NewRecorder()
	srv.ServeHTTP(ready, httptest.NewRequest("GET", "http://ags.test/readyz", nil))
	if ready.Code != 503 || !strings.Contains(ready.Body.String(), `"read_path_wired":true`) || !strings.Contains(ready.Body.String(), "read_operational_gates_pending") {
		t.Fatalf("premature operational readiness: %d %s", ready.Code, ready.Body.String())
	}
}

func TestReadConfigurationFailsClosedOnAmbiguityAndUnsafeKeys(t *testing.T) {
	f := newControlFixture(t)
	for _, tc := range []struct {
		name   string
		change func(string)
	}{
		{"public config", func(path string) {
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"public key file", func(path string) {
			if err := os.Chmod(filepath.Join(filepath.Dir(path), "node-key.pem"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink key", func(path string) {
			dir := filepath.Dir(path)
			if err := os.Rename(filepath.Join(dir, "node-key.pem"), filepath.Join(dir, "key-original.pem")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("key-original.pem", filepath.Join(dir, "node-key.pem")); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown setting", func(path string) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = append([]byte(`{"unsafe":true,`), data[1:]...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"duplicate version", func(path string) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = append([]byte(`{"version":"ags.edge.read-config.v1",`), data[1:]...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeReadConfig(t, f)
			tc.change(path)
			resources, err := edge.OpenReadResources(context.Background(), "edge-fixture-test", path)
			if err == nil {
				_ = resources.Close()
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}
