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
			"example-owner/weird": {Owner: "mirror team", Repo: "demo/ci"},
		},
	})
	got, ok := integration.BrowserURL("example-owner/weird", "/pulls/1")
	if !ok {
		t.Fatal("expected a redirect target")
	}
	want := "http://forgejo.example.test/mirror%20team/demo%2Fci/pulls/1"
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
