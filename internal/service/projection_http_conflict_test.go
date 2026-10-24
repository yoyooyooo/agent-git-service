package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gittransport"
)

// A competing creator may commit while the provider's PR list is still stale.
// Exhausting bounded readback must leave the job eligible for later recovery.
func TestProjectionDiagnosticPRCreateConflictKeepsRecovery(t *testing.T) {
	for _, readbackStatus := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(readbackStatus), func(t *testing.T) {
			var posts, gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					if gets.Add(1) > 1 {
						w.WriteHeader(readbackStatus)
					}
					fmt.Fprint(w, `[]`)
				case http.MethodPost:
					posts.Add(1)
					w.WriteHeader(http.StatusConflict)
					fmt.Fprint(w, `{"message":"pull request already exists; sentinel-body"}`)
				default:
					t.Errorf("unexpected method %s", r.Method)
				}
			}))
			defer server.Close()
			_, err := forgejointegration.NewHTTPClient(server.URL, "").EnsurePullRequest(context.Background(), forgejointegration.PullRequestRequest{Owner: "fixture", Repo: "repo", Head: "topic", Base: "main"})
			assertProjectionHTTPConflict(t, err, "pull_request")
			if posts.Load() != 1 || gets.Load() != 6 {
				t.Fatalf("expected one POST and bounded readback, got POST=%d GET=%d", posts.Load(), gets.Load())
			}
		})
	}
}

func TestProjectionDiagnosticMergeConflictIsNotRefCAS(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/repos/fixture/repo/pulls/7/merge" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		posts.Add(1)
		w.WriteHeader(http.StatusConflict)
		// Even CAS-looking arbitrary body text is not typed ref evidence.
		fmt.Fprint(w, `{"message":"cannot lock ref: but expected; sentinel-body"}`)
	}))
	defer server.Close()
	err := forgejointegration.NewHTTPClient(server.URL, "").MergePullRequest(context.Background(), "fixture", "repo", 7, forgejointegration.PullRequestMergeRequest{})
	assertProjectionHTTPConflict(t, err, "pull_merge")
	if posts.Load() != 1 {
		t.Fatal("merge was replayed")
	}
}

func assertProjectionHTTPConflict(t *testing.T, err error, route string) {
	t.Helper()
	var typed *forgejointegration.ProviderHTTPError
	if !errors.As(err, &typed) || typed.Status != http.StatusConflict || typed.Method != http.MethodPost || typed.RouteKind != route {
		t.Fatalf("lost typed HTTP evidence: %v", err)
	}
	classified := forgejointegration.ClassifyProjectionError("refs/heads/topic", "", err)
	if classified.Type != forgejointegration.ProjectionFailureUnknown || classified.Diagnostic == nil || classified.Diagnostic.Category != "http_conflict" || classified.Diagnostic.EvidenceSource != "http_status" || classified.Diagnostic.HTTPStatus == nil || *classified.Diagnostic.HTTPStatus != http.StatusConflict {
		t.Fatalf("HTTP conflict misclassified: %+v", classified)
	}
	for _, defaultRetryable := range []bool{true, false} {
		failure := newProjectionJobFailure("refs/heads/topic", "", fmt.Errorf("provider: %w", err), defaultRetryable)
		if failure.retryable != defaultRetryable || failure.failureType != forgejointegration.ProjectionFailureUnknown || strings.Contains(failure.summary+err.Error(), "sentinel-body") {
			t.Fatalf("HTTP conflict changed caller recovery policy or exposed body: %+v", failure)
		}
	}
}

func TestProjectionDiagnosticExpectedOldMismatchRemainsTerminal(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("fixture Git failed: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--bare")
	tree := git("mktree")
	old := git("commit-tree", tree, "-m", "old")
	actual := git("commit-tree", tree, "-p", old, "-m", "actual")
	next := git("commit-tree", tree, "-p", old, "-m", "next")
	ref := "refs/heads/topic"
	git("update-ref", ref, actual)
	_, err := gittransport.Run(context.Background(), dir, "", "-C", dir, "update-ref", ref, next, old)
	var typed *gittransport.TransportError
	if !errors.As(err, &typed) || typed.Failure().Category != "cas_conflict" || typed.Failure().ReasonCode != "expected_old_mismatch" {
		t.Fatalf("lost actual Git CAS rejection: %v", err)
	}
	failure := newProjectionJobFailure(ref, next, err, true)
	if failure.retryable || failure.failureType != forgejointegration.ProjectionFailureNonFastForward || git("rev-parse", ref) != actual {
		t.Fatalf("real expected-old mismatch must remain terminal and leave ref intact: %+v", failure)
	}
}
