package replicationadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
)

func sampleRegistration() edgeprotocol.Registration {
	identity := edgeprotocol.RepositoryIdentity{AuthorityID: "primary", StoreID: "store-repo", RepositoryID: 3, Kind: "repo"}
	return edgeprotocol.Registration{Version: edgeprotocol.RegistrationVersion, AuthorityID: "primary", Repository: "owner/repo", RepositoryID: 3, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Identity: &identity}
}
func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{PrimaryURL: server.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func TestOperatorClientNoRetryRedirectOrCredentialDisclosure(t *testing.T) {
	const secret = "test-only-sensitive-token"
	for _, mode := range []string{"disconnect", "redirect", "reject", "wrong-response", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			var calls, redirected atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer other.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+secret {
					t.Error("original credential/method lost")
				}
				switch mode {
				case "disconnect":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
				case "redirect":
					http.Redirect(w, r, other.URL, 307)
				case "reject":
					http.Error(w, secret, 500)
				case "wrong-response":
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Cache-Control", "no-store")
					value := sampleRegistration()
					value.RepositoryID++
					value.Identity.RepositoryID++
					json.NewEncoder(w).Encode(value)
				case "oversize":
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Cache-Control", "no-store")
					w.Write([]byte(strings.Repeat(secret, 10000)))
				}
			}))
			defer server.Close()
			_, err := newTestClient(t, server).Register(context.Background(), sampleRegistration(), secret)
			if err == nil || strings.Contains(err.Error(), secret) || calls.Load() != 1 || redirected.Load() != 0 {
				t.Fatalf("unsafe operator failure: calls=%d redirected=%d err=%v", calls.Load(), redirected.Load(), err)
			}
		})
	}
}
func TestOperatorClientRefusesUnsafeOriginsAndIgnoresAmbientProxy(t *testing.T) {
	for _, origin := range []string{"http://remote.example", "https://user:secret@example.test", "https://example.test/path", "https://example.test/?", "https://example.test/#fragment", "file:///private/db"} {
		if _, err := NewClient(ClientConfig{PrimaryURL: origin, Timeout: time.Second}); err == nil {
			t.Fatalf("accepted unsafe origin %q", origin)
		}
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(sampleRegistration())
	}))
	defer server.Close()
	if _, err := newTestClient(t, server).Status(context.Background(), "owner/repo", "test-token"); err != nil {
		t.Fatal(err)
	}
}
func TestOperatorCommandRejectsUnsafeFilesAndUnknownArguments(t *testing.T) {
	dir := t.TempDir()
	secret := "test-only-secret"
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte(secret), 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"status", "--primary", "http://127.0.0.1:1", "--repo", "owner/repo", "--token-file", token}, &out); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("public credential file accepted or leaked")
	}
	if err := os.Chmod(token, 0600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(dir, "linked")
	if err := os.Symlink(token, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := readFile(linked, true, 16384); err == nil {
		t.Fatal("symlink credential accepted")
	}
	if err := Run(context.Background(), []string{"status", "--token", secret}, &out); err == nil || strings.Contains(err.Error(), secret) || out.Len() != 0 {
		t.Fatal("argv credential echoed")
	}
	if err := Run(context.Background(), []string{"help"}, &out); err != nil || !strings.Contains(out.String(), "peer-plan") {
		t.Fatal("help missing", err)
	}
}
