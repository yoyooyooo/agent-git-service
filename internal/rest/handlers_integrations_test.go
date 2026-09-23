package rest_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/rest"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func seedDispatchedIntent(t *testing.T, svc *service.Service, prID, repoID uint, agsPRNum, forgejoPRNum int, forgejoRepo, headRef, baseRef, headSHA string, postLabels []string) {
	t.Helper()
	var pr db.PullRequest
	if err := svc.DB.Preload("Repository").First(&pr, prID).Error; err != nil {
		t.Fatalf("load exact intent PR: %v", err)
	}
	baseSHA, err := svc.Git.HeadSHA(context.Background(), pr.Repository.FullName, baseRef)
	if err != nil {
		t.Fatalf("load exact intent base: %v", err)
	}
	encoded, err := json.Marshal(postLabels)
	if err != nil {
		t.Fatalf("marshal post labels: %v", err)
	}
	if svc.PrincipalSessions == nil {
		authority, authorityErr := sessionauthority.New(sessionauthority.Config{
			Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
			TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "rest-test", Issuer: "test", KeyIDs: []string{"test-key"}, Status: "active", TrustRevision: "trust-v1"}},
			TeamBindings:   []sessionauthority.TeamBinding{{ID: "rest-team", IssuerInstanceID: "rest-test", WorkspaceID: "workspace-1", TeamIdentityID: "rest-team", PolicyClass: "rest.action.v1", PrincipalID: pr.Repository.OwnerID, Status: "active", BindingRevision: "rest-team-v1", EpochFloor: 1}},
			PolicyClasses:  []sessionauthority.PolicyClass{{ID: "rest.action.v1", Status: "active", PolicyRevision: "rest-class-v1", Operations: []string{"pr.rebase"}}},
			Resources:      []sessionauthority.ResourcePolicy{{ID: "rest-repo", Target: "test", Service: "ags", Repository: pr.Repository.FullName, Status: "active", MaxSessionTTL: "15m", PolicyRevision: "rest-repo-v1"}},
		})
		if authorityErr != nil {
			t.Fatalf("create REST action authority: %v", authorityErr)
		}
		svc.PrincipalSessions = authority
	}
	intent := db.PullRequestActionIntent{
		ID:              uuid.NewString(),
		IdempotencyKey:  uuid.NewString(),
		Action:          "pr.rebase",
		State:           service.ForgejoActionIntentDispatched,
		PullRequestID:   prID,
		RepositoryID:    repoID,
		AGSPRNumber:     agsPRNum,
		Repository:      pr.Repository.FullName,
		ForgejoRepo:     forgejoRepo,
		ForgejoPRNumber: forgejoPRNum,
		HeadRef:         headRef,
		BaseRef:         baseRef,
		ExpectedHeadSHA: headSHA,
		ExpectedBaseSHA: baseSHA,
		ExpectedLabels:  "[]",
		PostLabels:      string(encoded),
		PrincipalID:     pr.Repository.OwnerID,
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}
	if err := service.StampDurableActionIntentAuthorityForTest(context.Background(), svc, &intent); err != nil {
		t.Fatalf("stamp durable intent authority: %v", err)
	}
	if err := svc.DB.Create(&intent).Error; err != nil {
		t.Fatalf("seed dispatched intent: %v", err)
	}
}

func TestRequestForgejoActionRebaseRejectsNullSHABeforeIntentOrProviderWrite(t *testing.T) {
	h := testharness.New(t)
	client := &restForgejoWebhookClient{}
	h.Svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "token",
	}, client, nil)
	canonical := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "null expected head", body: fmt.Sprintf(`{"idempotency_key":"null-head","forgejo_pr_number":42,"expected_head_sha":null,"expected_base_sha":%q,"expected_labels":[],"post_labels":[%q]}`, canonical, forgejointegration.AGSActionRebaseLabel)},
		{name: "null expected base", body: fmt.Sprintf(`{"idempotency_key":"null-base","forgejo_pr_number":42,"expected_head_sha":%q,"expected_base_sha":null,"expected_labels":[],"post_labels":[%q]}`, canonical, forgejointegration.AGSActionRebaseLabel)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v3/repos/testuser/repo/pulls/1/actions/pr.rebase", strings.NewReader(tc.body))
			routeCtx := chi.NewRouteContext()
			routeCtx.URLParams.Add("owner", "testuser")
			routeCtx.URLParams.Add("repo", "repo")
			routeCtx.URLParams.Add("number", "1")
			ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx)
			req = req.WithContext(service.ContextWithUser(ctx, h.User))
			recorder := httptest.NewRecorder()
			(&rest.Deps{Svc: h.Svc}).RequestForgejoActionRebase(recorder, req)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
	var intents int64
	if err := h.DB.Model(&db.PullRequestActionIntent{}).Count(&intents).Error; err != nil {
		t.Fatal(err)
	}
	if intents != 0 || len(client.added) != 0 || len(client.removed) != 0 || len(client.comments) != 0 {
		t.Fatalf("null SHA crossed validation boundary: intents=%d added=%v removed=%v comments=%v", intents, client.added, client.removed, client.comments)
	}
}

func TestForgejoWebhookUsesContextIndependentFromRequestCancel(t *testing.T) {
	svc, cleanup := testharness.NewService(t, testharness.ServiceConfig{})
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "operator", Name: "operator", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "operator", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/rebase-webhook"
	if err := svc.Git.CreateBranch(ctx, "operator/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, "operator/repo", branch, "feature.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "operator/repo", Title: "Projection label", HeadRef: branch, BaseRef: "main", AuthorLogin: "operator"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	baseSHA, err := svc.Git.WriteFile(ctx, "operator/repo", "main", "base.txt", "base", []byte("base\n"))
	if err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID:  pr.ID,
		RepositoryID:   pr.RepositoryID,
		Provider:       service.ProjectionProviderForgejo,
		ExternalRepo:   "forgejo/repo",
		ExternalNumber: 42,
		ExternalURL:    "http://forgejo.local/forgejo/repo/pulls/42",
		SourceBranch:   branch,
		TargetBranch:   "main",
		State:          service.ProjectionStateOpen,
		LastSyncedSHA:  pr.HeadSHA,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}

	repoPath, err := svc.Git.GetRepoPath(ctx, "operator/repo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	forgejoRoot := t.TempDir()
	forgejoBare := filepath.Join(forgejoRoot, "forgejo", "repo.git")
	if err := os.MkdirAll(filepath.Dir(forgejoBare), 0o755); err != nil {
		t.Fatalf("create Forgejo repo parent: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", forgejoBare).CombinedOutput(); err != nil {
		t.Fatalf("init Forgejo bare repo: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, pr.HeadSHA+":refs/heads/"+branch, baseSHA+":refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("seed Forgejo refs: %v: %s", err, out)
	}
	forgejoClient := &restForgejoWebhookClient{headSHA: pr.HeadSHA, headRef: branch, baseRef: "main", permissions: map[string]string{"operator": "owner"}, labels: []string{forgejointegration.AGSActionRebaseLabel}}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"operator/repo": {Owner: "forgejo", Repo: "repo", BaseBranch: "main"},
		},
	}, forgejoClient, func(ctx context.Context, req forgejointegration.PushRequest) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := forgejointegration.GitPush(ctx, req); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "git", "-C", req.RepoPath, "ls-remote", req.RemoteURL, "refs/heads/"+req.BranchName).Output()
		if err != nil {
			return err
		}
		forgejoClient.headSHA = strings.Fields(string(out))[0]
		return nil
	})

	body := []byte(fmt.Sprintf(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/repo", "default_branch": "main"},
	  "pull_request": {
	    "number": 42,
	    "html_url": "http://forgejo.local/forgejo/repo/pulls/42",
	    "head": {"ref": "agent/rebase-webhook", "sha": %q},
	    "base": {"ref": "main"},
	    "labels": [{"name": "ags/action-rebase"}]
	  },
	  "label": {"name": "ags/action-rebase"},
	  "sender": {"login": "operator"}
	}`, pr.HeadSHA))
	seedDispatchedIntent(t, svc, pr.ID, pr.RepositoryID, pr.Number, 42, "forgejo/repo", branch, "main", pr.HeadSHA, []string{forgejointegration.AGSActionRebaseLabel})
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)

	reqCtx, reqCancel := context.WithCancel(context.Background())
	reqCancel()
	req := httptest.NewRequest(http.MethodPost, "/api/v3/integrations/forgejo/webhook", bytes.NewReader(body)).WithContext(reqCtx)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rr := httptest.NewRecorder()

	(&rest.Deps{Svc: svc}).ForgejoWebhook(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	updated, err := svc.GetPR(ctx, "operator/repo", pr.Number)
	if err != nil {
		t.Fatalf("get updated PR: %v", err)
	}
	if updated.HeadSHA == pr.HeadSHA || forgejoClient.headSHA != updated.HeadSHA {
		t.Fatalf("rebase/projection did not complete: old=%s updated=%s forgejo=%s", pr.HeadSHA, updated.HeadSHA, forgejoClient.headSHA)
	}
	if !containsString(forgejoClient.removed, forgejointegration.AGSActionRebaseLabel) || !containsString(forgejoClient.added, forgejointegration.AGSStatusRebasingLabel) {
		t.Fatalf("label workflow did not run: removed=%v added=%v", forgejoClient.removed, forgejoClient.added)
	}
}

type restForgejoWebhookClient struct {
	prNumber    int
	headSHA     string
	headRef     string
	baseRef     string
	permissions map[string]string
	labels      []string
	added       []string
	removed     []string
	comments    []string
}

func (f *restForgejoWebhookClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}

func (f *restForgejoWebhookClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{Number: f.number(), URL: fmt.Sprintf("http://forgejo.local/pulls/%d", f.number()), ExternalRepo: in.Owner + "/" + in.Repo, HeadSHA: f.headSHA}, nil
}

func (f *restForgejoWebhookClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{Number: number, URL: fmt.Sprintf("http://forgejo.local/pulls/%d", number), ExternalRepo: owner + "/" + repo, HeadSHA: f.headSHA}, nil
}

func (f *restForgejoWebhookClient) ListPullRequests(ctx context.Context, owner, repo, state string) ([]forgejointegration.PullRequestSnapshot, error) {
	return []forgejointegration.PullRequestSnapshot{{
		Number: f.number(), URL: fmt.Sprintf("http://forgejo.local/pulls/%d", f.number()), State: "open",
		HeadRef: f.headRef, HeadSHA: f.headSHA, BaseRef: f.baseRef,
	}}, nil
}

func (f *restForgejoWebhookClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (forgejointegration.PullRequestSnapshot, bool, error) {
	if number != f.number() {
		return forgejointegration.PullRequestSnapshot{}, false, nil
	}
	return forgejointegration.PullRequestSnapshot{
		Number: number, URL: fmt.Sprintf("http://forgejo.local/pulls/%d", number), State: "open",
		HeadRef: f.headRef, HeadSHA: f.headSHA, BaseRef: f.baseRef,
	}, true, nil
}

func (f *restForgejoWebhookClient) AddIssueLabels(ctx context.Context, owner, repo string, issueNumber int, labels []string) error {
	f.added = append(f.added, labels...)
	for _, label := range labels {
		if !containsString(f.labels, label) {
			f.labels = append(f.labels, label)
		}
	}
	return nil
}

func (f *restForgejoWebhookClient) RemoveIssueLabel(ctx context.Context, owner, repo string, issueNumber int, label string) error {
	f.removed = append(f.removed, label)
	out := f.labels[:0]
	for _, existing := range f.labels {
		if existing != label {
			out = append(out, existing)
		}
	}
	f.labels = out
	return nil
}

func (f *restForgejoWebhookClient) ListIssueLabels(ctx context.Context, owner, repo string, issueNumber int) ([]string, error) {
	return append([]string(nil), f.labels...), nil
}

func (f *restForgejoWebhookClient) CreateIssueComment(ctx context.Context, owner, repo string, issueNumber int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

func (f *restForgejoWebhookClient) ListIssueComments(ctx context.Context, owner, repo string, issueNumber, page, limit int) ([]forgejointegration.PullRequestComment, error) {
	if page > 1 {
		return nil, nil
	}
	comments := make([]forgejointegration.PullRequestComment, 0, len(f.comments))
	for _, body := range f.comments {
		comments = append(comments, forgejointegration.PullRequestComment{Body: body})
	}
	return comments, nil
}

func (f *restForgejoWebhookClient) CollaboratorPermission(ctx context.Context, owner, repo, username string) (string, error) {
	permission, ok := f.permissions[username]
	if !ok {
		return "", errors.New("permission not configured")
	}
	return permission, nil
}

func (f *restForgejoWebhookClient) number() int {
	if f.prNumber > 0 {
		return f.prNumber
	}
	return 42
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
