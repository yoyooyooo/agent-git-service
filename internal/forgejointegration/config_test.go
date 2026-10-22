package forgejointegration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ngaut/agent-git-service/internal/attribution"
)

func TestLoadRepoMapFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "map.json")
	if err := os.WriteFile(path, []byte(`{
		"example-owner/demo": {"owner":"forgejo", "repo":"demo-ci", "base_branch":"develop", "auto_pr": false},
		"example-owner/skip": {"enabled": false}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{RepoMapFile: path, AutoPullRequest: true}
	loaded, err := LoadRepoMapFile(cfg)
	if err != nil {
		t.Fatalf("LoadRepoMapFile: %v", err)
	}
	if loaded.RepoMap["example-owner/demo"].Owner != "forgejo" || loaded.RepoMap["example-owner/demo"].Repo != "demo-ci" {
		t.Fatalf("unexpected mapping: %#v", loaded.RepoMap["example-owner/demo"])
	}
	if loaded.RepoMap["example-owner/demo"].AutoPR == nil || *loaded.RepoMap["example-owner/demo"].AutoPR {
		t.Fatalf("expected auto_pr false override")
	}
	if loaded.RepoMap["example-owner/skip"].Enabled == nil || *loaded.RepoMap["example-owner/skip"].Enabled {
		t.Fatalf("expected enabled=false mapping")
	}
}

func TestLoadTokenFileDoesNotLoadOperatorCredential(t *testing.T) {
	botFile := filepath.Join(t.TempDir(), "bot.token")
	if err := os.WriteFile(botFile, []byte("fixture-bot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, operatorFile := range []string{"", filepath.Join(t.TempDir(), "missing.token")} {
		loaded, err := LoadTokenFile(Config{TokenFile: botFile, AuthorityPolicyEnabled: true, AuthorityPolicyTokenFile: operatorFile, AuthorityPolicyToken: "fixture-operator"})
		if err != nil || loaded.Token != "fixture-bot" {
			t.Fatal("projection must use bot independently of operator", err)
		}
		if got := loaded.withDefaults().Token; got != "fixture-bot" {
			t.Fatal("constructor replaced bot credential")
		}
	}
}

func TestEmptyAttributionCannotOverrideProjectionCredential(t *testing.T) {
	for _, principals := range []map[string]attribution.Principal{nil, {}} {
		cfg, err := LoadTokenFile(Config{Token: "bot", AuthorityPolicyToken: "operator", Attribution: attribution.Config{Principals: principals}})
		if err != nil || cfg.Token != "bot" || cfg.withDefaults().Token != "bot" {
			t.Fatal("operator credential replaced projection credential", err)
		}
	}
}
