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
