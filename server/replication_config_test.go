package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplicationConfigurationRejectsUnsafeInputsBeforeBootstrap(t *testing.T) {
	f := newReplicationRuntimeFixture(t)
	original := f.file
	for _, name := range []string{"unknown field", "duplicate field", "public config", "public private key", "symlink key", "wildcard listener", "public listener", "wrong server name", "missing topology acknowledgement", "wrong authority", "invalid duration"} {
		t.Run(name, func(t *testing.T) {
			f.file = original
			f.file.Peers = append([]ReplicationFilePeer(nil), original.Peers...)
			f.save(t)
			switch name {
			case "unknown field", "duplicate field":
				data, err := os.ReadFile(f.path)
				if err != nil {
					t.Fatal(err)
				}
				prefix := []byte(`{"unknown":true,`)
				if name == "duplicate field" {
					prefix = []byte(`{"VERSION":"ags.primary.replication-config.v1",`)
				}
				if err := os.WriteFile(f.path, append(prefix, data[1:]...), 0600); err != nil {
					t.Fatal(err)
				}
			case "public config":
				os.Chmod(f.path, 0644)
				defer os.Chmod(f.path, 0600)
			case "public private key":
				p := filepath.Join(f.root, "server-key.pem")
				os.Chmod(p, 0644)
				defer os.Chmod(p, 0600)
			case "symlink key":
				p := filepath.Join(f.root, "linked-key.pem")
				if err := os.Symlink("server-key.pem", p); err != nil {
					t.Fatal(err)
				}
				f.file.PrivateKeyFile = "linked-key.pem"
				f.save(t)
			case "wildcard listener":
				f.file.ListenAddr = "0.0.0.0:7443"
				f.save(t)
			case "public listener":
				f.file.ListenAddr = "8.8.8.8:7443"
				f.save(t)
			case "wrong server name":
				f.file.ServerName = "not-in-certificate.test"
				f.save(t)
			case "missing topology acknowledgement":
				f.file.ManagedWritesOnly = false
				f.save(t)
			case "wrong authority":
				f.file.AuthorityID = "another-primary"
				f.save(t)
			case "invalid duration":
				f.file.RequestTimeout = "0s"
				f.save(t)
			}
			if loaded, err := loadReplicationConfig(f.path); err == nil || loaded != nil {
				t.Fatal("unsafe configuration accepted")
			}
			// Invalid file configuration is rejected before an unrelated DB can
			// be created/migrated or any listeners/workers can be started.
			cfg := f.cfg
			dbPath := filepath.Join(t.TempDir(), "must-not-exist.db")
			cfg.DBdsn = "file:" + dbPath
			if srv, err := New(cfg); err == nil {
				cleanupFailedBootstrap(srv.deps)
				t.Fatal("invalid config bootstrapped")
			}
			if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
				t.Fatal("invalid config touched business DB", err)
			}
		})
	}
}

func TestReplicationConfigExampleHasNoCredentials(t *testing.T) {
	f := newReplicationRuntimeFixture(t)
	data, err := json.Marshal(f.file)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"BEGIN PRIVATE KEY", "runtime-original-test-only", "DB_DSN"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatal("configuration embedded sensitive runtime material")
		}
	}
	loaded, err := loadReplicationConfig(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.tls.SessionTicketsDisabled || !strings.HasPrefix(loaded.options.SnapshotRoot, f.root) {
		t.Fatal("unsafe TLS lifecycle or relative path resolution")
	}
}
