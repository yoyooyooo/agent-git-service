package forgejointegration

import (
	"os"
	"path/filepath"
	"testing"
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

func TestLoadTokenFileUsesAdminBeforeLegacyTokenFile(t *testing.T) {
	adminFile := filepath.Join(t.TempDir(), "admin.token")
	if err := os.WriteFile(adminFile, []byte("fixture-admin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadTokenFile(Config{
		TokenFile:              filepath.Join(t.TempDir(), "missing-legacy.token"),
		AuthorityPolicyEnabled: true, AuthorityPolicyTokenFile: adminFile,
	})
	if err != nil || loaded.Token != "fixture-admin" {
		t.Fatal("legacy projection credential must not precede the admin credential", err)
	}
}

func TestLoadTokenFileRejectsEmptyAdminCredential(t *testing.T) {
	adminFile := filepath.Join(t.TempDir(), "admin.token")
	if err := os.WriteFile(adminFile, []byte("\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadTokenFile(Config{
		Token: "fixture-old-projection", AuthorityPolicyEnabled: true, AuthorityPolicyTokenFile: adminFile,
	})
	if err == nil {
		t.Fatal("empty explicit admin credential must not silently select the old projection token")
	}
}
