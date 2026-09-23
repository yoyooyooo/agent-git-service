package gitlabintegration

import (
	"net/url"
	"strings"
	"testing"
)

func TestRemoteURLKeepsNonHTTPTransportsCredentialFree(t *testing.T) {
	for _, base := range []string{"file:///tmp/provider-fixture", "ssh://git@provider.example.test"} {
		t.Run(strings.Split(base, ":")[0], func(t *testing.T) {
			remote, err := (Config{BaseURL: base, Token: "unused-http-token"}).authenticatedRemoteURL(RepoMapping{ProjectPath: "team/project"})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(remote)
			if err != nil || strings.Contains(remote, "unused-http-token") {
				t.Fatal("HTTP credential entered non-HTTP URL")
			}
			if parsed.Scheme == "file" && parsed.User != nil {
				t.Fatal("file URL acquired userinfo")
			}
			if parsed.Scheme == "ssh" && (parsed.User == nil || parsed.User.Username() != "git") {
				t.Fatal("operator SSH user changed")
			}
		})
	}
}
