package forgejointegration

import (
	"net/url"
	"testing"
)

// credentialedWebBaseURL builds a URL with an embedded userinfo credential. It
// is constructed rather than written as a literal so the string cannot be
// mistaken for a real operator credential in source review.
func credentialedWebBaseURL() string {
	return (&url.URL{Scheme: "http", User: url.UserPassword("synthetic-user", "synthetic-secret"), Host: "forgejo.example.test"}).String()
}

func webRedirectIntegration(t *testing.T, cfg Config) *Integration {
	t.Helper()
	return New(cfg, nil, nil)
}

func TestWebRedirectBaseURLRequiresExplicitSetting(t *testing.T) {
	enabled := true
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "disabled integration",
			cfg:  Config{Enabled: false, WebBaseURL: "https://forgejo.example.test"},
			want: "",
		},
		{
			name: "enabled without web base url does not fall back to the api base url",
			cfg:  Config{Enabled: enabled, BaseURL: "http://forgejo-api.internal:3000"},
			want: "",
		},
		{
			name: "enabled with web base url",
			cfg:  Config{Enabled: enabled, BaseURL: "http://forgejo-api.internal:3000", WebBaseURL: "http://forgejo.example.test:5555/"},
			want: "http://forgejo.example.test:5555",
		},
		{
			name: "credentialed web base url is rejected",
			cfg:  Config{Enabled: enabled, WebBaseURL: credentialedWebBaseURL()},
			want: "",
		},
		{
			name: "web base url with query is rejected",
			cfg:  Config{Enabled: enabled, WebBaseURL: "http://forgejo.example.test/?token=abc"},
			want: "",
		},
		{
			name: "non http scheme is rejected",
			cfg:  Config{Enabled: enabled, WebBaseURL: "ssh://forgejo.example.test"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webRedirectIntegration(t, tc.cfg).WebRedirectBaseURL(); got != tc.want {
				t.Fatalf("WebRedirectBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBrowserURLUsesConfiguredRepoMapping(t *testing.T) {
	disabled := false
	integration := webRedirectIntegration(t, Config{
		Enabled:    true,
		BaseURL:    "http://forgejo-api.internal:3000",
		WebBaseURL: "http://forgejo.example.test:5555",
		RepoMap: map[string]RepoMapping{
			"example-owner/demo": {Owner: "mirror-team", Repo: "demo-ci"},
			"example-owner/off":  {Enabled: &disabled},
		},
	})

	cases := []struct {
		name   string
		repo   string
		path   string
		want   string
		wantOK bool
	}{
		{name: "mapped owner and repo", repo: "example-owner/demo", path: "/pulls/875", want: "http://forgejo.example.test:5555/mirror-team/demo-ci/pulls/875", wantOK: true},
		{name: "repo root", repo: "example-owner/demo", path: "", want: "http://forgejo.example.test:5555/mirror-team/demo-ci", wantOK: true},
		{name: "unmapped repo keeps its own names", repo: "example-owner/other", path: "/pulls/1", want: "http://forgejo.example.test:5555/example-owner/other/pulls/1", wantOK: true},
		{name: "disabled mapping", repo: "example-owner/off", path: "/pulls/1", wantOK: false},
		{name: "malformed repo", repo: "example-owner", path: "/pulls/1", wantOK: false},
		{name: "query in path", repo: "example-owner/demo", path: "/pulls/1?token=abc", wantOK: false},
		{name: "fragment in path", repo: "example-owner/demo", path: "/pulls/1#x", wantOK: false},
		{name: "relative path", repo: "example-owner/demo", path: "pulls/1", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := integration.BrowserURL(tc.repo, tc.path)
			if ok != tc.wantOK {
				t.Fatalf("BrowserURL(%q, %q) ok = %v, want %v (got %q)", tc.repo, tc.path, ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("BrowserURL(%q, %q) = %q, want %q", tc.repo, tc.path, got, tc.want)
			}
			if !ok && got != "" {
				t.Fatalf("BrowserURL(%q, %q) returned a partial URL %q", tc.repo, tc.path, got)
			}
		})
	}
}

func TestBrowserURLEscapesOwnerAndRepo(t *testing.T) {
	integration := webRedirectIntegration(t, Config{
		Enabled:    true,
		BaseURL:    "http://forgejo.example.test",
		WebBaseURL: "http://forgejo.example.test",
		RepoMap: map[string]RepoMapping{
			"example-owner/weird": {Owner: "mirror team", Repo: "demo ci"},
		},
	})
	got, ok := integration.BrowserURL("example-owner/weird", "/pulls/1")
	if !ok {
		t.Fatal("expected a redirect target")
	}
	want := "http://forgejo.example.test/mirror%20team/demo%20ci/pulls/1"
	if got != want {
		t.Fatalf("BrowserURL = %q, want %q", got, want)
	}
}

func TestValidateWebBaseURL(t *testing.T) {
	for _, raw := range []string{"", "   ", "http://forgejo.example.test", "https://forgejo.example.test:5555/prefix"} {
		if err := ValidateWebBaseURL(raw); err != nil {
			t.Fatalf("ValidateWebBaseURL(%q) = %v, want nil", raw, err)
		}
	}
	for _, raw := range []string{"forgejo.example.test", "ftp://forgejo.example.test", credentialedWebBaseURL(), "http://forgejo.example.test/?a=1", "http://forgejo.example.test/#f"} {
		if err := ValidateWebBaseURL(raw); err == nil {
			t.Fatalf("ValidateWebBaseURL(%q) = nil, want error", raw)
		}
	}
}

func TestBrowserURLRejectsUnsafeCoordinatesAndPaths(t *testing.T) {
	integration := webRedirectIntegration(t, Config{Enabled: true, WebBaseURL: "https://forgejo.example.test/prefix"})
	for _, repo := range []string{"../demo", "alice/..", "alice/%2e%2e", "alice/demo/extra", "alice/de\\mo", "alice/de\x00mo"} {
		if got, ok := integration.BrowserURL(repo, "/pulls"); ok || got != "" {
			t.Errorf("unsafe repo %q: %q", repo, got)
		}
	}
	for _, path := range []string{"//evil.example", "/../user/login", "/src/%2e%2e", "/src/%252e%252e", "/src/%2fsecret", "/src/%5csecret", "/src/%0a", "/src//secret", "/src/%zz"} {
		if got, ok := integration.BrowserURL("alice/demo", path); ok || got != "" {
			t.Errorf("unsafe path %q: %q", path, got)
		}
	}
	got, ok := integration.BrowserURL("alice/demo", "/src/main/a%20b/%E4%B8%AD.txt")
	if !ok || got != "https://forgejo.example.test/prefix/alice/demo/src/main/a%20b/%E4%B8%AD.txt" {
		t.Fatalf("escaped path: %v %q", ok, got)
	}
	for _, repo := range []string{"../demo", "alice/..", "alice/demo/extra", "//evil.example", "alice/%252e%252e"} {
		if got, ok := integration.PullRequestBrowserURL("alice/demo", repo, 178); ok || got != "" {
			t.Errorf("unsafe projection %q: %q", repo, got)
		}
	}
	for _, target := range []RepoMapping{{Owner: "..", Repo: "demo"}, {Owner: "alice", Repo: "../escape"}, {Owner: "alice", Repo: "de%2fmo"}} {
		mapped := webRedirectIntegration(t, Config{Enabled: true, WebBaseURL: "https://forgejo.example.test", RepoMap: map[string]RepoMapping{"alice/demo": target}})
		if got, ok := mapped.BrowserURL("alice/demo", "/pulls"); ok || got != "" {
			t.Errorf("unsafe configured target %v: %q", target, got)
		}
	}
	for _, base := range []string{"https://forgejo.example.test/prefix/..", "https://forgejo.example.test/%2e%2e", "https://forgejo.example.test/%252e%252e", "https://forgejo.example.test/prefix//escape", "https://forgejo.example.test/?"} {
		if err := ValidateWebBaseURL(base); err == nil {
			t.Errorf("unsafe base %q accepted", base)
		}
	}
}
