package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Preserve the existing exact PR/ref/authority fixture while exercising the
// production HTTP transport at the failing write and its exact comment readback.
// Every HTTP request is served by a loopback httptest server.
type startupHTTPProvider struct {
	*minimalForgejoClient
	http *forgejointegration.HTTPClient
}

func (p *startupHTTPProvider) AddIssueLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	return p.http.AddIssueLabels(ctx, owner, repo, number, labels)
}

func (p *startupHTTPProvider) CreateIssueComment(ctx context.Context, owner, repo string, number int, body string) error {
	return p.http.CreateIssueComment(ctx, owner, repo, number, body)
}

func (p *startupHTTPProvider) ListIssueComments(ctx context.Context, owner, repo string, number, page, limit int) ([]forgejointegration.PullRequestComment, error) {
	return p.http.ListIssueComments(ctx, owner, repo, number, page, limit)
}

func startupIntegration(provider *startupHTTPProvider, url string) *forgejointegration.Integration {
	enabled := true
	return forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: url, Token: "fixture-token",
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "repo", Enabled: &enabled}},
	}, provider, func(context.Context, forgejointegration.PushRequest) error {
		return fmt.Errorf("unexpected Git push during recovery")
	}, func(_ context.Context, _, _, ref string) (string, error) {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return provider.remoteRefs[strings.TrimPrefix(ref, "refs/heads/")], nil
	})
}

// Reopen the same file and construct a new service; do not reuse worker maps,
// once guards, contexts or in-memory claims from the preceding incarnation.
func restartStartupService(t *testing.T, old *Service) *Service {
	t.Helper()
	old.Wg.Wait()
	var files []struct{ File string }
	if err := old.DB.Raw("PRAGMA database_list").Scan(&files).Error; err != nil || len(files) == 0 {
		t.Fatalf("locate fixture DB: %v %v", files, err)
	}
	sqlDB, err := old.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := gorm.Open(sqlite.Open(files[0].File), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB, _ := database.DB(); _ = sqlDB.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Service{DB: database, Git: old.Git, ForgejoIntegration: old.ForgejoIntegration,
		PrincipalSessions: old.PrincipalSessions, SourceRevision: old.SourceRevision, Ctx: ctx}
}

func startupRefs(t *testing.T, svc *Service) string {
	t.Helper()
	path, err := svc.Git.GetRepoPath(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("git", "--git-dir", path, "show-ref").CombinedOutput()
	if err != nil {
		t.Fatalf("read refs: %v %s", err, output)
	}
	return string(output)
}

func assertStartupUnchangedPRAndRefs(t *testing.T, svc *Service, before db.PullRequest, refs string) {
	t.Helper()
	var after db.PullRequest
	if err := svc.DB.First(&after, before.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || startupRefs(t, svc) != refs {
		t.Fatal("dependency recovery unexpectedly changed the PR or Git refs")
	}
}

func TestStartupDependencyForgejoDispatchRecovery(t *testing.T) {
	for _, fault := range []string{"503", "timeout_after_apply"} {
		t.Run(fault, func(t *testing.T) {
			svc, pr, projection, minimal := setupIntentTestServiceWithClientAt(t, filepath.Join(t.TempDir(), "recovery.sqlite"))
			intent := seedDispatchedIntent(t, svc, pr, projection)
			if err := svc.DB.Model(&intent).Updates(map[string]any{"state": ForgejoActionIntentPlanned, "dispatched_at": nil}).Error; err != nil {
				t.Fatal(err)
			}
			var before db.PullRequest
			if err := svc.DB.First(&before, pr.ID).Error; err != nil {
				t.Fatal(err)
			}
			refs := startupRefs(t, svc)
			var mu sync.Mutex
			requests := []string{}
			effects := 0
			labels := map[string]bool{}
			labelRequest := "POST /api/v1/repos/forgejo/repo/issues/42/labels"
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.Method+" "+r.URL.RequestURI())
				first := len(requests) == 1
				mu.Unlock()
				if r.Method+" "+r.URL.RequestURI() != labelRequest {
					t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", 500)
					return
				}
				var body struct {
					Labels []string `json:"labels"`
				}
				decoder := json.NewDecoder(r.Body)
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&body); err != nil {
					t.Errorf("decode label request: %v", err)
					http.Error(w, "invalid labels", http.StatusBadRequest)
					return
				}
				if err := decoder.Decode(new(any)); err != io.EOF || !reflect.DeepEqual(body.Labels, []string{forgejointegration.AGSActionRebaseLabel}) {
					t.Errorf("unexpected label payload: labels=%v trailing JSON error=%v", body.Labels, err)
					http.Error(w, "unexpected labels", http.StatusBadRequest)
					return
				}
				mu.Lock()
				if !(first && fault == "503") && !labels[body.Labels[0]] {
					labels[body.Labels[0]] = true
					effects++
				}
				mu.Unlock()
				if first && fault == "503" {
					http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
					return
				}
				if first && fault == "timeout_after_apply" {
					select {
					case <-r.Context().Done():
					case <-time.After(time.Second):
					}
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("[]"))
			}))
			defer provider.Close()
			client := &startupHTTPProvider{minimalForgejoClient: minimal, http: forgejointegration.NewHTTPClient(provider.URL, "fixture-token")}
			svc.ForgejoIntegration = startupIntegration(client, provider.URL)
			if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
				t.Fatal(err)
			}
			var uncertain db.PullRequestActionIntent
			if err := svc.DB.First(&uncertain, "id = ?", intent.ID).Error; err != nil {
				t.Fatal(err)
			}
			if uncertain.State != ForgejoActionIntentDispatching || uncertain.FailureCode != "dispatch_outcome_unknown" {
				t.Fatalf("fault lost durable uncertainty: state=%s code=%s", uncertain.State, uncertain.FailureCode)
			}
			mu.Lock()
			firstRequests, firstEffects, firstLabels := append([]string{}, requests...), effects, len(labels)
			mu.Unlock()
			wantEffects := 0
			if fault == "timeout_after_apply" {
				wantEffects = 1
			}
			if !reflect.DeepEqual(firstRequests, []string{labelRequest}) || firstEffects != wantEffects || firstLabels != wantEffects {
				t.Fatalf("fault requests/effects/labels=%v/%d/%d want [%s]/%d/%d", firstRequests, firstEffects, firstLabels, labelRequest, wantEffects, wantEffects)
			}
			previousRequests := len(firstRequests)
			for cycle := 0; cycle < 2; cycle++ {
				svc = restartStartupService(t, svc)
				if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := svc.ResumePendingForgejoProjectionJobs(svc.Ctx); err != nil {
					t.Fatal(err)
				}
				svc.Wg.Wait()
				var recovered db.PullRequestActionIntent
				if err := svc.DB.First(&recovered, "id = ?", intent.ID).Error; err != nil {
					t.Fatal(err)
				}
				if recovered.State != ForgejoActionIntentDispatched || recovered.FailureCode != "" || recovered.DispatchedAt == nil {
					t.Fatalf("restart did not converge: state=%s code=%s", recovered.State, recovered.FailureCode)
				}
				mu.Lock()
				gotRequests, gotEffects := append([]string{}, requests...), effects
				gotLabels := make(map[string]bool, len(labels))
				for label, present := range labels {
					gotLabels[label] = present
				}
				mu.Unlock()
				wantDelta := []string{}
				if cycle == 0 {
					wantDelta = []string{labelRequest}
				}
				if !reflect.DeepEqual(gotRequests, []string{labelRequest, labelRequest}) || gotEffects != 1 || !reflect.DeepEqual(gotLabels, map[string]bool{forgejointegration.AGSActionRebaseLabel: true}) {
					t.Fatalf("restart requests/effects/labels=%v/%d/%v", gotRequests, gotEffects, gotLabels)
				}
				if !reflect.DeepEqual(gotRequests[previousRequests:], wantDelta) {
					t.Fatalf("restart cycle=%d requests delta=%v want %v", cycle, gotRequests[previousRequests:], wantDelta)
				}
				previousRequests = len(gotRequests)
				assertStartupUnchangedPRAndRefs(t, svc, before, refs)
			}
		})
	}
}

func TestStartupDependencyUnknownCommentRecoveredByWorker(t *testing.T) {
	svc, pr, projection, minimal := setupIntentTestServiceWithClientAt(t, filepath.Join(t.TempDir(), "recovery.sqlite"))
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Update("state", ForgejoActionIntentRecovery).Error; err != nil {
		t.Fatal(err)
	}
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA, DesiredAGSHeadSHA: pr.HeadSHA,
		ExternalRepo: projection.ExternalRepo, ExternalNumber: projection.ExternalNumber, Phase: ForgejoProjectionPhaseVerifyingPR,
		PreflightBaseSHA: pr.BaseSHA, ExpectedForgejoOldHeadSHA: strings.Repeat("b", 40), RemoteRef: "refs/heads/" + pr.HeadRef,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	var before db.PullRequest
	if err := svc.DB.First(&before, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	refs := startupRefs(t, svc)
	var mu sync.Mutex
	writes, reads, labelWrites := 0, 0, 0
	requests := []string{}
	commentRead := "GET /api/v1/repos/forgejo/repo/issues/42/comments?page=1&limit=50"
	commentWrite := "POST /api/v1/repos/forgejo/repo/issues/42/comments"
	comments := []forgejointegration.PullRequestComment{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/forgejo/repo/issues/42/labels" {
			mu.Lock()
			labelWrites++
			mu.Unlock()
			_, _ = w.Write([]byte("[]"))
			return
		}
		if r.URL.Path != "/api/v1/repos/forgejo/repo/issues/42/comments" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.Error(w, "unexpected", 500)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			reads++
			_ = json.NewEncoder(w).Encode(comments)
		case http.MethodPost:
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			writes++
			comments = append(comments, forgejointegration.PullRequestComment{Body: body.Body})
			// The provider committed, then the connection died before a response.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer provider.Close()
	client := &startupHTTPProvider{minimalForgejoClient: minimal, http: forgejointegration.NewHTTPClient(provider.URL, "fixture-token")}
	svc.ForgejoIntegration = startupIntegration(client, provider.URL)
	event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber}
	if err := svc.publishForgejoActionRebaseSuccess(ctx, event, pr, job.ID, pr.HeadSHA); err == nil {
		t.Fatal("lost response was reported as known success")
	}
	var uncertain db.PullRequestProjectionJob
	if err := svc.DB.First(&uncertain, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if uncertain.SuccessCommentedAt != nil || uncertain.SuccessCommentClaimToken == "" || uncertain.SuccessCommentDispatchID == "" {
		t.Fatal("unknown write lost durable claim")
	}
	mu.Lock()
	initialRequests := append([]string{}, requests...)
	initialWrites, initialReads, initialLabels, initialComments := writes, reads, labelWrites, len(comments)
	mu.Unlock()
	if !reflect.DeepEqual(initialRequests, []string{commentRead, commentWrite}) || initialWrites != 1 || initialReads != 1 || initialLabels != 0 || initialComments != 1 {
		t.Fatalf("fault requests=%v writes/reads/labels/comments=%d/%d/%d/%d", initialRequests, initialWrites, initialReads, initialLabels, initialComments)
	}
	previousRequests := len(initialRequests)
	for cycle := 0; cycle < 2; cycle++ {
		svc = restartStartupService(t, svc)
		if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := svc.ResumePendingForgejoProjectionJobs(svc.Ctx); err != nil {
			t.Fatal(err)
		}
		svc.Wg.Wait()
		var recovered db.PullRequestProjectionJob
		if err := svc.DB.First(&recovered, job.ID).Error; err != nil {
			t.Fatal(err)
		}
		if recovered.Phase != ForgejoProjectionPhaseProjected || recovered.SuccessCommentedAt == nil || recovered.SuccessCommentClaimToken != "" || recovered.SuccessCommentClaimedAt != nil || recovered.Attempt != uncertain.Attempt {
			t.Fatalf("worker failed exact recovery: phase=%s attempts=%d/%d claim=%t", recovered.Phase, recovered.Attempt, uncertain.Attempt, recovered.SuccessCommentClaimToken != "")
		}
		var completed db.PullRequestActionIntent
		if err := svc.DB.First(&completed, "id = ?", intent.ID).Error; err != nil {
			t.Fatal(err)
		}
		if completed.State != ForgejoActionIntentCompleted || completed.ResultSHA != pr.HeadSHA {
			t.Fatal("intent did not complete exact result")
		}
		mu.Lock()
		gotWrites, gotReads, gotComments, gotLabels := writes, reads, len(comments), labelWrites
		gotRequests := append([]string{}, requests...)
		mu.Unlock()
		if gotWrites != 1 || gotComments != 1 || gotReads != 2 || gotLabels != 0 {
			t.Fatalf("provider writes/comments/reads/labels=%d/%d/%d/%d want 1/1/2/0", gotWrites, gotComments, gotReads, gotLabels)
		}
		if !reflect.DeepEqual(gotRequests, []string{commentRead, commentWrite, commentRead}) {
			t.Fatalf("provider requests=%v want exact read/write/read sequence", gotRequests)
		}
		wantDelta := []string{}
		if cycle == 0 {
			wantDelta = []string{commentRead}
		}
		if !reflect.DeepEqual(gotRequests[previousRequests:], wantDelta) {
			t.Fatalf("restart cycle=%d requests delta=%v want %v", cycle, gotRequests[previousRequests:], wantDelta)
		}
		previousRequests = len(gotRequests)
		t.Logf("recovery cycle=%d provider comment writes=%d reads=%d label writes=%d", cycle, gotWrites, gotReads, gotLabels)
		assertStartupUnchangedPRAndRefs(t, svc, before, refs)
	}
}
