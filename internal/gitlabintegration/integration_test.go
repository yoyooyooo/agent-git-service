package gitlabintegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(" tok \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadTokenFile(Config{TokenFile: path})
	if err != nil {
		t.Fatalf("LoadTokenFile: %v", err)
	}
	if cfg.Token != "tok" {
		t.Fatalf("token=%q", cfg.Token)
	}
}

func TestProjectEnvPushesConfiguredEnvBranch(t *testing.T) {
	var gotRepoPath, gotRemoteURL, gotRefspec string
	integration := New(Config{
		Enabled: true,
		BaseURL: "https://gitlab.example.local",
		Token:   "secret-token",
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectPath: "backup/demo", EnvBranches: map[string]string{"uat": "uat"}},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRepoPath = repoPath
		gotRemoteURL = remoteURL
		gotRefspec = refspec
		if token != "secret-token" {
			t.Fatalf("token=%q", token)
		}
		return nil
	})

	res, handled, err := integration.ProjectEnv(context.Background(), EnvProjectionRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/tmp/demo.git",
		Env:          "uat",
		SourceSHA:    "abc123",
	})
	if err != nil {
		t.Fatalf("ProjectEnv: %v", err)
	}
	if !handled {
		t.Fatal("env projection not handled")
	}
	if gotRepoPath != "/tmp/demo.git" || gotRefspec != "+abc123:refs/heads/uat" {
		t.Fatalf("repoPath=%q refspec=%q", gotRepoPath, gotRefspec)
	}
	if !strings.Contains(gotRemoteURL, "oauth2:secret-token@gitlab.example.local/backup/demo.git") {
		t.Fatalf("remoteURL=%q", gotRemoteURL)
	}
	if res.ProjectPath != "backup/demo" || res.Env != "uat" || res.TargetBranch != "uat" || res.PushedSHA != "abc123" {
		t.Fatalf("result=%#v", res)
	}
}

func TestEnsureShadowMergeRequestFinalAuthorityDriftMakesZeroProviderWrites(t *testing.T) {
	pushes := 0
	integration := New(Config{
		Enabled: true, BaseURL: "https://gitlab.example.local", Token: "secret-token", MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{"example-owner/demo": {ProjectPath: "backup/demo", TargetBranch: "main"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	})
	checks := 0
	_, _, err := integration.EnsureShadowMergeRequest(context.Background(), ShadowMergeRequestRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/tmp/demo.git", HeadBranch: "agent/demo", HeadSHA: "abc123", BaseBranch: "main",
		BeforeWrite: func(context.Context) error {
			checks++
			return context.Canceled
		},
	})
	if err == nil || checks != 1 || pushes != 0 {
		t.Fatalf("authority drift: checks=%d pushes=%d err=%v", checks, pushes, err)
	}
}

func TestEnsureShadowMergeRequestRechecksAuthorityBetweenPushAndMR(t *testing.T) {
	pushes := 0
	api := &fakeAPIClient{}
	integration := NewWithClient(Config{
		Enabled: true, BaseURL: "https://gitlab.example.local", Token: "secret-token", MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{"example-owner/demo": {ProjectPath: "backup/demo", TargetBranch: "main"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	}, api)
	checks := 0
	_, _, err := integration.EnsureShadowMergeRequest(context.Background(), ShadowMergeRequestRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/tmp/demo.git", HeadBranch: "agent/demo", HeadSHA: "abc123", BaseBranch: "main",
		BeforeWrite: func(context.Context) error {
			checks++
			if checks == 2 {
				return context.Canceled
			}
			return nil
		},
	})
	if err == nil || checks != 2 || pushes != 1 || api.ensureProject != "" {
		t.Fatalf("second authority check: checks=%d pushes=%d ensureProject=%q err=%v", checks, pushes, api.ensureProject, err)
	}
}

func TestPushRefFinalAuthorityDriftMakesZeroProviderWrites(t *testing.T) {
	pushes := 0
	integration := New(Config{
		Enabled: true, BaseURL: "https://gitlab.example.local", Token: "secret-token", MergeAuthority: "forgejo",
		MirrorBranchIncludes: []string{"agent/*"},
		Repos:                map[string]RepoMapping{"example-owner/demo": {ProjectPath: "backup/demo", TargetBranch: "main"}},
	}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	})
	checks := 0
	_, _, err := integration.PushRef(context.Background(), PushRefRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/tmp/demo.git", Ref: "refs/heads/agent/demo", SourceSHA: "abc123",
		BeforeWrite: func(context.Context) error {
			checks++
			return context.Canceled
		},
	})
	if err == nil || checks != 1 || pushes != 0 {
		t.Fatalf("authority drift: checks=%d pushes=%d err=%v", checks, pushes, err)
	}
}

func TestPushRefMirrorsEligibleBranchToSameGitLabBranch(t *testing.T) {
	var gotRefspec string
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "https://gitlab.example.local",
		Token:                "secret-token",
		MergeAuthority:       "forgejo",
		MirrorBranchIncludes: []string{"main", "agent/*"},
		MirrorBranchExcludes: []string{"ci/*"},
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectPath: "backup/demo", TargetBranch: "main"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	})

	res, handled, err := integration.PushRef(context.Background(), PushRefRequest{RepoFullName: "example-owner/demo", RepoPath: "/tmp/demo.git", Ref: "refs/heads/agent/demo", SourceSHA: "abc123", Forced: true})
	if err != nil {
		t.Fatalf("PushRef: %v", err)
	}
	if !handled || gotRefspec != "+abc123:refs/heads/agent/demo" {
		t.Fatalf("handled=%v refspec=%q", handled, gotRefspec)
	}
	if res.ProjectPath != "backup/demo" || res.Branch != "agent/demo" || res.PushedSHA != "abc123" || res.Deleted {
		t.Fatalf("result=%#v", res)
	}
}

func TestMirrorBranchEnabledSkipsDutyResolvePrefixes(t *testing.T) {
	cfg := Config{
		MirrorBranchExcludes: []string{"sync/upstream-resolve/", "sync/upstream-resolve/*", "sync/upstream-*"},
	}
	for _, branch := range []string{
		"sync/upstream-resolve/4bbcd043-conflict",
		"sync/upstream-resolve/handoff-7f489e5d-restore",
		"sync/upstream-resolve",
		"sync/upstream-foo",
	} {
		if cfg.MirrorBranchEnabled(branch) {
			t.Fatalf("MirrorBranchEnabled(%q)=true, want excluded", branch)
		}
	}
	for _, branch := range []string{"main", "master", "release/v1", "uat", "test", "agent/demo", "env/prod"} {
		if !cfg.MirrorBranchEnabled(branch) {
			t.Fatalf("MirrorBranchEnabled(%q)=false, want admitted", branch)
		}
	}
	globOnly := Config{MirrorBranchExcludes: []string{"sync/upstream-resolve/*", "sync/upstream-*"}}
	if globOnly.MirrorBranchEnabled("sync/upstream-resolve/4bbcd043-conflict") || globOnly.MirrorBranchEnabled("sync/upstream-foo") {
		t.Fatal("path.Match glob exclude should skip reported duty branches without prefix matching")
	}
	if !globOnly.MirrorBranchEnabled("main") {
		t.Fatal("glob-only exclude must not skip main")
	}
}

func TestPushRefSkipsDutyResolvePrefixBeforeGitLabWrite(t *testing.T) {
	var pushed bool
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "https://gitlab.example.local",
		Token:                "secret-token",
		MergeAuthority:       "forgejo",
		MirrorBranchExcludes: []string{"sync/upstream-resolve/", "sync/upstream-resolve/*", "sync/upstream-*"},
		Repos: map[string]RepoMapping{
			"example-team/shipping-fixture": {ProjectPath: "example-team/shipping-fixture"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		pushed = true
		return nil
	})

	_, handled, err := integration.PushRef(context.Background(), PushRefRequest{
		RepoFullName: "example-team/shipping-fixture",
		RepoPath:     "/tmp/merdi.git",
		Ref:          "refs/heads/sync/upstream-resolve/4bbcd043-conflict",
		SourceSHA:    "abc123",
	})
	if err != nil {
		t.Fatalf("PushRef: %v", err)
	}
	if handled || pushed {
		t.Fatalf("duty resolve branch should not be mirrored: handled=%v pushed=%v", handled, pushed)
	}
}

func TestPushRefSkipsExcludedBranchBeforeRepoMapping(t *testing.T) {
	var pushed bool
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "https://gitlab.example.local",
		Token:                "secret-token",
		MergeAuthority:       "forgejo",
		MirrorBranchIncludes: []string{"main"},
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectPath: "backup/demo"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		pushed = true
		return nil
	})

	_, handled, err := integration.PushRef(context.Background(), PushRefRequest{RepoFullName: "missing/repo", RepoPath: "/tmp/demo.git", Ref: "refs/heads/agent/demo", SourceSHA: "abc123"})
	if err != nil {
		t.Fatalf("PushRef: %v", err)
	}
	if handled || pushed {
		t.Fatalf("excluded branch should not be handled: handled=%v pushed=%v", handled, pushed)
	}
}

func TestPushRefDeletesEligibleBranch(t *testing.T) {
	var gotRefspec string
	integration := New(Config{
		Enabled:              true,
		BaseURL:              "https://gitlab.example.local",
		Token:                "secret-token",
		MergeAuthority:       "forgejo",
		MirrorBranchIncludes: []string{"agent/*"},
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectPath: "backup/demo"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	})

	res, handled, err := integration.PushRef(context.Background(), PushRefRequest{RepoFullName: "example-owner/demo", RepoPath: "/tmp/demo.git", Ref: "refs/heads/agent/demo", SourceSHA: zeroSHA, Deleted: true})
	if err != nil {
		t.Fatalf("PushRef: %v", err)
	}
	if !handled || gotRefspec != ":refs/heads/agent/demo" || !res.Deleted {
		t.Fatalf("handled=%v refspec=%q result=%#v", handled, gotRefspec, res)
	}
}

func TestPushBackupUsesProjectPathAndPRBaseBranch(t *testing.T) {
	var gotRepoPath, gotRemoteURL, gotRefspec string
	integration := New(Config{
		Enabled:        true,
		BaseURL:        "https://gitlab.example.local",
		Token:          "secret-token",
		MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectPath: "backup/demo", TargetBranch: "main"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRepoPath = repoPath
		gotRemoteURL = remoteURL
		gotRefspec = refspec
		if token != "secret-token" {
			t.Fatalf("token=%q", token)
		}
		return nil
	})

	res, handled, err := integration.PushBackup(context.Background(), PushRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/tmp/demo.git",
		SourceSHA:    "abc123",
		BaseBranch:   "develop",
	})
	if err != nil {
		t.Fatalf("PushBackup: %v", err)
	}
	if !handled {
		t.Fatal("backup not handled")
	}
	if gotRepoPath != "/tmp/demo.git" || gotRefspec != "abc123:refs/heads/develop" {
		t.Fatalf("repoPath=%q refspec=%q", gotRepoPath, gotRefspec)
	}
	if !strings.Contains(gotRemoteURL, "oauth2:secret-token@gitlab.example.local/backup/demo.git") {
		t.Fatalf("remoteURL=%q", gotRemoteURL)
	}
	if res.ProjectPath != "backup/demo" || res.TargetBranch != "develop" || res.PushedSHA != "abc123" {
		t.Fatalf("result=%#v", res)
	}
}

func TestPushBackupFallsBackToConfiguredTargetBranch(t *testing.T) {
	var gotRefspec string
	integration := New(Config{
		Enabled:        true,
		BaseURL:        "https://gitlab.example.local",
		Token:          "secret-token",
		MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectPath: "backup/demo", TargetBranch: "main"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	})

	res, handled, err := integration.PushBackup(context.Background(), PushRequest{RepoFullName: "example-owner/demo", RepoPath: "/tmp/demo.git", SourceSHA: "abc123"})
	if err != nil {
		t.Fatalf("PushBackup: %v", err)
	}
	if !handled || gotRefspec != "abc123:refs/heads/main" || res.TargetBranch != "main" {
		t.Fatalf("handled=%v refspec=%q result=%#v", handled, gotRefspec, res)
	}
}

type fakeAPIClient struct {
	ensureProject string
	ensureInput   MergeRequestInput
	closeProject  string
	closeSource   string
	closeTarget   string
	closeNote     string
}

func (f *fakeAPIClient) EnsureMergeRequest(ctx context.Context, project string, in MergeRequestInput) (MergeRequest, error) {
	f.ensureProject = project
	f.ensureInput = in
	return MergeRequest{IID: 7, WebURL: "https://gitlab.example.local/backup/demo/-/merge_requests/7"}, nil
}

func (f *fakeAPIClient) CloseMergeRequestForBranch(ctx context.Context, project, sourceBranch, targetBranch, note string) (MergeRequest, bool, error) {
	f.closeProject = project
	f.closeSource = sourceBranch
	f.closeTarget = targetBranch
	f.closeNote = note
	return MergeRequest{IID: 7, WebURL: "https://gitlab.example.local/backup/demo/-/merge_requests/7"}, true, nil
}

func TestHTTPClientEnsureMergeRequestReconcilesExistingMR(t *testing.T) {
	var patched bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/123/merge_requests" {
			_ = json.NewEncoder(w).Encode([]MergeRequest{{IID: 7, WebURL: "https://gitlab.example.local/demo/-/merge_requests/7", Title: "old", Description: "old body"}})
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/api/v4/projects/123/merge_requests/7" {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
			if body["title"] != "new" || body["description"] != "new body" {
				t.Fatalf("patch body=%#v", body)
			}
			patched = true
			_ = json.NewEncoder(w).Encode(MergeRequest{IID: 7, WebURL: "https://gitlab.example.local/demo/-/merge_requests/7", Title: "new", Description: "new body"})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	mr, err := client.EnsureMergeRequest(context.Background(), "123", MergeRequestInput{SourceBranch: "agent/test", TargetBranch: "main", Title: "new", Description: "new body"})
	if err != nil {
		t.Fatalf("EnsureMergeRequest: %v", err)
	}
	if !patched || mr.Title != "new" || mr.Description != "new body" {
		t.Fatalf("patched=%v mr=%#v", patched, mr)
	}
}

func TestEnsureShadowMergeRequestPushesBranchAndCreatesMR(t *testing.T) {
	var gotRefspec string
	api := &fakeAPIClient{}
	integration := NewWithClient(Config{
		Enabled:        true,
		BaseURL:        "https://gitlab.example.local",
		Token:          "secret-token",
		MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectID: "123", ProjectPath: "backup/demo", TargetBranch: "main"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	}, api)

	res, handled, err := integration.EnsureShadowMergeRequest(context.Background(), ShadowMergeRequestRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/tmp/demo.git",
		HeadBranch:   "agent/test",
		HeadSHA:      "abc123",
		BaseBranch:   "main",
		Title:        "Test PR",
		Body:         "body",
		AGSNumber:    5,
	})
	if err != nil {
		t.Fatalf("EnsureShadowMergeRequest: %v", err)
	}
	if !handled || gotRefspec != "+abc123:refs/heads/agent/test" {
		t.Fatalf("handled=%v refspec=%q", handled, gotRefspec)
	}
	if api.ensureProject != "123" || api.ensureInput.SourceBranch != "agent/test" || api.ensureInput.TargetBranch != "main" {
		t.Fatalf("api=%#v input=%#v", api.ensureProject, api.ensureInput)
	}
	if !strings.Contains(api.ensureInput.Description, "Do not merge this GitLab MR") || !strings.Contains(api.ensureInput.Description, "AGS PR: #5") {
		t.Fatalf("description=%q", api.ensureInput.Description)
	}
	if res.IID != 7 || res.WebURL == "" {
		t.Fatalf("result=%#v", res)
	}
}

func TestEnsureShadowMergeRequestUsesPRBaseBeforeConfiguredTargetBranch(t *testing.T) {
	api := &fakeAPIClient{}
	integration := NewWithClient(Config{
		Enabled:        true,
		BaseURL:        "https://gitlab.example.local",
		Token:          "secret-token",
		MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectID: "123", ProjectPath: "backup/demo", TargetBranch: "uat"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error { return nil }, api)

	res, handled, err := integration.EnsureShadowMergeRequest(context.Background(), ShadowMergeRequestRequest{
		RepoFullName: "example-owner/demo",
		RepoPath:     "/tmp/demo.git",
		HeadBranch:   "agent/test",
		HeadSHA:      "abc123",
		BaseBranch:   "main",
		Title:        "Test PR",
		AGSNumber:    5,
	})
	if err != nil {
		t.Fatalf("EnsureShadowMergeRequest: %v", err)
	}
	if !handled || api.ensureInput.TargetBranch != "main" || res.TargetBranch != "main" {
		t.Fatalf("handled=%v input=%#v result=%#v", handled, api.ensureInput, res)
	}
}

func TestPushBackupUsesPRBaseBeforeConfiguredTargetBranch(t *testing.T) {
	var gotRefspec string
	integration := New(Config{
		Enabled:        true,
		BaseURL:        "https://gitlab.example.local",
		Token:          "secret-token",
		MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectID: "123", ProjectPath: "backup/demo", TargetBranch: "uat"},
		},
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		gotRefspec = refspec
		return nil
	})

	res, handled, err := integration.PushBackup(context.Background(), PushRequest{RepoFullName: "example-owner/demo", RepoPath: "/tmp/demo.git", SourceSHA: "abc123", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("PushBackup: %v", err)
	}
	if !handled || gotRefspec != "abc123:refs/heads/main" || res.TargetBranch != "main" {
		t.Fatalf("handled=%v refspec=%q result=%#v", handled, gotRefspec, res)
	}
}

func TestCloseShadowMergeRequestCommentsAndClosesMR(t *testing.T) {
	api := &fakeAPIClient{}
	integration := NewWithClient(Config{
		Enabled:        true,
		BaseURL:        "https://gitlab.example.local",
		Token:          "secret-token",
		MergeAuthority: "forgejo",
		Repos: map[string]RepoMapping{
			"example-owner/demo": {ProjectID: "123", ProjectPath: "backup/demo", TargetBranch: "uat"},
		},
	}, nil, api)

	res, handled, err := integration.CloseShadowMergeRequest(context.Background(), CloseShadowMergeRequestRequest{
		RepoFullName: "example-owner/demo",
		HeadBranch:   "agent/test",
		BaseBranch:   "main",
		PRNumber:     9,
		MergedSHA:    "def456",
	})
	if err != nil {
		t.Fatalf("CloseShadowMergeRequest: %v", err)
	}
	if !handled || res.IID != 7 {
		t.Fatalf("handled=%v result=%#v", handled, res)
	}
	if api.closeProject != "123" || api.closeSource != "agent/test" || api.closeTarget != "main" {
		t.Fatalf("api close=%#v source=%q target=%q", api.closeProject, api.closeSource, api.closeTarget)
	}
	if !strings.Contains(api.closeNote, "Forgejo PR #9 merged") || !strings.Contains(api.closeNote, "def456") {
		t.Fatalf("note=%q", api.closeNote)
	}
}

func TestPushBackupSkipsNonForgejoAuthority(t *testing.T) {
	called := false
	integration := New(Config{
		Enabled:        true,
		BaseURL:        "https://gitlab.example.local",
		Token:          "secret-token",
		MergeAuthority: "gitlab",
	}, func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
		called = true
		return nil
	})
	_, handled, err := integration.PushBackup(context.Background(), PushRequest{RepoFullName: "example-owner/demo", RepoPath: "/tmp/demo.git", SourceSHA: "abc123", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("PushBackup: %v", err)
	}
	if handled || called {
		t.Fatalf("handled=%v called=%v", handled, called)
	}
}

func TestDeletedRefFinalAuthorityDenialMakesZeroProviderWrites(t *testing.T) {
	pushes := 0
	integration := New(Config{Enabled: true, BaseURL: "http://gitlab.local", Token: "token", Repos: map[string]RepoMapping{"example-owner/demo": {ProjectPath: "backup/demo"}}}, func(context.Context, string, string, string, string) error {
		pushes++
		return nil
	})
	calls := 0
	_, handled, err := integration.PushRef(context.Background(), PushRefRequest{
		RepoFullName: "example-owner/demo", RepoPath: "/repos/example-owner/demo.git", Ref: "refs/heads/agent/deleted", Deleted: true,
		BeforeWrite: func(context.Context) error { calls++; return context.Canceled },
	})
	if err == nil || handled || calls != 1 || pushes != 0 {
		t.Fatalf("delete denial err=%v handled=%v checks=%d pushes=%d", err, handled, calls, pushes)
	}
}
