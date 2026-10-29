package githttp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githttp"
	applog "github.com/ngaut/agent-git-service/internal/logging"
	"github.com/ngaut/agent-git-service/internal/service"
)

// Cancel only after the real receive-pack has returned and released its lock:
// the Git mutation has committed, but application follow-up has not started.
type cancelAfterReceivePackStore struct {
	githttp.Store
	cancel      context.CancelFunc
	calls       int
	followup    context.Context
	followupErr error
}

func (s *cancelAfterReceivePackStore) WithRepoLock(ctx context.Context, fullName string, fn func() error) error {
	s.calls++
	if s.calls == 2 {
		s.followup = ctx
		s.followupErr = ctx.Err()
	}
	err := s.Store.WithRepoLock(ctx, fullName, fn)
	if s.calls == 1 && err == nil {
		s.cancel()
	}
	return err
}

func TestReceivePack_ClientCanceledAfterRefUpdateStillSyncsPRHeadAndProjection(t *testing.T) {
	env := setupTestServer(t, "example-owner", "demo", "main", true)
	env.Svc.DisableForgejoProjectionWorker = true
	if err := env.DB.AutoMigrate(&db.PullRequestProjectionJob{}, &db.PullRequestProjection{}, &db.ProjectionRefState{}, &db.ProjectionEvent{}); err != nil {
		t.Fatalf("migrate projection tables: %v", err)
	}
	var repo db.Repository
	if err := env.DB.First(&repo, "full_name = ?", "example-owner/demo").Error; err != nil {
		t.Fatal(err)
	}
	var user db.User
	if err := env.DB.First(&user, repo.OwnerID).Error; err != nil {
		t.Fatal(err)
	}

	const branch = "agent/post-push"
	localDir := filepath.Join(t.TempDir(), "local")
	runGit(t, t.TempDir(), "clone", env.RepoURL, localDir)
	runGit(t, localDir, "checkout", "-b", branch)
	runGit(t, localDir, "push", "origin", branch)
	env.Svc.Wg.Wait()
	oldSHA := strings.TrimSpace(runGit(t, localDir, "rev-parse", "HEAD"))
	pr := db.PullRequest{
		Number: 1, RepositoryID: repo.ID, HeadRepositoryID: repo.ID, AuthorID: user.ID,
		Title: "post-push context regression", State: db.StateOpen,
		HeadRef: branch, HeadSHA: oldSHA, BaseRef: "main", BaseSHA: oldSHA,
	}
	if err := env.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	// Exercise the real durable enqueue without a provider network call or worker.
	env.Svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "https://forgejo.example.test", Token: "test-token",
		MirrorBranchIncludes: []string{"mirror-only/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			repo.FullName: {Owner: "example-owner", Repo: "demo"},
		},
	}, nil, nil)
	job, err := env.Svc.EnqueueForgejoPullRequestProjection(context.Background(), pr)
	if err != nil {
		t.Fatalf("enqueue initial projection: %v", err)
	}
	if err := env.DB.Model(&job).Updates(map[string]any{
		"phase": service.ForgejoProjectionPhaseProjected, "next_run_at": nil,
	}).Error; err != nil {
		t.Fatal(err)
	}

	type requestValueKey struct{}
	type result struct {
		requestErr  error
		followup    context.Context
		followupErr error
		trace       *applog.GitHTTPTrace
	}
	completed := make(chan result, 1)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := service.ContextWithDB(req.Context(), env.DB)
			ctx = service.ContextWithUser(ctx, user)
			ctx = context.WithValue(ctx, requestValueKey{}, "trace-value")
			ctx, _ = applog.WithGitHTTPTrace(ctx, repo.FullName, "git-receive-pack")
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/{owner}/{repo}.git/info/refs", githttp.New(env.Store, env.Svc).InfoRefs)
	r.Post("/{owner}/{repo}.git/git-receive-pack", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		store := &cancelAfterReceivePackStore{Store: env.Store, cancel: cancel}
		githttp.New(store, env.Svc).ReceivePack(w, req.WithContext(ctx))
		completed <- result{requestErr: ctx.Err(), followup: store.followup, followupErr: store.followupErr, trace: applog.GitHTTPTraceFromContext(ctx)}
	})
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	runGit(t, localDir, "remote", "set-url", "origin", fmt.Sprintf("%s/%s.git", ts.URL, repo.FullName))

	workflowDir := filepath.Join(localDir, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workflowDir, "ci.yml"), []byte("name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, localDir, "add", ".github/workflows/ci.yml")
	runGit(t, localDir, "commit", "-m", "add workflow after PR creation")
	newSHA := strings.TrimSpace(runGit(t, localDir, "rev-parse", "HEAD"))
	// Workflow discovery reads the default branch; update it in the same
	// receive-pack as the PR branch so both follow-up paths are exercised.
	runGit(t, localDir, "push", "origin", branch, "HEAD:main")
	var got result
	select {
	case got = <-completed:
	case <-time.After(10 * time.Second):
		t.Fatal("receive-pack follow-up did not finish")
	}
	env.Svc.Wg.Wait()
	if got.requestErr != context.Canceled {
		t.Fatalf("request context error = %v, want canceled", got.requestErr)
	}
	if sha, err := env.Store.HeadSHA(context.Background(), repo.FullName, branch); err != nil || sha != newSHA {
		t.Fatalf("authoritative Git ref = %q, %v; want %s", sha, err, newSHA)
	}
	var updatedPR db.PullRequest
	if err := env.DB.First(&updatedPR, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if updatedPR.HeadSHA != newSHA {
		t.Errorf("PR head stayed stale after successful receive-pack and request cancellation: got %s, want %s", updatedPR.HeadSHA, newSHA)
	}
	var updatedJob db.PullRequestProjectionJob
	if err := env.DB.First(&updatedJob, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if updatedJob.HeadSHA != newSHA || updatedJob.RemoteSHA != newSHA || updatedJob.Phase != service.ForgejoProjectionPhaseQueued || updatedJob.Attempt <= job.Attempt {
		t.Errorf("projection was not requeued for new head: head=%s remote=%s phase=%s attempt=%d; want head=%s queued attempt>%d", updatedJob.HeadSHA, updatedJob.RemoteSHA, updatedJob.Phase, updatedJob.Attempt, newSHA, job.Attempt)
	}
	var workflowCount int64
	if err := env.DB.Model(&db.Workflow{}).Where("repository_id = ? AND path = ?", repo.ID, ".github/workflows/ci.yml").Count(&workflowCount).Error; err != nil {
		t.Fatal(err)
	}
	if workflowCount != 1 {
		t.Errorf("workflow sync did not complete after request cancellation: count=%d", workflowCount)
	}
	if got.followup == nil {
		t.Fatal("post-push HEAD repair was not attempted")
	}
	if got.followupErr != nil {
		t.Errorf("post-push context was already canceled: %v", got.followupErr)
	}
	if scopedDB, ok := service.DBFromContext(got.followup); !ok || scopedDB != env.DB {
		t.Error("post-push context lost scoped DB")
	}
	if actor, ok := service.UserFromContext(got.followup); !ok || actor.ID != user.ID {
		t.Error("post-push context lost original user")
	}
	if applog.GitHTTPTraceFromContext(got.followup) != got.trace || got.trace == nil {
		t.Error("post-push context lost Git HTTP trace")
	}
	if got.followup.Value(requestValueKey{}) != "trace-value" {
		t.Error("post-push context lost request values")
	}
	deadline, ok := got.followup.Deadline()
	if !ok || time.Until(deadline) > 30*time.Second {
		t.Error("post-push context must have its own bounded deadline")
	}
}
