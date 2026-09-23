package server

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/replicationadmin"
	"time"
)

func TestRegistrationAuthorityMustMatchPeerConfigurationBeforeBootstrap(t *testing.T) {
	f := newReplicationRuntimeFixture(t)
	bad := f.cfg
	bad.ReplicationAuthorityID = "another-primary"
	unopened := filepath.Join(t.TempDir(), "must-not-open.db")
	bad.DBdsn = "file:" + unopened
	if srv, err := New(bad); err == nil {
		srv.Shutdown(context.Background())
		t.Fatal("conflicting authority accepted")
	} else if !strings.Contains(err.Error(), "authority") {
		t.Fatal(err)
	}
	if _, err := os.Stat(unopened); !os.IsNotExist(err) {
		t.Fatal("invalid authority opened DB", err)
	}
	// With no separate identity variable, the peer file supplies the same
	// authoritative identity for operator registration and read control.
	srv, err := New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	h := httptest.NewServer(srv.Handler())
	defer h.Close()
	client, err := replicationadmin.NewClient(replicationadmin.ClientConfig{PrimaryURL: h.URL, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	receipt, err := client.Status(context.Background(), "peer-owner/project", "runtime-original-test-only")
	if err != nil || receipt.Identity == nil || *receipt.Identity != f.identity {
		t.Fatal("operator/peer authority diverged", err)
	}
}
