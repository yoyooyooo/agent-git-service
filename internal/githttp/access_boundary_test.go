package githttp_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/edge"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestGitAccessBoundaryDoesNotPrepareStorage(t *testing.T) {
	env := setupTestServer(t, "access-owner", "demo", "main", true)
	var owner db.User
	if err := env.DB.Where("login = ?", "access-owner").First(&owner).Error; err != nil {
		t.Fatal(err)
	}
	other := db.User{Login: "access-other", Type: db.TypeUser, Status: db.UserStatusActive}
	if err := env.DB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := env.Store.Delete(context.Background(), "access-owner/demo"); err != nil {
		t.Fatal(err)
	}
	// A valid authorization must not depend on Git being initialized. Only
	// the primary adapter may subsequently create/refresh repository storage.
	env.Svc.Git = nil
	for _, tc := range []struct {
		name      string
		private   bool
		user      *db.User
		operation string
		stage     service.GitTransportAccessStage
		cause     error
	}{
		{"anonymous public read", false, nil, "git-upload-pack", "", nil},
		{"anonymous private read", true, nil, "git-upload-pack", service.GitAccessAuthentication, service.ErrUnauthorized},
		{"anonymous public push", false, nil, "git-receive-pack", service.GitAccessAuthentication, service.ErrUnauthorized},
		{"owner private read", true, &owner, "git-upload-pack", "", nil},
		{"owner private push admission", true, &owner, "git-receive-pack", "", nil},
		{"unrelated private read", true, &other, "git-upload-pack", service.GitAccessPermission, service.ErrNotFound},
		{"unknown service", false, &owner, "not-git", service.GitAccessOperation, service.ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := env.DB.Model(&db.Repository{}).Where("full_name = ?", "access-owner/demo").Update("private", tc.private).Error; err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.user != nil {
				ctx = service.ContextWithUser(ctx, *tc.user)
			}
			rep, err := env.Svc.AuthorizeGitTransport(ctx, "access-owner/demo", tc.operation)
			if tc.cause == nil {
				if err != nil || rep.FullName != "access-owner/demo" {
					t.Fatalf("rep=%+v err=%v", rep, err)
				}
			} else {
				var denied *service.GitTransportAccessError
				if !errors.Is(err, tc.cause) || !errors.As(err, &denied) || denied.Stage != tc.stage {
					t.Fatalf("denial=%v want stage=%s cause=%v", err, tc.stage, tc.cause)
				}
			}
			if env.Store.Exists(ctx, "access-owner/demo") {
				t.Fatal("authorization initialized a repository")
			}
		})
	}
}

// This is real receive-pack through the Edge, not a fake HTTP success test.
// Clone still comes from the primary because Edge reads are deliberately off.
func TestEdgeRealGitPushKeepsPrimaryAuthority(t *testing.T) {
	env := setupTestServer(t, "edge-owner", "demo", "main", true)
	t.Cleanup(func() { env.Server.Close(); env.Svc.Wg.Wait() })
	edgeHTTP := httptest.NewUnstartedServer(nil)
	t.Cleanup(edgeHTTP.Close)
	canonical := "http://" + edgeHTTP.Listener.Addr().String()
	proxy, err := edge.New(config.EdgeConfig{ID: "test-edge", PrimaryURL: env.Server.URL, CanonicalURL: canonical})
	if err != nil {
		t.Fatal(err)
	}
	edgeHTTP.Config.Handler = proxy.Handler()
	edgeHTTP.Start()

	clone := filepath.Join(env.TmpDir, "proxy-client")
	runGit(t, env.TmpDir, "clone", env.RepoURL, clone)
	runGit(t, clone, "remote", "set-url", "--push", "origin", canonical+"/edge-owner/demo.git")
	runGit(t, clone, "commit", "--allow-empty", "-m", "through edge")
	runGit(t, clone, "push", "origin", "HEAD:refs/heads/edge-write")
	accepted := strings.TrimSpace(runGit(t, clone, "rev-parse", "HEAD"))
	primarySHA, err := env.Store.HeadSHA(context.Background(), "edge-owner/demo", "edge-write")
	if err != nil || primarySHA != accepted {
		t.Fatalf("primary did not accept exact pushed SHA: %s %s %v", primarySHA, accepted, err)
	}

	runGit(t, clone, "commit", "--allow-empty", "-m", "must remain denied")
	t.Setenv("GIT_HTTP_EXTRA_HEADER", "Authorization: token definitely-invalid-test-token")
	runGitExpectFail(t, clone, "push", "origin", "HEAD:refs/heads/edge-write")
	primarySHA, err = env.Store.HeadSHA(context.Background(), "edge-owner/demo", "edge-write")
	if err != nil || primarySHA != accepted {
		t.Fatalf("Edge bypassed original-user authorization: %s %v", primarySHA, err)
	}
}

func TestExtractedBackendRealGitProtocolRoundTrip(t *testing.T) {
	for _, version := range []string{"0", "2"} {
		t.Run("protocol-"+version, func(t *testing.T) {
			env := setupTestServer(t, "protocol-owner", "demo", "main", true)
			t.Cleanup(func() { env.Server.Close(); env.Svc.Wg.Wait() })
			if version == "2" {
				t.Setenv("GIT_TRACE_PACKET", "1")
			}
			refs := runGit(t, env.TmpDir, "-c", "protocol.version="+version, "ls-remote", "--symref", env.RepoURL)
			if !strings.Contains(refs, "refs/heads/main") {
				t.Fatalf("no main advertised: %s", refs)
			}
			if version == "2" && !strings.Contains(refs, "version 2") {
				t.Fatalf("protocol v2 silently fell back: %s", refs)
			}
			clone := filepath.Join(env.TmpDir, "client-"+version)
			runGit(t, env.TmpDir, "-c", "protocol.version="+version, "clone", env.RepoURL, clone)
			runGit(t, clone, "commit", "--allow-empty", "-m", "backend extraction regression")
			runGit(t, clone, "push", "origin", "HEAD:refs/heads/backend-regression")
			runGit(t, clone, "-c", "protocol.version="+version, "fetch", "origin")
			local := strings.TrimSpace(runGit(t, clone, "rev-parse", "HEAD"))
			remote, err := env.Store.HeadSHA(context.Background(), "protocol-owner/demo", "backend-regression")
			if err != nil || remote != local {
				t.Fatalf("local=%s primary=%s err=%v", local, remote, err)
			}
		})
	}
}
