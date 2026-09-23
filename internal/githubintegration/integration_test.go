package githubintegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPushRefMirrorsEligibleBranchToSameGitHubBranch(t *testing.T) {
	var gotURL, gotRefspec string
	integration := New(Config{
		Enabled:              true,
		MirrorBranchIncludes: []string{"main", "agent/*"},
		Repos: map[string]RepoMapping{
			"operator/project-kit": {Owner: "example-org", Repo: "project-kit", TargetBranch: "main"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotURL = remoteURL
		gotRefspec = refspec
		return nil
	})

	res, handled, err := integration.PushRef(context.Background(), PushRefRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git",
		Ref: "refs/heads/agent/demo", SourceSHA: "abc123", Forced: true,
	})
	if err != nil {
		t.Fatalf("PushRef: %v", err)
	}
	if !handled || gotRefspec != "+abc123:refs/heads/agent/demo" {
		t.Fatalf("handled=%v refspec=%q", handled, gotRefspec)
	}
	if gotURL != "git@github.com:example-org/project-kit.git" {
		t.Fatalf("remote=%q", gotURL)
	}
	if res.TargetRepo != "example-org/project-kit" || res.Branch != "agent/demo" || res.PushedSHA != "abc123" || res.Deleted {
		t.Fatalf("result=%#v", res)
	}
}

func TestPushRefUsesHTTPSTokenWhenConfigured(t *testing.T) {
	var gotURL string
	integration := New(Config{
		Enabled: true,
		Token:   "secret-token",
		Repos: map[string]RepoMapping{
			"operator/project-kit": {Owner: "example-org", Repo: "project-kit"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotURL = remoteURL
		if token != "secret-token" {
			t.Fatalf("token=%q", token)
		}
		return nil
	})
	_, handled, err := integration.PushRef(context.Background(), PushRefRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git",
		Ref: "refs/heads/main", SourceSHA: "abc123",
	})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(gotURL, "x-access-token:secret-token@github.com/example-org/project-kit.git") {
		t.Fatalf("remote=%q", gotURL)
	}
}

func TestPushRefFinalAuthorityDriftMakesZeroProviderWrites(t *testing.T) {
	pushes := 0
	integration := New(Config{
		Enabled: true,
		Repos:   map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	})
	checks := 0
	_, _, err := integration.PushRef(context.Background(), PushRefRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git",
		Ref: "refs/heads/main", SourceSHA: "abc123",
		BeforeWrite: func(context.Context) error {
			checks++
			return context.Canceled
		},
	})
	if err == nil || checks != 1 || pushes != 0 {
		t.Fatalf("authority drift: checks=%d pushes=%d err=%v", checks, pushes, err)
	}
}

func TestPushRefSkipsUnmappedRepo(t *testing.T) {
	var pushed bool
	integration := New(Config{
		Enabled: true,
		Repos:   map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit"}},
	}, func(context.Context, string, string, string, string) error {
		pushed = true
		return nil
	})
	_, handled, err := integration.PushRef(context.Background(), PushRefRequest{
		RepoFullName: "operator/other", RepoPath: "/tmp/demo.git",
		Ref: "refs/heads/main", SourceSHA: "abc123",
	})
	if err != nil || handled || pushed {
		t.Fatalf("handled=%v pushed=%v err=%v", handled, pushed, err)
	}
}

func TestPushRefDeletesEligibleBranch(t *testing.T) {
	var gotRefspec string
	integration := New(Config{
		Enabled: true,
		Repos:   map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit"}},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	})
	res, handled, err := integration.PushRef(context.Background(), PushRefRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git",
		Ref: "refs/heads/agent/demo", SourceSHA: zeroSHA, Deleted: true,
	})
	if err != nil {
		t.Fatalf("PushRef: %v", err)
	}
	if !handled || gotRefspec != ":refs/heads/agent/demo" || !res.Deleted {
		t.Fatalf("handled=%v refspec=%q result=%#v", handled, gotRefspec, res)
	}
}

func TestPushBackupUsesPRBaseBeforeConfiguredTargetBranch(t *testing.T) {
	var gotRefspec string
	integration := New(Config{
		Enabled: true,
		Repos:   map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit", TargetBranch: "uat"}},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	})
	res, handled, err := integration.PushBackup(context.Background(), PushRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git",
		SourceSHA: "def456", BaseBranch: "main",
	})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if gotRefspec != "def456:refs/heads/main" || res.TargetBranch != "main" {
		t.Fatalf("refspec=%q result=%#v", gotRefspec, res)
	}
}

func TestPushBackupFallsBackToConfiguredTargetBranch(t *testing.T) {
	var gotRefspec string
	integration := New(Config{
		Enabled: true,
		Repos:   map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit", TargetBranch: "main"}},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	})
	res, handled, err := integration.PushBackup(context.Background(), PushRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git", SourceSHA: "def456",
	})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if gotRefspec != "def456:refs/heads/main" || res.TargetBranch != "main" {
		t.Fatalf("refspec=%q result=%#v", gotRefspec, res)
	}
}

func TestDisabledIntegrationIsNoop(t *testing.T) {
	var pushed bool
	integration := New(Config{Enabled: false}, func(context.Context, string, string, string, string) error {
		pushed = true
		return nil
	})
	_, handled, err := integration.PushRef(context.Background(), PushRefRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git",
		Ref: "refs/heads/main", SourceSHA: "abc123",
	})
	if err != nil || handled || pushed {
		t.Fatalf("handled=%v pushed=%v err=%v", handled, pushed, err)
	}
}

func TestPushBackupSkipsNonForgejoAuthority(t *testing.T) {
	called := false
	integration := New(Config{
		Enabled: true, Token: "secret-token", MergeAuthority: "github",
		Repos: map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit"}},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		called = true
		return nil
	})
	_, handled, err := integration.PushBackup(context.Background(), PushRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git", SourceSHA: "abc123", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("PushBackup: %v", err)
	}
	if handled || called {
		t.Fatalf("handled=%v called=%v", handled, called)
	}
}

type fakeAPIClient struct {
	ensureRepo  string
	ensureInput PullRequestInput
	closeRepo   string
	closeSource string
	closeTarget string
	closeNote   string
}

func (f *fakeAPIClient) EnsurePullRequest(ctx context.Context, repo string, in PullRequestInput) (PullRequest, error) {
	f.ensureRepo = repo
	f.ensureInput = in
	return PullRequest{Number: 8, HTMLURL: "https://github.com/example-org/project-kit/pull/8"}, nil
}

func (f *fakeAPIClient) ClosePullRequestForBranch(ctx context.Context, repo, sourceBranch, targetBranch, note string) (PullRequest, bool, error) {
	f.closeRepo = repo
	f.closeSource = sourceBranch
	f.closeTarget = targetBranch
	f.closeNote = note
	return PullRequest{Number: 8, HTMLURL: "https://github.com/example-org/project-kit/pull/8"}, true, nil
}

func TestEnsureShadowPullRequestFinalAuthorityDriftMakesZeroProviderWrites(t *testing.T) {
	pushes := 0
	integration := New(Config{
		Enabled: true, Token: "secret-token", MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit", TargetBranch: "main"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	})
	checks := 0
	_, _, err := integration.EnsureShadowPullRequest(context.Background(), ShadowPullRequestRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git", HeadBranch: "agent/demo", HeadSHA: "abc123", BaseBranch: "main",
		BeforeWrite: func(context.Context) error {
			checks++
			return context.Canceled
		},
	})
	if err == nil || checks != 1 || pushes != 0 {
		t.Fatalf("authority drift: checks=%d pushes=%d err=%v", checks, pushes, err)
	}
}

func TestEnsureShadowPullRequestRechecksAuthorityBetweenPushAndPR(t *testing.T) {
	pushes := 0
	api := &fakeAPIClient{}
	integration := NewWithClient(Config{
		Enabled: true, Token: "secret-token", MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit", TargetBranch: "main"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	}, api)
	checks := 0
	_, _, err := integration.EnsureShadowPullRequest(context.Background(), ShadowPullRequestRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git", HeadBranch: "agent/demo", HeadSHA: "abc123", BaseBranch: "main",
		BeforeWrite: func(context.Context) error {
			checks++
			if checks == 2 {
				return context.Canceled
			}
			return nil
		},
	})
	if err == nil || checks != 2 || pushes != 1 || api.ensureRepo != "" {
		t.Fatalf("second authority check: checks=%d pushes=%d ensureRepo=%q err=%v", checks, pushes, api.ensureRepo, err)
	}
}

func TestEnsureShadowPullRequestSkipsWithoutToken(t *testing.T) {
	var pushed bool
	api := &fakeAPIClient{}
	integration := NewWithClient(Config{
		Enabled: true, MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit"}},
	}, func(context.Context, string, string, string, string) error {
		pushed = true
		return nil
	}, api)
	_, handled, err := integration.EnsureShadowPullRequest(context.Background(), ShadowPullRequestRequest{
		RepoFullName: "operator/project-kit", RepoPath: "/tmp/demo.git", HeadBranch: "agent/demo", HeadSHA: "abc123", BaseBranch: "main",
	})
	if err != nil || handled || pushed || api.ensureRepo != "" {
		t.Fatalf("tokenless shadow PR: handled=%v pushed=%v ensure=%q err=%v", handled, pushed, api.ensureRepo, err)
	}
}

func TestEnsureShadowPullRequestPushesBranchAndCreatesPR(t *testing.T) {
	var gotRefspec string
	api := &fakeAPIClient{}
	integration := NewWithClient(Config{
		Enabled: true, Token: "secret-token", MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit", TargetBranch: "main"}},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	}, api)

	res, handled, err := integration.EnsureShadowPullRequest(context.Background(), ShadowPullRequestRequest{
		RepoFullName: "operator/project-kit",
		RepoPath:     "/tmp/demo.git",
		HeadBranch:   "agent/test",
		HeadSHA:      "abc123",
		BaseBranch:   "main",
		Title:        "Test PR",
		Body:         "body",
		AGSNumber:    5,
	})
	if err != nil {
		t.Fatalf("EnsureShadowPullRequest: %v", err)
	}
	if !handled || gotRefspec != "+abc123:refs/heads/agent/test" {
		t.Fatalf("handled=%v refspec=%q", handled, gotRefspec)
	}
	if api.ensureRepo != "example-org/project-kit" || api.ensureInput.SourceBranch != "agent/test" || api.ensureInput.TargetBranch != "main" {
		t.Fatalf("api=%#v input=%#v", api.ensureRepo, api.ensureInput)
	}
	if !strings.Contains(api.ensureInput.Body, "Do not merge this GitHub PR") || !strings.Contains(api.ensureInput.Body, "AGS PR: #5") {
		t.Fatalf("body=%q", api.ensureInput.Body)
	}
	if res.Number != 8 || res.WebURL == "" {
		t.Fatalf("result=%#v", res)
	}
}

func TestEnsureShadowPullRequestUsesPRBaseBeforeConfiguredTargetBranch(t *testing.T) {
	api := &fakeAPIClient{}
	integration := NewWithClient(Config{
		Enabled: true, Token: "secret-token", MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit", TargetBranch: "uat"}},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error { return nil }, api)

	res, handled, err := integration.EnsureShadowPullRequest(context.Background(), ShadowPullRequestRequest{
		RepoFullName: "operator/project-kit",
		RepoPath:     "/tmp/demo.git",
		HeadBranch:   "agent/test",
		HeadSHA:      "abc123",
		BaseBranch:   "main",
		Title:        "Test PR",
		AGSNumber:    5,
	})
	if err != nil {
		t.Fatalf("EnsureShadowPullRequest: %v", err)
	}
	if !handled || api.ensureInput.TargetBranch != "main" || res.TargetBranch != "main" {
		t.Fatalf("handled=%v input=%#v result=%#v", handled, api.ensureInput, res)
	}
}

func TestCloseShadowPullRequestCommentsAndClosesPR(t *testing.T) {
	api := &fakeAPIClient{}
	integration := NewWithClient(Config{
		Enabled: true, Token: "secret-token", MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{"operator/project-kit": {Owner: "example-org", Repo: "project-kit", TargetBranch: "uat"}},
	}, nil, api)

	res, handled, err := integration.CloseShadowPullRequest(context.Background(), CloseShadowPullRequestRequest{
		RepoFullName: "operator/project-kit",
		HeadBranch:   "agent/test",
		BaseBranch:   "main",
		PRNumber:     9,
		MergedSHA:    "def456",
	})
	if err != nil {
		t.Fatalf("CloseShadowPullRequest: %v", err)
	}
	if !handled || res.Number != 8 {
		t.Fatalf("handled=%v result=%#v", handled, res)
	}
	if api.closeRepo != "example-org/project-kit" || api.closeSource != "agent/test" || api.closeTarget != "main" {
		t.Fatalf("api close=%#v source=%q target=%q", api.closeRepo, api.closeSource, api.closeTarget)
	}
	if !strings.Contains(api.closeNote, "Forgejo PR #9 merged") || !strings.Contains(api.closeNote, "def456") {
		t.Fatalf("note=%q", api.closeNote)
	}
}

func TestHTTPClientEnsurePullRequestReconcilesExistingPR(t *testing.T) {
	var patched bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Method == http.MethodGet && r.URL.Path == "/repos/example-org/project-kit/pulls" {
			if r.URL.Query().Get("head") != "example-org:agent/test" || r.URL.Query().Get("base") != "main" {
				t.Fatalf("query=%v", r.URL.Query())
			}
			_ = json.NewEncoder(w).Encode([]PullRequest{{Number: 8, HTMLURL: "https://github.com/example-org/project-kit/pull/8", Title: "old", Body: "old body"}})
			return
		}
		if r.Method == http.MethodPatch && r.URL.Path == "/repos/example-org/project-kit/pulls/8" {
			patched = true
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
			if body["title"] != "new" || body["body"] != "new body" {
				t.Fatalf("patch=%#v", body)
			}
			_ = json.NewEncoder(w).Encode(PullRequest{Number: 8, HTMLURL: "https://github.com/example-org/project-kit/pull/8", Title: "new", Body: "new body"})
			return
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	pr, err := client.EnsurePullRequest(context.Background(), "example-org/project-kit", PullRequestInput{SourceBranch: "agent/test", TargetBranch: "main", Title: "new", Body: "new body"})
	if err != nil {
		t.Fatalf("EnsurePullRequest: %v", err)
	}
	if !patched || pr.Title != "new" || pr.Body != "new body" {
		t.Fatalf("patched=%v pr=%#v", patched, pr)
	}
}

func TestHTTPClientCreatesPullRequestWhenNoneOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/example-org/project-kit/pulls" {
			_ = json.NewEncoder(w).Encode([]PullRequest{})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/repos/example-org/project-kit/pulls" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(PullRequest{Number: 11, HTMLURL: "https://github.com/example-org/project-kit/pull/11", Title: "created"})
			return
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	pr, err := client.EnsurePullRequest(context.Background(), "example-org/project-kit", PullRequestInput{SourceBranch: "agent/test", TargetBranch: "main", Title: "created"})
	if err != nil {
		t.Fatalf("EnsurePullRequest: %v", err)
	}
	if pr.Number != 11 {
		t.Fatalf("pr=%#v", pr)
	}
}
