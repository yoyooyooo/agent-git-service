package gitlabintegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMirrorFactsUsePushRefConfiguration(t *testing.T) {
	disabled := false
	for _, tc := range []struct {
		name       string
		ref        string
		change     func(*Config)
		wantTarget bool
	}{
		{name: "default mapping", ref: "refs/heads/main", wantTarget: true},
		{name: "mapped path", ref: "refs/heads/topic", change: func(c *Config) { c.Repos = map[string]RepoMapping{"example-owner/demo": {ProjectPath: "backup/demo"}} }, wantTarget: true},
		{name: "disabled integration", ref: "refs/heads/main", change: func(c *Config) { c.Enabled = false }},
		{name: "different authority", ref: "refs/heads/main", change: func(c *Config) { c.MergeAuthority = "gitlab" }},
		{name: "empty authority stays empty", ref: "refs/heads/main", change: func(c *Config) { c.MergeAuthority = "" }, wantTarget: true},
		{name: "missing credential", ref: "refs/heads/main", change: func(c *Config) { c.Token = "" }},
		{name: "missing base URL", ref: "refs/heads/main", change: func(c *Config) { c.BaseURL = "" }},
		{name: "disabled mapping", ref: "refs/heads/main", change: func(c *Config) { c.Repos = map[string]RepoMapping{"example-owner/demo": {Enabled: &disabled}} }},
		{name: "unmapped repository", ref: "refs/heads/main", change: func(c *Config) { c.Repos = map[string]RepoMapping{"other/repo": {ProjectPath: "other/repo"}} }},
		{name: "project ID alone cannot push", ref: "refs/heads/main", change: func(c *Config) { c.Repos = map[string]RepoMapping{"example-owner/demo": {ProjectID: "123"}} }},
		{name: "excluded prefix itself", ref: "refs/heads/sync/private", change: func(c *Config) { c.MirrorBranchExcludes = []string{"sync/private/"} }},
		{name: "excluded nested prefix", ref: "refs/heads/sync/private/one/two", change: func(c *Config) { c.MirrorBranchExcludes = []string{"sync/private/"} }},
		{name: "glob does not cross slash", ref: "refs/heads/topic/one/two", change: func(c *Config) { c.MirrorBranchIncludes = []string{"topic/*"} }},
		{name: "include exact ref", ref: "refs/heads/topic/one", change: func(c *Config) { c.MirrorBranchIncludes = []string{"topic/*"} }, wantTarget: true},
		{name: "exclude takes precedence", ref: "refs/heads/topic/one", change: func(c *Config) {
			c.MirrorBranchIncludes = []string{"topic/*"}
			c.MirrorBranchExcludes = []string{"topic/one"}
		}},
		{name: "tag is not mirrored", ref: "refs/tags/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Enabled: true, BaseURL: "https://gitlab.example.test", Token: "fixture-token", MergeAuthority: "forgejo"}
			if tc.change != nil {
				tc.change(&cfg)
			}
			pushes := 0
			var refspec string
			i := New(cfg, func(_ context.Context, _, _, spec, _ string) error { pushes++; refspec = spec; return nil })
			facts, err := i.MirrorFacts("example-owner/demo", tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if pushes != 0 {
				t.Fatal("facts performed a push")
			}
			if facts.Configuration == nil || facts.Configuration.MergeAuthority != cfg.MergeAuthority || facts.Ref != tc.ref {
				t.Fatalf("facts changed configured values: %+v", facts)
			}
			if (facts.Target != nil) != tc.wantTarget {
				t.Fatalf("target=%+v, want configured=%t", facts.Target, tc.wantTarget)
			}
			result, handled, pushErr := i.PushRef(context.Background(), PushRefRequest{RepoFullName: "example-owner/demo", RepoPath: "/fixture/repo.git", Ref: tc.ref, SourceSHA: strings.Repeat("a", 40)})
			if handled != tc.wantTarget || (pushes == 1) != tc.wantTarget {
				t.Fatalf("PushRef disagrees: handled=%t pushes=%d err=%v", handled, pushes, pushErr)
			}
			if facts.Target != nil && (facts.Target.ProjectPath != result.ProjectPath || facts.Target.Ref != "refs/heads/"+result.Branch || refspec != strings.Repeat("a", 40)+":"+facts.Target.Ref) {
				t.Fatalf("target differs from actual push: %+v", facts.Target)
			}
		})
	}
}

func TestMirrorFactsAreReadOnlyAndWhitelistConfiguration(t *testing.T) {
	requests := 0
	provider := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer provider.Close()
	i := New(Config{Enabled: true, BaseURL: provider.URL, MergeAuthority: "forgejo", Token: "sentinel-inline-secret", TokenFile: "/private/sentinel-token-file", Repos: map[string]RepoMapping{
		"example-owner/demo": {ProjectPath: "backup/demo", ProjectID: "123", TargetBranch: "sentinel-unrelated-target", EnvBranches: map[string]string{"uat": "sentinel-env-branch"}},
		"other/private-repo": {ProjectPath: "sentinel-other-project"},
	}}, func(context.Context, string, string, string, string) error {
		t.Fatal("facts invoked pusher")
		return nil
	})
	facts, err := i.MirrorFacts("example-owner/demo", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 0 || facts.Target == nil || facts.Target.Ref != "refs/heads/main" {
		t.Fatalf("unexpected facts/provider access: %+v requests=%d", facts, requests)
	}
	b, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sentinel-") || strings.Contains(string(b), "token") || strings.Contains(string(b), "other/private-repo") {
		t.Fatal("facts exposed fields outside the whitelist")
	}
	var absent *Integration
	empty, err := absent.MirrorFacts("example-owner/demo", "refs/heads/main")
	if err != nil || empty.Configuration != nil || empty.Target != nil {
		t.Fatalf("absent integration invented configuration: %+v %v", empty, err)
	}
}

func TestMirrorFactsNeverExposeCredentialBearingBaseURLs(t *testing.T) {
	for _, base := range []string{"https://user:sentinel-secret@gitlab.example.test", "https://gitlab.example.test?token=sentinel-secret", "https://gitlab.example.test#sentinel-secret", "https://%sentinel-secret", "file:///private/sentinel-secret"} {
		i := New(Config{Enabled: true, BaseURL: base, Token: "token"}, nil)
		facts, err := i.MirrorFacts("example-owner/demo", "refs/heads/main")
		if err == nil {
			t.Fatal("unsafe base URL accepted")
		}
		b, _ := json.Marshal(facts)
		if strings.Contains(string(b)+err.Error(), "sentinel-secret") {
			t.Fatal("base URL leaked through facts or error")
		}
	}
}
