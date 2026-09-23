package forgejointegration

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeClient struct {
	ensuredRepos         []string
	ensuredPRs           []string
	pullRequests         []PullRequestRequest
	pullRequestHead      string
	runs                 []WorkflowRun
	runPages             map[int][]WorkflowRun
	runLogs              map[int64][]byte
	workflowRunListCalls int
	workflowRunListErr   error
}

func (f *fakeClient) EnsureRepository(ctx context.Context, owner, repo string, private bool) error {
	f.ensuredRepos = append(f.ensuredRepos, owner+"/"+repo)
	return nil
}

func (f *fakeClient) EnsurePullRequest(ctx context.Context, in PullRequestRequest) (PullRequestResult, error) {
	f.ensuredPRs = append(f.ensuredPRs, in.Owner+"/"+in.Repo+":"+in.Head+"->"+in.Base)
	f.pullRequests = append(f.pullRequests, in)
	return PullRequestResult{Number: len(f.ensuredPRs), URL: "http://forgejo.local/pulls/1", HeadSHA: f.pullRequestHead}, nil
}
func (f *fakeClient) UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (PullRequestResult, error) {
	return PullRequestResult{Number: number, URL: "http://forgejo.local/pulls/1", ExternalRepo: owner + "/" + repo}, nil
}

func (f *fakeClient) ListWorkflowRuns(ctx context.Context, owner, repo string, limit int) ([]WorkflowRun, error) {
	f.workflowRunListCalls++
	if f.workflowRunListErr != nil {
		return nil, f.workflowRunListErr
	}
	return f.runs, nil
}

func (f *fakeClient) ListWorkflowRunsPage(ctx context.Context, owner, repo string, page, limit int) ([]WorkflowRun, bool, error) {
	f.workflowRunListCalls++
	if f.workflowRunListErr != nil {
		return nil, false, f.workflowRunListErr
	}
	if f.runPages == nil {
		return f.runs, false, nil
	}
	runs := f.runPages[page]
	_, hasMore := f.runPages[page+1]
	return runs, hasMore, nil
}

func (f *fakeClient) GetWorkflowRunLogs(ctx context.Context, owner, repo string, runID int64) ([]byte, error) {
	if f.runLogs == nil {
		return nil, errors.New("no logs")
	}
	logs, ok := f.runLogs[runID]
	if !ok {
		return nil, errors.New("missing logs")
	}
	return logs, nil
}

func (f *fakeClient) ListPullRequests(ctx context.Context, owner, repo, state string) ([]PullRequestSnapshot, error) {
	return []PullRequestSnapshot{{
		Number: 1, URL: "http://forgejo.local/pulls/1", State: "open",
		HeadRef: "agent/demo", HeadSHA: f.pullRequestHead, BaseRef: "main",
	}}, nil
}

type actorPermissionClient struct {
	fakeClient
	permission      string
	permissionErr   error
	permissionCalls int
}

func (c *actorPermissionClient) CollaboratorPermission(context.Context, string, string, string) (string, error) {
	c.permissionCalls++
	return c.permission, c.permissionErr
}

func (c *actorPermissionClient) InspectRepositoryAuthority(context.Context, string, string, string, string, string) (RepositoryAuthorityState, error) {
	return RepositoryAuthorityState{}, nil
}

func (c *actorPermissionClient) ApplyRepositoryAuthority(context.Context, string, string, string, string, string, string, RepositoryAuthorityState) error {
	return nil
}

func TestProviderMergeBindingIsServerDerivedAndRevisioned(t *testing.T) {
	enabled := true
	config := Config{Enabled: true, AuthorityPolicyEnabled: true, IntegrationBot: "ags-bot", RepoMap: map[string]RepoMapping{
		"example-team/app-fixture": {Owner: "example-team", Repo: "app-fixture", Enabled: &enabled, BaseBranch: "main", DelegatedMergeMethod: "rebase"},
	}}
	binding, err := New(config, &fakeClient{}, nil).ProviderMergeBinding("example-team/app-fixture", 27, "primary-b")
	if err != nil {
		t.Fatal(err)
	}
	if binding.CanonicalRepository != "example-team/app-fixture" || binding.ProviderRepository != "example-team/app-fixture" || binding.BaseRef != "main" || binding.MergeMethod != "rebase" {
		t.Fatalf("binding=%#v", binding)
	}
	for _, digest := range []string{binding.CanonicalRepositoryID, binding.ProviderBindingID, binding.ProviderBindingRevision} {
		if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
			t.Fatalf("digest=%q", digest)
		}
	}
	changed := config
	changed.IntegrationBot = "other-bot"
	other, err := New(changed, &fakeClient{}, nil).ProviderMergeBinding("example-team/app-fixture", 27, "primary-b")
	if err != nil {
		t.Fatal(err)
	}
	if other.CanonicalRepositoryID != binding.CanonicalRepositoryID || other.ProviderBindingID != binding.ProviderBindingID || other.ProviderBindingRevision == binding.ProviderBindingRevision {
		t.Fatalf("revision did not isolate mutable binding facts: before=%#v after=%#v", binding, other)
	}
	config.RepoMap["example-team/app-fixture"] = RepoMapping{Owner: "example-team", Repo: "app-fixture", Enabled: &enabled, BaseBranch: "main"}
	if _, err := New(config, &fakeClient{}, nil).ProviderMergeBinding("example-team/app-fixture", 27, "primary-b"); err == nil {
		t.Fatal("missing server-owned merge method was accepted")
	}
}

func TestProviderMergeBindingForBaseUsesAllowedAuthoritativePRBase(t *testing.T) {
	enabled := true
	config := Config{
		Enabled:              true,
		MirrorBranchIncludes: []string{"main", "conformance/c3/*/*/*"},
		RepoMap: map[string]RepoMapping{
			"example-team/app-fixture": {Owner: "example-team", Repo: "app-fixture", Enabled: &enabled, BaseBranch: "main", DelegatedMergeMethod: "rebase"},
		},
	}
	integration := New(config, &fakeClient{}, nil)
	mainBinding, err := integration.ProviderMergeBinding("example-team/app-fixture", 27, "primary-b")
	if err != nil {
		t.Fatal(err)
	}
	base := "conformance/c3/run/rebase/base"
	binding, err := integration.ProviderMergeBindingForBase("example-team/app-fixture", 27, "primary-b", base)
	if err != nil {
		t.Fatal(err)
	}
	if binding.BaseRef != base || binding.MergeMethod != "rebase" || binding.ProviderBindingID != mainBinding.ProviderBindingID || binding.ProviderBindingRevision == mainBinding.ProviderBindingRevision {
		t.Fatalf("dynamic binding=%#v main=%#v", binding, mainBinding)
	}
	if _, err := integration.ProviderMergeBindingForBase("example-team/app-fixture", 27, "primary-b", "private/not-mirrored"); err == nil {
		t.Fatal("non-mirrored provider base was accepted")
	}
}

func TestActionPrincipalBindingNormalizesActorAndBindsRevisionToPrincipal(t *testing.T) {
	integration := New(Config{ActionPrincipalBindings: map[string]uint{"  EXAMPLE-HUMAN  ": 7}}, &fakeClient{}, nil)

	principalID, revision, ok := integration.ActionPrincipalBinding(" Example-Human ")
	if !ok || principalID != 7 {
		t.Fatalf("binding=(%d,%q,%t), want principal 7", principalID, revision, ok)
	}
	if !strings.HasPrefix(revision, "sha256:") || len(revision) != len("sha256:")+64 {
		t.Fatalf("revision=%q, want sha256 content identity", revision)
	}
	_, sameRevision, ok := integration.ActionPrincipalBinding("example-human")
	if !ok || sameRevision != revision {
		t.Fatalf("normalized revision=%q, want %q", sameRevision, revision)
	}

	changed := New(Config{ActionPrincipalBindings: map[string]uint{"example-human": 8}}, &fakeClient{}, nil)
	_, changedRevision, ok := changed.ActionPrincipalBinding("example-human")
	if !ok || changedRevision == revision {
		t.Fatalf("changed revision=%q, must differ from %q", changedRevision, revision)
	}
	if _, _, ok := integration.ActionPrincipalBinding("unmapped"); ok {
		t.Fatal("unmapped actor unexpectedly resolved")
	}
}

func TestActorHasWriteAccessUsesAuthorityReadClient(t *testing.T) {
	integrationClient := &actorPermissionClient{permissionErr: errors.New("integration bot cannot query another collaborator")}
	authorityClient := &actorPermissionClient{permission: "write"}
	integration := &Integration{
		cfg:             Config{Enabled: true},
		client:          integrationClient,
		authorityClient: authorityClient,
	}

	allowed, permission, err := integration.ActorHasWriteAccess(context.Background(), "operator/project-kit", "example-human")
	if err != nil {
		t.Fatalf("ActorHasWriteAccess: %v", err)
	}
	if !allowed || permission != "write" {
		t.Fatalf("allowed=%t permission=%q, want write access", allowed, permission)
	}
	if integrationClient.permissionCalls != 0 {
		t.Fatalf("integration permission calls=%d, want 0", integrationClient.permissionCalls)
	}
	if authorityClient.permissionCalls != 1 {
		t.Fatalf("authority permission calls=%d, want 1", authorityClient.permissionCalls)
	}
}

func TestConfigBranchPoliciesSeparateMirrorAndPullRequest(t *testing.T) {
	cfg := Config{
		Enabled:              true,
		MirrorBranchIncludes: []string{"main", "agent/*", "feature/*"},
		MirrorBranchExcludes: []string{"ci/*"},
		PRBranchIncludes:     []string{"agent/*", "feature/*"},
		PRBranchExcludes:     []string{"main", "ci/*"},
	}

	cases := []struct {
		branch     string
		wantMirror bool
		wantPR     bool
	}{
		{"main", true, false},
		{"agent/demo", true, true},
		{"feature/demo", true, true},
		{"ci/gitlab-mr-1", false, false},
		{"bug/demo", false, false},
	}
	for _, tc := range cases {
		if got := cfg.MirrorBranchEnabled(tc.branch); got != tc.wantMirror {
			t.Fatalf("MirrorBranchEnabled(%q)=%v, want %v", tc.branch, got, tc.wantMirror)
		}
		if got := cfg.PullRequestBranchEnabled(tc.branch); got != tc.wantPR {
			t.Fatalf("PullRequestBranchEnabled(%q)=%v, want %v", tc.branch, got, tc.wantPR)
		}
	}
}

func TestConfigEmptyBranchPoliciesAdmitAllBranches(t *testing.T) {
	cfg := Config{Enabled: true}
	for _, branch := range []string{"main", "ci/generated", "tmp/deep/nested/branch", "unclassified"} {
		if !cfg.MirrorBranchEnabled(branch) {
			t.Fatalf("MirrorBranchEnabled(%q)=false, want true", branch)
		}
		if !cfg.PullRequestBranchEnabled(branch) {
			t.Fatalf("PullRequestBranchEnabled(%q)=false, want true", branch)
		}
	}
}

func TestConfiguredRepoFullNamesExcludesDisabledMappingsAndSorts(t *testing.T) {
	disabled := false
	integration := New(Config{
		Enabled: true,
		RepoMap: map[string]RepoMapping{
			"z/enabled":  {},
			"a/disabled": {Enabled: &disabled},
			"b/enabled":  {},
		},
	}, nil, nil)

	got := integration.ConfiguredRepoFullNames()
	want := []string{"b/enabled", "z/enabled"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfiguredRepoFullNames()=%v, want %v", got, want)
	}
}

func TestLatestWorkflowRunForPullRequestMatchesPRAndHeadSHA(t *testing.T) {
	client := &fakeClient{runs: []WorkflowRun{
		{ID: 1, Status: "failure", HeadBranch: "#9", HeadSHA: "old", DisplayTitle: "old (#9)"},
		{ID: 2, Status: "success", HeadBranch: "#16", HeadSHA: "75029c1d787a16dacfec1d1e734e730a881ff9cd", DisplayTitle: "Fix HUM-38"},
	}}
	integration := New(Config{
		Enabled:      true,
		BaseURL:      "http://forgejo.local",
		Token:        "secret-token",
		DefaultOwner: "ci",
		RepoMap: map[string]RepoMapping{
			"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci"},
		},
	}, client, nil)
	run, ok, err := integration.LatestWorkflowRunForPullRequest(context.Background(), "example-owner/demo", 16, "75029c1d")
	if err != nil {
		t.Fatalf("LatestWorkflowRunForPullRequest: %v", err)
	}
	if !ok || run.ID != 2 || run.Status != "success" {
		t.Fatalf("run=%#v ok=%v", run, ok)
	}
}

func TestWorkflowRunsForPullRequestRequiresExactMappingAndHead(t *testing.T) {
	head := "75029c1d787a16dacfec1d1e734e730a881ff9cd"
	client := &fakeClient{runs: []WorkflowRun{
		{ID: 1, Name: "build", Status: "failure", HeadBranch: "#16", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{ID: 2, Name: "build", Status: "success", HeadBranch: "#16", HeadSHA: head},
		{ID: 3, Name: "other", Status: "success", HeadBranch: "#17", HeadSHA: head},
		{ID: 4, Name: "build", Status: "success", Event: "workflow_dispatch", HeadBranch: "feature/work", HeadSHA: head},
		{ID: 5, Name: "build", Status: "success", Event: "workflow_dispatch", HeadBranch: "other/work", HeadSHA: head},
	}}
	integration := New(Config{Enabled: true, RepoMap: map[string]RepoMapping{
		"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci"},
	}}, client, nil)

	runs, supported, err := integration.WorkflowRunsForPullRequest(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "feature/work", head, 100)
	if err != nil {
		t.Fatalf("WorkflowRunsForPullRequest: %v", err)
	}
	if !supported || len(runs) != 2 || runs[0].ID != 2 || runs[1].ID != 4 {
		t.Fatalf("runs=%#v supported=%t, want exact pull-request run 2 and workflow-dispatch run 4", runs, supported)
	}
	client.runLogs = map[int64][]byte{2: []byte("job failed")}
	logs, supported, err := integration.WorkflowRunLogs(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "feature/work", head, 2)
	if err != nil || !supported || string(logs) != "job failed" {
		t.Fatalf("WorkflowRunLogs=%q supported=%t err=%v", logs, supported, err)
	}
	if _, _, err := integration.WorkflowRunLogs(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "feature/work", head, 3); err == nil {
		t.Fatal("unbound run logs unexpectedly accepted")
	}
	if _, _, err := integration.WorkflowRunsForPullRequest(context.Background(), "example-owner/demo", "other/demo-ci", 16, "feature/work", head, 100); err == nil {
		t.Fatal("mismatched external repository unexpectedly accepted")
	}
	if _, _, err := integration.WorkflowRunsForPullRequest(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "", head, 100); err == nil {
		t.Fatal("empty pull request head ref unexpectedly accepted")
	}
	for _, invalidHead := range []string{"", strings.ToUpper(head), head[:12], " " + head} {
		if _, _, err := integration.WorkflowRunsForPullRequest(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "feature/work", invalidHead, 100); err == nil {
			t.Fatalf("non-canonical head %q unexpectedly accepted", invalidHead)
		}
	}
}

func TestWorkflowRunsForPullRequestScansLaterPagesBeforeReportingEmpty(t *testing.T) {
	head := "75029c1d787a16dacfec1d1e734e730a881ff9cd"
	client := &fakeClient{runPages: map[int][]WorkflowRun{
		1: {{ID: 1, Name: "other", Status: "success", HeadBranch: "#15", HeadSHA: head}},
		2: {{ID: 2, Name: "build", Status: "success", HeadBranch: "#16", HeadSHA: head}},
	}}
	integration := New(Config{Enabled: true, RepoMap: map[string]RepoMapping{
		"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci"},
	}}, client, nil)

	runs, supported, err := integration.WorkflowRunsForPullRequest(context.Background(), "example-owner/demo", "forgejo/demo-ci", 16, "feature/work", head, 100)
	if err != nil || !supported || len(runs) != 1 || runs[0].ID != 2 {
		t.Fatalf("paginated runs=%#v supported=%t err=%v", runs, supported, err)
	}
}

func TestHandlePushSkipsUnmappedRepoWhenNoChangedBranchMatchesMirrorPolicy(t *testing.T) {
	client := &fakeClient{}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "operator",
		MirrorBranchIncludes: []string{"main", "agent/*", "feature/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	result, err := integration.HandlePushWithResult(context.Background(), PushEvent{
		RepoFullName: "example-owner/smoke-repo",
		RepoPath:     "/repos/example-owner/smoke-repo.git",
		Changes: []RefChange{{
			Ref:   "refs/heads/smoke/123",
			After: "abc123",
		}},
	})
	if err != nil {
		t.Fatalf("HandlePushWithResult should skip non-mirror branch before repo mapping, got %v", err)
	}
	if len(pushes) != 0 || len(client.ensuredRepos) != 0 || len(result.AutoPullRequests) != 0 {
		t.Fatalf("non-mirror branch should have no side effects: pushes=%v repos=%v result=%#v", pushes, client.ensuredRepos, result)
	}
}

func TestHandlePushFinalAuthorityDriftMakesZeroProviderWrites(t *testing.T) {
	client := &fakeClient{}
	pushes := 0
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", DefaultOwner: "mirror",
		AutoCreateRepo: true, MirrorBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]RepoMapping{"example-owner/demo": {Owner: "mirror", Repo: "demo"}},
	}, client, func(context.Context, PushRequest) error {
		pushes++
		return nil
	})
	checks := 0
	_, err := integration.HandlePushWithResult(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git",
		Changes: []RefChange{{Ref: "refs/heads/agent/demo", After: "abc123"}},
		BeforeWrite: func(context.Context) error {
			checks++
			return context.Canceled
		},
	})
	if err == nil || checks != 1 || pushes != 0 || len(client.ensuredRepos) != 0 {
		t.Fatalf("authority drift: checks=%d pushes=%d ensured=%v err=%v", checks, pushes, client.ensuredRepos, err)
	}
}

func TestHandlePushRechecksAuthorityBetweenEnsureAndPush(t *testing.T) {
	client := &fakeClient{}
	pushes := 0
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", DefaultOwner: "mirror",
		AutoCreateRepo: true, MirrorBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]RepoMapping{"example-owner/demo": {Owner: "mirror", Repo: "demo"}},
	}, client, func(context.Context, PushRequest) error {
		pushes++
		return nil
	})
	checks := 0
	_, err := integration.HandlePushWithResult(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git",
		Changes: []RefChange{{Ref: "refs/heads/agent/demo", After: "abc123"}},
		BeforeWrite: func(context.Context) error {
			checks++
			if checks == 2 {
				return context.Canceled
			}
			return nil
		},
	})
	if err == nil || checks != 2 || pushes != 0 || len(client.ensuredRepos) != 1 {
		t.Fatalf("second authority check: checks=%d pushes=%d ensured=%v err=%v", checks, pushes, client.ensuredRepos, err)
	}
}

func TestEnsurePullRequestSkipsUnmappedRepoWhenBranchNotPRProjected(t *testing.T) {
	client := &fakeClient{}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "operator",
		MirrorBranchIncludes: []string{"main", "agent/*", "feature/*"},
		PRBranchIncludes:     []string{"agent/*", "feature/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	result, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/smoke-repo",
		RepoPath:     "/repos/example-owner/smoke-repo.git",
		Head:         "smoke/123",
		Base:         "main",
		Title:        "smoke",
		HeadSHA:      "abc123",
	})
	if err != nil {
		t.Fatalf("EnsurePullRequest should skip non-PR branch before repo mapping, got %v", err)
	}
	if result.Number != 0 || len(pushes) != 0 || len(client.ensuredRepos) != 0 || len(client.ensuredPRs) != 0 {
		t.Fatalf("non-PR branch should have no side effects: result=%#v pushes=%v repos=%v prs=%v", result, pushes, client.ensuredRepos, client.ensuredPRs)
	}
}

func TestHandlePushSyncsMainWithoutCreatingPullRequest(t *testing.T) {
	client := &fakeClient{}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		AutoCreateRepo:       true,
		AutoPullRequest:      true,
		DefaultBaseBranch:    "main",
		MirrorBranchIncludes: []string{"main", "agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		PRBranchExcludes:     []string{"main"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	err := integration.HandlePush(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Changes: []RefChange{{
			Ref:   "refs/heads/main",
			After: "abc123",
		}},
	})
	if err != nil {
		t.Fatalf("HandlePush returned error: %v", err)
	}
	if len(pushes) != 1 || pushes[0].Refspec != "refs/heads/main:refs/heads/main" {
		t.Fatalf("pushes=%v", pushes)
	}
	if len(client.ensuredPRs) != 0 {
		t.Fatalf("main push should not create PRs: %v", client.ensuredPRs)
	}
}

func TestEnsurePullRequestMirrorsHeadBranchBeforeProjectingPR(t *testing.T) {
	client := &fakeClient{pullRequestHead: "abc123"}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "ci",
		AutoCreateRepo:       true,
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]RepoMapping{
			"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci", BaseBranch: "develop"},
		},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	res, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Head:         "agent/demo",
		Base:         "develop",
		Title:        "title",
		Body:         "body",
		HeadSHA:      "abc123",
	})
	if err != nil {
		t.Fatalf("EnsurePullRequest returned error: %v", err)
	}
	if res.Number != 1 || res.HeadSHA != "abc123" {
		t.Fatalf("result=%#v", res)
	}
	if len(pushes) != 1 {
		t.Fatalf("push count=%d, want 1", len(pushes))
	}
	if pushes[0].RepoPath != "/repos/example-owner/demo.git" {
		t.Fatalf("push repo path=%q", pushes[0].RepoPath)
	}
	if pushes[0].RemoteURL != "http://x-access-token:secret-token@forgejo.local/forgejo/demo-ci.git" {
		t.Fatalf("remote url=%q", pushes[0].RemoteURL)
	}
	if pushes[0].Refspec != "refs/heads/agent/demo:refs/heads/agent/demo" {
		t.Fatalf("refspec=%q", pushes[0].Refspec)
	}
	if len(client.ensuredPRs) != 1 || client.ensuredPRs[0] != "forgejo/demo-ci:agent/demo->develop" {
		t.Fatalf("ensured PRs=%v", client.ensuredPRs)
	}
	if len(client.pullRequests) != 1 || client.pullRequests[0].SkipMetadataReconcile {
		t.Fatalf("AGS PR lifecycle projection must reconcile metadata: %#v", client.pullRequests)
	}
}

func TestEnsurePullRequestPassesPushTuningToGitPusher(t *testing.T) {
	client := &fakeClient{pullRequestHead: "abc123"}
	var got PushRequest
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		PushGitConfig:        []string{" core.compression=0 ", "", "pack.window=0"},
		PushTimeout:          7 * time.Minute,
	}, client, func(ctx context.Context, req PushRequest) error {
		got = req
		return nil
	})

	if _, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Head:         "agent/demo",
		Base:         "main",
		Title:        "title",
		Body:         "body",
		HeadSHA:      "abc123",
	}); err != nil {
		t.Fatalf("EnsurePullRequest returned error: %v", err)
	}
	if !reflect.DeepEqual(got.GitConfig, []string{"core.compression=0", "pack.window=0"}) {
		t.Fatalf("push git config=%#v", got.GitConfig)
	}
	if got.Timeout != 7*time.Minute {
		t.Fatalf("push timeout=%s", got.Timeout)
	}
}

func TestGitPushArgsIncludePackTuningBeforePush(t *testing.T) {
	remoteURL := "http://x-access-token:secret-token@forgejo.local/example-owner/demo.git"
	got := gitPushArgs(PushRequest{
		RepoPath:  "/repos/example-owner/demo.git",
		RemoteURL: remoteURL,
		Refspec:   "refs/heads/agent/demo:refs/heads/agent/demo",
		GitConfig: []string{"core.compression=0", "pack.window=0"},
	})
	want := []string{"-C", "/repos/example-owner/demo.git", "-c", "core.compression=0", "-c", "pack.window=0", "push", remoteURL, "refs/heads/agent/demo:refs/heads/agent/demo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("git push args=%#v, want %#v", got, want)
	}
}

func TestGitPushArgsUseExactForceWithLeaseWithoutForcedRefspec(t *testing.T) {
	const oldSHA = "1111111111111111111111111111111111111111"
	remoteURL := "http://x-access-token:secret-token@forgejo.local/example-owner/demo.git"
	got := gitPushArgs(PushRequest{
		RepoPath:          "/repos/example-owner/demo.git",
		RemoteURL:         remoteURL,
		Refspec:           "refs/heads/agent/demo:refs/heads/agent/demo",
		ForceWithLeaseRef: "refs/heads/agent/demo",
		ForceWithLeaseSHA: oldSHA,
	})
	want := []string{
		"-C", "/repos/example-owner/demo.git", "push",
		"--force-with-lease=refs/heads/agent/demo:" + oldSHA,
		remoteURL, "refs/heads/agent/demo:refs/heads/agent/demo",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("git push args=%#v, want %#v", got, want)
	}
	if strings.HasPrefix(got[len(got)-1], "+") || slices.Contains(got, "--force") {
		t.Fatalf("lease push used unconditional force: %#v", got)
	}
}

func TestGitPushRejectsLeaseCombinedWithUnconditionalForcedRefspec(t *testing.T) {
	err := GitPush(context.Background(), PushRequest{
		RepoPath: "/repos/example-owner/demo.git", RemoteURL: "file:///tmp/unused.git",
		Refspec:           "+refs/heads/agent/demo:refs/heads/agent/demo",
		ForceWithLeaseRef: "refs/heads/agent/demo", ForceWithLeaseSHA: "1111111111111111111111111111111111111111",
	})
	if err == nil || !strings.Contains(err.Error(), "unconditional force") {
		t.Fatalf("unconditional force was not rejected: %v", err)
	}
}

func TestEnsurePullRequestLeaseRewriteVerifiesRemoteAndPullRequestHead(t *testing.T) {
	const (
		oldSHA = "1111111111111111111111111111111111111111"
		newSHA = "2222222222222222222222222222222222222222"
	)
	client := &fakeClient{pullRequestHead: oldSHA}
	var got PushRequest
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token",
		DefaultOwner: "example-owner", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		got = req
		client.pullRequestHead = newSHA
		return nil
	})
	remoteReads := 0
	integration.remoteRef = func(ctx context.Context, repoPath, remoteURL, ref string) (string, error) {
		remoteReads++
		if remoteReads == 1 {
			return oldSHA, nil
		}
		return newSHA, nil
	}

	res, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git",
		Head: "agent/demo", Base: "main", Title: "title", HeadSHA: newSHA,
		RewriteLease: &PullRequestRewriteLease{
			ExternalRepo: "example-owner/demo", ExternalNumber: 1, ExpectedOldHeadSHA: oldSHA,
		},
	})
	if err != nil {
		t.Fatalf("lease rewrite: %v", err)
	}
	if got.ForceWithLeaseRef != "refs/heads/agent/demo" || got.ForceWithLeaseSHA != oldSHA || strings.HasPrefix(got.Refspec, "+") {
		t.Fatalf("unsafe lease push request=%#v", got)
	}
	if remoteReads < 2 || res.Number != 1 || res.HeadSHA != newSHA {
		t.Fatalf("post-push convergence not verified: reads=%d result=%#v", remoteReads, res)
	}
}

func TestEnsurePullRequestLeaseRewriteFinalAuthorityDriftMakesZeroProviderWrites(t *testing.T) {
	const (
		oldSHA = "1111111111111111111111111111111111111111"
		newSHA = "2222222222222222222222222222222222222222"
	)
	client := &fakeClient{pullRequestHead: oldSHA}
	pushes := 0
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token",
		DefaultOwner: "example-owner", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
	}, client, func(context.Context, PushRequest) error {
		pushes++
		return nil
	})
	integration.remoteRef = func(context.Context, string, string, string) (string, error) { return oldSHA, nil }
	checks := 0
	_, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git", Head: "agent/demo", Base: "main", HeadSHA: newSHA,
		RewriteLease: &PullRequestRewriteLease{ExternalRepo: "example-owner/demo", ExternalNumber: 1, ExpectedOldHeadSHA: oldSHA},
		BeforeWrite: func(context.Context) error {
			checks++
			return errors.New("authority_snapshot_changed")
		},
	})
	if err == nil || checks != 1 || pushes != 0 || len(client.ensuredPRs) != 0 {
		t.Fatalf("final authority drift: checks=%d pushes=%d prs=%v err=%v", checks, pushes, client.ensuredPRs, err)
	}
}

func TestEnsurePullRequestLeaseRewriteRejectsConcurrentRemoteChangeBeforePush(t *testing.T) {
	const (
		oldSHA     = "1111111111111111111111111111111111111111"
		newSHA     = "2222222222222222222222222222222222222222"
		unknownSHA = "3333333333333333333333333333333333333333"
	)
	client := &fakeClient{pullRequestHead: oldSHA}
	pushes := 0
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token",
		DefaultOwner: "example-owner", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes++
		return nil
	})
	integration.remoteRef = func(ctx context.Context, repoPath, remoteURL, ref string) (string, error) {
		return unknownSHA, nil
	}

	_, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git",
		Head: "agent/demo", Base: "main", HeadSHA: newSHA,
		RewriteLease: &PullRequestRewriteLease{
			ExternalRepo: "example-owner/demo", ExternalNumber: 1, ExpectedOldHeadSHA: oldSHA,
		},
	})
	if err == nil || pushes != 0 {
		t.Fatalf("concurrent drift was not rejected before push: pushes=%d err=%v", pushes, err)
	}
	var projectionErr *ProjectionError
	if !errors.As(err, &projectionErr) || projectionErr.ActualSHA != unknownSHA || projectionErr.ExpectedSHA != oldSHA {
		t.Fatalf("concurrent drift error=%#v", projectionErr)
	}
}

func TestEnsurePullRequestLeaseRewriteRejectsWrongMappedPullRequest(t *testing.T) {
	const (
		oldSHA = "1111111111111111111111111111111111111111"
		newSHA = "2222222222222222222222222222222222222222"
	)
	pushes := 0
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token",
		DefaultOwner: "example-owner", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
	}, &fakeClient{pullRequestHead: oldSHA}, func(ctx context.Context, req PushRequest) error {
		pushes++
		return nil
	})
	_, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git", Head: "agent/demo", Base: "main", HeadSHA: newSHA,
		RewriteLease: &PullRequestRewriteLease{ExternalRepo: "example-owner/demo", ExternalNumber: 99, ExpectedOldHeadSHA: oldSHA},
	})
	if err == nil || pushes != 0 {
		t.Fatalf("wrong mapped PR was not rejected before push: pushes=%d err=%v", pushes, err)
	}
}

func TestEnsurePullRequestLeaseRewriteFailsWhenPullRequestAPINeverConverges(t *testing.T) {
	const (
		oldSHA = "1111111111111111111111111111111111111111"
		newSHA = "2222222222222222222222222222222222222222"
	)
	client := &fakeClient{pullRequestHead: oldSHA}
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token",
		DefaultOwner: "example-owner", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"agent/*"}, PRBranchIncludes: []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error { return nil })
	reads := 0
	integration.remoteRef = func(ctx context.Context, repoPath, remoteURL, ref string) (string, error) {
		reads++
		if reads == 1 {
			return oldSHA, nil
		}
		return newSHA, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := integration.EnsurePullRequest(ctx, PullRequestSyncRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git", Head: "agent/demo", Base: "main", HeadSHA: newSHA,
		RewriteLease: &PullRequestRewriteLease{ExternalRepo: "example-owner/demo", ExternalNumber: 1, ExpectedOldHeadSHA: oldSHA},
	})
	if err == nil || res.HeadSHA != oldSHA {
		t.Fatalf("stale Forgejo PR API was treated as converged or not reported: result=%#v err=%v", res, err)
	}
}

func TestEnsurePullRequestLeaseRewriteRejectsBaseBranch(t *testing.T) {
	const oldSHA = "1111111111111111111111111111111111111111"
	pushes := 0
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token",
		DefaultOwner: "example-owner", DefaultBaseBranch: "main", AutoPullRequest: true,
		MirrorBranchIncludes: []string{"main"}, PRBranchIncludes: []string{"main"},
	}, &fakeClient{pullRequestHead: oldSHA}, func(ctx context.Context, req PushRequest) error {
		pushes++
		return nil
	})
	_, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git",
		Head: "main", Base: "main", HeadSHA: "2222222222222222222222222222222222222222",
		RewriteLease: &PullRequestRewriteLease{ExternalRepo: "example-owner/demo", ExternalNumber: 1, ExpectedOldHeadSHA: oldSHA},
	})
	if err == nil || pushes != 0 || !strings.Contains(err.Error(), "base branch") {
		t.Fatalf("base branch rewrite was not blocked: pushes=%d err=%v", pushes, err)
	}
}

func TestEnsurePullRequestAcceptsConcurrentMirrorWhenRemoteHeadAlreadyMatches(t *testing.T) {
	client := &fakeClient{pullRequestHead: "abc123"}
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		return errors.New("remote: error: cannot lock ref 'refs/heads/agent/demo': reference already exists")
	})
	integration.remoteRef = func(ctx context.Context, repoPath, remoteURL, ref string) (string, error) {
		if repoPath != "/repos/example-owner/demo.git" || ref != "refs/heads/agent/demo" {
			t.Fatalf("unexpected remote ref read repoPath=%q ref=%q", repoPath, ref)
		}
		return "abc123", nil
	}

	res, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Head:         "agent/demo",
		Base:         "main",
		Title:        "title",
		Body:         "body",
		HeadSHA:      "abc123",
	})
	if err != nil {
		t.Fatalf("EnsurePullRequest returned error: %v", err)
	}
	if res.Number != 1 || len(client.ensuredPRs) != 1 {
		t.Fatalf("result=%#v ensured=%v", res, client.ensuredPRs)
	}
}

func TestEnsurePullRequestRejectsRemoteDriftWithoutForcePushing(t *testing.T) {
	const (
		branch      = "agent/demo"
		ref         = "refs/heads/" + branch
		expectedSHA = "abc123"
		actualSHA   = "def456"
	)
	client := &fakeClient{pullRequestHead: expectedSHA}
	var gotRefspec string
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		gotRefspec = req.Refspec
		return errors.New("updates were rejected because the remote contains work that you do not have locally (non-fast-forward)")
	})
	integration.remoteRef = func(ctx context.Context, repoPath, remoteURL, gotRef string) (string, error) {
		if gotRef != ref {
			t.Fatalf("remote ref read ref=%q, want %q", gotRef, ref)
		}
		return actualSHA, nil
	}

	_, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Head:         branch,
		Base:         "main",
		Title:        "title",
		Body:         "body",
		HeadSHA:      expectedSHA,
	})
	if err == nil {
		t.Fatal("expected remote drift to block Forgejo PR projection")
	}
	if gotRefspec != "refs/heads/agent/demo:refs/heads/agent/demo" {
		t.Fatalf("PR branch mirror used refspec %q, want non-force refspec", gotRefspec)
	}
	classified := ClassifyProjectionError(ref, expectedSHA, err)
	if classified.Type != ProjectionFailureNonFastForward || classified.ActualSHA != actualSHA || classified.ExpectedSHA != expectedSHA {
		t.Fatalf("classified=%#v, want non-fast-forward with expected/actual SHA", classified)
	}
	if strings.Contains(classified.ErrorSummary, "secret-token") {
		t.Fatalf("classified error leaked token: %q", classified.ErrorSummary)
	}
	if len(client.ensuredPRs) != 0 {
		t.Fatalf("drift must not continue to Forgejo PR ensure: %v", client.ensuredPRs)
	}
}

func TestEnsurePullRequestRejectsStaleForgejoHead(t *testing.T) {
	client := &fakeClient{pullRequestHead: "oldsha"}
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error { return nil })

	_, err := integration.EnsurePullRequest(context.Background(), PullRequestSyncRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Head:         "agent/demo",
		Base:         "main",
		Title:        "title",
		Body:         "body",
		HeadSHA:      "abc123",
	})
	if err == nil {
		t.Fatal("expected stale Forgejo head error")
	}
}

func TestValidatePullRequestProjectionRejectsExcludedHeadForMappedAutoPRRepo(t *testing.T) {
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		AutoPullRequest:      true,
		MirrorBranchIncludes: []string{"main", "agent/*"},
		PRBranchIncludes:     []string{"agent/*"},
		RepoMap: map[string]RepoMapping{
			"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci", BaseBranch: "main"},
		},
	}, &fakeClient{}, nil)

	required, err := integration.ValidatePullRequestProjection("example-owner/demo", "claude/debug", "main")
	if !required {
		t.Fatal("mapped auto-PR repository must require a Forgejo projection")
	}
	if err == nil || !strings.Contains(err.Error(), "not pull-request-enabled") {
		t.Fatalf("expected excluded-head validation error, got %v", err)
	}
}

func TestHandlePushRejectsForcedDefaultBranchUpdate(t *testing.T) {
	client := &fakeClient{}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		DefaultBaseBranch:    "main",
		MirrorBranchIncludes: []string{"main", "agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	err := integration.HandlePush(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Changes: []RefChange{{
			Ref:    "refs/heads/main",
			Before: "oldsha",
			After:  "newsha",
			Forced: true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing forced update of authoritative base branch") {
		t.Fatalf("expected forced-base rejection, got %v", err)
	}
	if len(pushes) != 0 {
		t.Fatalf("forced base update must not reach Forgejo pusher: %v", pushes)
	}
}

func TestHandlePushRejectsDefaultBranchDeletion(t *testing.T) {
	client := &fakeClient{}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		DefaultBaseBranch:    "main",
		MirrorBranchIncludes: []string{"main", "agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	err := integration.HandlePush(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Changes: []RefChange{{
			Ref:     "refs/heads/main",
			Before:  "oldsha",
			After:   zeroSHA,
			Deleted: true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing deletion of authoritative base branch") {
		t.Fatalf("expected base-deletion rejection, got %v", err)
	}
	if len(pushes) != 0 {
		t.Fatalf("base deletion must not reach Forgejo pusher: %v", pushes)
	}
}

func TestHandlePushForceMirrorsForcedWorkBranchUpdates(t *testing.T) {
	client := &fakeClient{}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		MirrorBranchIncludes: []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	err := integration.HandlePush(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Changes: []RefChange{{
			Ref:    "refs/heads/agent/demo",
			Before: "oldsha",
			After:  "newsha",
			Forced: true,
		}},
	})
	if err != nil {
		t.Fatalf("HandlePush returned error: %v", err)
	}
	if len(pushes) != 1 || pushes[0].Refspec != "+refs/heads/agent/demo:refs/heads/agent/demo" {
		t.Fatalf("pushes=%v", pushes)
	}
}

func TestHandlePushSyncsAllowedBranchToMappedForgejoRepoWithoutCreatingPR(t *testing.T) {
	client := &fakeClient{}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:           true,
		BaseURL:           "http://forgejo.local",
		Token:             "secret-token",
		DefaultOwner:      "ci",
		AutoCreateRepo:    true,
		AutoPullRequest:   true,
		DefaultBaseBranch: "main",
		BranchIncludes:    []string{"agent/*"},
		RepoMap: map[string]RepoMapping{
			"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci", BaseBranch: "develop"},
		},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	result, err := integration.HandlePushWithResult(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Changes: []RefChange{{
			Ref:    "refs/heads/agent/demo",
			Before: "0000000000000000000000000000000000000000",
			After:  "abc123",
		}},
	})
	if err != nil {
		t.Fatalf("HandlePush returned error: %v", err)
	}
	if len(client.ensuredRepos) != 1 || client.ensuredRepos[0] != "forgejo/demo-ci" {
		t.Fatalf("ensured repos=%v", client.ensuredRepos)
	}
	if len(pushes) != 1 {
		t.Fatalf("push count=%d, want 1", len(pushes))
	}
	if pushes[0].RepoPath != "/repos/example-owner/demo.git" {
		t.Fatalf("push repo path=%q", pushes[0].RepoPath)
	}
	if pushes[0].RemoteURL != "http://x-access-token:secret-token@forgejo.local/forgejo/demo-ci.git" {
		t.Fatalf("remote url=%q", pushes[0].RemoteURL)
	}
	if pushes[0].Refspec != "refs/heads/agent/demo:refs/heads/agent/demo" {
		t.Fatalf("refspec=%q", pushes[0].Refspec)
	}
	if len(client.ensuredPRs) != 0 || len(client.pullRequests) != 0 {
		t.Fatalf("push must not create Forgejo PRs before an AGS PR exists: ensured=%v requests=%#v", client.ensuredPRs, client.pullRequests)
	}
	if len(result.AutoPullRequests) != 0 {
		t.Fatalf("push result must not report auto PRs: %#v", result.AutoPullRequests)
	}
}

func TestHandlePushMirrorsAllowedBranchDeletesToForgejo(t *testing.T) {
	client := &fakeClient{}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "http://forgejo.local",
		Token:                "secret-token",
		DefaultOwner:         "example-owner",
		MirrorBranchIncludes: []string{"agent/*"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	err := integration.HandlePush(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Changes: []RefChange{{
			Ref:     "refs/heads/agent/deleted",
			Before:  "abc123",
			After:   "0000000000000000000000000000000000000000",
			Deleted: true,
		}},
	})
	if err != nil {
		t.Fatalf("HandlePush returned error: %v", err)
	}
	if len(pushes) != 1 {
		t.Fatalf("push count=%d, want 1", len(pushes))
	}
	if pushes[0].Refspec != ":refs/heads/agent/deleted" {
		t.Fatalf("refspec=%q", pushes[0].Refspec)
	}
	if len(client.ensuredRepos) != 0 || len(client.ensuredPRs) != 0 {
		t.Fatalf("delete projection should not create repo or PR: repos=%v prs=%v", client.ensuredRepos, client.ensuredPRs)
	}
}

func TestHandlePushSkipsDeletedTagsAndExcludedBranches(t *testing.T) {
	client := &fakeClient{}
	var pushes []PushRequest
	integration := New(Config{
		Enabled:        true,
		BaseURL:        "http://forgejo.local",
		Token:          "secret-token",
		DefaultOwner:   "example-owner",
		BranchExcludes: []string{"main"},
	}, client, func(ctx context.Context, req PushRequest) error {
		pushes = append(pushes, req)
		return nil
	})

	err := integration.HandlePush(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/repos/example-owner/demo.git",
		Changes: []RefChange{
			{Ref: "refs/tags/v1", After: "abc123"},
			{Ref: "refs/heads/main", After: "abc123"},
			{Ref: "refs/heads/main", Deleted: true, After: "0000000000000000000000000000000000000000"},
		},
	})
	if err != nil {
		t.Fatalf("HandlePush returned error: %v", err)
	}
	if len(pushes) != 0 || len(client.ensuredRepos) != 0 || len(client.ensuredPRs) != 0 {
		t.Fatalf("unexpected work: pushes=%v repos=%v prs=%v", pushes, client.ensuredRepos, client.ensuredPRs)
	}
}

func TestHandlePushFailsFastForUnmappedOrgRepoWhenDefaultOwnerWouldMisroute(t *testing.T) {
	client := &fakeClient{}
	integration := New(Config{
		Enabled:      true,
		BaseURL:      "http://forgejo.local",
		Token:        "secret-token",
		DefaultOwner: "example-owner",
	}, client, nil)

	err := integration.HandlePush(context.Background(), PushEvent{
		RepoFullName: "example-team/imd",
		RepoPath:     "/repos/example-team/imd.git",
		Changes: []RefChange{{
			Ref:   "refs/heads/main",
			After: "abc123",
		}},
	})
	if err == nil {
		t.Fatal("expected unmapped org repo to fail fast")
	}
	if len(client.ensuredRepos) != 0 || len(client.ensuredPRs) != 0 {
		t.Fatalf("unexpected integration work: repos=%v prs=%v", client.ensuredRepos, client.ensuredPRs)
	}
}

func TestHandleDeletedRefFinalAuthorityDenialMakesZeroProviderWrites(t *testing.T) {
	client := &fakeClient{}
	pushes := 0
	integration := New(Config{
		Enabled: true, BaseURL: "http://forgejo.local", Token: "secret-token", DefaultOwner: "mirror",
		AutoCreateRepo: true, MirrorBranchIncludes: []string{"agent/*"},
		RepoMap: map[string]RepoMapping{"example-owner/demo": {Owner: "mirror", Repo: "demo"}},
	}, client, func(context.Context, PushRequest) error { pushes++; return nil })
	_, err := integration.HandlePushWithResult(context.Background(), PushEvent{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git",
		Changes:     []RefChange{{Ref: "refs/heads/agent/deleted", Before: "abc123", Deleted: true}},
		BeforeWrite: func(context.Context) error { return context.Canceled },
	})
	if err == nil || pushes != 0 || len(client.ensuredRepos) != 0 {
		t.Fatalf("delete denial err=%v pushes=%d ensured=%v", err, pushes, client.ensuredRepos)
	}
}
