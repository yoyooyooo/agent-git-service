package rest_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

type restAcceptanceOutboundDispatcher struct {
	results []service.OutboundDeliveryResult
	calls   []service.OutboundDispatchCall
}

func (d *restAcceptanceOutboundDispatcher) DispatchOutbound(_ context.Context, call service.OutboundDispatchCall) service.OutboundDeliveryResult {
	d.calls = append(d.calls, call)
	if len(d.results) == 0 {
		return service.OutboundDeliveryResult{Delivered: true}
	}
	result := d.results[0]
	d.results = d.results[1:]
	return result
}

func TestForgejoRebaseProjectionIncidentAcceptance(t *testing.T) {
	h := testharness.New(t)
	ctx := context.Background()
	const (
		repoFullName    = "operator/acceptance"
		branch          = "agent/rebase-acceptance"
		agsPRNumber     = 127
		forgejoPRNumber = 116
	)
	if err := h.DB.Create(&db.User{Login: "operator", Name: "operator", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := h.Svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "operator", Name: "acceptance", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := h.Svc.Git.CreateBranch(ctx, repoFullName, branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := h.Svc.Git.WriteFile(ctx, repoFullName, branch, "feature.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := h.Svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: repoFullName, Title: "acceptance", HeadRef: branch, BaseRef: "main", AuthorLogin: "operator"})
	if err != nil {
		t.Fatalf("create PR: %v", err)
	}
	if err := h.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("number", agsPRNumber).Error; err != nil {
		t.Fatalf("set AGS PR number: %v", err)
	}
	pr.Number = agsPRNumber
	oldHead := pr.HeadSHA
	baseSHA, err := h.Svc.Git.WriteFile(ctx, repoFullName, "main", "base.txt", "advance base", []byte("base\n"))
	if err != nil {
		t.Fatalf("advance base: %v", err)
	}
	if err := h.Svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "forgejo/acceptance", ExternalNumber: forgejoPRNumber,
		ExternalURL: "http://forgejo.local/forgejo/acceptance/pulls/116", SourceBranch: branch,
		TargetBranch: "main", State: service.ProjectionStateOpen, LastSyncedSHA: oldHead,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}

	repoPath, err := h.Svc.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	forgejoRoot := t.TempDir()
	forgejoBare := filepath.Join(forgejoRoot, "forgejo", "acceptance.git")
	if err := os.MkdirAll(filepath.Dir(forgejoBare), 0o755); err != nil {
		t.Fatalf("create Forgejo parent: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", forgejoBare).CombinedOutput(); err != nil {
		t.Fatalf("init Forgejo bare repo: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, oldHead+":refs/heads/"+branch, baseSHA+":refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("seed Forgejo refs: %v: %s", err, out)
	}

	client := &restForgejoWebhookClient{
		prNumber: forgejoPRNumber, headSHA: oldHead, headRef: branch, baseRef: "main",
		permissions: map[string]string{"operator": "owner"}, labels: []string{forgejointegration.AGSActionRebaseLabel},
	}
	dispatcher := &restAcceptanceOutboundDispatcher{results: []service.OutboundDeliveryResult{
		{Delivered: false, Retryable: true, Code: "feishu_11232", Message: "frequency limited"},
		{Delivered: true},
		{Delivered: true},
	}}
	h.Svc.OutboundDispatcher = dispatcher
	h.Svc.OutboundEventTargets = map[string][]service.OutboundTarget{
		service.OutboundEventProjectionDrift: {{Name: "acceptance-feishu", Type: service.OutboundTargetTypeFeishuWebhook}},
	}
	pushAttempts := 0
	h.Svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{repoFullName: {Owner: "forgejo", Repo: "acceptance", BaseBranch: "main"}},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushAttempts++
		if pushAttempts <= 2 {
			return &url.Error{Op: "push", URL: req.RemoteURL, Err: syscall.ECONNRESET}
		}
		if err := forgejointegration.GitPush(ctx, req); err != nil {
			return err
		}
		remote, err := forgejointegration.GitRemoteRefSHA(ctx, req.RepoPath, req.RemoteURL, "refs/heads/"+req.BranchName)
		if err != nil {
			return err
		}
		client.headSHA = remote
		return nil
	})

	body := []byte(fmt.Sprintf(`{"action":"label_updated","repository":{"full_name":"forgejo/acceptance","default_branch":"main"},"pull_request":{"number":%d,"html_url":"http://forgejo.local/forgejo/acceptance/pulls/116","head":{"ref":%q,"sha":%q},"base":{"ref":"main"},"labels":[{"name":"ags/action-rebase"}]},"label":{"name":"ags/action-rebase"},"sender":{"login":"operator"}}`, forgejoPRNumber, branch, oldHead))
	seedDispatchedIntent(t, h.Svc, pr.ID, pr.RepositoryID, agsPRNumber, forgejoPRNumber, "forgejo/acceptance", branch, "main", oldHead, []string{forgejointegration.AGSActionRebaseLabel})
	postWebhook := func(payload []byte, delivery string) *httptest.ResponseRecorder {
		t.Helper()
		mac := hmac.New(sha256.New, []byte("secret"))
		_, _ = mac.Write(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v3/integrations/forgejo/webhook", bytes.NewReader(payload))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-Forgejo-Delivery", delivery)
		rec := httptest.NewRecorder()
		h.Mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("webhook status=%d body=%s", rec.Code, rec.Body.String())
		}
		return rec
	}

	postWebhook(body, "acceptance-failure")
	failedPR, err := h.Svc.GetPR(ctx, repoFullName, agsPRNumber)
	if err != nil || failedPR.HeadSHA == oldHead {
		var failedJob db.PullRequestProjectionJob
		_ = h.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&failedJob).Error
		t.Fatalf("first action did not persist rebased head: pr=%#v job=%#v comments=%v err=%v", failedPR, failedJob, client.comments, err)
	}
	desired := failedPR.HeadSHA
	assertNoSuccessComment(t, client.comments)
	if !containsString(client.labels, forgejointegration.AGSStatusProjectionDriftLabel) {
		t.Fatalf("failure labels=%v", client.labels)
	}
	var active db.ProjectionRefState
	if err := h.DB.Where("provider = ? AND repository_id = ? AND ref = ? AND status = ?", service.ProjectionProviderForgejo, pr.RepositoryID, "refs/heads/"+branch, service.ProjectionStatusActive).First(&active).Error; err != nil {
		t.Fatalf("active drift: %v", err)
	}
	if active.AGSSHA != desired || active.ExternalSHA != oldHead {
		t.Fatalf("active drift facts=%#v", active)
	}
	var alert db.OutboundDelivery
	if err := h.DB.Where("event_type = ? AND subject_key LIKE ?", service.OutboundEventProjectionDrift, "%:active:%").First(&alert).Error; err != nil {
		t.Fatalf("active outbound: %v", err)
	}
	if alert.Status != service.OutboundDeliveryStatusRetryWait {
		t.Fatalf("active outbound status=%s", alert.Status)
	}
	payload := string(alert.PayloadJSON)
	for _, want := range []string{"AGS repo/PR: operator/acceptance#127", "Forgejo PR: http://forgejo.local/forgejo/acceptance/pulls/116", "Expected Forgejo SHA: " + desired, "Actual Forgejo SHA: " + oldHead, "Recovery:"} {
		if !strings.Contains(payload, want) {
			t.Fatalf("alert payload missing %q: %s", want, payload)
		}
	}
	for _, forbidden := range []string{"token", "secret", "access_token="} {
		if strings.Contains(strings.ToLower(payload), forbidden) {
			t.Fatalf("alert payload leaked %q: %s", forbidden, payload)
		}
	}
	if err := h.Svc.DeliverOutboundDeliveryNow(ctx, alert.ID); err != nil {
		t.Fatalf("retry Feishu delivery: %v", err)
	}

	client.labels = []string{forgejointegration.AGSActionRebaseLabel, forgejointegration.AGSStatusProjectionDriftLabel}
	seedDispatchedIntent(t, h.Svc, pr.ID, pr.RepositoryID, agsPRNumber, forgejoPRNumber, "forgejo/acceptance", branch, "main", oldHead, []string{forgejointegration.AGSActionRebaseLabel, forgejointegration.AGSStatusProjectionDriftLabel})
	postWebhook(body, "acceptance-duplicate-resume")
	secondPR, err := h.Svc.GetPR(ctx, repoFullName, agsPRNumber)
	if err != nil || secondPR.HeadSHA != desired {
		t.Fatalf("duplicate label created another rebase: got=%#v desired=%s err=%v", secondPR, desired, err)
	}
	assertNoSuccessComment(t, client.comments)
	var interrupted db.PullRequestProjectionJob
	if err := h.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&interrupted).Error; err != nil {
		t.Fatalf("load interrupted job: %v", err)
	}
	if interrupted.DesiredAGSHeadSHA != desired || interrupted.ActionGeneration != 1 || interrupted.ActionIntentID == nil || interrupted.Phase != service.ForgejoProjectionPhaseFailedRetryable {
		t.Fatalf("stale signed retry changed the interrupted exact generation: %#v", interrupted)
	}
	var deniedRetry db.PullRequestActionIntent
	if err := h.DB.Where("pull_request_id = ?", pr.ID).Order("created_at DESC").First(&deniedRetry).Error; err != nil ||
		deniedRetry.State != service.ForgejoActionIntentDenied || deniedRetry.FailureCode != "exact_action_fact_drift" {
		t.Fatalf("stale signed retry intent=%#v err=%v", deniedRetry, err)
	}
	if pushAttempts != 1 {
		t.Fatalf("stale signed retry reached provider: attempts=%d", pushAttempts)
	}
}

func assertNoSuccessComment(t *testing.T, comments []string) {
	t.Helper()
	for _, comment := range comments {
		if strings.Contains(comment, "AGS rebase completed") || strings.Contains(comment, "projection was refreshed") {
			t.Fatalf("failure path emitted success comment: %s", comment)
		}
	}
}
