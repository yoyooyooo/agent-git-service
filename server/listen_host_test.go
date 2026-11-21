package server

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
)

func TestListenHostServerAddressesAndLabels(t *testing.T) {
	// Development composition loads its existing certificate paths. Keep these
	// fixtures and the working directory local to this test.
	tlsFixture := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := tlsFixture.TLS.Certificates[0]
	tlsFixture.Close()
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, block := range map[string]*pem.Block{
		"cert.pem": {Type: "CERTIFICATE", Bytes: certificate.Certificate[0]},
		"key.pem":  {Type: "PRIVATE KEY", Bytes: key},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	for _, tc := range []struct {
		name, mode, host string
		addresses        []string
		labels           []string
	}{
		{
			name: "production wildcard", mode: "production",
			addresses: []string{":9090"}, labels: []string{"http://0.0.0.0:9090"},
		},
		{
			name: "production IPv4", mode: "production", host: "127.0.0.1",
			addresses: []string{"127.0.0.1:9090"}, labels: []string{"http://127.0.0.1:9090"},
		},
		{
			name: "production IPv6", mode: "production", host: "::1",
			addresses: []string{"[::1]:9090"}, labels: []string{"http://[::1]:9090"},
		},
		{
			name: "development wildcard", mode: "development",
			addresses: []string{":443", ":9090", ":8081", ":80", ":4003"},
			labels:    []string{"https://github.localhost:443", "https://localhost:9090", "https://github.localhost:8081", "http://github.localhost:80", "http://github.localhost:4003"},
		},
		{
			name: "development IPv4", mode: "development", host: "127.0.0.1",
			addresses: []string{"127.0.0.1:443", "127.0.0.1:9090", "127.0.0.1:8081", "127.0.0.1:80", "127.0.0.1:4003"},
			labels:    []string{"https://127.0.0.1:443", "https://127.0.0.1:9090", "https://127.0.0.1:8081", "http://127.0.0.1:80", "http://127.0.0.1:4003"},
		},
		{
			name: "development IPv6", mode: "development", host: "::1",
			addresses: []string{"[::1]:443", "[::1]:9090", "[::1]:8081", "[::1]:80", "[::1]:4003"},
			labels:    []string{"https://[::1]:443", "https://[::1]:9090", "https://[::1]:8081", "http://[::1]:80", "http://[::1]:4003"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, err := buildServers(config.Config{ListenHost: tc.host, ListenMode: tc.mode, Port: "9090"}, http.NotFoundHandler())
			if err != nil {
				t.Fatal(err)
			}
			var addresses []string
			for i, srv := range deps.servers {
				addresses = append(addresses, srv.Addr)
				wantTLS := tc.mode == "development" && i < 3
				if (srv.TLSConfig != nil) != wantTLS {
					t.Fatalf("listener %d changed protocol", i)
				}
			}
			if !reflect.DeepEqual(addresses, tc.addresses) || !reflect.DeepEqual(deps.labels, tc.labels) {
				t.Fatalf("addresses=%v labels=%v, want addresses=%v labels=%v", addresses, deps.labels, tc.addresses, tc.labels)
			}
		})
	}
}

func TestListenHostBindsLoopbackListener(t *testing.T) {
	deps, err := buildServers(config.Config{ListenHost: "127.0.0.1", ListenMode: "production", Port: "0"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	instance := &Server{deps: &bootstrapDeps{Servers: deps.servers, Labels: deps.labels}}
	if err := instance.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := instance.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	bound, ok := instance.listeners[0].Addr().(*net.TCPAddr)
	if !ok || !bound.IP.Equal(net.ParseIP("127.0.0.1")) || !bound.IP.IsLoopback() || bound.Port == 0 {
		t.Fatalf("kernel listener did not bind the requested loopback IP: %v", instance.listeners[0].Addr())
	}
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("http://" + bound.String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("bound listener returned %d", response.StatusCode)
	}
}

func TestListenHostRejectedBeforeStartupSideEffects(t *testing.T) {
	for _, entry := range []string{"environment", "embedded"} {
		t.Run(entry, func(t *testing.T) {
			root := t.TempDir()
			databasePath, gitPath := filepath.Join(root, "database.sqlite"), filepath.Join(root, "repositories")
			cfg := config.Config{
				ListenHost: "localhost", ListenMode: "production", Environment: "production",
				DBdsn: "file:" + databasePath, GitRepoDir: gitPath,
			}
			var err error
			if entry == "environment" {
				t.Setenv("AGS_LISTEN_HOST", cfg.ListenHost)
				t.Setenv("LISTEN_MODE", cfg.ListenMode)
				t.Setenv("ENVIRONMENT", cfg.Environment)
				t.Setenv("DB_DSN", cfg.DBdsn)
				t.Setenv("GIT_REPO_DIR", cfg.GitRepoDir)
				result := bootstrap()
				err = result.Err
				if result.Partial != nil || result.Deps.DB != nil || result.Deps.StartBackground != nil {
					t.Fatal("invalid listener configuration reached bootstrap effects")
				}
			} else {
				var instance *Server
				instance, err = New(cfg)
				if instance != nil {
					t.Fatal("invalid listener configuration constructed a server")
				}
			}
			if err == nil || !strings.Contains(err.Error(), "AGS_LISTEN_HOST") {
				t.Fatalf("invalid listener configuration was not rejected first: %v", err)
			}
			for _, path := range []string{databasePath, gitPath} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid listener configuration touched %s: %v", path, err)
				}
			}
		})
	}
}
