package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

type projectionCloseForgejoClient struct {
	closedState  string
	closedNumber int
}

func (f *projectionCloseForgejoClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}

func (f *projectionCloseForgejoClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{Number: 42, URL: "http://forgejo.local/pulls/42", ExternalRepo: in.Owner + "/" + in.Repo}, nil
}

func (f *projectionCloseForgejoClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	f.closedNumber = number
	f.closedState = state
	return forgejointegration.PullRequestResult{Number: number, URL: "http://forgejo.local/pulls/42", ExternalRepo: owner + "/" + repo}, nil
}

type projectionSyncForgejoClient struct {
	headSHA  string
	requests []forgejointegration.PullRequestRequest
}

func (f *projectionSyncForgejoClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}

func (f *projectionSyncForgejoClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	f.requests = append(f.requests, in)
	return forgejointegration.PullRequestResult{Number: 42, URL: "http://forgejo.local/pulls/42", ExternalRepo: in.Owner + "/" + in.Repo, HeadSHA: f.headSHA}, nil
}

func (f *projectionSyncForgejoClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{Number: number, URL: "http://forgejo.local/pulls/42", ExternalRepo: owner + "/" + repo, HeadSHA: f.headSHA}, nil
}

type projectionWorkflowForgejoClient struct {
	headSHA               string
	headRef               string
	baseRef               string
	prNumber              int
	permissions           map[string]string
	labels                []string
	added                 []string
	removed               []string
	comments              []string
	operations            []string
	listPullRequestsCalls int
	onListPullRequests    func(int) error
}

func (f *projectionWorkflowForgejoClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}

func (f *projectionWorkflowForgejoClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{Number: 42, URL: "http://forgejo.local/pulls/42", ExternalRepo: in.Owner + "/" + in.Repo, HeadSHA: f.headSHA}, nil
}

func (f *projectionWorkflowForgejoClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{Number: number, URL: "http://forgejo.local/pulls/42", ExternalRepo: owner + "/" + repo, HeadSHA: f.headSHA}, nil
}

func (f *projectionWorkflowForgejoClient) ListPullRequests(ctx context.Context, owner, repo, state string) ([]forgejointegration.PullRequestSnapshot, error) {
	f.listPullRequestsCalls++
	if f.onListPullRequests != nil {
		if err := f.onListPullRequests(f.listPullRequestsCalls); err != nil {
			return nil, err
		}
	}
	number := f.prNumber
	if number == 0 {
		number = 42
	}
	return []forgejointegration.PullRequestSnapshot{{
		Number: number, URL: fmt.Sprintf("http://forgejo.local/%s/%s/pulls/%d", owner, repo, number), State: "open",
		HeadRef: f.headRef, HeadSHA: f.headSHA, BaseRef: f.baseRef,
	}}, nil
}

func (f *projectionWorkflowForgejoClient) GetPullRequest(ctx context.Context, owner, repo string, number int) (forgejointegration.PullRequestSnapshot, bool, error) {
	f.listPullRequestsCalls++
	if f.onListPullRequests != nil {
		if err := f.onListPullRequests(f.listPullRequestsCalls); err != nil {
			return forgejointegration.PullRequestSnapshot{}, false, err
		}
	}
	configured := f.prNumber
	if configured == 0 {
		configured = 42
	}
	if number != configured {
		return forgejointegration.PullRequestSnapshot{}, false, nil
	}
	return forgejointegration.PullRequestSnapshot{
		Number: number, URL: fmt.Sprintf("http://forgejo.local/%s/%s/pulls/%d", owner, repo, number), State: "open",
		HeadRef: f.headRef, HeadSHA: f.headSHA, BaseRef: f.baseRef,
	}, true, nil
}

func (f *projectionWorkflowForgejoClient) AddIssueLabels(ctx context.Context, owner, repo string, issueNumber int, labels []string) error {
	f.added = append(f.added, labels...)
	for _, label := range labels {
		f.operations = append(f.operations, "add:"+label)
		if !slices.Contains(f.labels, label) {
			f.labels = append(f.labels, label)
		}
	}
	return nil
}

func (f *projectionWorkflowForgejoClient) RemoveIssueLabel(ctx context.Context, owner, repo string, issueNumber int, label string) error {
	f.removed = append(f.removed, label)
	f.operations = append(f.operations, "remove:"+label)
	f.labels = slices.DeleteFunc(f.labels, func(existing string) bool { return existing == label })
	return nil
}

func (f *projectionWorkflowForgejoClient) ListIssueLabels(ctx context.Context, owner, repo string, issueNumber int) ([]string, error) {
	return append([]string(nil), f.labels...), nil
}

func (f *projectionWorkflowForgejoClient) CreateIssueComment(ctx context.Context, owner, repo string, issueNumber int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

func (f *projectionWorkflowForgejoClient) ListIssueComments(ctx context.Context, owner, repo string, issueNumber, page, limit int) ([]forgejointegration.PullRequestComment, error) {
	if page > 1 {
		return nil, nil
	}
	comments := make([]forgejointegration.PullRequestComment, 0, len(f.comments))
	for _, body := range f.comments {
		comments = append(comments, forgejointegration.PullRequestComment{Body: body})
	}
	return comments, nil
}

func (f *projectionWorkflowForgejoClient) CollaboratorPermission(ctx context.Context, owner, repo, username string) (string, error) {
	if f.permissions == nil {
		return "", errors.New("permission not configured")
	}
	permission, ok := f.permissions[username]
	if !ok {
		return "", errors.New("permission not configured")
	}
	return permission, nil
}

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
			TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "projection-test", Issuer: "test", KeyIDs: []string{"test-key"}, Status: "active", TrustRevision: "trust-v1"}},
			TeamBindings:   []sessionauthority.TeamBinding{{ID: "projection-team", IssuerInstanceID: "projection-test", WorkspaceID: "workspace-1", TeamIdentityID: "projection-team", PolicyClass: "projection.action.v1", PrincipalID: pr.Repository.OwnerID, Status: "active", BindingRevision: "projection-team-v1", EpochFloor: 1}},
			PolicyClasses:  []sessionauthority.PolicyClass{{ID: "projection.action.v1", Status: "active", PolicyRevision: "projection-class-v1", Operations: []string{"pr.rebase"}}},
			Resources:      []sessionauthority.ResourcePolicy{{ID: "projection-repo", Target: "test", Service: "ags", Repository: pr.Repository.FullName, Status: "active", MaxSessionTTL: "15m", PolicyRevision: "projection-repo-v1"}},
		})
		if authorityErr != nil {
			t.Fatalf("create projection action authority: %v", authorityErr)
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

type projectionCloseGitLabClient struct {
	closeSource string
	closeTarget string
	closeNote   string
}

func (f *projectionCloseGitLabClient) EnsureMergeRequest(ctx context.Context, project string, in gitlabintegration.MergeRequestInput) (gitlabintegration.MergeRequest, error) {
	return gitlabintegration.MergeRequest{IID: 7, WebURL: "http://gitlab.local/mr/7"}, nil
}

func (f *projectionCloseGitLabClient) CloseMergeRequestForBranch(ctx context.Context, project, sourceBranch, targetBranch, note string) (gitlabintegration.MergeRequest, bool, error) {
	f.closeSource = sourceBranch
	f.closeTarget = targetBranch
	f.closeNote = note
	return gitlabintegration.MergeRequest{IID: 7, WebURL: "http://gitlab.local/mr/7"}, true, nil
}

type projectionCloseGitHubClient struct {
	closeSource string
	closeTarget string
	closeNote   string
}

func (f *projectionCloseGitHubClient) EnsurePullRequest(ctx context.Context, repo string, in githubintegration.PullRequestInput) (githubintegration.PullRequest, error) {
	return githubintegration.PullRequest{Number: 8, HTMLURL: "https://github.com/example-org/project-kit/pull/8"}, nil
}

func (f *projectionCloseGitHubClient) ClosePullRequestForBranch(ctx context.Context, repo, sourceBranch, targetBranch, note string) (githubintegration.PullRequest, bool, error) {
	f.closeSource = sourceBranch
	f.closeTarget = targetBranch
	f.closeNote = note
	return githubintegration.PullRequest{Number: 8, HTMLURL: "https://github.com/example-org/project-kit/pull/8"}, true, nil
}

func TestCreatePRPersistsAGSFactWhenHeadIsExcludedFromForgejoProjection(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "projection-owner", Name: "projection-owner", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "projection-owner", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "projection-owner/repo", "claude/debug", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"main", "agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"projection-owner/repo": {Owner: "forgejo", Repo: "repo", BaseBranch: "main"},
		},
	}, &projectionSyncForgejoClient{}, nil)

	pr, err := svc.CreatePR(ctx, service.CreatePRInput{
		RepoFullName: "projection-owner/repo",
		Title:        "must remain available",
		HeadRef:      "claude/debug",
		BaseRef:      "main",
		AuthorLogin:  "projection-owner",
	})
	if err != nil {
		t.Fatalf("provider projection eligibility must not reject AGS PR creation: %v", err)
	}
	if pr.ID == 0 || pr.Number == 0 || pr.HeadRef != "claude/debug" || pr.State != db.StateOpen {
		t.Fatalf("unexpected durable AGS PR: %#v", pr)
	}
	var count int64
	if err := svc.DB.Model(&db.PullRequest{}).Where("repository_id = (SELECT id FROM repositories WHERE full_name = ?)", "projection-owner/repo").Count(&count).Error; err != nil {
		t.Fatalf("count PRs: %v", err)
	}
	if count != 1 {
		t.Fatalf("excluded provider projection must preserve one AGS PR fact, count=%d", count)
	}
}

func TestPullRequestProjectionMarksAGSPRFromForgejoMerge(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	mergeSHA := strings.Repeat("a", 40)

	merged, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, mergeSHA, "forgejo")
	if err != nil {
		t.Fatalf("MarkPullRequestMergedFromProjection: %v", err)
	}
	if !merged.Merged || merged.State != db.StateClosed || merged.MergeCommitSHA != mergeSHA || merged.MergedByLogin != "forgejo" {
		t.Fatalf("merged PR not updated: %#v", merged)
	}

	var rows []db.PullRequestProjection
	if err := svc.DB.Where("pull_request_id = ?", pr.ID).Find(&rows).Error; err != nil {
		t.Fatalf("load projections: %v", err)
	}
	if len(rows) != 1 || rows[0].State != service.ProjectionStateClosed || rows[0].LastSyncedSHA != mergeSHA {
		t.Fatalf("projection not closed: %#v", rows)
	}
}

func TestForgejoClosedUnmergedWebhookClosesAGSPR(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, WebhookSecret: "secret"}, &projectionCloseForgejoClient{}, nil)
	configureMulticaTerminalDelivery(svc)
	pr := createProjectedPRForLifecycleTest(t, svc, ctx)
	addAuthoritativeMulticaLink(t, svc, pr)

	body := []byte(`{"action":"closed","repository":{"full_name":"forgejo/repo","default_branch":"main"},"pull_request":{"number":42,"merged":false,"html_url":"http://forgejo.local/forgejo/repo/pulls/42","head":{"ref":"agent/projection"},"base":{"ref":"main"}}}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body)
	if err != nil {
		t.Fatalf("HandleForgejoWebhook: %v", err)
	}
	if !result.Handled || result.AGSPRNumber != pr.Number || result.PRNumber != 42 {
		t.Fatalf("unexpected result=%#v", result)
	}

	closed, err := svc.GetPR(ctx, "proj-user/repo", pr.Number)
	if err != nil {
		t.Fatalf("load closed PR: %v", err)
	}
	if closed.Merged || closed.State != db.StateClosed || closed.ClosedAt == nil || closed.MergedAt != nil || closed.MergeCommitSHA != "" {
		t.Fatalf("closed PR not updated as unmerged close: %#v", closed)
	}

	var rows []db.PullRequestProjection
	if err := svc.DB.Where("pull_request_id = ?", pr.ID).Find(&rows).Error; err != nil {
		t.Fatalf("load projections: %v", err)
	}
	if len(rows) != 1 || rows[0].State != service.ProjectionStateClosed || rows[0].LastSyncedSHA != "" {
		t.Fatalf("projection not closed: %#v", rows)
	}
	var deliveries []db.OutboundDelivery
	if err := svc.DB.Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Find(&deliveries).Error; err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || !strings.Contains(string(deliveries[0].PayloadJSON), `"state":"closed"`) || !strings.Contains(string(deliveries[0].PayloadJSON), `"provider":"ags"`) {
		t.Fatalf("Forgejo closed webhook did not durably enqueue canonical closed fact: %#v", deliveries)
	}
	if err := svc.DB.Delete(&deliveries[0]).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkPullRequestClosedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, "forgejo"); err != nil {
		t.Fatalf("repeated close fact should repair a missing delivery row: %v", err)
	}
	var repaired int64
	if err := svc.DB.Model(&db.OutboundDelivery{}).Where("event_type = ?", service.OutboundEventMulticaExternalPRTerminal).Count(&repaired).Error; err != nil {
		t.Fatal(err)
	}
	if repaired != 1 {
		t.Fatalf("repeated close fact did not repair exactly one delivery row: %d", repaired)
	}
}

func createProjectedPRForLifecycleTest(t *testing.T, svc *service.Service, ctx context.Context) db.PullRequest {
	t.Helper()
	if err := svc.DB.Create(&db.User{Login: "proj-user", Name: "proj-user", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "proj-user", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "proj-user/repo", "agent/projection", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "proj-user/repo", Title: "Projection", HeadRef: "agent/projection", BaseRef: "main", AuthorLogin: "proj-user"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID:  pr.ID,
		RepositoryID:   pr.RepositoryID,
		Provider:       service.ProjectionProviderForgejo,
		ExternalRepo:   "forgejo/repo",
		ExternalNumber: 42,
		ExternalURL:    "http://forgejo.local/forgejo/repo/pulls/42",
		SourceBranch:   "agent/projection",
		TargetBranch:   "main",
		State:          service.ProjectionStateOpen,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}
	return pr
}

func TestForgejoDeleteBranchWebhookDeletesMergedAGSSourceBranch(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, WebhookSecret: "secret"}, &projectionCloseForgejoClient{}, nil)

	if err := svc.DB.Create(&db.User{Login: "cleanup", Name: "cleanup", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "cleanup", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/lane-d/delete-cleanup"
	if err := svc.Git.CreateBranch(ctx, "cleanup/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "cleanup/repo", Title: "cleanup", HeadRef: branch, BaseRef: "main", AuthorLogin: "cleanup"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID:  pr.ID,
		RepositoryID:   pr.RepositoryID,
		Provider:       service.ProjectionProviderForgejo,
		ExternalRepo:   "forgejo/repo",
		ExternalNumber: 42,
		SourceBranch:   branch,
		TargetBranch:   "main",
		State:          service.ProjectionStateOpen,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}
	mergeSHA := strings.Repeat("b", 40)
	if _, err := svc.MarkPullRequestMergedFromProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 42, mergeSHA, "forgejo"); err != nil {
		t.Fatalf("mark merged: %v", err)
	}
	if err := svc.RecordForgejoProjectionFailure(ctx, "cleanup/repo", nil, &forgejointegration.ProjectionError{
		Type:         forgejointegration.ProjectionFailureSHADrift,
		Repo:         "cleanup/repo",
		Ref:          "refs/heads/" + branch,
		Branch:       branch,
		ExpectedSHA:  mergeSHA,
		ErrorSummary: "Forgejo ref missing for AGS authoritative ref",
	}); err != nil {
		t.Fatalf("record active projection drift: %v", err)
	}

	body := []byte(`{"ref":"agent/lane-d/delete-cleanup","ref_type":"branch","repository":{"full_name":"forgejo/repo"},"pusher_type":"user"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body)
	if err != nil {
		t.Fatalf("HandleForgejoWebhook: %v", err)
	}
	if !result.Handled || !result.BranchDeleted || result.DeletedBranch != branch || result.AGSPRNumber != pr.Number {
		t.Fatalf("unexpected result=%#v", result)
	}
	if _, err := svc.Git.HeadSHA(ctx, "cleanup/repo", branch); err == nil {
		t.Fatalf("expected source branch %q to be deleted", branch)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, "cleanup/repo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoPath, "show-ref", "--verify", "refs/pull/"+strconv.Itoa(pr.Number)+"/head").CombinedOutput(); err != nil {
		t.Fatalf("expected PR head ref to remain: %v %s", err, out)
	}
	status, err := svc.GetProjectionStatus(ctx, "cleanup/repo")
	if err != nil {
		t.Fatalf("projection status: %v", err)
	}
	if len(status.Refs) != 1 || status.Refs[0].Status != service.ProjectionStatusResolved || status.Refs[0].AGSSHA != "" || status.Refs[0].ForgejoSHA != "" {
		t.Fatalf("branch delete should resolve active missing-ref drift: %#v", status.Refs)
	}
}

func TestForgejoMergeWebhookCleansSourceBranchWhenDeleteWebhookArrivesFirst(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "cleanup-race", Name: "cleanup-race", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "cleanup-race", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/delete-before-merge"
	if err := svc.Git.CreateBranch(ctx, "cleanup-race/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "cleanup-race/repo", Title: "cleanup race", HeadRef: branch, BaseRef: "main", AuthorLogin: "cleanup-race"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
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
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}

	forgejoRoot := t.TempDir()
	forgejoBare := filepath.Join(forgejoRoot, "forgejo", "repo.git")
	if out, err := exec.Command("git", "init", "--bare", forgejoBare).CombinedOutput(); err != nil {
		t.Fatalf("init forgejo bare: %v %s", err, out)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, "cleanup-race/repo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, "refs/heads/main:refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("seed forgejo main: %v %s", err, out)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:       true,
		BaseURL:       "file://" + forgejoRoot,
		Token:         "token",
		WebhookSecret: "secret",
		RepoMap: map[string]forgejointegration.RepoMapping{
			"cleanup-race/repo": {Owner: "forgejo", Repo: "repo", BaseBranch: "main"},
		},
	}, &projectionCloseForgejoClient{}, nil)

	deleteBody := []byte(`{"ref":"agent/delete-before-merge","ref_type":"branch","repository":{"full_name":"forgejo/repo"},"pusher_type":"user"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(deleteBody)
	deleteResult, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, deleteBody)
	if err != nil {
		t.Fatalf("delete HandleForgejoWebhook: %v", err)
	}
	if !deleteResult.Handled || deleteResult.AGSSourceBranchDeleted {
		t.Fatalf("delete-before-merge should be accepted but not delete AGS branch yet: %#v", deleteResult)
	}
	if _, err := svc.Git.HeadSHA(ctx, "cleanup-race/repo", branch); err != nil {
		t.Fatalf("source branch should still exist before merge projection: %v", err)
	}

	mergeBody := []byte(`{"action":"closed","repository":{"full_name":"forgejo/repo","default_branch":"main"},"pull_request":{"number":42,"merged":true,"html_url":"http://forgejo.local/forgejo/repo/pulls/42","head":{"ref":"agent/delete-before-merge"},"base":{"ref":"main"}}}`)
	mac = hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(mergeBody)
	mergeResult, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, mergeBody)
	if err != nil {
		t.Fatalf("merge HandleForgejoWebhook: %v", err)
	}
	if !mergeResult.Handled || mergeResult.AGSPRNumber != pr.Number || !mergeResult.AGSSourceBranchDeleted {
		t.Fatalf("merge webhook should record merge and clean deleted source branch: %#v", mergeResult)
	}
	if _, err := svc.Git.HeadSHA(ctx, "cleanup-race/repo", branch); err == nil {
		t.Fatalf("expected source branch %q to be deleted after merge projection", branch)
	}
	merged, err := svc.GetPR(ctx, "cleanup-race/repo", pr.Number)
	if err != nil {
		t.Fatalf("get merged PR: %v", err)
	}
	if !merged.Merged || merged.State != db.StateClosed {
		t.Fatalf("PR was not marked merged: %#v", merged)
	}
}

func TestForgejoMergeWebhookAcceptsGitLabBackupPushFailure(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "merge-proj", Name: "merge-proj", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "merge-proj", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/gitlab-backup-fails"
	if err := svc.Git.CreateBranch(ctx, "merge-proj/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "merge-proj/repo", Title: "merge", HeadRef: branch, BaseRef: "main", AuthorLogin: "merge-proj"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID:  pr.ID,
		RepositoryID:   pr.RepositoryID,
		Provider:       service.ProjectionProviderForgejo,
		ExternalRepo:   "forgejo/repo",
		ExternalNumber: 42,
		SourceBranch:   branch,
		TargetBranch:   "main",
		State:          service.ProjectionStateOpen,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}

	forgejoRoot := t.TempDir()
	forgejoBare := filepath.Join(forgejoRoot, "forgejo", "repo.git")
	if out, err := exec.Command("git", "init", "--bare", forgejoBare).CombinedOutput(); err != nil {
		t.Fatalf("init forgejo bare: %v %s", err, out)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, "merge-proj/repo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, "refs/heads/main:refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("seed forgejo main: %v %s", err, out)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:       true,
		BaseURL:       "file://" + forgejoRoot,
		Token:         "token",
		WebhookSecret: "secret",
		RepoMap: map[string]forgejointegration.RepoMapping{
			"merge-proj/repo": {Owner: "forgejo", Repo: "repo", BaseBranch: "main"},
		},
	}, &projectionCloseForgejoClient{}, nil)
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled:        true,
		BaseURL:        "http://gitlab.local",
		Token:          "token",
		MergeAuthority: "forgejo",
		Repos: map[string]gitlabintegration.RepoMapping{
			"merge-proj/repo": {ProjectPath: "merge-proj/repo", TargetBranch: "uat"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		return errors.New("non-fast-forward")
	}, &projectionCloseGitLabClient{})

	body := []byte(`{"action":"closed","repository":{"full_name":"forgejo/repo","default_branch":"main"},"pull_request":{"number":42,"merged":true,"html_url":"http://forgejo.local/forgejo/repo/pulls/42","head":{"ref":"agent/gitlab-backup-fails"},"base":{"ref":"main"}}}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body)
	if err != nil {
		t.Fatalf("HandleForgejoWebhook should accept merge even when GitLab backup push fails: %v", err)
	}
	if !result.Handled || result.GitLabBackupHandled {
		t.Fatalf("unexpected result: %#v", result)
	}
	merged, err := svc.GetPR(ctx, "merge-proj/repo", pr.Number)
	if err != nil {
		t.Fatalf("load merged pr: %v", err)
	}
	if !merged.Merged || merged.State != db.StateClosed {
		t.Fatalf("Forgejo merge was not persisted despite GitLab backup failure: %#v", merged)
	}
}

func TestForgejoMergeWebhookRejectsNonFastForwardBaseRewrite(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "nonff-proj", Name: "nonff-proj", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "nonff-proj", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "nonff-proj/repo", "agent/change", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "nonff-proj/repo", Title: "change", HeadRef: "agent/change", BaseRef: "main", AuthorLogin: "nonff-proj"})
	if err != nil {
		t.Fatalf("create PR: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "forgejo/repo", ExternalNumber: 42, SourceBranch: "agent/change", TargetBranch: "main", State: service.ProjectionStateOpen,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, "nonff-proj/repo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	originalMain, err := svc.Git.HeadSHA(ctx, "nonff-proj/repo", "main")
	if err != nil {
		t.Fatalf("main SHA: %v", err)
	}
	forgejoRoot := t.TempDir()
	forgejoBare := filepath.Join(forgejoRoot, "forgejo", "repo.git")
	if out, err := exec.Command("git", "init", "--bare", forgejoBare).CombinedOutput(); err != nil {
		t.Fatalf("init forgejo bare: %v %s", err, out)
	}
	work := t.TempDir()
	for _, cmd := range [][]string{
		{"clone", repoPath, work},
		{"-C", work, "config", "user.name", "rewrite"},
		{"-C", work, "config", "user.email", "rewrite@example.invalid"},
		{"-C", work, "checkout", "--orphan", "rewritten"},
		{"-C", work, "rm", "-rf", "."},
	} {
		if out, err := exec.Command("git", cmd...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", cmd, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "rewritten.txt"), []byte("rewritten\n"), 0o644); err != nil {
		t.Fatalf("write rewritten tree: %v", err)
	}
	for _, cmd := range [][]string{
		{"-C", work, "add", "rewritten.txt"},
		{"-C", work, "commit", "-m", "rewrite main"},
		{"-C", work, "push", "file://" + forgejoBare, "HEAD:refs/heads/main"},
	} {
		if out, err := exec.Command("git", cmd...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", cmd, err, out)
		}
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret",
		RepoMap: map[string]forgejointegration.RepoMapping{"nonff-proj/repo": {Owner: "forgejo", Repo: "repo", BaseBranch: "main"}},
	}, &projectionCloseForgejoClient{}, nil)
	body := []byte(`{"action":"closed","repository":{"full_name":"forgejo/repo","default_branch":"main"},"pull_request":{"number":42,"merged":true,"head":{"ref":"agent/change"},"base":{"ref":"main"}}}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	_, err = svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body)
	if err == nil || !strings.Contains(err.Error(), "non-fast-forward") {
		t.Fatalf("expected non-fast-forward rejection, got %v", err)
	}
	currentMain, err := svc.Git.HeadSHA(ctx, "nonff-proj/repo", "main")
	if err != nil {
		t.Fatalf("main SHA after webhook: %v", err)
	}
	if currentMain != originalMain {
		t.Fatalf("AGS main changed from %s to %s", originalMain, currentMain)
	}
	updated, err := svc.GetPR(ctx, "nonff-proj/repo", pr.Number)
	if err != nil {
		t.Fatalf("get PR: %v", err)
	}
	if updated.Merged || updated.State != db.StateOpen {
		t.Fatalf("PR should remain open after rejected rewrite: %#v", updated)
	}
}

func TestUpdatePREditRefreshesForgejoProjectionWithDelegatedActor(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	forgejoClient := &projectionSyncForgejoClient{}
	var pushes []forgejointegration.PushRequest
	pusher := func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushes = append(pushes, req)
		return nil
	}
	if err := svc.DB.Create(&db.User{Login: "edit-proj", Name: "edit-proj", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "edit-proj", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/edit"
	if err := svc.Git.CreateBranch(ctx, "edit-proj/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "edit-proj/repo", Title: "before", Body: "old body", HeadRef: branch, BaseRef: "main", AuthorLogin: "edit-proj"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	now := time.Now().UTC()
	humanID := pr.AuthorID
	session := db.DelegatedAgentSession{
		ID: "projection-session", CredentialHash: strings.Repeat("c", 64), CredentialPrefix: "projection-prefix",
		PrincipalUserID: pr.AuthorID, PrincipalLogin: "edit-proj", DelegatedByUserID: &humanID, DelegatedByLogin: "edit-proj", DelegatedBySource: "session_snapshot",
		Issuer: "multica", AssertionVersion: 1, AssertionPurpose: "ags_session_exchange", AssertionJTI: "projection-session-jti",
		AssertionAudience: "urn:ags:workload-session-exchange:v1", IssuerWorkspaceID: "workspace-projection", IssuerWorkspace: "example-workspace",
		ExternalAgentID: "projection-agent-id", ExternalAgentName: "projection-agent", ExternalTaskID: "projection-task", ExternalRunID: "projection-run",
		ExternalIssueID: "projection-issue-id", ExternalIssueKey: "MINI-541", TargetInstance: "primary-a", RepositoryID: pr.RepositoryID,
		GrantedCapabilities: []string{"pr:create", "repo:read"}, PolicyVersion: "2026-07-14.1", PolicySnapshotHash: strings.Repeat("d", 64),
		CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	}
	if err := svc.DB.Create(&session).Error; err != nil {
		t.Fatalf("create delegated session: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("agent_session_id", session.ID).Error; err != nil {
		t.Fatalf("link delegated session: %v", err)
	}
	pr, err = svc.GetPR(ctx, "edit-proj/repo", pr.Number)
	if err != nil {
		t.Fatalf("reload delegated pr: %v", err)
	}
	forgejoClient.headSHA = pr.HeadSHA
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"edit-proj/repo": {Owner: "forgejo", Repo: "repo"},
		},
	}, forgejoClient, pusher)
	if _, err := svc.DispatchPullRequestIntegrations(ctx, pr); err != nil {
		t.Fatalf("initial dispatch: %v", err)
	}
	title := "after"
	body := "new body"
	if _, err := svc.UpdatePR(ctx, "edit-proj/repo", pr.Number, service.UpdatePRInput{Title: &title, Body: &body}); err != nil {
		t.Fatalf("update pr: %v", err)
	}
	if len(forgejoClient.requests) != 2 {
		t.Fatalf("requests=%#v", forgejoClient.requests)
	}
	last := forgejoClient.requests[1]
	if last.Title != "after" || !strings.Contains(last.Body, "new body") || last.Head != branch || last.Base != "main" {
		t.Fatalf("last request=%#v", last)
	}
	for _, expected := range []string{"Delegated workload:", "projection-agent [Multica Agent]", "projection-run", "MINI-541", "projection-session"} {
		if !strings.Contains(last.Body, expected) {
			t.Fatalf("reconciled projection missing %q: %s", expected, last.Body)
		}
	}
	for _, forbidden := range []string{"projection-session-jti", strings.Repeat("c", 64), strings.Repeat("d", 64)} {
		if strings.Contains(last.Body, forbidden) {
			t.Fatalf("reconciled projection leaked %q: %s", forbidden, last.Body)
		}
	}
	if len(pushes) != 2 || pushes[1].Refspec != "refs/heads/agent/edit:refs/heads/agent/edit" {
		t.Fatalf("pushes=%#v", pushes)
	}
}

func TestForgejoActionRebaseLabelRebasesAGSPRAndRefreshesProjection(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "label-user", Name: "label-user", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "label-user", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/rebase-label"
	if err := svc.Git.CreateBranch(ctx, "label-user/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, "label-user/repo", branch, "feature.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "label-user/repo", Title: "Projection label", HeadRef: branch, BaseRef: "main", AuthorLogin: "label-user"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	mainSHA, err := svc.Git.WriteFile(ctx, "label-user/repo", "main", "base.txt", "base", []byte("base\n"))
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
	now := time.Now().UTC()
	if err := svc.DB.Create(&db.ProjectionRefState{
		Provider: service.ProjectionProviderForgejo, RepositoryID: pr.RepositoryID,
		RepoFullName: "label-user/repo", TargetRepo: "forgejo/repo", Ref: "refs/heads/" + branch, Branch: branch,
		Type: forgejointegration.ProjectionFailureSHADrift, Status: service.ProjectionStatusActive, Authority: "ags",
		AGSSHA: pr.HeadSHA, ExternalSHA: pr.HeadSHA, FirstSeenAt: now, LastSeenAt: now,
	}).Error; err != nil {
		t.Fatalf("seed active projection drift: %v", err)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, "label-user/repo")
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
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, pr.HeadSHA+":refs/heads/"+branch, mainSHA+":refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("seed Forgejo refs: %v: %s", err, out)
	}
	forgejoClient := &projectionWorkflowForgejoClient{
		headSHA: pr.HeadSHA, headRef: branch, baseRef: "main", prNumber: 42,
		permissions: map[string]string{"operator": "owner"},
		labels:      []string{forgejointegration.AGSStatusRebaseConflictLabel, forgejointegration.AGSActionRebaseLabel},
	}
	var leasePush forgejointegration.PushRequest
	pushAttempts := 0
	pusher := func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushAttempts++
		leasePush = req
		if err := forgejointegration.GitPush(ctx, req); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "git", "-C", req.RepoPath, "ls-remote", req.RemoteURL, "refs/heads/"+req.BranchName).Output()
		if err != nil {
			return err
		}
		forgejoClient.headSHA = strings.Fields(string(out))[0]
		return nil
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		ActionPrincipalBindings: map[string]uint{"operator": pr.AuthorID},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"label-user/repo": {Owner: "forgejo", Repo: "repo", BaseBranch: "main"},
		},
	}, forgejoClient, pusher)

	var actionViewer db.User
	if err := svc.DB.First(&actionViewer, "login = ?", "label-user").Error; err != nil {
		t.Fatalf("load action viewer: %v", err)
	}
	authority, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "test-issuer", Issuer: "test", KeyIDs: []string{"test-key"}, Status: "active", TrustRevision: "trust-v1"}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: "test.owner.v1", Status: "active", PolicyRevision: "class-v1", Operations: []string{"pr.rebase"}}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "test-owner", IssuerInstanceID: "test-issuer", WorkspaceID: "test-workspace", TeamIdentityID: "test-team", PolicyClass: "test.owner.v1", PrincipalID: actionViewer.ID, Status: "active", BindingRevision: "binding-v1", EpochFloor: 1}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo", Target: "primary-a", Service: "ags", Repository: "label-user/repo", Status: "active", MaxSessionTTL: "30m", PolicyRevision: "repo-policy-v1"}},
	})
	if err != nil {
		t.Fatalf("create native action authority: %v", err)
	}
	svc.PrincipalSessions = authority
	ctx = service.ContextWithUser(ctx, actionViewer)
	if authorization, err := svc.AuthorizeDurableOperation(ctx, actionViewer, "ags", "label-user/repo", "pr.rebase", map[string]any{"pull_request_number": json.Number(fmt.Sprint(pr.Number)), "forgejo_pull_request_number": json.Number("42"), "expected_head_sha": pr.HeadSHA, "expected_base_sha": mainSHA}); err != nil || !authorization.Authorized {
		t.Fatalf("authorize label-first rebase action: authorization=%#v err=%v", authorization, err)
	}

	body := []byte(fmt.Sprintf(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/repo", "default_branch": "main"},
	  "pull_request": {
	    "number": 42,
	    "html_url": "http://forgejo.local/forgejo/repo/pulls/42",
	    "head": {"ref": "agent/rebase-label", "sha": %q},
	    "base": {"ref": "main"},
	    "labels": [{"name": "ags/action-rebase"}]
	  },
	  "label": {"name": "ags/action-rebase"},
	  "sender": {"login": "operator"}
	}`, pr.HeadSHA))
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{
		"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))},
		"X-Forgejo-Delivery":  {"label-first-live-shape-1"},
	}, body)
	if err != nil {
		t.Fatalf("HandleForgejoWebhook: %v", err)
	}
	if !result.Handled || result.WorkflowAction != "rebase" || result.WorkflowStatus != "rebased" || result.AGSPRNumber != pr.Number || result.SyncedSHA == "" || result.SyncedSHA == pr.HeadSHA {
		t.Fatalf("unexpected result=%#v oldHead=%s", result, pr.HeadSHA)
	}
	updated, err := svc.GetPR(ctx, "label-user/repo", pr.Number)
	if err != nil {
		t.Fatalf("get updated PR: %v", err)
	}
	if updated.HeadSHA != result.SyncedSHA {
		t.Fatalf("updated head=%s result=%s", updated.HeadSHA, result.SyncedSHA)
	}
	agsBranchHead, err := svc.Git.HeadSHA(ctx, "label-user/repo", branch)
	if err != nil || agsBranchHead != updated.HeadSHA {
		t.Fatalf("AGS branch/PR did not converge: branch=%s pr=%s err=%v", agsBranchHead, updated.HeadSHA, err)
	}
	parentOut, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", updated.HeadSHA+"^").Output()
	if err != nil {
		t.Fatalf("read rebased parent: %v", err)
	}
	parent := strings.TrimSpace(string(parentOut))
	if parent != mainSHA {
		t.Fatalf("rebased parent=%s want main=%s", parent, mainSHA)
	}
	var projection db.PullRequestProjection
	if err := svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&projection).Error; err != nil {
		t.Fatalf("load projection: %v", err)
	}
	if projection.LastSyncedSHA != updated.HeadSHA {
		t.Fatalf("projection head=%s want %s", projection.LastSyncedSHA, updated.HeadSHA)
	}
	if leasePush.ForceWithLeaseRef != "refs/heads/"+branch || leasePush.ForceWithLeaseSHA != pr.HeadSHA || strings.HasPrefix(leasePush.Refspec, "+") {
		t.Fatalf("rebase projection did not use exact old-head lease: %#v", leasePush)
	}
	remoteHead, err := forgejointegration.GitRemoteRefSHA(ctx, repoPath, "file://"+forgejoBare, "refs/heads/"+branch)
	if err != nil || remoteHead != updated.HeadSHA || forgejoClient.headSHA != updated.HeadSHA {
		t.Fatalf("Forgejo branch/PR did not converge: remote=%s api=%s want=%s err=%v", remoteHead, forgejoClient.headSHA, updated.HeadSHA, err)
	}
	var resolvedState db.ProjectionRefState
	if err := svc.DB.Where("provider = ? AND repository_id = ? AND ref = ?", service.ProjectionProviderForgejo, pr.RepositoryID, "refs/heads/"+branch).First(&resolvedState).Error; err != nil {
		t.Fatalf("load resolved drift state: %v", err)
	}
	if resolvedState.Status != service.ProjectionStatusResolved || resolvedState.ResolvedAt == nil || resolvedState.AGSSHA != updated.HeadSHA || resolvedState.ExternalSHA != updated.HeadSHA {
		t.Fatalf("exact convergence did not resolve drift: %#v", resolvedState)
	}
	if !slices.Contains(forgejoClient.removed, forgejointegration.AGSActionRebaseLabel) || !slices.Contains(forgejoClient.added, forgejointegration.AGSStatusRebasingLabel) || slices.Contains(forgejoClient.added, forgejointegration.AGSActionRebaseLabel) {
		t.Fatalf("label-first operations removed=%v added=%v", forgejoClient.removed, forgejoClient.added)
	}
	var labelIntent db.PullRequestActionIntent
	if err := svc.DB.Where("pull_request_id = ? AND request_source = ?", pr.ID, "forgejo_label").First(&labelIntent).Error; err != nil {
		t.Fatalf("load label-first intent: %v", err)
	}
	if labelIntent.RequestActor != "operator" || labelIntent.PrincipalID != actionViewer.ID || labelIntent.State != service.ForgejoActionIntentCompleted || !strings.HasPrefix(labelIntent.RequestBindingRev, "sha256:") {
		t.Fatalf("label-first intent=%#v", labelIntent)
	}
	operationsBeforeReplay := len(forgejoClient.operations)
	replay, err := svc.HandleForgejoWebhook(ctx, http.Header{
		"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))},
		"X-Forgejo-Delivery":  {"label-first-live-shape-1"},
	}, body)
	if err != nil || replay.WorkflowStatus != "rebased" || replay.SyncedSHA != updated.HeadSHA {
		t.Fatalf("duplicate delivery replay=%#v err=%v", replay, err)
	}
	if len(forgejoClient.operations) != operationsBeforeReplay {
		t.Fatalf("duplicate delivery repeated provider effects: before=%d after=%d operations=%v", operationsBeforeReplay, len(forgejoClient.operations), forgejoClient.operations)
	}
	if len(forgejoClient.comments) != 1 {
		t.Fatalf("success comment count=%d comments=%v", len(forgejoClient.comments), forgejoClient.comments)
	}
	for _, want := range []string{"AGS PR #" + strconv.Itoa(pr.Number), "Forgejo PR #42", updated.HeadSHA} {
		if !strings.Contains(forgejoClient.comments[0], want) {
			t.Fatalf("success comment missing %q: %s", want, forgejoClient.comments[0])
		}
	}
	removeConflict := slices.Index(forgejoClient.operations, "remove:"+forgejointegration.AGSStatusRebaseConflictLabel)
	addRebasing := slices.Index(forgejoClient.operations, "add:"+forgejointegration.AGSStatusRebasingLabel)
	if removeConflict == -1 || addRebasing == -1 || removeConflict > addRebasing {
		t.Fatalf("old status labels must be cleared before rebasing is added; operations=%v", forgejoClient.operations)
	}
	var actionJob db.PullRequestProjectionJob
	if err := svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&actionJob).Error; err != nil {
		t.Fatalf("load durable rebase action job: %v", err)
	}
	if actionJob.Phase != service.ForgejoProjectionPhaseProjected || actionJob.HeadSHA != updated.HeadSHA || actionJob.RemoteSHA != updated.HeadSHA {
		t.Fatalf("durable rebase action job=%#v", actionJob)
	}
	// Simulate a restart after the AGS ref was rebased but before desired-head
	// persistence. Recovery must discover the already-written head rather than
	// create a second rebase commit.
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).Update("last_synced_sha", pr.HeadSHA).Error; err != nil {
		t.Fatalf("rewind projection row for rebasing restart: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", actionJob.ID).Updates(map[string]any{"phase": service.ForgejoProjectionPhaseRebasing, "desired_ags_head_sha": "", "finished_at": nil}).Error; err != nil {
		t.Fatalf("set rebasing restart phase: %v", err)
	}
	if err := svc.ResumePendingForgejoProjectionJobs(ctx); err != nil {
		t.Fatalf("resume rebasing job: %v", err)
	}
	svc.Wg.Wait()
	var rebasingRecovered db.PullRequestProjectionJob
	if err := svc.DB.First(&rebasingRecovered, actionJob.ID).Error; err != nil {
		t.Fatalf("reload rebasing-recovered job: %v", err)
	}
	if rebasingRecovered.DesiredAGSHeadSHA != updated.HeadSHA || rebasingRecovered.Phase != service.ForgejoProjectionPhaseProjected {
		t.Fatalf("rebasing restart generated or lost desired head: %#v", rebasingRecovered)
	}

	// Simulate another restart after provider PR verification but before
	// projection-row recording. This path is also idempotent.
	if err := svc.DB.Model(&db.PullRequestProjection{}).Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).Update("last_synced_sha", pr.HeadSHA).Error; err != nil {
		t.Fatalf("rewind projection row for verifying-pr restart: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", actionJob.ID).Updates(map[string]any{"phase": service.ForgejoProjectionPhaseVerifyingPR, "finished_at": nil}).Error; err != nil {
		t.Fatalf("set verifying-pr restart phase: %v", err)
	}
	if err := svc.ResumePendingForgejoProjectionJobs(ctx); err != nil {
		t.Fatalf("resume verifying-pr job: %v", err)
	}
	svc.Wg.Wait()
	if err := svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&projection).Error; err != nil {
		t.Fatalf("reload resumed projection: %v", err)
	}
	if projection.LastSyncedSHA != updated.HeadSHA || len(forgejoClient.comments) != 1 {
		t.Fatalf("verifying-pr restart projection=%#v comments=%v", projection, forgejoClient.comments)
	}

	// A later generic work-branch projection can converge the live AGS and
	// Forgejo heads while leaving the durable projection row stale. If main has
	// also advanced, ags/action-rebase must still rebase the converged head onto
	// current main instead of treating head equality as completed rebase work.
	followupHead, err := svc.Git.WriteFile(ctx, "label-user/repo", branch, "followup.txt", "followup", []byte("followup\n"))
	if err != nil {
		t.Fatalf("write follow-up commit: %v", err)
	}
	if err := svc.UpdatePRFields(ctx, pr.ID, map[string]any{"head_sha": followupHead}); err != nil {
		t.Fatalf("update follow-up PR head: %v", err)
	}
	followupBase, err := svc.Git.WriteFile(ctx, "label-user/repo", "main", "followup-base.txt", "followup base", []byte("followup base\n"))
	if err != nil {
		t.Fatalf("advance follow-up base: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, followupHead+":refs/heads/"+branch, followupBase+":refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("project follow-up refs: %v: %s", err, out)
	}
	forgejoClient.headSHA = followupHead
	if err := svc.DB.Model(&db.PullRequestProjection{}).
		Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).
		Update("last_synced_sha", updated.HeadSHA).Error; err != nil {
		t.Fatalf("leave follow-up projection row stale: %v", err)
	}
	forgejoClient.labels = []string{forgejointegration.AGSActionRebaseLabel}
	seedDispatchedIntent(t, svc, pr.ID, pr.RepositoryID, pr.Number, 42, "forgejo/repo", branch, "main", followupHead, []string{forgejointegration.AGSActionRebaseLabel})
	followupBody := []byte(fmt.Sprintf(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/repo", "default_branch": "main"},
	  "pull_request": {
	    "number": 42,
	    "html_url": "http://forgejo.local/forgejo/repo/pulls/42",
	    "head": {"ref": "agent/rebase-label", "sha": %q},
	    "base": {"ref": "main"},
	    "labels": [{"name": "ags/action-rebase"}]
	  },
	  "label": {"name": "ags/action-rebase"},
	  "sender": {"login": "operator"}
	}`, followupHead))
	followupMAC := hmac.New(sha256.New, []byte("secret"))
	_, _ = followupMAC.Write(followupBody)
	followupResult, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(followupMAC.Sum(nil))}}, followupBody)
	if err != nil || followupResult.WorkflowStatus != "denied" {
		t.Fatalf("stale projection exact intent was not denied: before=%s result=%#v err=%v", followupHead, followupResult, err)
	}
	if pushAttempts != 1 {
		t.Fatalf("stale projection reached another provider push: attempts=%d", pushAttempts)
	}
}

func TestForgejoActionRebaseLabelRejectsInterruptedGenerationAsNewIntent(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	const repoFullName = "resume-rebase/repo"
	if err := svc.DB.Create(&db.User{Login: "resume-rebase", Name: "resume-rebase", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "resume-rebase", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/resume-rebase"
	if err := svc.Git.CreateBranch(ctx, repoFullName, branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, repoFullName, branch, "feature.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: repoFullName, Title: "resume rebase", HeadRef: branch, BaseRef: "main", AuthorLogin: "resume-rebase"})
	if err != nil {
		t.Fatalf("create PR: %v", err)
	}
	oldHead := pr.HeadSHA
	baseSHA, err := svc.Git.WriteFile(ctx, repoFullName, "main", "base.txt", "advance base", []byte("base\n"))
	if err != nil {
		t.Fatalf("advance base: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "forgejo/repo", ExternalNumber: 42, ExternalURL: "http://forgejo.local/forgejo/repo/pulls/42",
		SourceBranch: branch, TargetBranch: "main", State: service.ProjectionStateOpen, LastSyncedSHA: oldHead,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	forgejoRoot := t.TempDir()
	forgejoBare := filepath.Join(forgejoRoot, "forgejo", "repo.git")
	if err := os.MkdirAll(filepath.Dir(forgejoBare), 0o755); err != nil {
		t.Fatalf("create Forgejo parent: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", forgejoBare).CombinedOutput(); err != nil {
		t.Fatalf("init Forgejo repo: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, oldHead+":refs/heads/"+branch, baseSHA+":refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("seed Forgejo refs: %v: %s", err, out)
	}
	client := &projectionWorkflowForgejoClient{
		headSHA: oldHead, headRef: branch, baseRef: "main", prNumber: 42,
		permissions: map[string]string{"operator": "owner"}, labels: []string{forgejointegration.AGSActionRebaseLabel},
	}
	pushAttempts := 0
	pusher := func(ctx context.Context, req forgejointegration.PushRequest) error {
		pushAttempts++
		if pushAttempts == 1 {
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
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{repoFullName: {Owner: "forgejo", Repo: "repo", BaseBranch: "main"}},
	}, client, pusher)
	body := []byte(fmt.Sprintf(`{"action":"label_updated","repository":{"full_name":"forgejo/repo","default_branch":"main"},"pull_request":{"number":42,"html_url":"http://forgejo.local/forgejo/repo/pulls/42","head":{"ref":%q,"sha":%q},"base":{"ref":"main"},"labels":[{"name":"ags/action-rebase"}]},"label":{"name":"ags/action-rebase"},"sender":{"login":"operator"}}`, branch, oldHead))
	seedDispatchedIntent(t, svc, pr.ID, pr.RepositoryID, pr.Number, 42, "forgejo/repo", branch, "main", oldHead, []string{forgejointegration.AGSActionRebaseLabel})
	signedHeaders := func() http.Header {
		mac := hmac.New(sha256.New, []byte("secret"))
		_, _ = mac.Write(body)
		return http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}
	}
	first, err := svc.HandleForgejoWebhook(ctx, signedHeaders(), body)
	if err != nil || first.WorkflowStatus != "projection_failed" {
		t.Fatalf("first action result=%#v err=%v", first, err)
	}
	desired := first.SyncedSHA
	if desired == "" || desired == oldHead {
		t.Fatalf("first action did not persist rebased desired head: %#v", first)
	}
	var failedJob db.PullRequestProjectionJob
	if err := svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&failedJob).Error; err != nil {
		t.Fatalf("load failed action job: %v", err)
	}
	if failedJob.DesiredAGSHeadSHA != desired || failedJob.ExpectedForgejoOldHeadSHA != oldHead || failedJob.Phase != service.ForgejoProjectionPhaseFailedRetryable {
		t.Fatalf("failed action job=%#v", failedJob)
	}
	// Operator inspection does not let a new intent borrow an interrupted
	// generation's newer AGS head. Only the original bound worker may recover it.
	now := time.Now().UTC()
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", failedJob.ID).Updates(map[string]any{
		"phase": service.ForgejoProjectionPhaseFailedTerminal, "next_run_at": nil, "finished_at": &now,
	}).Error; err != nil {
		t.Fatalf("mark operator-inspected terminal generation: %v", err)
	}
	client.labels = []string{forgejointegration.AGSActionRebaseLabel}
	seedDispatchedIntent(t, svc, pr.ID, pr.RepositoryID, pr.Number, 42, "forgejo/repo", branch, "main", oldHead, []string{forgejointegration.AGSActionRebaseLabel})
	second, err := svc.HandleForgejoWebhook(ctx, signedHeaders(), body)
	if err != nil || second.WorkflowStatus != "denied" {
		t.Fatalf("signed stale-head retry was not denied: result=%#v err=%v", second, err)
	}
	if pushAttempts != 1 {
		t.Fatalf("stale-head retry reached provider: attempts=%d", pushAttempts)
	}
}

func TestForgejoActionRebaseConflictLeavesAGSAndForgejoHeadsUnchanged(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	const repoFullName = "conflict-user/repo"
	if err := svc.DB.Create(&db.User{Login: "conflict-user", Name: "conflict-user", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "conflict-user", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, repoFullName, "main", "conflict.txt", "seed", []byte("seed\n")); err != nil {
		t.Fatalf("seed conflict file: %v", err)
	}
	branch := "agent/conflict"
	if err := svc.Git.CreateBranch(ctx, repoFullName, branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, repoFullName, branch, "conflict.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatalf("write feature conflict: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: repoFullName, Title: "conflict", HeadRef: branch, BaseRef: "main", AuthorLogin: "conflict-user"})
	if err != nil {
		t.Fatalf("create PR: %v", err)
	}
	oldHead := pr.HeadSHA
	baseSHA, err := svc.Git.WriteFile(ctx, repoFullName, "main", "conflict.txt", "base", []byte("base\n"))
	if err != nil {
		t.Fatalf("write base conflict: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "forgejo/repo", ExternalNumber: 42, SourceBranch: branch, TargetBranch: "main",
		State: service.ProjectionStateOpen, LastSyncedSHA: oldHead,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, repoFullName)
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
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, oldHead+":refs/heads/"+branch, baseSHA+":refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("seed Forgejo refs: %v: %s", err, out)
	}
	client := &projectionWorkflowForgejoClient{
		headSHA: oldHead, headRef: branch, baseRef: "main", prNumber: 42,
		permissions: map[string]string{"operator": "owner"}, labels: []string{forgejointegration.AGSActionRebaseLabel},
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{repoFullName: {Owner: "forgejo", Repo: "repo", BaseBranch: "main"}},
	}, client, func(ctx context.Context, req forgejointegration.PushRequest) error {
		t.Fatal("rebase conflict must not enter Forgejo projection")
		return nil
	})
	body := []byte(fmt.Sprintf(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/repo", "default_branch": "main"},
	  "pull_request": {"number": 42, "head": {"ref": %q, "sha": %q}, "base": {"ref": "main"}, "labels": [{"name": "ags/action-rebase"}]},
	  "label": {"name": "ags/action-rebase"}, "sender": {"login": "operator"}
	}`, branch, oldHead))
	seedDispatchedIntent(t, svc, pr.ID, pr.RepositoryID, pr.Number, 42, "forgejo/repo", branch, "main", oldHead, []string{forgejointegration.AGSActionRebaseLabel})
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body)
	if err != nil {
		t.Fatalf("HandleForgejoWebhook: %v", err)
	}
	if result.WorkflowStatus != "conflict" {
		var intent db.PullRequestActionIntent
		_ = svc.DB.Where("pull_request_id = ?", pr.ID).Order("created_at DESC").First(&intent).Error
		t.Fatalf("conflict result=%#v comments=%v intent=%#v", result, client.comments, intent)
	}
	agsHead, err := svc.Git.HeadSHA(ctx, repoFullName, branch)
	if err != nil || agsHead != oldHead {
		t.Fatalf("AGS head changed on conflict: got=%s want=%s err=%v", agsHead, oldHead, err)
	}
	freshPR, err := svc.GetPR(ctx, repoFullName, pr.Number)
	if err != nil || freshPR.HeadSHA != oldHead {
		t.Fatalf("AGS PR head changed on conflict: pr=%#v err=%v", freshPR, err)
	}
	remoteHead, err := forgejointegration.GitRemoteRefSHA(ctx, repoPath, "file://"+forgejoBare, "refs/heads/"+branch)
	if err != nil || remoteHead != oldHead {
		t.Fatalf("Forgejo head changed on conflict: got=%s want=%s err=%v", remoteHead, oldHead, err)
	}
	var job db.PullRequestProjectionJob
	if err := svc.DB.Where("pull_request_id = ? AND provider = ?", pr.ID, service.ProjectionProviderForgejo).First(&job).Error; err != nil {
		t.Fatalf("load conflict job: %v", err)
	}
	if job.Phase != service.ForgejoProjectionPhaseFailedTerminal || job.DesiredAGSHeadSHA != "" {
		t.Fatalf("conflict action job=%#v", job)
	}
}

func TestFindPullRequestByForgejoProjectionDoesNotAssumeMatchingNumbers(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	if err := svc.DB.Create(&db.User{Login: "mapping-user", Name: "mapping-user", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "mapping-user", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "mapping-user/repo", "agent/mapped", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "mapping-user/repo", Title: "mapped", HeadRef: "agent/mapped", BaseRef: "main", AuthorLogin: "mapping-user"})
	if err != nil {
		t.Fatalf("create PR: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("number", 127).Error; err != nil {
		t.Fatalf("set AGS PR number: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "forgejo/repo", ExternalNumber: 116, SourceBranch: pr.HeadRef,
		TargetBranch: pr.BaseRef, State: service.ProjectionStateOpen, LastSyncedSHA: pr.HeadSHA,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}
	mapped, err := svc.FindPullRequestByProjection(ctx, service.ProjectionProviderForgejo, "forgejo/repo", 116)
	if err != nil {
		t.Fatalf("resolve mapped PR: %v", err)
	}
	if mapped.Number != 127 || mapped.Number == 116 {
		t.Fatalf("Forgejo #116 resolved to AGS #%d, want #127", mapped.Number)
	}
}

func TestForgejoActionRebaseLabelRejectsConcurrentRemoteChangeWithLease(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	dispatcher := &captureOutboundDispatcher{}
	svc.OutboundDispatcher = dispatcher
	svc.OutboundEventTargets = map[string][]service.OutboundTarget{
		service.OutboundEventProjectionDrift: {{Name: "department_official", Type: service.OutboundTargetTypeFeishuWebhook}},
	}

	const (
		repoFullName    = "example-team/shipping-fixture"
		branch          = "chore/sync-repo-flow-integrity-caca953c"
		agsPRNumber     = 146
		forgejoPRNumber = 132
	)
	// Preserve the production #146 -> #132 mapping while injecting a concurrent
	// Forgejo write after preflight. The exact-old-SHA lease must preserve it.

	if err := svc.DB.Create(&db.User{Login: "example-team", Name: "example-team", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "example-team", Name: "shipping-fixture", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, repoFullName, branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, repoFullName, branch, "feature.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: repoFullName, Title: "Projection incident", HeadRef: branch, BaseRef: "main", AuthorLogin: "example-team"})
	if err != nil {
		t.Fatalf("create PR: %v", err)
	}
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("number", agsPRNumber).Error; err != nil {
		t.Fatalf("set AGS PR number: %v", err)
	}
	pr.Number = agsPRNumber
	oldHead := pr.HeadSHA
	mainSHA, err := svc.Git.WriteFile(ctx, repoFullName, "main", "base.txt", "advance base", []byte("advanced base\n"))
	if err != nil {
		t.Fatalf("advance base: %v", err)
	}

	repoPath, err := svc.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	forgejoRoot := t.TempDir()
	forgejoBare := filepath.Join(forgejoRoot, "example-team", "shipping-fixture.git")
	if err := os.MkdirAll(filepath.Dir(forgejoBare), 0o755); err != nil {
		t.Fatalf("create Forgejo repo parent: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", forgejoBare).CombinedOutput(); err != nil {
		t.Fatalf("init Forgejo bare repo: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, oldHead+":refs/heads/"+branch, mainSHA+":refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("seed Forgejo refs: %v: %s", err, out)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "example-team/shipping-fixture", ExternalNumber: forgejoPRNumber,
		ExternalURL:  "http://forgejo.example.test/example-team/shipping-fixture/pulls/132",
		SourceBranch: branch, TargetBranch: "main", State: service.ProjectionStateOpen, LastSyncedSHA: oldHead,
	}); err != nil {
		t.Fatalf("upsert projection: %v", err)
	}

	forgejoClient := &projectionWorkflowForgejoClient{
		headSHA: oldHead, headRef: branch, baseRef: "main", prNumber: forgejoPRNumber,
		permissions: map[string]string{"operator": "owner"},
		labels:      []string{forgejointegration.AGSActionRebaseLabel, forgejointegration.AGSStatusRebasingLabel},
	}
	concurrentSHA := mainSHA
	pusher := func(ctx context.Context, req forgejointegration.PushRequest) error {
		if out, err := exec.CommandContext(ctx, "git", "--git-dir", forgejoBare, "update-ref", "refs/heads/"+branch, concurrentSHA, oldHead).CombinedOutput(); err != nil {
			return fmt.Errorf("inject concurrent Forgejo update: %w: %s", err, out)
		}
		forgejoClient.headSHA = concurrentSHA
		return forgejointegration.GitPush(ctx, req)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"chore/*"}, PRBranchIncludes: []string{"chore/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{repoFullName: {Owner: "example-team", Repo: "shipping-fixture", BaseBranch: "main"}},
	}, forgejoClient, pusher)

	body := []byte(fmt.Sprintf(`{
	  "action": "label_updated",
	  "repository": {"full_name": "example-team/shipping-fixture", "default_branch": "main"},
	  "pull_request": {
	    "number": %d,
	    "html_url": "http://forgejo.example.test/example-team/shipping-fixture/pulls/132",
	    "head": {"ref": %q, "sha": %q},
	    "base": {"ref": "main"},
	    "labels": [{"name": "ags/action-rebase"}]
	  },
	  "label": {"name": "ags/action-rebase"},
	  "sender": {"login": "operator"}
	}`, forgejoPRNumber, branch, oldHead))
	seedDispatchedIntent(t, svc, pr.ID, pr.RepositoryID, agsPRNumber, forgejoPRNumber, repoFullName, branch, "main", oldHead, []string{forgejointegration.AGSActionRebaseLabel, forgejointegration.AGSStatusRebasingLabel})
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{
		"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))},
		"X-Gitea-Delivery":    {"delivery-lease-132"},
	}, body)
	if !errors.Is(err, service.ErrDelegatedSessionUseTimeDenied) || result.WorkflowStatus != "projection_failed" || result.AGSPRNumber != agsPRNumber || result.PRNumber != forgejoPRNumber {
		t.Fatalf("concurrent provider drift was not terminally denied: result=%#v err=%v", result, err)
	}
	var deniedIntent db.PullRequestActionIntent
	if loadErr := svc.DB.Where("pull_request_id = ?", pr.ID).Order("created_at DESC").First(&deniedIntent).Error; loadErr != nil || deniedIntent.State != service.ForgejoActionIntentDenied {
		t.Fatalf("concurrent drift intent=%#v loadErr=%v", deniedIntent, loadErr)
	}
}

func TestForgejoActionRebaseLabelIgnoresWebhookWhenActionLabelAlreadyRemoved(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "label-idem", Name: "label-idem", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "label-idem", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/rebase-label-idempotent"
	if err := svc.Git.CreateBranch(ctx, "label-idem/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, "label-idem/repo", branch, "feature.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "label-idem/repo", Title: "Projection label", HeadRef: branch, BaseRef: "main", AuthorLogin: "label-idem"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
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
	forgejoClient := &projectionWorkflowForgejoClient{headSHA: pr.HeadSHA, permissions: map[string]string{"operator": "owner"}, labels: []string{forgejointegration.AGSStatusRebasingLabel}}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "token",
		WebhookSecret:        "secret",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"label-idem/repo": {Owner: "forgejo", Repo: "repo"},
		},
	}, forgejoClient, func(ctx context.Context, req forgejointegration.PushRequest) error {
		t.Fatalf("stale action-label webhook must not rebase or project")
		return nil
	})

	body := []byte(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/repo", "default_branch": "main"},
	  "pull_request": {
	    "number": 42,
	    "html_url": "http://forgejo.local/forgejo/repo/pulls/42",
	    "head": {"ref": "agent/rebase-label-idempotent", "sha": "old"},
	    "base": {"ref": "main"},
	    "labels": [{"name": "ags/action-rebase"}]
	  },
	  "label": {"name": "ags/action-rebase"},
	  "sender": {"login": "operator"}
	}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body)
	if err != nil {
		t.Fatalf("HandleForgejoWebhook: %v", err)
	}
	if result.WorkflowStatus != "ignored" || len(forgejoClient.added) != 0 || len(forgejoClient.comments) != 0 {
		t.Fatalf("stale action should be ignored; result=%#v added=%v comments=%v", result, forgejoClient.added, forgejoClient.comments)
	}
}

func TestForgejoActionRebaseLabelTerminallyDeniesBaseMoveAfterProjection(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "label-race", Name: "label-race", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "label-race", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/rebase-label-race"
	if err := svc.Git.CreateBranch(ctx, "label-race/repo", branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if _, err := svc.Git.WriteFile(ctx, "label-race/repo", branch, "feature.txt", "feature", []byte("feature\n")); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "label-race/repo", Title: "Projection label", HeadRef: branch, BaseRef: "main", AuthorLogin: "label-race"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	baseSHA, err := svc.Git.WriteFile(ctx, "label-race/repo", "main", "base.txt", "base", []byte("base\n"))
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
	repoPath, err := svc.Git.GetRepoPath(ctx, "label-race/repo")
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
	forgejoClient := &projectionWorkflowForgejoClient{
		headSHA: pr.HeadSHA, headRef: branch, baseRef: "main", prNumber: 42,
		permissions: map[string]string{"operator": "owner"}, labels: []string{forgejointegration.AGSActionRebaseLabel},
	}
	movedBase := false
	pusher := func(ctx context.Context, req forgejointegration.PushRequest) error {
		if err := forgejointegration.GitPush(ctx, req); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "git", "-C", req.RepoPath, "ls-remote", req.RemoteURL, "refs/heads/"+req.BranchName).Output()
		if err != nil {
			return err
		}
		forgejoClient.headSHA = strings.Fields(string(out))[0]
		if !movedBase {
			movedBase = true
			if _, err := svc.Git.WriteFile(ctx, "label-race/repo", "main", "late-base.txt", "late base", []byte("late base\n")); err != nil {
				return err
			}
		}
		return nil
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{
			"label-race/repo": {Owner: "forgejo", Repo: "repo", BaseBranch: "main"},
		},
	}, forgejoClient, pusher)

	body := []byte(fmt.Sprintf(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/repo", "default_branch": "main"},
	  "pull_request": {
	    "number": 42,
	    "html_url": "http://forgejo.local/forgejo/repo/pulls/42",
	    "head": {"ref": "agent/rebase-label-race", "sha": %q},
	    "base": {"ref": "main"},
	    "labels": [{"name": "ags/action-rebase"}]
	  },
	  "label": {"name": "ags/action-rebase"},
	  "sender": {"login": "operator"}
	}`, pr.HeadSHA))
	seedDispatchedIntent(t, svc, pr.ID, pr.RepositoryID, pr.Number, 42, "forgejo/repo", branch, "main", pr.HeadSHA, []string{forgejointegration.AGSActionRebaseLabel})
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body)
	if !errors.Is(err, service.ErrDelegatedSessionUseTimeDenied) || result.WorkflowStatus == "rebased" {
		t.Fatalf("late base move was not terminally denied; result=%#v err=%v", result, err)
	}
	for _, comment := range forgejoClient.comments {
		if strings.Contains(comment, "AGS rebase completed") {
			t.Fatalf("late base move published false success: %v", forgejoClient.comments)
		}
	}
}

func TestForgejoActionRebaseLabelRejectsBaseMoveDuringConvergenceReconciliation(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	const repoFullName = "label-converged-race/repo"

	if err := svc.DB.Create(&db.User{Login: "label-converged-race", Name: "label-converged-race", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "label-converged-race", Name: "repo", DefaultBranch: "main", AddReadme: true}); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	branch := "agent/rebase-converged-race"
	if err := svc.Git.CreateBranch(ctx, repoFullName, branch, "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	staleHead, err := svc.Git.WriteFile(ctx, repoFullName, branch, "feature.txt", "feature", []byte("feature\n"))
	if err != nil {
		t.Fatalf("write feature: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: repoFullName, Title: "Converged race", HeadRef: branch, BaseRef: "main", AuthorLogin: "label-converged-race"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	convergedHead, err := svc.Git.WriteFile(ctx, repoFullName, branch, "followup.txt", "followup", []byte("followup\n"))
	if err != nil {
		t.Fatalf("write converged head: %v", err)
	}
	if err := svc.UpdatePRFields(ctx, pr.ID, map[string]any{"head_sha": convergedHead}); err != nil {
		t.Fatalf("update PR head: %v", err)
	}
	baseSHA, err := svc.Git.HeadSHA(ctx, repoFullName, "main")
	if err != nil {
		t.Fatalf("read base: %v", err)
	}
	if err := svc.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo,
		ExternalRepo: "forgejo/repo", ExternalNumber: 42, ExternalURL: "http://forgejo.local/forgejo/repo/pulls/42",
		SourceBranch: branch, TargetBranch: "main", State: service.ProjectionStateOpen, LastSyncedSHA: staleHead,
	}); err != nil {
		t.Fatalf("upsert stale projection: %v", err)
	}

	repoPath, err := svc.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	forgejoRoot := t.TempDir()
	forgejoBare := filepath.Join(forgejoRoot, "forgejo", "repo.git")
	if err := os.MkdirAll(filepath.Dir(forgejoBare), 0o755); err != nil {
		t.Fatalf("create Forgejo parent: %v", err)
	}
	if out, err := exec.Command("git", "init", "--bare", forgejoBare).CombinedOutput(); err != nil {
		t.Fatalf("init Forgejo bare repo: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, convergedHead+":refs/heads/"+branch, baseSHA+":refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("seed Forgejo refs: %v: %s", err, out)
	}

	forgejoClient := &projectionWorkflowForgejoClient{
		headSHA: convergedHead, headRef: branch, baseRef: "main", prNumber: 42,
		permissions: map[string]string{"operator": "owner"}, labels: []string{forgejointegration.AGSActionRebaseLabel},
	}
	movedBase := ""
	forgejoClient.onListPullRequests = func(call int) error {
		if call != 2 {
			return nil
		}
		var err error
		movedBase, err = svc.Git.WriteFile(ctx, repoFullName, "main", "late-converged-base.txt", "late converged base", []byte("late base\n"))
		if err != nil {
			return err
		}
		if out, err := exec.Command("git", "-C", repoPath, "push", "file://"+forgejoBare, movedBase+":refs/heads/main").CombinedOutput(); err != nil {
			return fmt.Errorf("project moved base: %w: %s", err, out)
		}
		return nil
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true, BaseURL: "file://" + forgejoRoot, Token: "token", WebhookSecret: "secret", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]forgejointegration.RepoMapping{repoFullName: {Owner: "forgejo", Repo: "repo", BaseBranch: "main"}},
	}, forgejoClient, func(ctx context.Context, req forgejointegration.PushRequest) error {
		t.Fatalf("already-converged reconciliation must not rewrite before the post-check")
		return nil
	})

	body := []byte(fmt.Sprintf(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/repo", "default_branch": "main"},
	  "pull_request": {
	    "number": 42,
	    "html_url": "http://forgejo.local/forgejo/repo/pulls/42",
	    "head": {"ref": %q, "sha": %q},
	    "base": {"ref": "main"},
	    "labels": [{"name": "ags/action-rebase"}]
	  },
	  "label": {"name": "ags/action-rebase"},
	  "sender": {"login": "operator"}
	}`, branch, convergedHead))
	seedDispatchedIntent(t, svc, pr.ID, pr.RepositoryID, pr.Number, 42, "forgejo/repo", branch, "main", convergedHead, []string{forgejointegration.AGSActionRebaseLabel})
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(ctx, http.Header{"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}, body)
	if err != nil || result.WorkflowStatus != "denied" || movedBase != "" {
		t.Fatalf("stale converged head was not denied before mutation: movedBase=%s result=%#v err=%v", movedBase, result, err)
	}
	if len(forgejoClient.comments) == 0 {
		t.Fatal("denied rebase request must still publish a visible comment")
	}
}

func TestUpdatePRCloseMarksExternalProjectionsClosed(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()
	forgejoClient := &projectionCloseForgejoClient{}
	gitLabClient := &projectionCloseGitLabClient{}
	gitHubClient := &projectionCloseGitHubClient{}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true,
		BaseURL: "http://forgejo.local",
		Token:   "token",
		RepoMap: map[string]forgejointegration.RepoMapping{"close-proj/repo": {Owner: "forgejo", Repo: "repo"}},
	}, forgejoClient, nil)
	svc.GitLabIntegration = gitlabintegration.NewWithClient(gitlabintegration.Config{
		Enabled:        true,
		BaseURL:        "http://gitlab.local",
		Token:          "token",
		MergeAuthority: "forgejo",
		Repos:          map[string]gitlabintegration.RepoMapping{"close-proj/repo": {ProjectPath: "gitlab/repo", TargetBranch: "main"}},
	}, nil, gitLabClient)
	svc.GitHubIntegration = githubintegration.NewWithClient(githubintegration.Config{
		Enabled:        true,
		Token:          "token",
		MergeAuthority: "forgejo",
		Repos:          map[string]githubintegration.RepoMapping{"close-proj/repo": {Owner: "example-org", Repo: "project-kit", TargetBranch: "main"}},
	}, nil, gitHubClient)
	if err := svc.DB.Create(&db.User{Login: "close-proj", Name: "close-proj", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "close-proj", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "close-proj/repo", "agent/close", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "close-proj/repo", Title: "Projection close", Body: "Multica: HUM-60", HeadRef: "agent/close", BaseRef: "main", AuthorLogin: "close-proj"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	for _, projection := range []db.PullRequestProjection{
		{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderForgejo, ExternalRepo: "forgejo/repo", ExternalNumber: 42, SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: service.ProjectionStateOpen, LastSyncedSHA: "abc123"},
		{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderGitLab, ExternalRepo: "gitlab/repo", ExternalNumber: 7, SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: service.ProjectionStateOpen, LastSyncedSHA: "abc123"},
		{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: service.ProjectionProviderGitHub, ExternalRepo: "example-org/project-kit", ExternalNumber: 8, SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: service.ProjectionStateOpen, LastSyncedSHA: "abc123"},
	} {
		if err := svc.UpsertPullRequestProjection(ctx, projection); err != nil {
			t.Fatalf("upsert projection: %v", err)
		}
	}
	closed := db.StateClosed
	if _, err := svc.UpdatePR(ctx, "close-proj/repo", pr.Number, service.UpdatePRInput{State: &closed}); err != nil {
		t.Fatalf("close pr: %v", err)
	}
	var rows []db.PullRequestProjection
	if err := svc.DB.Where("pull_request_id = ?", pr.ID).Order("provider").Find(&rows).Error; err != nil {
		t.Fatalf("load projections: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%#v", rows)
	}
	for _, row := range rows {
		if row.State != service.ProjectionStateClosed || row.LastSyncedSHA == "" {
			t.Fatalf("projection not closed: %#v", row)
		}
	}
	if forgejoClient.closedNumber != 42 || forgejoClient.closedState != service.ProjectionStateClosed {
		t.Fatalf("forgejo close not called: number=%d state=%q", forgejoClient.closedNumber, forgejoClient.closedState)
	}
	if gitLabClient.closeSource != "agent/close" || gitLabClient.closeTarget != "main" {
		t.Fatalf("gitlab close source=%q target=%q", gitLabClient.closeSource, gitLabClient.closeTarget)
	}
	if gitLabClient.closeNote == "" || strings.Contains(gitLabClient.closeNote, "merged") {
		t.Fatalf("gitlab close note=%q", gitLabClient.closeNote)
	}
	if gitHubClient.closeSource != "agent/close" || gitHubClient.closeTarget != "main" {
		t.Fatalf("github close source=%q target=%q", gitHubClient.closeSource, gitHubClient.closeTarget)
	}
	if gitHubClient.closeNote == "" || strings.Contains(gitHubClient.closeNote, "merged") {
		t.Fatalf("github close note=%q", gitHubClient.closeNote)
	}
}

func TestRecoverDurableActionIntents_DeniesExpiredPlannedIntent(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "recovery-user", Name: "recovery-user", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "recovery-user", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "recovery-user/repo", "agent/recovery", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "recovery-user/repo", Title: "recovery", HeadRef: "agent/recovery", BaseRef: "main", AuthorLogin: "recovery-user"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	expiredIntent := db.PullRequestActionIntent{
		ID:              "expired-planned",
		IdempotencyKey:  "expired-planned-key",
		Action:          "pr.rebase",
		State:           service.ForgejoActionIntentPlanned,
		PullRequestID:   pr.ID,
		RepositoryID:    pr.RepositoryID,
		AGSPRNumber:     pr.Number,
		Repository:      "recovery-user/repo",
		ForgejoRepo:     "forgejo/repo",
		ForgejoPRNumber: 42,
		HeadRef:         "agent/recovery",
		BaseRef:         "main",
		ExpectedHeadSHA: pr.HeadSHA,
		ExpectedBaseSHA: pr.HeadSHA,
		ExpectedLabels:  "[]",
		PostLabels:      "[\"ags/action-rebase\"]",
		PrincipalID:     1,
		ExpiresAt:       time.Now().UTC().Add(-1 * time.Minute), // already expired
	}
	if err := svc.DB.Create(&expiredIntent).Error; err != nil {
		t.Fatalf("seed expired intent: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true}, nil, nil)

	if err := svc.RecoverDurableActionIntents(ctx); err != nil {
		t.Fatalf("RecoverDurableActionIntents: %v", err)
	}

	var result db.PullRequestActionIntent
	if err := svc.DB.First(&result, "id = ?", "expired-planned").Error; err != nil {
		t.Fatalf("load recovered intent: %v", err)
	}
	if result.State != service.ForgejoActionIntentDenied {
		t.Fatalf("expired planned intent state=%s, want denied", result.State)
	}
	if result.FailureCode != "expired" {
		t.Fatalf("expired planned intent failure_code=%s, want expired", result.FailureCode)
	}
}

func TestRecoverDurableActionIntents_DeniesExpiredRunningIntent(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "recovery-run", Name: "recovery-run", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "recovery-run", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "recovery-run/repo", "agent/recovery-run", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "recovery-run/repo", Title: "recovery", HeadRef: "agent/recovery-run", BaseRef: "main", AuthorLogin: "recovery-run"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	expiredIntent := db.PullRequestActionIntent{
		ID:              "expired-running",
		IdempotencyKey:  "expired-running-key",
		Action:          "pr.rebase",
		State:           service.ForgejoActionIntentRunning,
		PullRequestID:   pr.ID,
		RepositoryID:    pr.RepositoryID,
		AGSPRNumber:     pr.Number,
		Repository:      "recovery-run/repo",
		ForgejoRepo:     "forgejo/repo",
		ForgejoPRNumber: 42,
		HeadRef:         "agent/recovery-run",
		BaseRef:         "main",
		ExpectedHeadSHA: pr.HeadSHA,
		ExpectedBaseSHA: pr.HeadSHA,
		ExpectedLabels:  "[]",
		PostLabels:      "[\"ags/action-rebase\"]",
		PrincipalID:     1,
		ExpiresAt:       time.Now().UTC().Add(-1 * time.Minute),
	}
	if err := svc.DB.Create(&expiredIntent).Error; err != nil {
		t.Fatalf("seed expired running intent: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true}, nil, nil)

	if err := svc.RecoverDurableActionIntents(ctx); err != nil {
		t.Fatalf("RecoverDurableActionIntents: %v", err)
	}

	var result db.PullRequestActionIntent
	if err := svc.DB.First(&result, "id = ?", "expired-running").Error; err != nil {
		t.Fatalf("load recovered intent: %v", err)
	}
	if result.State != service.ForgejoActionIntentDenied {
		t.Fatalf("expired running intent state=%s, want denied", result.State)
	}
}

func TestRecoverDurableActionIntents_LeavesNonExpiredDispatchedPending(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "recovery-disp", Name: "recovery-disp", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "recovery-disp", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "recovery-disp/repo", "agent/recovery-disp", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "recovery-disp/repo", Title: "recovery", HeadRef: "agent/recovery-disp", BaseRef: "main", AuthorLogin: "recovery-disp"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	authority, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "recovery", Issuer: "urn:recovery", KeyIDs: []string{"recovery-key"}, Status: "active", TrustRevision: "trust-v1"}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "recovery-team", IssuerInstanceID: "recovery", WorkspaceID: "workspace", TeamIdentityID: "team", PolicyClass: "recovery.action.v1", PrincipalID: pr.AuthorID, Status: "active", BindingRevision: "team-v1", EpochFloor: 1}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: "recovery.action.v1", Status: "active", PolicyRevision: "class-v1", Operations: []string{"pr.rebase"}}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "recovery-repo", Target: "test", Service: "ags", Repository: "recovery-disp/repo", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "repo-v1"}},
	})
	if err != nil {
		t.Fatalf("create recovery authority: %v", err)
	}
	svc.PrincipalSessions = authority
	intent := db.PullRequestActionIntent{
		ID:              "live-dispatched",
		IdempotencyKey:  "live-dispatched-key",
		Action:          "pr.rebase",
		State:           service.ForgejoActionIntentDispatched,
		PullRequestID:   pr.ID,
		RepositoryID:    pr.RepositoryID,
		AGSPRNumber:     pr.Number,
		Repository:      "recovery-disp/repo",
		ForgejoRepo:     "forgejo/repo",
		ForgejoPRNumber: 42,
		HeadRef:         "agent/recovery-disp",
		BaseRef:         "main",
		ExpectedHeadSHA: pr.HeadSHA,
		ExpectedBaseSHA: pr.HeadSHA,
		ExpectedLabels:  "[]",
		PostLabels:      "[\"ags/action-rebase\"]",
		PrincipalID:     pr.AuthorID,
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute), // not expired
	}
	if err := service.StampDurableActionIntentAuthorityForTest(ctx, svc, &intent); err != nil {
		t.Fatalf("stamp dispatched intent authority: %v", err)
	}
	if err := svc.DB.Create(&intent).Error; err != nil {
		t.Fatalf("seed dispatched intent: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true}, nil, nil)

	if err := svc.RecoverDurableActionIntents(ctx); err != nil {
		t.Fatalf("RecoverDurableActionIntents: %v", err)
	}

	var result db.PullRequestActionIntent
	if err := svc.DB.First(&result, "id = ?", "live-dispatched").Error; err != nil {
		t.Fatalf("load recovered intent: %v", err)
	}
	// Dispatched intents are left pending for webhook/reconciliation.
	if result.State != service.ForgejoActionIntentDispatched {
		t.Fatalf("non-expired dispatched intent state=%s, want dispatched", result.State)
	}
}

func TestRecoverDurableActionIntents_DeniesExpiredDispatchedIntent(t *testing.T) {
	svc, cleanup := setupTestService(t)
	defer cleanup()
	ctx := context.Background()

	if err := svc.DB.Create(&db.User{Login: "recovery-exp", Name: "recovery-exp", Type: db.TypeUser}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, err := svc.CreateRepo(ctx, service.CreateRepoInput{OwnerLogin: "recovery-exp", Name: "repo", DefaultBranch: "main", AddReadme: true})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := svc.Git.CreateBranch(ctx, "recovery-exp/repo", "agent/recovery-exp", "main"); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	pr, err := svc.CreatePR(ctx, service.CreatePRInput{RepoFullName: "recovery-exp/repo", Title: "recovery", HeadRef: "agent/recovery-exp", BaseRef: "main", AuthorLogin: "recovery-exp"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	expiredIntent := db.PullRequestActionIntent{
		ID:              "expired-dispatched",
		IdempotencyKey:  "expired-dispatched-key",
		Action:          "pr.rebase",
		State:           service.ForgejoActionIntentDispatched,
		PullRequestID:   pr.ID,
		RepositoryID:    pr.RepositoryID,
		AGSPRNumber:     pr.Number,
		Repository:      "recovery-exp/repo",
		ForgejoRepo:     "forgejo/repo",
		ForgejoPRNumber: 42,
		HeadRef:         "agent/recovery-exp",
		BaseRef:         "main",
		ExpectedHeadSHA: pr.HeadSHA,
		ExpectedBaseSHA: pr.HeadSHA,
		ExpectedLabels:  "[]",
		PostLabels:      "[\"ags/action-rebase\"]",
		PrincipalID:     1,
		ExpiresAt:       time.Now().UTC().Add(-1 * time.Minute), // expired
	}
	if err := svc.DB.Create(&expiredIntent).Error; err != nil {
		t.Fatalf("seed expired dispatched intent: %v", err)
	}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true}, nil, nil)

	if err := svc.RecoverDurableActionIntents(ctx); err != nil {
		t.Fatalf("RecoverDurableActionIntents: %v", err)
	}

	var result db.PullRequestActionIntent
	if err := svc.DB.First(&result, "id = ?", "expired-dispatched").Error; err != nil {
		t.Fatalf("load recovered intent: %v", err)
	}
	if result.State != service.ForgejoActionIntentDenied {
		t.Fatalf("expired dispatched intent state=%s, want denied", result.State)
	}
}
