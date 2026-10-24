package forgejointegration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefContractHasNoExecutionCapability(t *testing.T) {
	c := RefCapabilities()
	if c.Execute || c.Retire || c.StrictFence || c.ReversePRHistory || c.Reason != "strict_fence_unavailable" {
		t.Fatal("unsupported capability advertised", c)
	}
	c.Execute = true // modifying a returned report cannot enable any runtime
	if RefCapabilities().Execute {
		t.Fatal("mutable global capability")
	}
	b, _ := json.Marshal(RefRetirementReviewSubject{})
	for _, key := range []string{"plan_id", "issued_at", "expires_at", "review_id"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Fatal("review recursion")
		}
	}
}

func contractGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test", "GIT_ALLOW_PROTOCOL=file"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git failed: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// This test demonstrates a LIMIT, not a receiver implementation: native SHA CAS
// accepts F after F->G->F. Only a complete mutation journal can detect the ABA.
func TestRefContractSHACASDoesNotDetectABA(t *testing.T) {
	dir := t.TempDir()
	contractGit(t, dir, "init", "--bare")
	tree := contractGit(t, dir, "mktree")
	f := contractGit(t, dir, "commit-tree", tree, "-m", "F")
	g := contractGit(t, dir, "commit-tree", tree, "-p", f, "-m", "G")
	a := contractGit(t, dir, "commit-tree", tree, "-p", g, "-m", "A")
	ref := "refs/heads/topic"
	contractGit(t, dir, "update-ref", ref, f, "")
	contractGit(t, dir, "update-ref", ref, g, f)
	contractGit(t, dir, "update-ref", ref, f, g)
	contractGit(t, dir, "update-ref", ref, a, f) // succeeds despite unobserved intermediate history
	if contractGit(t, dir, "rev-parse", ref) != a {
		t.Fatal("CAS fixture did not commit")
	}
	if RefCapabilities().StrictFence {
		t.Fatal("SHA CAS falsely advertised as strict")
	}
}

// A controller changing its local claim cannot retract a write already sent to
// a receiver. Channels make the old-worker ordering deterministic, without sleep.
func TestRefContractOldWorkerCanCommitAfterLocalClaimChanges(t *testing.T) {
	dir := t.TempDir()
	contractGit(t, dir, "init", "--bare")
	tree := contractGit(t, dir, "mktree")
	f := contractGit(t, dir, "commit-tree", tree, "-m", "F")
	a := contractGit(t, dir, "commit-tree", tree, "-p", f, "-m", "A")
	ref := "refs/heads/topic"
	contractGit(t, dir, "update-ref", ref, f, "")
	sent, commit, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		close(sent)
		<-commit
		cmd := exec.Command("git", "-C", dir, "update-ref", ref, a, f)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}
		done <- cmd.Run()
	}()
	<-sent
	// Local claim replacement is deliberately unrelated to the receiver's refs.
	if err := os.WriteFile(filepath.Join(dir, "claim"), []byte("new-epoch"), 0600); err != nil {
		t.Fatal(err)
	}
	close(commit)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if contractGit(t, dir, "rev-parse", ref) != a {
		t.Fatal("old dispatched writer did not commit")
	}
	if RefCapabilities().Execute {
		t.Fatal("local claim falsely enabled execution")
	}
}
