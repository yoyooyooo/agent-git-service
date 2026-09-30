package githubintegration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func retirementGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRewriteBranchWithLeaseUsesProviderOldHead(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	remote := filepath.Join(root, "remote.git")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	retirementGit(t, work, "init", "-b", "main")
	retirementGit(t, work, "config", "user.name", "Fixture")
	retirementGit(t, work, "config", "user.email", "fixture@example.test")
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retirementGit(t, work, "add", "file")
	retirementGit(t, work, "commit", "-m", "old")
	oldSHA := retirementGit(t, work, "rev-parse", "HEAD")
	retirementGit(t, root, "clone", "--bare", work, remote)
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retirementGit(t, work, "commit", "-am", "new")
	newSHA := retirementGit(t, work, "rev-parse", "HEAD")
	enabled := true
	integration := New(Config{
		Enabled: true,
		Repos: map[string]RepoMapping{
			"owner/repo": {Owner: "backup", Repo: "repo", RemoteURL: "file://" + remote, TargetBranch: "main", Enabled: &enabled},
		},
	}, nil)
	handled, err := integration.RewriteBranchWithLease(context.Background(), "owner/repo", work, "main", oldSHA, newSHA)
	if err != nil || !handled {
		t.Fatalf("rewrite: handled=%v err=%v", handled, err)
	}
	if got := retirementGit(t, remote, "rev-parse", "refs/heads/main"); got != newSHA {
		t.Fatalf("remote main=%s want %s", got, newSHA)
	}
	handled, err = integration.RewriteBranchWithLease(context.Background(), "owner/repo", work, "main", oldSHA, newSHA)
	if err != nil || !handled {
		t.Fatalf("idempotent rewrite: handled=%v err=%v", handled, err)
	}
	retirementGit(t, remote, "update-ref", "refs/heads/main", oldSHA, newSHA)
	other := retirementGit(t, work, "rev-parse", "HEAD^{tree}")
	_ = other // ensure no helper accidentally changes the source checkout.
	retirementGit(t, remote, "update-ref", "refs/heads/main", newSHA, oldSHA)
	// Move the remote to a third exact commit and prove the migration refuses it.
	if err := os.WriteFile(filepath.Join(work, "file"), []byte("third\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retirementGit(t, work, "commit", "-am", "third")
	third := retirementGit(t, work, "rev-parse", "HEAD")
	retirementGit(t, remote, "fetch", "file://"+work, third)
	retirementGit(t, remote, "update-ref", "refs/heads/main", third, newSHA)
	if _, err = integration.RewriteBranchWithLease(context.Background(), "owner/repo", work, "main", oldSHA, newSHA); err == nil {
		t.Fatal("drifted GitHub branch was overwritten")
	}
}
