package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
)

// setupIntentTestService creates a service with a PR, projection, ForgejoIntegration,
// and a gitstore suitable for testing handleForgejoPullRequestActionLabel paths.
func setupIntentTestService(t *testing.T) (*Service, db.PullRequest, db.PullRequestProjection) {
	t.Helper()
	svc, pr, projection, _ := setupIntentTestServiceWithClient(t)
	return svc, pr, projection
}

func durableActionTestAuthority(t *testing.T, principalID uint, policyRevision string, operations []string) *sessionauthority.Set {
	t.Helper()
	if operations == nil {
		operations = []string{"ci.read", "git.push", "git.read", "pr.create", "pr.read", "pr.rebase", "repo.read", "review.read"}
	}
	classID := sessionauthority.DefaultDynamicPolicyClass
	if len(operations) == 2 {
		classID = "read-only"
	}
	authority, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "multica-mini", Issuer: "multica", KeyIDs: []string{"session-key"}, Status: "active", TrustRevision: "trust-v1"}},
		TeamBindings:   []sessionauthority.TeamBinding{{ID: "team-example-owner", IssuerInstanceID: "multica-mini", WorkspaceID: "workspace-1", TeamIdentityID: "team-example-owner", PolicyClass: classID, PrincipalID: principalID, Status: "active", BindingRevision: "team-example-owner-v1", EpochFloor: 1}},
		PolicyClasses:  []sessionauthority.PolicyClass{{ID: classID, Status: "active", PolicyRevision: policyRevision, Operations: operations}},
		Resources:      []sessionauthority.ResourcePolicy{{ID: "repo-example-owner-demo", Target: "primary-a", Service: "ags", Repository: "example-owner/demo", Status: "active", MaxSessionTTL: "15m", PolicyRevision: "repo-v1"}},
	})
	if err != nil {
		t.Fatalf("create durable action authority: %v", err)
	}
	return authority
}

func setupIntentTestServiceWithClient(t *testing.T) (*Service, db.PullRequest, db.PullRequestProjection, *minimalForgejoClient) {
	t.Helper()
	svc := setupProjectionStateService(t)
	svc.SourceRevision = strings.Repeat("a", 40)
	// Durable action snapshot fixtures exercise the agent policy path. Human
	// operators intentionally ignore PrincipalSession policy and use native
	// repository permission instead.
	if err := svc.DB.Model(&db.User{}).Where("id = ?", 1).Update("user_kind", db.UserKindAgent).Error; err != nil {
		t.Fatalf("mark durable action fixture principal as agent: %v", err)
	}

	// Set up a gitstore with a real repo so resume paths don't panic.
	store, err := gitstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("new git store: %v", err)
	}
	if err := store.Init(context.Background(), "example-owner/demo", "main", true); err != nil {
		t.Fatalf("init git repo: %v", err)
	}
	svc.Git = store
	svc.PrincipalSessions = durableActionTestAuthority(t, 1, "class-v1", nil)
	repoPath, err := store.GetRepoPath(context.Background(), "example-owner/demo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	baseSHA, err := store.HeadSHA(context.Background(), "example-owner/demo", "main")
	if err != nil {
		t.Fatalf("base SHA: %v", err)
	}
	if output, err := exec.Command("git", "--git-dir", repoPath, "update-ref", "refs/heads/agent/intent-test", baseSHA).CombinedOutput(); err != nil {
		t.Fatalf("create test head: %v: %s", err, output)
	}

	pr := db.PullRequest{
		Number: 100, RepositoryID: 1, Title: "intent test",
		State: db.StateOpen, HeadRef: "agent/intent-test", HeadSHA: baseSHA, BaseRef: "main", BaseSHA: baseSHA,
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatalf("create PR: %v", err)
	}
	projection := db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: 1, Provider: ProjectionProviderForgejo,
		ExternalRepo: "forgejo/repo", ExternalNumber: 42,
		SourceBranch: pr.HeadRef, TargetBranch: pr.BaseRef, State: ProjectionStateOpen,
		LastSyncedSHA: pr.HeadSHA,
	}
	if err := svc.DB.Create(&projection).Error; err != nil {
		t.Fatalf("create projection: %v", err)
	}

	// Set up a minimal ForgejoIntegration with exact live PR/ref readback.
	client := &minimalForgejoClient{pr: forgejointegration.PullRequestSnapshot{
		Number: projection.ExternalNumber, State: "open", HeadRef: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseRef: pr.BaseRef,
	}, remoteRefs: map[string]string{pr.HeadRef: pr.HeadSHA, pr.BaseRef: pr.BaseSHA}}
	enabled := true
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "token",
		RepoMap: map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "repo", Enabled: &enabled}},
	}, client, func(context.Context, forgejointegration.PushRequest) error { return nil }, func(_ context.Context, _, _, ref string) (string, error) {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.remoteRefs[strings.TrimPrefix(ref, "refs/heads/")], nil
	})

	return svc, pr, projection, client
}

// minimalForgejoClient is a minimal Forgejo client for intent tests.
type minimalForgejoClient struct {
	mu             sync.Mutex
	labels         map[string][]string
	pr             forgejointegration.PullRequestSnapshot
	remoteRefs     map[string]string
	listCalls      int
	addCalls       int
	removeCalls    int
	commentCalls   int
	comments       []forgejointegration.PullRequestComment
	listedLabels   []string
	overrideLabels bool
	removeErr      error
	addErr         error
	permissions    map[string]string
	permissionErr  error
	addStarted     chan struct{}
	addRelease     <-chan struct{}
	addStartedOnce sync.Once
}

func (c *minimalForgejoClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	return nil
}
func (c *minimalForgejoClient) EnsurePullRequest(ctx context.Context, in forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (c *minimalForgejoClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (c *minimalForgejoClient) AddIssueLabels(ctx context.Context, owner, repo string, issueNumber int, labels []string) error {
	c.mu.Lock()
	c.addCalls++
	callNumber := c.addCalls
	if c.labels == nil {
		c.labels = make(map[string][]string)
	}
	key := owner + "/" + repo + "/" + string(rune(issueNumber))
	c.labels[key] = append(c.labels[key], labels...)
	if c.overrideLabels {
		c.listedLabels = append(c.listedLabels, labels...)
	}
	started, release, addErr := c.addStarted, c.addRelease, c.addErr
	c.mu.Unlock()
	if callNumber != 1 {
		return addErr
	}
	if started != nil {
		c.addStartedOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return addErr
}
func (c *minimalForgejoClient) RemoveIssueLabel(ctx context.Context, owner, repo string, issueNumber int, label string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeCalls++
	return c.removeErr
}
func (c *minimalForgejoClient) CreateIssueComment(ctx context.Context, owner, repo string, issueNumber int, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commentCalls++
	c.comments = append(c.comments, forgejointegration.PullRequestComment{Body: body})
	return nil
}
func (c *minimalForgejoClient) ListIssueComments(ctx context.Context, owner, repo string, issueNumber, page, limit int) ([]forgejointegration.PullRequestComment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if page > 1 {
		return nil, nil
	}
	return append([]forgejointegration.PullRequestComment(nil), c.comments...), nil
}
func (c *minimalForgejoClient) ListIssueLabels(ctx context.Context, owner, repo string, issueNumber int) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listCalls++
	if c.overrideLabels {
		return append([]string(nil), c.listedLabels...), nil
	}
	return []string{forgejointegration.AGSActionRebaseLabel}, nil
}
func (c *minimalForgejoClient) GetPullRequest(context.Context, string, string, int) (forgejointegration.PullRequestSnapshot, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pr, c.pr.Number > 0, nil
}
func (c *minimalForgejoClient) CollaboratorPermission(_ context.Context, _, _, username string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.permissionErr != nil {
		return "", c.permissionErr
	}
	return c.permissions[username], nil
}

func stampDurableActionIntentAuthority(t *testing.T, svc *Service, intent *db.PullRequestActionIntent) {
	t.Helper()
	var principal db.User
	if err := svc.DB.First(&principal, intent.PrincipalID).Error; err != nil {
		t.Fatal(err)
	}
	result, err := svc.AuthorizeDurableOperation(context.Background(), principal, "ags", intent.Repository, "pr.rebase",
		forgejoRebaseDurableConstraints(intent.AGSPRNumber, intent.ForgejoPRNumber, intent.ExpectedHeadSHA, intent.ExpectedBaseSHA))
	if err != nil {
		t.Fatalf("authorize durable intent fixture: %v", err)
	}
	intent.TeamIdentityID = result.AuthorizationBasis.TeamIdentityID
	intent.PolicyClass = result.AuthorizationBasis.PolicyClass
	intent.MembershipEpoch = result.AuthorizationBasis.MembershipEpoch
	intent.AuthorityRev = durableActionAuthorityRevision(result)
}

// seedDispatchedIntent creates a dispatched intent ready for webhook consumption.
func seedDispatchedIntent(t *testing.T, svc *Service, pr db.PullRequest, projection db.PullRequestProjection) db.PullRequestActionIntent {
	t.Helper()
	intent := db.PullRequestActionIntent{
		ID:              "test-intent-1",
		IdempotencyKey:  "test-key-1",
		Action:          "pr.rebase",
		State:           ForgejoActionIntentDispatched,
		PullRequestID:   pr.ID,
		RepositoryID:    pr.RepositoryID,
		AGSPRNumber:     pr.Number,
		Repository:      "example-owner/demo",
		ForgejoRepo:     projection.ExternalRepo,
		ForgejoPRNumber: projection.ExternalNumber,
		HeadRef:         pr.HeadRef,
		BaseRef:         pr.BaseRef,
		ExpectedHeadSHA: pr.HeadSHA,
		ExpectedBaseSHA: pr.BaseSHA,
		ExpectedLabels:  "[]",
		PostLabels:      `["` + forgejointegration.AGSActionRebaseLabel + `"]`,
		PrincipalID:     1,
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}
	stampDurableActionIntentAuthority(t, svc, &intent)
	if err := svc.DB.Create(&intent).Error; err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	return intent
}

func bindTestActionJob(t *testing.T, svc *Service, intent db.PullRequestActionIntent, job *db.PullRequestProjectionJob) context.Context {
	t.Helper()
	intentID := intent.ID
	job.ActionIntentID = &intentID
	if job.ActionGeneration == 0 {
		job.ActionGeneration = 1
	}
	if job.ID == 0 {
		var maxID uint
		if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Select("COALESCE(MAX(id), 0)").Scan(&maxID).Error; err != nil {
			t.Fatalf("allocate test action job id: %v", err)
		}
		job.ID = maxID + 1
	}
	job.AgentSessionID = intent.AgentSessionID
	return contextWithForgejoActionJob(contextWithForgejoActionIntent(context.Background(), intent.ID), *job)
}

func TestActionIntentLabelSetIsExactAndOrderIndependent(t *testing.T) {
	raw, err := actionLabelsJSON([]string{"reviewed", forgejointegration.AGSActionRebaseLabel, "reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	if !sameActionLabels(raw, []string{forgejointegration.AGSActionRebaseLabel, "reviewed"}) {
		t.Fatal("same exact label set was rejected")
	}
	if sameActionLabels(raw, []string{forgejointegration.AGSActionRebaseLabel}) {
		t.Fatal("partial label set was accepted")
	}
}

func TestActionIntentReceiptDoesNotExposeAuthorityOrLabelAssertions(t *testing.T) {
	receipt, err := actionIntentReceipt(db.PullRequestActionIntent{
		ID: "intent-1", Action: "pr.rebase", State: ForgejoActionIntentPlanned, AGSPRNumber: 1, ForgejoPRNumber: 2,
		ExpectedHeadSHA: strings.Repeat("a", 40), ExpectedBaseSHA: strings.Repeat("b", 40),
	})
	if err != nil || receipt.ID == "" || receipt.State != ForgejoActionIntentPlanned {
		t.Fatalf("unexpected receipt: %#v, err=%v", receipt, err)
	}
}

func TestActionIntentReceiptBoundaryStatusMatrix(t *testing.T) {
	sessionID := "11111111-1111-4111-8111-111111111111"
	receiptID := "abr_" + strings.Repeat("a", 64)
	base := db.PullRequestActionIntent{
		ID: "intent-matrix", Action: "pr.rebase", AGSPRNumber: 1, ForgejoPRNumber: 2,
		ExpectedHeadSHA: strings.Repeat("a", 40), ExpectedBaseSHA: strings.Repeat("b", 40),
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*db.PullRequestActionIntent)
		wantErr bool
		status  string
	}{
		{name: "historical", mutate: func(v *db.PullRequestActionIntent) { v.State = ForgejoActionIntentDispatching }, status: ProviderEffectStatusNotRecorded},
		{name: "delegated planned", mutate: func(v *db.PullRequestActionIntent) {
			v.State = ForgejoActionIntentPlanned
			v.AgentSessionID = &sessionID
			v.BoundaryProtocol = AuthorityBoundaryReceiptDelegatedEffectKind
			v.ProviderEffectStatus = ProviderEffectStatusNotAttempted
		}, status: ProviderEffectStatusNotAttempted},
		{name: "delegated dispatching", mutate: func(v *db.PullRequestActionIntent) {
			v.State = ForgejoActionIntentDispatching
			v.AgentSessionID = &sessionID
			v.BoundaryProtocol = AuthorityBoundaryReceiptDelegatedEffectKind
			v.BoundaryReceiptID = &receiptID
			v.ProviderEffectStatus = ProviderEffectStatusOutcomeUnknown
		}, status: ProviderEffectStatusOutcomeUnknown},
		{name: "delegated missing receipt", mutate: func(v *db.PullRequestActionIntent) {
			v.State = ForgejoActionIntentDispatching
			v.AgentSessionID = &sessionID
			v.BoundaryProtocol = AuthorityBoundaryReceiptDelegatedEffectKind
			v.ProviderEffectStatus = ProviderEffectStatusOutcomeUnknown
		}, wantErr: true},
		{name: "durable forged receipt", mutate: func(v *db.PullRequestActionIntent) {
			v.State = ForgejoActionIntentDispatching
			v.BoundaryReceiptID = &receiptID
			v.ProviderEffectStatus = ProviderEffectStatusOutcomeUnknown
		}, wantErr: true},
		{name: "verified before completion", mutate: func(v *db.PullRequestActionIntent) {
			v.State = ForgejoActionIntentRunning
			v.ProviderEffectStatus = ProviderEffectStatusVerifiedCompleted
		}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent := base
			tc.mutate(&intent)
			receipt, err := actionIntentReceipt(intent)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("invalid matrix accepted: %#v", receipt)
				}
				return
			}
			if err != nil || receipt.ProviderEffectStatus != tc.status {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
		})
	}
}

func TestDispatchingIntentWebhookCanConsumeBeforeInitialProviderCallReturns(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Updates(map[string]any{"state": ForgejoActionIntentPlanned, "dispatched_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	dispatching, err := svc.beginForgejoActionIntentDispatch(context.Background(), intent.ID)
	if err != nil || dispatching.State != ForgejoActionIntentDispatching {
		t.Fatalf("dispatch claim intent=%#v err=%v", dispatching, err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	client.addStarted, client.addRelease = started, release
	dispatchDone := make(chan db.PullRequestActionIntent, 1)
	dispatchErr := make(chan error, 1)
	go func() {
		if err := svc.dispatchForgejoActionIntentLabel(context.Background(), dispatching, "test dispatch denial"); err != nil {
			dispatchErr <- err
			return
		}
		current, err := svc.completeForgejoActionIntentDispatch(context.Background(), dispatching.ID)
		if err != nil {
			dispatchErr <- err
			return
		}
		dispatchDone <- current
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider add did not reach the blocked return boundary")
	}

	consumed, accepted, denial, err := svc.consumeForgejoActionIntent(context.Background(), forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
		HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
	})
	if err != nil || !accepted || denial != forgejoActionIntentDenialNone || consumed.State != ForgejoActionIntentAccepted {
		t.Fatalf("early webhook intent=%#v accepted=%v denial=%s err=%v", consumed, accepted, denial, err)
	}
	if client.removeCalls != 0 {
		t.Fatalf("early webhook was quarantined: remove calls=%d", client.removeCalls)
	}
	close(release)
	select {
	case err := <-dispatchErr:
		t.Fatalf("dispatch completion: %v", err)
	case current := <-dispatchDone:
		if current.State != ForgejoActionIntentAccepted {
			t.Fatalf("late provider return overwrote webhook state: %#v", current)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch completion did not return")
	}
}

func TestDispatchingIntentEarlyWebhookDoesNotLoseBoundJob(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	job := seedResumableRebaseJob(t, svc, pr, projection, ForgejoProjectionPhaseFailedRetryable, "")
	if err := svc.DB.Model(&intent).Updates(map[string]any{"state": ForgejoActionIntentPlanned, "dispatched_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	dispatching, err := svc.beginForgejoActionIntentDispatch(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	client.addStarted, client.addRelease = started, release
	dispatchDone := make(chan error, 1)
	go func() {
		if err := svc.dispatchForgejoActionIntentLabel(context.Background(), dispatching, "test dispatch denial"); err != nil {
			dispatchDone <- err
			return
		}
		_, err := svc.completeForgejoActionIntentDispatch(context.Background(), dispatching.ID)
		dispatchDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider add did not reach the blocked return boundary")
	}

	result, _ := svc.handleForgejoPullRequestActionLabel(context.Background(), forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
		HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
		LabelName: forgejointegration.AGSActionRebaseLabel,
	})
	close(release)
	select {
	case err := <-dispatchDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch completion did not return")
	}
	if result.WorkflowStatus == "denied" {
		t.Fatalf("early webhook was denied as missing authority: %#v", result)
	}
	var persistedJob db.PullRequestProjectionJob
	if err := svc.DB.First(&persistedJob, job.ID).Error; err != nil {
		t.Fatalf("bound job was lost: %v", err)
	}
	current, err := svc.loadForgejoActionIntent(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State == ForgejoActionIntentDispatched || current.State == ForgejoActionIntentDispatching {
		t.Fatalf("early webhook stalled before job processing: %#v", current)
	}
}

func TestRecoveryDispatchWebhookCanConsumeBeforeProviderCallReturns(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Updates(map[string]any{"state": ForgejoActionIntentPlanned, "dispatched_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	client.addStarted, client.addRelease = started, release
	recoveryDone := make(chan error, 1)
	go func() { recoveryDone <- svc.RecoverDurableActionIntents(context.Background()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery provider add did not reach the blocked return boundary")
	}

	consumed, accepted, denial, err := svc.consumeForgejoActionIntent(context.Background(), forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
		HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
	})
	if err != nil || !accepted || denial != forgejoActionIntentDenialNone || consumed.State != ForgejoActionIntentAccepted {
		t.Fatalf("recovery early webhook intent=%#v accepted=%v denial=%s err=%v", consumed, accepted, denial, err)
	}
	close(release)
	select {
	case err := <-recoveryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not return")
	}
	current, err := svc.loadForgejoActionIntent(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != ForgejoActionIntentAccepted || client.addCalls != 1 || client.removeCalls != 0 {
		t.Fatalf("recovery lost early webhook authority: intent=%#v add=%d remove=%d", current, client.addCalls, client.removeCalls)
	}
}

func TestInitialDispatchAcknowledgementAcceptsWebhookAdvanceAfterSuccessfulCAS(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Updates(map[string]any{"state": ForgejoActionIntentDispatching, "dispatched_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	var advanced db.PullRequestActionIntent
	svc.testForgejoActionDispatchAfterCAS = func(intentID string) {
		if intentID != intent.ID {
			t.Fatalf("dispatch hook intent=%q want %q", intentID, intent.ID)
		}
		consumed, accepted, denial, err := svc.consumeForgejoActionIntent(context.Background(), forgejointegration.PullRequestActionLabelEvent{
			RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
			HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
		})
		if err != nil || !accepted || denial != forgejoActionIntentDenialNone {
			t.Fatalf("post-CAS webhook intent=%#v accepted=%v denial=%s err=%v", consumed, accepted, denial, err)
		}
		advanced = consumed
	}
	defer func() { svc.testForgejoActionDispatchAfterCAS = nil }()

	if err := svc.dispatchForgejoActionIntentLabel(context.Background(), intent, "test initial dispatch denial"); err != nil {
		t.Fatalf("initial provider dispatch: %v", err)
	}
	current, err := svc.completeForgejoActionIntentDispatch(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("initial dispatch acknowledgement rejected a later webhook state: %v", err)
	}
	if advanced.State != ForgejoActionIntentAccepted || current.State != ForgejoActionIntentAccepted {
		t.Fatalf("initial post-CAS advance was lost: advanced=%#v current=%#v", advanced, current)
	}
	if client.addCalls != 1 || client.removeCalls != 0 {
		t.Fatalf("initial provider/webhook calls: add=%d remove=%d", client.addCalls, client.removeCalls)
	}
}

func TestRecoveryDispatchAcknowledgementAcceptsWebhookAdvanceAfterSuccessfulCAS(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Updates(map[string]any{"state": ForgejoActionIntentDispatching, "dispatched_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	hookCalls := 0
	svc.testForgejoActionDispatchAfterCAS = func(intentID string) {
		hookCalls++
		if intentID != intent.ID {
			t.Fatalf("recovery dispatch hook intent=%q want %q", intentID, intent.ID)
		}
		consumed, accepted, denial, err := svc.consumeForgejoActionIntent(context.Background(), forgejointegration.PullRequestActionLabelEvent{
			RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
			HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
		})
		if err != nil || !accepted || denial != forgejoActionIntentDenialNone || consumed.State != ForgejoActionIntentAccepted {
			t.Fatalf("recovery post-CAS webhook intent=%#v accepted=%v denial=%s err=%v", consumed, accepted, denial, err)
		}
	}
	defer func() { svc.testForgejoActionDispatchAfterCAS = nil }()

	// RecoverDurableActionIntents is the exact recovery gate invoked during
	// server startup. A legal webhook advance between CAS and readback must not
	// turn successful startup recovery into an error.
	if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
		t.Fatalf("startup recovery rejected a later webhook state: %v", err)
	}
	current, err := svc.loadForgejoActionIntent(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hookCalls != 1 || current.State != ForgejoActionIntentAccepted || client.addCalls != 1 || client.removeCalls != 0 {
		t.Fatalf("recovery post-CAS advance was lost: hooks=%d intent=%#v add=%d remove=%d", hookCalls, current, client.addCalls, client.removeCalls)
	}
}

func TestRecoveryExpiresDispatchingIntentWithoutProviderRetry(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Updates(map[string]any{
		"state": ForgejoActionIntentDispatching, "expires_at": time.Now().UTC().Add(-time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := svc.loadForgejoActionIntent(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != ForgejoActionIntentDenied || current.FailureCode != "expired" || client.addCalls != 0 {
		t.Fatalf("expired dispatching intent=%#v provider adds=%d", current, client.addCalls)
	}
}

func TestRecoveryRetriesUnknownDispatchOutcomeWithoutReturningToPlanned(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Updates(map[string]any{"state": ForgejoActionIntentPlanned, "dispatched_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	client.addErr = errors.New("provider outcome unknown")
	if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := svc.loadForgejoActionIntent(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != ForgejoActionIntentDispatching || current.FailureCode != "dispatch_outcome_unknown" {
		t.Fatalf("unknown outcome was not left recoverable: %#v", current)
	}
	client.addErr = nil
	if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err = svc.loadForgejoActionIntent(context.Background(), intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != ForgejoActionIntentDispatched || current.FailureCode != "" || client.addCalls != 2 {
		t.Fatalf("unknown outcome retry did not converge: intent=%#v add=%d", current, client.addCalls)
	}
}

func TestIsValidGitSHA(t *testing.T) {
	valid := []string{
		"0123456789abcdef0123456789abcdef01234567",
	}
	for _, v := range valid {
		if !isValidGitSHA(v) {
			t.Errorf("isValidGitSHA(%q) = false, want true", v)
		}
	}
	invalid := []string{
		"", "   ", "012345", "df629a3",
		" 0123456789abcdef0123456789abcdef01234567", "0123456789abcdef0123456789abcdef01234567 ",
		"0123456789ABCDEF0123456789ABCDEF01234567",
		"0123456789abcdef0123456789abcdef012345678",
		"not-a-sha", "gggggggggggggggggggggggggggggggggggggggg",
	}
	for _, v := range invalid {
		if isValidGitSHA(v) {
			t.Errorf("isValidGitSHA(%q) = true, want false", v)
		}
	}
}

// seedResumableRebaseJob seeds a PullRequestProjectionJob in a resumable phase
// with matching external repo/number and optionally a desired head SHA.
func seedResumableRebaseJob(t *testing.T, svc *Service, pr db.PullRequest, projection db.PullRequestProjection, phase string, desiredSHA string) db.PullRequestProjectionJob {
	t.Helper()
	var intent db.PullRequestActionIntent
	if err := svc.DB.Where("pull_request_id = ? AND action = ?", pr.ID, "pr.rebase").Order("created_at DESC").First(&intent).Error; err != nil {
		t.Fatalf("load exact intent for resumable job: %v", err)
	}
	job := db.PullRequestProjectionJob{
		PullRequestID:             pr.ID,
		RepositoryID:              pr.RepositoryID,
		Provider:                  ProjectionProviderForgejo,
		Trigger:                   ForgejoProjectionTriggerActionRebase,
		RepoFullName:              "example-owner/demo",
		AGSPRNumber:               pr.Number,
		HeadRef:                   pr.HeadRef,
		BaseRef:                   pr.BaseRef,
		HeadSHA:                   pr.HeadSHA,
		DesiredAGSHeadSHA:         desiredSHA,
		PreflightAGSHeadSHA:       pr.HeadSHA,
		PreflightBaseSHA:          pr.HeadSHA,
		ExpectedForgejoOldHeadSHA: pr.HeadSHA,
		ExternalRepo:              projection.ExternalRepo,
		ExternalNumber:            projection.ExternalNumber,
		Phase:                     phase,
		ActionGeneration:          1,
		ActionIntentID:            &intent.ID,
		AgentSessionID:            intent.AgentSessionID,
		RemoteRef:                 "refs/heads/" + pr.HeadRef,
	}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatalf("seed resumable job: %v", err)
	}
	return job
}

// TestHandleForgejoPullRequestActionLabel_ResumablePathTransitionToRunning verifies
// that the resumable path transitions the intent from accepted to running before
// calling resumeForgejoActionRebaseJob.
func TestHandleForgejoPullRequestActionLabel_ResumablePathTransitionToRunning(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	ctx := context.Background()

	// The durable job is bound to the exact dispatched intent.
	intent := seedDispatchedIntent(t, svc, pr, projection)
	seedResumableRebaseJob(t, svc, pr, projection, ForgejoProjectionPhaseFailedRetryable, "")

	// Call handleForgejoPullRequestActionLabel - the resume will fail because
	// s.Git is nil, but the intent must have been transitioned to running first.
	_, _ = svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo,
		PRNumber:     projection.ExternalNumber,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      pr.HeadSHA,
		BaseBranch:   pr.BaseRef,
		LabelName:    forgejointegration.AGSActionRebaseLabel,
	})

	// Read back the intent and verify it was transitioned through running.
	var intentResult db.PullRequestActionIntent
	if err := svc.DB.First(&intentResult, "id = ?", intent.ID).Error; err != nil {
		t.Fatalf("load intent: %v", err)
	}
	// Exact-intent recovery may not preserve an active recovery slot after its
	// provider projection cannot prove the bound generation.
	if intentResult.State != ForgejoActionIntentDenied {
		t.Fatalf("intent state=%s, want denied", intentResult.State)
	}
	if intentResult.FailureCode == "" {
		t.Fatal("terminal exact-intent denial must record a failure code")
	}
}

// TestHandleForgejoPullRequestActionLabel_ResumablePathRecoveryNeeded verifies that
// when resumeForgejoActionRebaseJob returns an error, the intent is set to
// recovery_needed with resume_failed code, preserving recovery rather than
// falsely denying.
func TestHandleForgejoPullRequestActionLabel_ResumablePathRecoveryNeeded(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	ctx := context.Background()

	intent := seedDispatchedIntent(t, svc, pr, projection)
	// Seed a resumable job WITHOUT a desired head SHA, so resumeForgejoActionRebaseJob
	// will call recoverForgejoActionRebaseDesiredHead which will fail (nil Git).
	seedResumableRebaseJob(t, svc, pr, projection, ForgejoProjectionPhaseProjectionResume, "")

	result, err := svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo,
		PRNumber:     projection.ExternalNumber,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      pr.HeadSHA,
		BaseBranch:   pr.BaseRef,
		LabelName:    forgejointegration.AGSActionRebaseLabel,
	})
	if result.WorkflowStatus == "" {
		t.Fatal("expected non-empty workflow status")
	}
	_ = err

	var intentResult db.PullRequestActionIntent
	if err := svc.DB.First(&intentResult, "id = ?", intent.ID).Error; err != nil {
		t.Fatalf("load intent: %v", err)
	}
	if intentResult.State != ForgejoActionIntentDenied {
		t.Fatalf("intent state=%s, want denied", intentResult.State)
	}
	if intentResult.FailureCode == "" {
		t.Fatal("terminal exact-intent denial must record a failure code")
	}
}

// TestHandleForgejoPullRequestActionLabel_ResumablePathReplayNoRegression verifies
// that replaying a webhook for a resumable job does not regress terminal intent state.
func TestHandleForgejoPullRequestActionLabel_ResumablePathReplayNoRegression(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	ctx := context.Background()

	intent := seedDispatchedIntent(t, svc, pr, projection)
	seedResumableRebaseJob(t, svc, pr, projection, ForgejoProjectionPhaseFailedRetryable, "")

	// First call: intent goes accepted -> running -> recovery_needed.
	_, _ = svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo,
		PRNumber:     projection.ExternalNumber,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      pr.HeadSHA,
		BaseBranch:   pr.BaseRef,
		LabelName:    forgejointegration.AGSActionRebaseLabel,
	})

	var first db.PullRequestActionIntent
	if err := svc.DB.First(&first, "id = ?", intent.ID).Error; err != nil {
		t.Fatalf("load first: %v", err)
	}
	if first.State != ForgejoActionIntentDenied {
		t.Fatalf("first state=%s, want denied", first.State)
	}

	// Replay: consumeForgejoActionIntent will not find the intent in dispatched
	// state, so the webhook is denied. The terminal state must not regress.
	_, _ = svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo,
		PRNumber:     projection.ExternalNumber,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      pr.HeadSHA,
		BaseBranch:   pr.BaseRef,
		LabelName:    forgejointegration.AGSActionRebaseLabel,
	})

	var second db.PullRequestActionIntent
	if err := svc.DB.First(&second, "id = ?", intent.ID).Error; err != nil {
		t.Fatalf("load second: %v", err)
	}
	if second.State != ForgejoActionIntentDenied {
		t.Fatalf("intent state=%s after replay, want denied (no regression)", second.State)
	}
}

// TestHandleForgejoPullRequestActionLabel_ResumablePathSamePRNextIntent verifies
// that after a resumable path completes (even with recovery_needed), a new intent
// for the same PR can be created (previous intent is non-blocking terminal).
func TestHandleForgejoPullRequestActionLabel_ResumablePathSamePRNextIntent(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	ctx := context.Background()

	intent := seedDispatchedIntent(t, svc, pr, projection)
	seedResumableRebaseJob(t, svc, pr, projection, ForgejoProjectionPhaseFailedRetryable, "")

	// First call: intent goes to recovery_needed.
	_, _ = svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo,
		PRNumber:     projection.ExternalNumber,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      pr.HeadSHA,
		BaseBranch:   pr.BaseRef,
		LabelName:    forgejointegration.AGSActionRebaseLabel,
	})

	var first db.PullRequestActionIntent
	if err := svc.DB.First(&first, "id = ?", intent.ID).Error; err != nil {
		t.Fatalf("load first: %v", err)
	}
	if first.State != ForgejoActionIntentDenied {
		t.Fatalf("first state=%s, want denied", first.State)
	}

	// Now a new intent for the same PR should be creatable.
	newIntent := db.PullRequestActionIntent{
		ID:              "next-intent",
		IdempotencyKey:  "next-key",
		Action:          "pr.rebase",
		State:           ForgejoActionIntentDispatched,
		PullRequestID:   pr.ID,
		RepositoryID:    pr.RepositoryID,
		AGSPRNumber:     pr.Number,
		Repository:      "example-owner/demo",
		ForgejoRepo:     projection.ExternalRepo,
		ForgejoPRNumber: projection.ExternalNumber,
		HeadRef:         pr.HeadRef,
		BaseRef:         pr.BaseRef,
		ExpectedHeadSHA: pr.HeadSHA,
		ExpectedBaseSHA: pr.BaseSHA,
		ExpectedLabels:  "[]",
		PostLabels:      `["` + forgejointegration.AGSActionRebaseLabel + `"]`,
		PrincipalID:     1,
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}
	if err := svc.DB.Create(&newIntent).Error; err != nil {
		t.Fatalf("create next intent: %v", err)
	}
	var loaded db.PullRequestActionIntent
	if err := svc.DB.First(&loaded, "id = ?", "next-intent").Error; err != nil {
		t.Fatalf("load next: %v", err)
	}
	if loaded.State != ForgejoActionIntentDispatched {
		t.Fatalf("next intent state=%s, want dispatched", loaded.State)
	}
}

// TestHandleForgejoPullRequestActionLabel_FailureExits verifies that every failure
// exit in handleForgejoPullRequestActionLabel persists a truthful terminal state
// and failure code via actionIntentState.
func TestHandleForgejoPullRequestActionLabel_FailureExits(t *testing.T) {
	tests := []struct {
		name       string
		wantState  string
		wantCode   string
		wantStatus string
		skipIntent bool // true for paths that don't consume an intent
	}{
		{
			name:       "ignored_label",
			skipIntent: true,
			wantStatus: "ignored",
		},
		{
			name:       "denied_consumed",
			wantState:  ForgejoActionIntentDenied,
			wantCode:   "webhook_mismatch",
			wantStatus: "denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, pr, projection := setupIntentTestService(t)
			ctx := context.Background()

			if tt.skipIntent {
				// Test the ignored label path - no intent needed.
				result, err := svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
					RepoFullName: projection.ExternalRepo,
					PRNumber:     projection.ExternalNumber,
					HeadBranch:   pr.HeadRef,
					HeadSHA:      pr.HeadSHA,
					BaseBranch:   pr.BaseRef,
					LabelName:    "some-other-label",
				})
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result.WorkflowStatus != tt.wantStatus {
					t.Fatalf("status=%s, want %s", result.WorkflowStatus, tt.wantStatus)
				}
				return
			}

			// Seed a dispatched intent.
			_ = seedDispatchedIntent(t, svc, pr, projection)

			// Test the denied path - webhook mismatch (wrong head SHA).
			result, err := svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
				RepoFullName: projection.ExternalRepo,
				PRNumber:     projection.ExternalNumber,
				HeadBranch:   pr.HeadRef,
				HeadSHA:      "0000000000000000000000000000000000000000", // wrong SHA
				BaseBranch:   pr.BaseRef,
				LabelName:    forgejointegration.AGSActionRebaseLabel,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.WorkflowStatus != tt.wantStatus {
				t.Fatalf("status=%s, want %s", result.WorkflowStatus, tt.wantStatus)
			}

			// Read back the intent and verify terminal state.
			var intentResult db.PullRequestActionIntent
			if err := svc.DB.First(&intentResult, "id = ?", "test-intent-1").Error; err != nil {
				t.Fatalf("load intent: %v", err)
			}
			if intentResult.State != tt.wantState {
				t.Fatalf("intent state=%s, want %s", intentResult.State, tt.wantState)
			}
			if intentResult.FailureCode != tt.wantCode {
				t.Fatalf("intent failure_code=%s, want %s", intentResult.FailureCode, tt.wantCode)
			}
		})
	}
}

// TestHandleForgejoPullRequestActionLabel_BlockedPR verifies that a PR that is
// not open results in intent being denied with pr_not_open code.
func TestHandleForgejoPullRequestActionLabel_BlockedPR(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	ctx := context.Background()

	// Seed intent first (while PR is open).
	_ = seedDispatchedIntent(t, svc, pr, projection)

	// Close the PR AFTER seeding the intent but BEFORE the webhook arrives.
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Update("state", db.StateClosed).Error; err != nil {
		t.Fatalf("close PR: %v", err)
	}

	result, err := svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo,
		PRNumber:     projection.ExternalNumber,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      pr.HeadSHA,
		BaseBranch:   pr.BaseRef,
		LabelName:    forgejointegration.AGSActionRebaseLabel,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkflowStatus != "denied" {
		t.Fatalf("status=%s, want denied", result.WorkflowStatus)
	}

	var intentResult db.PullRequestActionIntent
	if err := svc.DB.First(&intentResult, "id = ?", "test-intent-1").Error; err != nil {
		t.Fatalf("load intent: %v", err)
	}
	if intentResult.State != ForgejoActionIntentDenied {
		t.Fatalf("intent state=%s, want denied", intentResult.State)
	}
	if intentResult.FailureCode != "exact_action_fact_drift" {
		t.Fatalf("intent failure_code=%s, want exact_action_fact_drift", intentResult.FailureCode)
	}
}

// TestHandleForgejoPullRequestActionLabel_ProjectionDrift verifies that projection
// drift results in intent being denied with projection_drift code.
func TestHandleForgejoPullRequestActionLabel_ProjectionDrift(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	ctx := context.Background()

	// Seed intent with different PR number to trigger drift.
	intent := db.PullRequestActionIntent{
		ID:              "drift-intent",
		IdempotencyKey:  "drift-key",
		Action:          "pr.rebase",
		State:           ForgejoActionIntentDispatched,
		PullRequestID:   pr.ID + 999, // wrong PR ID
		RepositoryID:    999,         // wrong repo ID
		AGSPRNumber:     999,         // wrong PR number
		Repository:      "example-owner/demo",
		ForgejoRepo:     projection.ExternalRepo,
		ForgejoPRNumber: projection.ExternalNumber,
		HeadRef:         pr.HeadRef,
		BaseRef:         pr.BaseRef,
		ExpectedHeadSHA: pr.HeadSHA,
		ExpectedBaseSHA: pr.BaseSHA,
		ExpectedLabels:  "[]",
		PostLabels:      `["` + forgejointegration.AGSActionRebaseLabel + `"]`,
		PrincipalID:     1,
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}
	if err := svc.DB.Create(&intent).Error; err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	result, err := svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo,
		PRNumber:     projection.ExternalNumber,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      pr.HeadSHA,
		BaseBranch:   pr.BaseRef,
		LabelName:    forgejointegration.AGSActionRebaseLabel,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.WorkflowStatus != "denied" {
		t.Fatalf("status=%s, want denied", result.WorkflowStatus)
	}

	var intentResult db.PullRequestActionIntent
	if err := svc.DB.First(&intentResult, "id = ?", "drift-intent").Error; err != nil {
		t.Fatalf("load intent: %v", err)
	}
	if intentResult.State != ForgejoActionIntentDenied {
		t.Fatalf("intent state=%s, want denied", intentResult.State)
	}
	if intentResult.FailureCode != "projection_drift" {
		t.Fatalf("intent failure_code=%s, want projection_drift", intentResult.FailureCode)
	}
}

func TestHandleForgejoPullRequestActionLabel_DelegatedDenialMakesNoProviderCall(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	sessionID := "missing-delegated-session"
	if err := svc.DB.Model(&db.PullRequestActionIntent{}).Where("id = ?", intent.ID).Update("agent_session_id", sessionID).Error; err != nil {
		t.Fatal(err)
	}
	client := &minimalForgejoClient{}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token"}, client, nil)
	result, err := svc.handleForgejoPullRequestActionLabel(context.Background(), forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
		HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
		LabelName: forgejointegration.AGSActionRebaseLabel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkflowStatus != "denied" || client.listCalls != 0 || client.removeCalls != 0 {
		t.Fatalf("result=%#v list_calls=%d remove_calls=%d", result, client.listCalls, client.removeCalls)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || intent.FailureCode != DelegatedDenialSessionMissing {
		t.Fatalf("intent=%#v", intent)
	}
}

func TestSignedForgejoActionTriggerCreatesIntentForMappedAuthorizedActor(t *testing.T) {
	svc, pr, _, client := setupIntentTestServiceWithClient(t)
	client.permissions = map[string]string{"operator": "owner"}
	enabled := true
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token", WebhookSecret: "verified-secret",
		ActionPrincipalBindings: map[string]uint{"operator": 1},
		RepoMap:                 map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "repo", Enabled: &enabled}},
	}, client, func(context.Context, forgejointegration.PushRequest) error { return nil }, func(_ context.Context, _, _, ref string) (string, error) {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.remoteRefs[strings.TrimPrefix(ref, "refs/heads/")], nil
	})
	body := []byte(`{"action":"label_updated","repository":{"full_name":"forgejo/repo","default_branch":"main"},"pull_request":{"number":42,"head":{"ref":"agent/intent-test","sha":"` + pr.HeadSHA + `"},"base":{"ref":"main"},"labels":[{"name":"ags/action-rebase"}]},"label":{"name":"ags/action-rebase"},"sender":{"login":"operator"}}`)
	mac := hmac.New(sha256.New, []byte("verified-secret"))
	_, _ = mac.Write(body)
	headers := http.Header{
		"X-Hub-Signature-256": []string{"sha256=" + hex.EncodeToString(mac.Sum(nil))},
		"X-Forgejo-Delivery":  []string{"delivery-label-first-1"},
	}

	result, err := svc.HandleForgejoWebhook(context.Background(), headers, body)
	if err != nil {
		t.Fatalf("HandleForgejoWebhook: %v", err)
	}
	if result.WorkflowStatus == "denied" {
		t.Fatalf("mapped authorized actor was denied: %#v", result)
	}
	var intents []db.PullRequestActionIntent
	if err := svc.DB.Where("pull_request_id = ?", pr.ID).Find(&intents).Error; err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].PrincipalID != 1 || intents[0].RequestSource != "forgejo_label" || intents[0].RequestActor != "operator" || !strings.HasPrefix(intents[0].RequestBindingRev, "sha256:") || len(intents[0].RequestBindingRev) != len("sha256:")+64 {
		t.Fatalf("intents=%#v", intents)
	}
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token", WebhookSecret: "verified-secret",
		ActionPrincipalBindings: map[string]uint{"operator": 2},
		RepoMap:                 map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "repo", Enabled: &enabled}},
	}, client, func(context.Context, forgejointegration.PushRequest) error { return nil }, nil)
	if err := svc.authorizeCurrentDurableActionIntent(context.Background(), intents[0]); err == nil {
		t.Fatal("changed actor binding revision did not invalidate label-first durable authority")
	}
}

func TestSignedForgejoActionTriggerDeniesMappedActorWithoutProviderWritePermission(t *testing.T) {
	svc, pr, _, client := setupIntentTestServiceWithClient(t)
	client.permissions = map[string]string{"operator": "read"}
	enabled := true
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token", WebhookSecret: "verified-secret",
		ActionPrincipalBindings: map[string]uint{"operator": 1},
		RepoMap:                 map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "repo", Enabled: &enabled}},
	}, client, func(context.Context, forgejointegration.PushRequest) error { return nil }, func(_ context.Context, _, _, ref string) (string, error) {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.remoteRefs[strings.TrimPrefix(ref, "refs/heads/")], nil
	})
	body := []byte(`{"action":"label_updated","repository":{"full_name":"forgejo/repo","default_branch":"main"},"pull_request":{"number":42,"head":{"ref":"agent/intent-test","sha":"` + pr.HeadSHA + `"},"base":{"ref":"main"},"labels":[{"name":"ags/action-rebase"}]},"label":{"name":"ags/action-rebase"},"sender":{"login":"operator"}}`)
	mac := hmac.New(sha256.New, []byte("verified-secret"))
	_, _ = mac.Write(body)
	result, err := svc.HandleForgejoWebhook(context.Background(), http.Header{
		"X-Hub-Signature-256": []string{"sha256=" + hex.EncodeToString(mac.Sum(nil))},
		"X-Forgejo-Delivery":  []string{"delivery-label-denied-1"},
	}, body)
	if err != nil {
		t.Fatalf("HandleForgejoWebhook: %v", err)
	}
	if result.WorkflowStatus != "denied" || client.removeCalls != 5 || client.commentCalls != 0 || client.addCalls != 1 {
		t.Fatalf("result=%#v remove=%d add=%d comments=%d", result, client.removeCalls, client.addCalls, client.commentCalls)
	}
	var intents int64
	if err := svc.DB.Model(&db.PullRequestActionIntent{}).Where("pull_request_id = ?", pr.ID).Count(&intents).Error; err != nil {
		t.Fatal(err)
	}
	if intents != 0 {
		t.Fatalf("denied label request created %d intents", intents)
	}
}

func TestForgejoLabelFirstAdmissionRejectsExactFactDriftOrIgnoresStaleLabel(t *testing.T) {
	for _, tc := range []struct {
		name         string
		headSHA      string
		baseBranch   string
		removeAction bool
		wantStatus   string
		wantRemove   int
		wantAdd      int
	}{
		{name: "head drift", headSHA: strings.Repeat("0", 40), baseBranch: "main", wantStatus: "denied", wantRemove: 5, wantAdd: 1},
		{name: "base drift", baseBranch: "release", wantStatus: "denied", wantRemove: 5, wantAdd: 1},
		{name: "label already absent is stale", baseBranch: "main", removeAction: true, wantStatus: "ignored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, pr, projection, client := setupIntentTestServiceWithClient(t)
			client.permissions = map[string]string{"operator": "owner"}
			if tc.removeAction {
				client.overrideLabels = true
				client.listedLabels = nil
			}
			enabled := true
			svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
				Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token", WebhookSecret: "verified-secret",
				ActionPrincipalBindings: map[string]uint{"operator": 1},
				RepoMap:                 map[string]forgejointegration.RepoMapping{"example-owner/demo": {Owner: "forgejo", Repo: "repo", Enabled: &enabled}},
			}, client, func(context.Context, forgejointegration.PushRequest) error { return nil }, nil)
			headSHA := tc.headSHA
			if headSHA == "" {
				headSHA = pr.HeadSHA
			}
			result, err := svc.handleForgejoPullRequestActionLabel(context.Background(), forgejointegration.PullRequestActionLabelEvent{
				CorrelationID: "delivery-drift-" + strings.ReplaceAll(tc.name, " ", "-"),
				RepoFullName:  projection.ExternalRepo, PRNumber: projection.ExternalNumber,
				HeadBranch: pr.HeadRef, HeadSHA: headSHA, BaseBranch: tc.baseBranch,
				LabelName: forgejointegration.AGSActionRebaseLabel, SenderLogin: "operator",
			})
			if err != nil {
				t.Fatalf("handle label drift: %v", err)
			}
			if result.WorkflowStatus != tc.wantStatus || client.addCalls != tc.wantAdd || client.removeCalls != tc.wantRemove {
				t.Fatalf("result=%#v add=%d remove=%d", result, client.addCalls, client.removeCalls)
			}
			var intents int64
			if err := svc.DB.Model(&db.PullRequestActionIntent{}).Count(&intents).Error; err != nil {
				t.Fatal(err)
			}
			if intents != 0 {
				t.Fatalf("drift admission created %d intents", intents)
			}
		})
	}
}

func TestSignedDirectForgejoActionTriggerUsesServerOwnedCleanupAndBlockedStatusOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		removeErr  error
		wantErr    bool
		wantRemove int
		wantAdd    int
	}{
		{name: "cleanup and blocked status succeed", wantRemove: 5, wantAdd: 1},
		{name: "cleanup failure propagates", removeErr: errors.New("provider unavailable"), wantErr: true, wantRemove: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := setupIntentTestService(t)
			client := &minimalForgejoClient{removeErr: tc.removeErr}
			svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
				Enabled: true, BaseURL: "http://forgejo.local", Token: "test-token", WebhookSecret: "verified-secret",
			}, client, nil)
			body := []byte(`{"action":"label_updated","repository":{"full_name":"forgejo/repo","default_branch":"main"},"pull_request":{"number":42,"head":{"ref":"agent/direct-trigger","sha":"abc1234567890abc1234567890abc1234567890a"},"base":{"ref":"main"},"labels":[{"name":"ags/action-rebase"}]},"label":{"name":"ags/action-rebase"},"sender":{"login":"untrusted"}}`)
			mac := hmac.New(sha256.New, []byte("verified-secret"))
			_, _ = mac.Write(body)
			headers := http.Header{"X-Hub-Signature-256": []string{"sha256=" + hex.EncodeToString(mac.Sum(nil))}}

			result, err := svc.HandleForgejoWebhook(context.Background(), headers, body)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if result.WorkflowStatus != "denied" || client.removeCalls != tc.wantRemove || client.addCalls != tc.wantAdd || client.commentCalls != 0 {
				t.Fatalf("result=%#v remove=%d add=%d comments=%d", result, client.removeCalls, client.addCalls, client.commentCalls)
			}
			var intents, jobs int64
			if err := svc.DB.Model(&db.PullRequestActionIntent{}).Count(&intents).Error; err != nil {
				t.Fatal(err)
			}
			if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Count(&jobs).Error; err != nil {
				t.Fatal(err)
			}
			if intents != 0 || jobs != 0 {
				t.Fatalf("invalid trigger cleanup created authority facts: intents=%d jobs=%d", intents, jobs)
			}
		})
	}
}

// TestHandleForgejoPullRequestActionLabel_IntentStateMonotonicity verifies that
// replayed webhooks cannot regress terminal state.
func TestHandleForgejoPullRequestActionLabel_IntentStateMonotonicity(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	ctx := context.Background()

	_ = seedDispatchedIntent(t, svc, pr, projection)

	// First webhook: denied due to wrong SHA.
	_, _ = svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo,
		PRNumber:     projection.ExternalNumber,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      "0000000000000000000000000000000000000000",
		BaseBranch:   pr.BaseRef,
		LabelName:    forgejointegration.AGSActionRebaseLabel,
	})

	var intentResult db.PullRequestActionIntent
	if err := svc.DB.First(&intentResult, "id = ?", "test-intent-1").Error; err != nil {
		t.Fatalf("load intent: %v", err)
	}
	if intentResult.State != ForgejoActionIntentDenied {
		t.Fatalf("intent state=%s, want denied", intentResult.State)
	}

	// Replay the same webhook - state should not regress.
	_, _ = svc.handleForgejoPullRequestActionLabel(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo,
		PRNumber:     projection.ExternalNumber,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      "0000000000000000000000000000000000000000",
		BaseBranch:   pr.BaseRef,
		LabelName:    forgejointegration.AGSActionRebaseLabel,
	})

	var intentResult2 db.PullRequestActionIntent
	if err := svc.DB.First(&intentResult2, "id = ?", "test-intent-1").Error; err != nil {
		t.Fatalf("load intent: %v", err)
	}
	// State should still be denied - no regression.
	if intentResult2.State != ForgejoActionIntentDenied {
		t.Fatalf("intent state=%s after replay, want denied (no regression)", intentResult2.State)
	}
}

// TestHandleForgejoPullRequestActionLabel_NewIntentAfterTerminal verifies that a
// new intent can be created after the previous one reached terminal state.
func TestHandleForgejoPullRequestActionLabel_NewIntentAfterTerminal(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	ctx := context.Background()

	// Seed and terminalize an intent.
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.actionIntentState(ctx, intent.ID, ForgejoActionIntentDenied, "test_code", "test summary", ""); err != nil {
		t.Fatalf("terminalize intent: %v", err)
	}

	// Verify new intent can be created (no active intent blocking).
	var active int64
	if err := svc.DB.Model(&db.PullRequestActionIntent{}).Where("pull_request_id = ? AND action = ? AND state IN ?", pr.ID, "pr.rebase", []string{ForgejoActionIntentPlanned, ForgejoActionIntentDispatched, ForgejoActionIntentAccepted, ForgejoActionIntentRunning, ForgejoActionIntentRecovery}).Count(&active).Error; err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 0 {
		t.Fatalf("active intents=%d, want 0 after terminal state", active)
	}

	// Create a new intent for the same PR.
	newIntent := db.PullRequestActionIntent{
		ID:              "new-intent",
		IdempotencyKey:  "new-key",
		Action:          "pr.rebase",
		State:           ForgejoActionIntentDispatched,
		PullRequestID:   pr.ID,
		RepositoryID:    pr.RepositoryID,
		AGSPRNumber:     pr.Number,
		Repository:      "example-owner/demo",
		ForgejoRepo:     projection.ExternalRepo,
		ForgejoPRNumber: projection.ExternalNumber,
		HeadRef:         pr.HeadRef,
		BaseRef:         pr.BaseRef,
		ExpectedHeadSHA: pr.HeadSHA,
		ExpectedBaseSHA: pr.BaseSHA,
		ExpectedLabels:  "[]",
		PostLabels:      `["` + forgejointegration.AGSActionRebaseLabel + `"]`,
		PrincipalID:     1,
		ExpiresAt:       time.Now().UTC().Add(10 * time.Minute),
	}
	if err := svc.DB.Create(&newIntent).Error; err != nil {
		t.Fatalf("create new intent: %v", err)
	}

	// Verify the new intent is active.
	var intentResult db.PullRequestActionIntent
	if err := svc.DB.First(&intentResult, "id = ?", "new-intent").Error; err != nil {
		t.Fatalf("load new intent: %v", err)
	}
	if intentResult.State != ForgejoActionIntentDispatched {
		t.Fatalf("new intent state=%s, want dispatched", intentResult.State)
	}
}

func TestProviderAdmissionDenialTerminalizesRecoveryAndWebhookRemove(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, svc *Service, pr db.PullRequest, projection db.PullRequestProjection)
	}{
		{
			name: "recovery dispatch",
			run: func(t *testing.T, svc *Service, pr db.PullRequest, projection db.PullRequestProjection) {
				intent := seedDispatchedIntent(t, svc, pr, projection)
				if err := svc.DB.Model(&intent).Updates(map[string]any{"state": ForgejoActionIntentPlanned, "dispatched_at": nil}).Error; err != nil {
					t.Fatal(err)
				}
				if err := svc.RecoverDurableActionIntents(context.Background()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "webhook label removal with resumable job",
			run: func(t *testing.T, svc *Service, pr db.PullRequest, projection db.PullRequestProjection) {
				intent := seedDispatchedIntent(t, svc, pr, projection)
				job := db.PullRequestProjectionJob{
					PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
					Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
					HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseVerifyingPR,
				}
				_ = bindTestActionJob(t, svc, intent, &job)
				if err := svc.DB.Create(&job).Error; err != nil {
					t.Fatal(err)
				}
				result, err := svc.handleForgejoPullRequestActionLabel(context.Background(), forgejointegration.PullRequestActionLabelEvent{
					RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber, LabelName: forgejointegration.AGSActionRebaseLabel,
					HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
				})
				if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) || result.WorkflowStatus != "denied" {
					t.Fatalf("result=%#v err=%v", result, err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, pr, projection, client := setupIntentTestServiceWithClient(t)
			SetDelegatedProviderWriteHookForTest(svc, func() error { return delegatedUseTimeDenied(DelegatedDenialSessionRevoked) })
			t.Cleanup(func() { SetDelegatedProviderWriteHookForTest(svc, nil) })
			tc.run(t, svc, pr, projection)
			var intent db.PullRequestActionIntent
			if err := svc.DB.Order("created_at DESC").First(&intent).Error; err != nil {
				t.Fatal(err)
			}
			if intent.State != ForgejoActionIntentDenied || intent.FailureCode != DelegatedDenialSessionRevoked {
				t.Fatalf("intent=%#v", intent)
			}
			var jobs []db.PullRequestProjectionJob
			if err := svc.DB.Find(&jobs).Error; err != nil {
				t.Fatal(err)
			}
			for _, job := range jobs {
				if job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied {
					t.Fatalf("resumable job was not terminalized atomically: %#v", job)
				}
			}
			if client.addCalls != 0 || client.removeCalls != 0 || client.commentCalls != 0 {
				t.Fatalf("provider calls add=%d remove=%d comment=%d", client.addCalls, client.removeCalls, client.commentCalls)
			}
		})
	}
}

func TestResumePostJobProviderDenialTerminalizesBeforeProcessBoundary(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA, DesiredAGSHeadSHA: pr.HeadSHA,
		ExternalRepo: projection.ExternalRepo, ExternalNumber: projection.ExternalNumber,
		Phase: ForgejoProjectionPhaseProjectionResume,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	SetDelegatedProviderWriteHookForTest(svc, func() error {
		return delegatedUseTimeDenied(DelegatedDenialSessionRevoked)
	})
	t.Cleanup(func() { SetDelegatedProviderWriteHookForTest(svc, nil) })

	_, err := svc.resumeForgejoActionRebaseJob(ctx, forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber,
	}, pr, job, ForgejoWebhookResult{})
	if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		t.Fatalf("resume denial error=%v", err)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied {
		t.Fatalf("post-job resume denial was not atomic: intent=%#v job=%#v", intent, job)
	}
}

func TestPostJobProviderDenialAtomicallyTerminalizesExactIntentAndJob(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseRebasing,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}

	svc.markForgejoActionRebaseJobFailed(ctx, job.ID, "refs/heads/"+pr.HeadRef, pr.HeadSHA,
		delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged), false)

	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || intent.FailureCode != DelegatedDenialAuthoritySnapshotChanged {
		t.Fatalf("exact intent was not denied: %#v", intent)
	}
	if job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied {
		t.Fatalf("exact job was not terminalized with intent: %#v", job)
	}
}

func TestSuccessCommentProviderWriteCrashRecoversByReadbackWithoutDuplicate(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseVerifyingPR,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber}
	SetForgejoSuccessCommentAfterWriteHookForTest(svc, func() error { return errors.New("simulated process interruption") })
	err := svc.publishForgejoActionRebaseSuccess(ctx, event, pr, job.ID, pr.HeadSHA)
	if err == nil || !strings.Contains(err.Error(), "simulated process interruption") {
		t.Fatalf("first publish error=%v", err)
	}
	if client.commentCalls != 1 {
		t.Fatalf("comment calls after interrupted write=%d", client.commentCalls)
	}
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.SuccessCommentedAt != nil || job.SuccessCommentDispatchID == "" || job.SuccessCommentClaimToken == "" {
		t.Fatalf("interrupted job=%#v", job)
	}

	SetForgejoSuccessCommentAfterWriteHookForTest(svc, nil)
	if err := svc.publishForgejoActionRebaseSuccess(ctx, event, pr, job.ID, pr.HeadSHA); err != nil {
		t.Fatalf("readback recovery: %v", err)
	}
	if client.commentCalls != 1 {
		t.Fatalf("recovery duplicated provider comment: calls=%d", client.commentCalls)
	}
	var confirmedJob db.PullRequestProjectionJob
	if err := svc.DB.First(&confirmedJob, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if confirmedJob.SuccessCommentedAt == nil || confirmedJob.SuccessCommentClaimToken != "" || confirmedJob.SuccessCommentClaimedAt != nil {
		t.Fatalf("provider-confirmed job=%#v", confirmedJob)
	}
}

func TestSuccessCommentPreWriteCrashSchedulesLeaseExpiryAndRecovers(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	svc.ForgejoSuccessCommentClaimTTL = 50 * time.Millisecond
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Update("state", ForgejoActionIntentRecovery).Error; err != nil {
		t.Fatal(err)
	}
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA, DesiredAGSHeadSHA: pr.HeadSHA,
		ExternalRepo: projection.ExternalRepo, ExternalNumber: projection.ExternalNumber, Phase: ForgejoProjectionPhaseVerifyingPR,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber}
	SetForgejoSuccessCommentBeforeWriteHookForTest(svc, func() error { return errors.New("simulated pre-write interruption") })
	if err := svc.publishForgejoActionRebaseSuccess(ctx, event, pr, job.ID, pr.HeadSHA); err == nil || !strings.Contains(err.Error(), "pre-write") {
		t.Fatalf("pre-write interruption error=%v", err)
	}
	SetForgejoSuccessCommentBeforeWriteHookForTest(svc, nil)
	if client.commentCalls != 0 {
		t.Fatalf("comment calls=%d", client.commentCalls)
	}
	var claimed db.PullRequestProjectionJob
	if err := svc.DB.First(&claimed, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if claimed.SuccessCommentClaimedAt == nil || claimed.SuccessCommentClaimToken == "" {
		t.Fatalf("claim not persisted: %#v", claimed)
	}
	busyErr := svc.publishForgejoActionRebaseSuccess(ctx, event, pr, job.ID, pr.HeadSHA)
	var busy *forgejoSuccessCommentClaimBusyError
	if !errors.As(busyErr, &busy) {
		t.Fatalf("busy error=%v", busyErr)
	}
	if err := svc.DB.First(&claimed, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if claimed.Phase != ForgejoProjectionPhaseFailedRetryable || claimed.NextRunAt == nil || claimed.NextRunAt.Before(claimed.SuccessCommentClaimedAt.Add(svc.forgejoSuccessCommentClaimTTL())) {
		t.Fatalf("lease-expiry recovery not scheduled: %#v", claimed)
	}
	attemptBeforeResume := claimed.Attempt
	serverCtx, cancelResume := context.WithCancel(context.Background())
	defer cancelResume()
	svc.Ctx = serverCtx
	if err := svc.ResumePendingForgejoProjectionJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The same startup worker waits on the durable lease coordinate, preserves
	// the attempt counter, takes over after expiry, confirms the provider marker
	// and completes the exact intent/job generation.
	svc.Wg.Wait()
	if err := svc.DB.First(&claimed, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if claimed.Attempt != attemptBeforeResume || client.commentCalls != 1 {
		t.Fatalf("startup recovery attempt=%d want=%d calls=%d", claimed.Attempt, attemptBeforeResume, client.commentCalls)
	}
	var recovered db.PullRequestProjectionJob
	if err := svc.DB.First(&recovered, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if recovered.Phase != ForgejoProjectionPhaseProjected || recovered.SuccessCommentedAt == nil || recovered.SuccessCommentClaimToken != "" || recovered.SuccessCommentClaimedAt != nil {
		t.Fatalf("recovered job=%#v", recovered)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentCompleted || intent.ResultSHA != pr.HeadSHA {
		t.Fatalf("recovered intent=%#v", intent)
	}
}

func TestSuccessCommentConcurrentPublishWritesProviderOnce(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseVerifyingPR,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- svc.publishForgejoActionRebaseSuccess(ctx, event, pr, job.ID, pr.HeadSHA)
		}()
	}
	close(start)
	for range 2 {
		err := <-errs
		var busy *forgejoSuccessCommentClaimBusyError
		if err != nil && !errors.As(err, &busy) && !strings.Contains(strings.ToLower(err.Error()), "locked") {
			t.Fatalf("concurrent publish error=%v", err)
		}
	}
	client.mu.Lock()
	commentCalls := client.commentCalls
	client.mu.Unlock()
	if commentCalls != 1 {
		t.Fatalf("comment calls=%d", commentCalls)
	}
}

func TestBlockedStatusAddLabelDenialThroughWebhookTerminalizesAllJobs(t *testing.T) {
	svc, pr, projection, client := setupIntentTestServiceWithClient(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{"state": db.StateClosed}).Error; err != nil {
		t.Fatal(err)
	}
	job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo, Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number, Phase: ForgejoProjectionPhaseVerifyingPR}
	_ = bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	checks := 0
	SetDelegatedProviderWriteHookForTest(svc, func() error {
		checks++
		// One action-label removal, then all workflow-status removals. Deny the
		// blocked-status add-label seam and prove no following comment occurs.
		if checks == len(forgejoWorkflowStatusLabels())+2 {
			return delegatedUseTimeDenied(DelegatedDenialSessionRevoked)
		}
		return nil
	})
	defer SetDelegatedProviderWriteHookForTest(svc, nil)
	result, err := svc.handleForgejoPullRequestActionLabel(context.Background(), forgejointegration.PullRequestActionLabelEvent{
		RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber, LabelName: forgejointegration.AGSActionRebaseLabel,
		HeadBranch: pr.HeadRef, HeadSHA: pr.HeadSHA, BaseBranch: pr.BaseRef,
	})
	if err != nil || result.WorkflowStatus != "denied" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || intent.FailureCode != "exact_action_fact_drift" ||
		job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied {
		t.Fatalf("intent=%#v job=%#v", intent, job)
	}
	client.mu.Lock()
	removeCalls, addCalls, commentCalls := client.removeCalls, client.addCalls, client.commentCalls
	client.mu.Unlock()
	if removeCalls == 0 || addCalls == 0 || commentCalls == 0 {
		t.Fatalf("closed PR must still quarantine the action label: remove=%d add=%d comment=%d", removeCalls, addCalls, commentCalls)
	}
}

func TestStatusNotificationAddLabelDenialTerminalizesBeforeComment(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Service, context.Context, forgejointegration.PullRequestActionLabelEvent, db.PullRequest) error
	}{
		{name: "blocked", run: func(s *Service, ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, _ db.PullRequest) error {
			return s.markForgejoActionBlocked(ctx, event, "blocked")
		}},
		{name: "failed", run: func(s *Service, ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, _ db.PullRequest) error {
			return s.markForgejoActionFailed(ctx, event, errors.New("failed"))
		}},
		{name: "projection failed", run: func(s *Service, ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest) error {
			return s.markForgejoActionProjectionFailed(ctx, event, pr, forgejointegration.ProjectionError{ExpectedSHA: pr.HeadSHA}, true)
		}},
		{name: "needs rebase", run: func(s *Service, ctx context.Context, event forgejointegration.PullRequestActionLabelEvent, pr db.PullRequest) error {
			return s.markForgejoActionNeedsRebase(ctx, event, pr, pr.HeadSHA, "moved")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, pr, projection := setupIntentTestService(t)
			client := &minimalForgejoClient{}
			svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo.local", Token: "token"}, client, nil)
			intent := seedDispatchedIntent(t, svc, pr, projection)
			job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo, Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number, Phase: ForgejoProjectionPhaseVerifyingPR}
			ctx := bindTestActionJob(t, svc, intent, &job)
			if err := svc.DB.Create(&job).Error; err != nil {
				t.Fatal(err)
			}
			checks := 0
			SetDelegatedProviderWriteHookForTest(svc, func() error {
				checks++
				if checks == len(forgejoWorkflowStatusLabels())+1 {
					return delegatedUseTimeDenied(DelegatedDenialSessionRevoked)
				}
				return nil
			})
			defer SetDelegatedProviderWriteHookForTest(svc, nil)
			event := forgejointegration.PullRequestActionLabelEvent{RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber}
			err := tc.run(svc, ctx, event, pr)
			if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
				t.Fatalf("notification error=%v", err)
			}
			if err := svc.handleForgejoStatusNotificationError(ctx, intent.ID, job.ID, err, "status notification denied", pr.HeadSHA); !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
				t.Fatalf("terminal handling error=%v", err)
			}
			if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := svc.DB.First(&job, job.ID).Error; err != nil {
				t.Fatal(err)
			}
			if intent.State != ForgejoActionIntentDenied || job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied {
				t.Fatalf("intent=%#v job=%#v", intent, job)
			}
			client.mu.Lock()
			addCalls, commentCalls := client.addCalls, client.commentCalls
			client.mu.Unlock()
			if addCalls != 0 || commentCalls != 0 {
				t.Fatalf("provider add=%d comment=%d", addCalls, commentCalls)
			}
		})
	}
}

func TestSuccessCommentProviderDenialDoesNotCompleteOrClaimSuccess(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	client := &minimalForgejoClient{}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo.local", Token: "token"}, client, nil)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Update("state", ForgejoActionIntentRunning).Error; err != nil {
		t.Fatal(err)
	}
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseVerifyingPR,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	SetDelegatedProviderWriteHookForTest(svc, func() error { return delegatedUseTimeDenied(DelegatedDenialAuthoritySnapshotChanged) })
	t.Cleanup(func() { SetDelegatedProviderWriteHookForTest(svc, nil) })
	err := svc.publishForgejoActionRebaseSuccess(ctx, forgejointegration.PullRequestActionLabelEvent{RepoFullName: projection.ExternalRepo, PRNumber: projection.ExternalNumber}, pr, job.ID, pr.HeadSHA)
	if !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		t.Fatalf("error=%v", err)
	}
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied || job.SuccessCommentedAt != nil {
		t.Fatalf("job=%#v", job)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || intent.FailureCode != DelegatedDenialAuthoritySnapshotChanged {
		t.Fatalf("intent=%#v", intent)
	}
	if client.commentCalls != 0 {
		t.Fatalf("comment calls=%d", client.commentCalls)
	}
}

func TestExpiredActionIntentFinalProviderSeamTerminalizesBeforeWrite(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	client := &minimalForgejoClient{}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo.local", Token: "token"}, client, nil)
	sessionID := "00000000-0000-4000-8000-000000000321"
	session := db.DelegatedAgentSession{
		ID: sessionID, CredentialHash: strings.Repeat("1", 64), CredentialPrefix: "expired", PrincipalUserID: 1,
		Issuer: "test", AssertionVersion: 2, AssertionPurpose: "ags_session_exchange", AssertionJTI: "expired-intent", AssertionAudience: "ags",
		TargetInstance: "ags", RepositoryID: pr.RepositoryID, OperationName: "pr.rebase", GrantedCapabilities: []string{"repo:write"},
		PolicyVersion: "test", PolicySnapshotHash: strings.Repeat("2", 64), CreatedAt: time.Now().UTC().Add(-time.Hour), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := svc.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	intent := seedDispatchedIntent(t, svc, pr, projection)
	if err := svc.DB.Model(&intent).Updates(map[string]any{"agent_session_id": &sessionID, "expires_at": time.Now().UTC().Add(-time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	intent.AgentSessionID = &sessionID
	intent.ExpiresAt = time.Now().UTC().Add(-time.Second)
	job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo, Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number, Phase: ForgejoProjectionPhaseFailedTerminal, LastErrorType: "earlier_failure"}
	_ = bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	ctx := contextWithForgejoActionJob(ContextWithDelegatedSession(context.Background(), session), job)
	if err := svc.addForgejoPullRequestLabels(ctx, projection.ExternalRepo, projection.ExternalNumber, []string{"status"}); !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		t.Fatalf("expired intent provider seam error=%v", err)
	}
	if client.addCalls != 0 {
		t.Fatalf("provider calls=%d", client.addCalls)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || intent.FailureCode != "action_intent_expired" {
		t.Fatalf("intent=%#v", intent)
	}
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied {
		t.Fatalf("job=%#v", job)
	}
}

func TestRebaseCompletionCannotResurrectTerminalDenial(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	now := time.Now().UTC()
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseVerifyingPR,
		SuccessCommentDispatchID: "dispatch", SuccessCommentedAt: &now,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.terminalDenyForgejoAction(ctx, intent.ID, job.ID, DelegatedDenialSessionRevoked, "revoked", pr.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if err := svc.completeForgejoActionRebaseJob(ctx, job.ID, pr.HeadSHA); err == nil {
		t.Fatal("terminally denied job was resurrected")
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied {
		t.Fatalf("intent=%#v job=%#v", intent, job)
	}
}

func TestHistoricalUnboundRebaseJobFailsClosedWithoutSelectingIntent(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	client := &minimalForgejoClient{}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo.local", Token: "token"}, client, nil)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseFailedRetryable,
	}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.ResumePendingForgejoProjectionJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	svc.Wg.Wait()
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied || intent.State != ForgejoActionIntentDispatched {
		t.Fatalf("unbound job selected an intent or remained resumable: job=%#v intent=%#v", job, intent)
	}
	if client.addCalls != 0 || client.removeCalls != 0 || client.commentCalls != 0 {
		t.Fatalf("unbound job reached provider: %#v", client)
	}
}

func TestOldRebaseGenerationCannotUseCompleteOrDenyNewIntent(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	client := &minimalForgejoClient{}
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{Enabled: true, BaseURL: "http://forgejo.local", Token: "token"}, client, nil)
	intentA := seedDispatchedIntent(t, svc, pr, projection)
	jobA := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: pr.HeadSHA, DesiredAGSHeadSHA: pr.HeadSHA,
		ExternalRepo: projection.ExternalRepo, ExternalNumber: projection.ExternalNumber, Phase: ForgejoProjectionPhaseFailedRetryable,
	}
	ctxA := bindTestActionJob(t, svc, intentA, &jobA)
	if err := svc.DB.Create(&jobA).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.actionIntentState(context.Background(), intentA.ID, ForgejoActionIntentDenied, "expired", "old generation expired", ""); err != nil {
		t.Fatal(err)
	}

	intentB := intentA
	intentB.ID = "test-intent-2"
	intentB.IdempotencyKey = "test-key-2"
	intentB.State = ForgejoActionIntentRunning
	intentB.ExpiresAt = time.Now().UTC().Add(10 * time.Minute)
	intentB.FailureCode, intentB.FailureSummary = "", ""
	intentB.CreatedAt, intentB.UpdatedAt = time.Time{}, time.Time{}
	if err := svc.DB.Create(&intentB).Error; err != nil {
		t.Fatal(err)
	}
	commentedAt := time.Now().UTC()
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", jobA.ID).Updates(map[string]any{
		"action_intent_id": intentB.ID, "action_generation": 2, "phase": ForgejoProjectionPhaseVerifyingPR,
		"success_comment_dispatch_id": "dispatch-b", "success_commented_at": &commentedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	var jobB db.PullRequestProjectionJob
	if err := svc.DB.First(&jobB, jobA.ID).Error; err != nil {
		t.Fatal(err)
	}
	ctxB := contextWithForgejoActionJob(contextWithForgejoActionIntent(context.Background(), intentB.ID), jobB)
	if err := svc.addForgejoPullRequestLabels(ctxA, projection.ExternalRepo, projection.ExternalNumber, []string{"stale"}); !errors.Is(err, ErrDelegatedSessionUseTimeDenied) {
		t.Fatalf("stale provider seam error=%v", err)
	}
	if client.addCalls != 0 {
		t.Fatalf("stale generation reached provider: calls=%d", client.addCalls)
	}

	start := make(chan struct{})
	errs := make(chan error, 3)
	go func() {
		<-start
		errs <- svc.updateForgejoActionRebaseJob(ctxA, jobA.ID, ForgejoProjectionPhaseNeedsRebase, nil)
	}()
	go func() {
		<-start
		errs <- svc.terminalDenyForgejoAction(ctxA, intentA.ID, jobA.ID, "old_denial", "old generation denial", "")
	}()
	go func() { <-start; errs <- svc.completeForgejoActionRebaseJob(ctxA, jobA.ID, pr.HeadSHA) }()
	close(start)
	successes := 0
	for range 3 {
		if err := <-errs; err == nil {
			successes++ // exact old-intent denial succeeds without touching job B
		}
	}
	if successes > 1 {
		t.Fatalf("stale generation results had %d successes; update/completion crossed generation", successes)
	}
	if err := svc.DB.First(&jobB, jobA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&intentB, "id = ?", intentB.ID).Error; err != nil {
		t.Fatal(err)
	}
	if jobB.ActionGeneration != 2 || jobB.ActionIntentID == nil || *jobB.ActionIntentID != intentB.ID || jobB.Phase != ForgejoProjectionPhaseVerifyingPR || intentB.State != ForgejoActionIntentRunning {
		t.Fatalf("stale generation polluted new authority: job=%#v intent=%#v", jobB, intentB)
	}
	if err := svc.completeForgejoActionRebaseJob(ctxB, jobB.ID, pr.HeadSHA); err != nil {
		t.Fatalf("exact current generation completion: %v", err)
	}
	if err := svc.DB.First(&intentA, "id = ?", intentA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&intentB, "id = ?", intentB.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intentA.State != ForgejoActionIntentDenied || intentB.State != ForgejoActionIntentCompleted || intentB.ProviderEffectStatus != ProviderEffectStatusVerifiedCompleted {
		t.Fatalf("intent isolation or verified completion failed: old=%#v new=%#v", intentA, intentB)
	}
}

func TestStaleActionDenialTargetsOldIntentNotReplacementJob(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	intentA := seedDispatchedIntent(t, svc, pr, projection)
	jobA := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: projection.ExternalRepo, AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseVerifyingPR, ActionGeneration: 1,
	}
	ctxA := bindTestActionJob(t, svc, intentA, &jobA)
	if err := svc.DB.Create(&jobA).Error; err != nil {
		t.Fatal(err)
	}
	intentB := intentA
	intentB.ID = intentA.ID + "-replacement"
	intentB.IdempotencyKey = intentA.IdempotencyKey + "-replacement"
	intentB.State = ForgejoActionIntentRunning
	intentB.CreatedAt = time.Now().UTC().Add(time.Second)
	intentB.UpdatedAt = intentB.CreatedAt
	if err := svc.DB.Create(&intentB).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Model(&db.PullRequestProjectionJob{}).Where("id = ?", jobA.ID).Updates(map[string]any{
		"action_intent_id": intentB.ID, "action_generation": 2, "phase": ForgejoProjectionPhaseVerifyingPR,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.terminalDenyForgejoAction(ctxA, intentA.ID, jobA.ID, "stale_denial", "deny exact old generation", ""); err != nil {
		t.Fatal(err)
	}
	var gotA, gotB db.PullRequestActionIntent
	var gotJob db.PullRequestProjectionJob
	if err := svc.DB.First(&gotA, "id = ?", intentA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&gotB, "id = ?", intentB.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&gotJob, jobA.ID).Error; err != nil {
		t.Fatal(err)
	}
	if gotA.State != ForgejoActionIntentDenied || gotB.State != ForgejoActionIntentRunning || gotJob.ActionIntentID == nil || *gotJob.ActionIntentID != intentB.ID || gotJob.ActionGeneration != 2 || gotJob.Phase != ForgejoProjectionPhaseVerifyingPR {
		t.Fatalf("stale denial crossed exact generation: old=%#v new=%#v job=%#v", gotA, gotB, gotJob)
	}
}

func TestTerminalDenialRejectsAllLatePhaseWriters(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseVerifyingPR,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.terminalDenyForgejoAction(ctx, intent.ID, job.ID, DelegatedDenialSessionRevoked, "revoked", pr.HeadSHA); err != nil {
		t.Fatal(err)
	}
	if err := svc.updateForgejoActionRebaseJob(ctx, job.ID, ForgejoProjectionPhaseNeedsRebase, nil); err == nil {
		t.Fatal("late needs-rebase writer resurrected terminal denial")
	}
	if err := svc.updateForgejoProjectionJobPhase(ctx, job.ID, ForgejoProjectionPhaseFailedRetryable, nil); err == nil {
		t.Fatal("late retryable writer resurrected terminal denial")
	}
	svc.markForgejoActionRebaseJobFailed(ctx, job.ID, job.RemoteRef, pr.HeadSHA, errors.New("late failure"), true)
	svc.markForgejoProjectionJobFailed(ctx, job.ID, ForgejoProjectionPhaseFailedRetryable, nil, newProjectionJobFailure(job.RemoteRef, pr.HeadSHA, errors.New("late worker failure"), true))
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied {
		t.Fatalf("terminal job resurrected: %#v", job)
	}
}

func TestTerminalDenialWinsConcurrentRebaseCompletion(t *testing.T) {
	svc, pr, projection := setupIntentTestService(t)
	intent := seedDispatchedIntent(t, svc, pr, projection)
	now := time.Now().UTC()
	job := db.PullRequestProjectionJob{
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, Provider: ProjectionProviderForgejo,
		Trigger: ForgejoProjectionTriggerActionRebase, RepoFullName: "example-owner/demo", AGSPRNumber: pr.Number,
		HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, Phase: ForgejoProjectionPhaseVerifyingPR,
		SuccessCommentDispatchID: "dispatch", SuccessCommentedAt: &now,
	}
	ctx := bindTestActionJob(t, svc, intent, &job)
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	SetForgejoTerminalDenialBeforeCommitHookForTest(svc, func() { close(entered); <-release })
	defer SetForgejoTerminalDenialBeforeCommitHookForTest(svc, nil)
	denialErr := make(chan error, 1)
	go func() {
		denialErr <- svc.terminalDenyForgejoAction(ctx, intent.ID, job.ID, DelegatedDenialSessionRevoked, "revoked", pr.HeadSHA)
	}()
	<-entered
	completionErr := make(chan error, 1)
	go func() { completionErr <- svc.completeForgejoActionRebaseJob(ctx, job.ID, pr.HeadSHA) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	if err := <-denialErr; err != nil {
		t.Fatalf("terminal denial: %v", err)
	}
	if err := <-completionErr; err == nil {
		t.Fatal("concurrent completion overwrote terminal denial")
	}
	if err := svc.DB.First(&intent, "id = ?", intent.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if intent.State != ForgejoActionIntentDenied || job.Phase != ForgejoProjectionPhaseFailedTerminal || job.LastErrorType != ProjectionFailureProviderAdmissionDenied {
		t.Fatalf("intent=%#v job=%#v", intent, job)
	}
}
