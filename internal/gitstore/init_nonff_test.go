package gitstore_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/gitstore"
)

// TestInit_InstallsNonFFRejectHook is the unit-level guard for the fix to
// #1234: every bare repo the server creates must have a pre-receive hook
// that rejects non-fast-forward pushes on non-standard ref namespaces.
func TestInit_InstallsNonFFRejectHook(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "gitstore-nonff-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := gitstore.New(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repoName := "user/nonff-hook"
	if err := store.Init(ctx, repoName, "main", false); err != nil {
		t.Fatalf("Init: %v", err)
	}

	root, err := store.RepoRoot(ctx)
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	repoDir := filepath.Join(root, repoName+".git")
	if _, err := os.Stat(repoDir); err != nil {
		repoDir = filepath.Join(root, repoName)
	}
	hook := filepath.Join(repoDir, "hooks", "pre-receive")
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatalf("pre-receive hook not found: %v", err)
	}
	if info.Mode()&0o100 == 0 {
		t.Errorf("pre-receive hook is not executable: mode=%v", info.Mode())
	}
	body, err := os.ReadFile(hook)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	if !strings.Contains(string(body), "managed pre-receive dispatcher") {
		t.Fatalf("pre-receive dispatcher missing managed marker:\n%s", body)
	}
	managedHook := filepath.Join(repoDir, "hooks", "pre-receive.d", "50-gh-server-authority")
	managedBody, err := os.ReadFile(managedHook)
	if err != nil {
		t.Fatalf("read managed authority hook: %v", err)
	}
	// Sanity-check the authority logic is installed behind the dispatcher.
	for _, need := range []string{"refs/heads/*", "refs/tags/*", "merge-base --is-ancestor", "non-fast-forward", "AGS_DELEGATED_SESSION", "AGS_DELEGATED_PROTECTED_REFS", "default branch"} {
		if !strings.Contains(string(managedBody), need) {
			t.Errorf("managed authority hook missing expected fragment %q", need)
		}
	}
}

func TestInit_ReconcilesReceiveHookForExistingRepository(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := gitstore.New(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repoName := "user/existing-hook"
	if err := store.Init(ctx, repoName, "main", true); err != nil {
		t.Fatalf("initial Init: %v", err)
	}
	root, _ := store.RepoRoot(ctx)
	repoDir := filepath.Join(root, repoName+".git")
	if _, err := os.Stat(repoDir); err != nil {
		repoDir = filepath.Join(root, repoName)
	}
	hook := filepath.Join(repoDir, "hooks", "pre-receive")
	customBody := "#!/bin/sh\necho custom-policy >&2\nexit 0\n"
	if err := os.WriteFile(hook, []byte(customBody), 0o755); err != nil {
		t.Fatalf("replace hook: %v", err)
	}

	if err := store.Init(ctx, repoName, "main", false); err != nil {
		t.Fatalf("reconcile Init: %v", err)
	}
	dispatcher, err := os.ReadFile(hook)
	if err != nil {
		t.Fatalf("read reconciled hook: %v", err)
	}
	if !strings.Contains(string(dispatcher), "managed pre-receive dispatcher") {
		t.Fatalf("existing repository hook was not reconciled to dispatcher:\n%s", dispatcher)
	}
	preserved, err := os.ReadFile(filepath.Join(repoDir, "hooks", "pre-receive.d", "10-local-preserved"))
	if err != nil {
		t.Fatalf("read preserved custom hook: %v", err)
	}
	if string(preserved) != customBody {
		t.Fatalf("custom hook changed during reconciliation:\n%s", preserved)
	}
	managed, err := os.ReadFile(filepath.Join(repoDir, "hooks", "pre-receive.d", "50-gh-server-authority"))
	if err != nil {
		t.Fatalf("read managed hook: %v", err)
	}
	for _, need := range []string{"default branch", "AGS_DELEGATED_SESSION", "AGS_DELEGATED_PROTECTED_REFS"} {
		if !strings.Contains(string(managed), need) {
			t.Fatalf("managed authority hook missing %q:\n%s", need, managed)
		}
	}
	cmd := exec.Command(hook)
	cmd.Dir = repoDir
	cmd.Stdin = strings.NewReader(strings.Repeat("0", 40) + " " + strings.Repeat("0", 40) + " refs/heads/feature\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run reconciled dispatcher: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "custom-policy") {
		t.Fatalf("preserved custom hook was not dispatched:\n%s", output)
	}

	// Reconciliation is idempotent and must not duplicate or replace the
	// preserved repo-local policy.
	if err := store.Init(ctx, repoName, "main", false); err != nil {
		t.Fatalf("second reconcile Init: %v", err)
	}
	preservedAgain, err := os.ReadFile(filepath.Join(repoDir, "hooks", "pre-receive.d", "10-local-preserved"))
	if err != nil || string(preservedAgain) != customBody {
		t.Fatalf("second reconciliation changed custom hook: err=%v body=%q", err, preservedAgain)
	}
}

// TestReceive_RejectsNonFastForwardOnCustomRef drives the hook end-to-end:
// clone the bare repo, build two SIBLING commits on separate branches, push
// the first to a custom ref (refs/locks/*), then push the second over the
// same ref without --force. The pre-receive hook must reject with
// "non-fast-forward"; pre-fix the second push succeeded silently.
func TestReceive_RejectsNonFastForwardOnCustomRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmpDir, err := os.MkdirTemp("", "gitstore-nonff-push-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := gitstore.New(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repoName := "user/nonff-push"
	if err := store.Init(ctx, repoName, "main", true); err != nil {
		t.Fatalf("Init: %v", err)
	}
	root, _ := store.RepoRoot(ctx)
	bareDir := filepath.Join(root, repoName+".git")
	if _, err := os.Stat(bareDir); err != nil {
		bareDir = filepath.Join(root, repoName)
	}

	// Clone the bare repo to a working copy. `git clone` takes paths, not
	// -C, so bypass the shared runGit helper here.
	workDir := filepath.Join(tmpDir, "work")
	if out, err := exec.Command("git", "clone", bareDir, workDir).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	runGit(t, workDir, "config", "user.name", "tester")
	runGit(t, workDir, "config", "user.email", "tester@example.com")

	runGit(t, workDir, "checkout", "-b", "c1")
	runGit(t, workDir, "commit", "--allow-empty", "-m", "c1")
	c1 := runGit(t, workDir, "rev-parse", "HEAD")
	runGit(t, workDir, "checkout", "main")
	runGit(t, workDir, "checkout", "-b", "c2")
	runGit(t, workDir, "commit", "--allow-empty", "-m", "c2")
	c2 := runGit(t, workDir, "rev-parse", "HEAD")

	// Sanity: c2 must not be a descendant of c1.
	cmd := exec.Command("git", "-C", workDir, "merge-base", "--is-ancestor", c1, c2)
	if err := cmd.Run(); err == nil {
		t.Fatal("test setup: c2 is an ancestor of c1 — need two true siblings")
	}

	// Push c1 to a fresh custom ref — must succeed (ref creation is allowed).
	runGit(t, workDir, "push", bareDir, "c1:refs/locks/nonff-test")

	// Push c2 over the same ref WITHOUT --force — hook must reject.
	cmd = exec.Command("git", "-C", workDir, "push", bareDir, "c2:refs/locks/nonff-test")
	combined, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("non-FF push unexpectedly succeeded; server accepted sibling commit over existing ref\noutput: %s", combined)
	}
	if !strings.Contains(string(combined), "non-fast-forward") {
		t.Errorf("rejection reason missing from server output; want non-fast-forward error, got:\n%s", combined)
	}
}

func TestReceive_DelegatedSessionEnforcesBranchWriteCeiling(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmpDir := t.TempDir()
	store, err := gitstore.New(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repoName := "user/delegated-heads"
	if err := store.Init(ctx, repoName, "main", true); err != nil {
		t.Fatalf("Init: %v", err)
	}
	root, _ := store.RepoRoot(ctx)
	bareDir := filepath.Join(root, repoName+".git")
	if _, err := os.Stat(bareDir); err != nil {
		bareDir = filepath.Join(root, repoName)
	}

	clone := func(name string) string {
		t.Helper()
		dir := filepath.Join(tmpDir, name)
		if out, err := exec.Command("git", "clone", bareDir, dir).CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, out)
		}
		runGit(t, dir, "config", "user.name", "tester")
		runGit(t, dir, "config", "user.email", "tester@example.com")
		return dir
	}
	delegatedPush := func(dir string, args ...string) (string, error) {
		t.Helper()
		cmdArgs := append([]string{"-C", dir, "push"}, args...)
		cmd := exec.Command("git", cmdArgs...)
		cmd.Env = append(os.Environ(), "AGS_DELEGATED_SESSION=1", "AGS_DELEGATED_PROTECTED_REFS=refs/heads/main")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	first := clone("first")
	runGit(t, first, "checkout", "-b", "feature")
	runGit(t, first, "commit", "--allow-empty", "-m", "feature-v1")
	if out, err := delegatedPush(first, bareDir, "HEAD:refs/heads/feature"); err != nil {
		t.Fatalf("delegated feature push failed: %v\n%s", err, out)
	}
	runGit(t, first, "commit", "--allow-empty", "-m", "feature-v2")
	if out, err := delegatedPush(first, bareDir, "HEAD:refs/heads/feature"); err != nil {
		t.Fatalf("delegated fast-forward feature update failed: %v\n%s", err, out)
	}
	if out, err := delegatedPush(first, bareDir, "HEAD:refs/heads/main"); err == nil || !strings.Contains(out, "protected branch") {
		t.Fatalf("delegated protected branch update was not rejected: err=%v\n%s", err, out)
	}
	if out, err := delegatedPush(first, bareDir, ":refs/heads/feature"); err == nil || !strings.Contains(out, "may not delete") {
		t.Fatalf("delegated branch deletion was not rejected: err=%v\n%s", err, out)
	}

	second := clone("second")
	runGit(t, second, "checkout", "-b", "sibling")
	runGit(t, second, "commit", "--allow-empty", "-m", "feature-sibling")
	if out, err := delegatedPush(second, "--force", bareDir, "HEAD:refs/heads/feature"); err == nil || !strings.Contains(out, "non-fast-forward") {
		t.Fatalf("delegated non-fast-forward update was not rejected: err=%v\n%s", err, out)
	}
}

// TestReceive_RejectsNonFastForwardOnDefaultBranch protects the repository's
// authority line while still allowing rebases on ordinary work branches.
func TestReceive_RejectsNonFastForwardOnDefaultBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmpDir, err := os.MkdirTemp("", "gitstore-default-branch-nonff-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := gitstore.New(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repoName := "user/default-branch-nonff"
	if err := store.Init(ctx, repoName, "main", true); err != nil {
		t.Fatalf("Init: %v", err)
	}
	root, _ := store.RepoRoot(ctx)
	bareDir := filepath.Join(root, repoName+".git")
	if _, err := os.Stat(bareDir); err != nil {
		bareDir = filepath.Join(root, repoName)
	}

	workDir := filepath.Join(tmpDir, "work")
	if out, err := exec.Command("git", "clone", bareDir, workDir).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	runGit(t, workDir, "config", "user.name", "tester")
	runGit(t, workDir, "config", "user.email", "tester@example.com")

	runGit(t, workDir, "checkout", "-b", "candidate-a")
	runGit(t, workDir, "commit", "--allow-empty", "-m", "candidate-a")
	runGit(t, workDir, "checkout", "main")
	runGit(t, workDir, "checkout", "-b", "candidate-b")
	runGit(t, workDir, "commit", "--allow-empty", "-m", "candidate-b")

	// Advancing main to candidate-a is a fast-forward and remains allowed.
	runGit(t, workDir, "push", bareDir, "candidate-a:refs/heads/main")

	cmd := exec.Command("git", "-C", workDir, "push", "--force", bareDir, "candidate-b:refs/heads/main")
	combined, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("non-FF default-branch push unexpectedly succeeded\noutput: %s", combined)
	}
	if !strings.Contains(string(combined), "default branch") || !strings.Contains(string(combined), "non-fast-forward") {
		t.Fatalf("missing default-branch rejection detail:\n%s", combined)
	}

	cmd = exec.Command("git", "-C", workDir, "push", bareDir, ":refs/heads/main")
	combined, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("default-branch deletion unexpectedly succeeded\noutput: %s", combined)
	}
	if !strings.Contains(string(combined), "default branch") || !strings.Contains(string(combined), "deletion") {
		t.Fatalf("missing default-branch deletion detail:\n%s", combined)
	}
}

// TestReceive_AllowsNonFastForwardOnWorkHeads is the negative-space test: the
// hook must allow history rewrites on non-default refs/heads/* so existing
// PR-rebase flows keep working.
func TestReceive_AllowsNonFastForwardOnWorkHeads(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmpDir, err := os.MkdirTemp("", "gitstore-nonff-heads-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := gitstore.New(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repoName := "user/nonff-heads"
	if err := store.Init(ctx, repoName, "main", true); err != nil {
		t.Fatalf("Init: %v", err)
	}
	root, _ := store.RepoRoot(ctx)
	bareDir := filepath.Join(root, repoName+".git")
	if _, err := os.Stat(bareDir); err != nil {
		bareDir = filepath.Join(root, repoName)
	}

	workDir := filepath.Join(tmpDir, "work")
	if out, err := exec.Command("git", "clone", bareDir, workDir).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	runGit(t, workDir, "config", "user.name", "tester")
	runGit(t, workDir, "config", "user.email", "tester@example.com")

	runGit(t, workDir, "checkout", "-b", "feature")
	runGit(t, workDir, "commit", "--allow-empty", "-m", "v1")
	runGit(t, workDir, "push", bareDir, "feature:refs/heads/feature")

	// Force a new history: reset to main, commit again on feature.
	runGit(t, workDir, "checkout", "main")
	runGit(t, workDir, "branch", "-D", "feature")
	runGit(t, workDir, "checkout", "-b", "feature")
	runGit(t, workDir, "commit", "--allow-empty", "-m", "v2-diff-history")

	// --force push of a non-FF onto refs/heads/feature — the hook lets
	// non-default work branches through, so this should succeed (or at worst
	// be blocked by some other policy, but not by our hook's default-branch
	// protection).
	cmd := exec.Command("git", "-C", workDir, "push", "--force", bareDir, "feature:refs/heads/feature")
	combined, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(combined), "non-fast-forward push") {
		t.Errorf("hook rejected refs/heads/feature non-FF push; work-branch rebases must remain allowed\noutput: %s", combined)
	}
}
