package integrations

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCIRunSelectorsInheritOnlyTheSameEnabledForgejoBrowserOrigin(t *testing.T) {
	for _, test := range []struct {
		name, api, kind, explicit, want string
		enabled                         bool
	}{
		{name: "same instance", api: "https://forgejo-api.example.test", kind: "forgejo", enabled: true, want: "https://forgejo-web.example.test"},
		{name: "trailing slash", api: "https://forgejo-api.example.test/", kind: "forgejo", enabled: true, want: "https://forgejo-web.example.test"},
		{name: "another host", api: "https://other.example.test", kind: "forgejo", enabled: true},
		{name: "another port", api: "https://forgejo-api.example.test:8443", kind: "forgejo", enabled: true},
		{name: "another mount", api: "https://forgejo-api.example.test/other", kind: "forgejo", enabled: true},
		{name: "disabled projection", api: "https://forgejo-api.example.test", kind: "forgejo"},
		{name: "different provider kind", api: "https://forgejo-api.example.test", kind: "github-actions", enabled: true},
		{name: "explicit CI browser origin", api: "https://forgejo-api.example.test", kind: "forgejo", enabled: true, explicit: "https://ci-web.example.test", want: "https://ci-web.example.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "integrations.yaml")
			body := fmt.Sprintf(`forgejo:
  enabled: %t
  base_url: https://forgejo-api.example.test
  web_base_url: https://forgejo-web.example.test
ci:
  backends:
    selected:
      kind: %s
      url: %s
      web_url: %q
      token_file: /run/secrets/provider-token
`, test.enabled, test.kind, test.api, test.explicit)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.CI.Backends["selected"].WebURL; got != test.want {
				t.Fatalf("CI browser origin = %q, want %q", got, test.want)
			}
			if cfg.CI.Backends["selected"].URL != test.api {
				t.Fatal("browser origin inheritance changed the API origin")
			}
		})
	}
}

func TestInheritedCIBrowserOriginIsValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	if err := os.WriteFile(path, []byte(`forgejo:
  enabled: true
  base_url: https://forgejo-api.example.test
  web_base_url: https://forgejo-web.example.test/?ambiguous=1
ci:
  backends:
    selected:
      kind: forgejo
      url: https://forgejo-api.example.test
      token_file: /run/secrets/provider-token
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Fatal("an invalid inherited browser origin was accepted")
	}
}
