package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edgeprotocol"
	"github.com/ngaut/agent-git-service/internal/replicationadmin"
	"github.com/ngaut/agent-git-service/internal/service"
)

type registrationFixture struct {
	server *Server
	http   *httptest.Server
	owner  db.User
	repo   db.Repository
	ctx    context.Context
	token  string
}

func newRegistrationFixture(t *testing.T, enabled bool) *registrationFixture {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{DBdsn: "file:" + filepath.Join(root, "primary.db"), GitRepoDir: filepath.Join(root, "repos"), BaseURL: "http://ags.test", ListenMode: "production", Environment: "production"}
	if enabled {
		cfg.ReplicationAuthorityID = "region-b-authority"
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	owner := db.User{Login: "enrollment-owner", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := srv.deps.DB.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	ctx := service.ContextWithUser(context.Background(), owner)
	repo, err := srv.deps.SvcDeps.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: owner.Login, Name: "repo", Private: true, AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	token := "test-registration-original-admin"
	if err := srv.deps.DB.Create(&db.Token{UserID: owner.ID, Value: token}).Error; err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)
	return &registrationFixture{srv, httpServer, owner, repo, ctx, token}
}
func (f *registrationFixture) client(t *testing.T) *replicationadmin.Client {
	t.Helper()
	c, err := replicationadmin.NewClient(replicationadmin.ClientConfig{PrimaryURL: f.http.URL, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func (f *registrationFixture) call(t *testing.T, method, token, body string) *http.Response {
	t.Helper()
	r, err := http.NewRequest(method, f.http.URL+"/api/v3/repos/"+f.repo.FullName+"/replication/identity", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := f.http.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
func TestReplicationRegistrationOperatorObserveRegisterAndRename(t *testing.T) {
	f := newRegistrationFixture(t, true)
	client := f.client(t)
	before, err := client.Status(f.ctx, f.repo.FullName, f.token)
	if err != nil {
		t.Fatal(err)
	}
	if before.Identity != nil || before.AuthorityID != "region-b-authority" {
		t.Fatal("status allocated or changed authority")
	}
	var stored db.Repository
	if err := f.server.deps.DB.First(&stored, f.repo.ID).Error; err != nil || stored.GitStorageID != nil {
		t.Fatal("GET changed identity", err)
	}
	registered, err := client.Register(f.ctx, before, f.token)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := client.Register(f.ctx, before, f.token)
	if err != nil || *replayed.Identity != *registered.Identity {
		t.Fatal("registration not exact/idempotent", err)
	}
	if len(f.server.deps.Servers) != 1 || f.server.deps.Replication != nil {
		t.Fatal("registration enabled a peer listener")
	}
	renamed, err := f.server.deps.SvcDeps.RenameRepo(f.ctx, f.repo.FullName, "renamed")
	if err != nil {
		t.Fatal(err)
	}
	alias, err := client.Status(f.ctx, f.repo.FullName, f.token)
	if err != nil || alias.Repository != renamed.FullName || *alias.Identity != *registered.Identity {
		t.Fatal("rename lost registration", err)
	}
	// Same row stays usable through its canonical locator, without rewriting
	// peer permissions or relying on the user-supplied repository name.
	if _, err := client.Register(f.ctx, alias, f.token); err != nil {
		t.Fatal(err)
	}
}
func TestReplicationRegistrationRejectsStaleObservationAndSameNameReplacement(t *testing.T) {
	f := newRegistrationFixture(t, true)
	client := f.client(t)
	observed, err := client.Status(f.ctx, f.repo.FullName, f.token)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*edgeprotocol.Registration){func(r *edgeprotocol.Registration) { r.AuthorityID = "wrong" }, func(r *edgeprotocol.Registration) { r.RepositoryID++ }, func(r *edgeprotocol.Registration) { r.CreatedAt = r.CreatedAt.Add(time.Second) }} {
		stale := observed
		change(&stale)
		if _, err := client.Register(f.ctx, stale, f.token); err == nil || !strings.Contains(err.Error(), "409") {
			t.Fatal("stale observation accepted", err)
		}
	}
	if err := f.server.deps.SvcDeps.DeleteRepo(f.ctx, f.repo.FullName); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.deps.SvcDeps.CreateRepo(f.ctx, service.CreateRepoInput{OwnerLogin: f.owner.Login, Name: f.repo.Name, Private: true, AutoInit: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Register(f.ctx, observed, f.token); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatal("stale receipt registered replacement", err)
	}
	current, err := client.Status(f.ctx, f.repo.FullName, f.token)
	if err != nil || current.Identity != nil {
		t.Fatal("replacement was auto registered", err)
	}
}
func TestReplicationRegistrationClosedEnvelopeAndNativeAdminBoundary(t *testing.T) {
	f := newRegistrationFixture(t, true)
	client := f.client(t)
	before, err := client.Status(f.ctx, f.repo.FullName, f.token)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(before.Request())
	for _, body := range []string{"{}", string(payload) + "{}", `{"version":"duplicate",` + string(payload[1:]), `{"store_id":"injected",` + string(payload[1:]), `{"authority_id":"injected",` + string(payload[1:])} {
		resp := f.call(t, "POST", f.token, body)
		if resp.StatusCode != 400 || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("bad body accepted: %d", resp.StatusCode)
		}
	}
	viewer := db.User{Login: "enrollment-viewer", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := f.server.deps.DB.Create(&viewer).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.server.deps.SvcDeps.AddCollaborator(f.ctx, f.repo.ID, viewer.ID, "write"); err != nil {
		t.Fatal(err)
	}
	if err := f.server.deps.DB.Create(&db.Token{UserID: viewer.ID, Value: "viewer-test"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "invalid-test", "viewer-test"} {
		for _, method := range []string{"GET", "POST"} {
			resp := f.call(t, method, token, string(payload))
			if resp.StatusCode < 400 {
				t.Fatalf("%s accepted insufficient credential", method)
			}
		}
	}
	var stored db.Repository
	if err := f.server.deps.DB.First(&stored, f.repo.ID).Error; err != nil || stored.GitStorageID != nil {
		t.Fatal("denials allocated identity", err)
	}
}
func TestReplicationRegistrationIsOptInAndCommandUsesActualPrimary(t *testing.T) {
	disabled := newRegistrationFixture(t, false)
	if resp := disabled.call(t, "GET", disabled.token, ""); resp.StatusCode != 404 {
		t.Fatalf("unconfigured registration exposed: %d", resp.StatusCode)
	}
	f := newRegistrationFixture(t, true)
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(f.token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := replicationadmin.Run(f.ctx, []string{"status", "--primary", f.http.URL, "--repo", f.repo.FullName, "--token-file", tokenPath}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), f.token) {
		t.Fatal("operator status leaked credential")
	}
	expected := filepath.Join(t.TempDir(), "expected.json")
	if err := os.WriteFile(expected, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := replicationadmin.Run(f.ctx, []string{"register", "--primary", f.http.URL, "--expected", expected, "--token-file", tokenPath}, &out); err != nil {
		t.Fatal(err)
	}
	var receipt edgeprotocol.Registration
	if err := edgeprotocol.DecodeControl(&out, &receipt); err != nil || receipt.Identity == nil {
		t.Fatal("command did not register", err)
	}
}
