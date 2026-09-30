package artifactretirement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testGit(t *testing.T, dir string, input []byte, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Stdin = strings.NewReader(string(input))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func testFixture(t *testing.T) (bare, originalHead, historicalCommit, blobOID string, blob []byte) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare = filepath.Join(root, "live.git")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, work, nil, "init", "-b", "main")
	testGit(t, work, nil, "config", "user.name", "Fixture")
	testGit(t, work, nil, "config", "user.email", "fixture@example.test")
	engineDir := filepath.Join(work, "packages", "ags-cli", "libexec")
	if err := os.MkdirAll(engineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob = []byte(strings.Repeat("legacy-native-engine\n", 8192))
	if err := os.WriteFile(filepath.Join(engineDir, "ags-gh-linux-amd64"), blob, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, work, nil, "add", ".")
	testGit(t, work, nil, "commit", "-m", "vendor engine")
	historicalCommit = testGit(t, work, nil, "rev-parse", "HEAD")
	blobOID = testGit(t, work, nil, "rev-parse", "HEAD:packages/ags-cli/libexec/ags-gh-linux-amd64")
	testGit(t, work, nil, "tag", "-a", "engine-source", "-m", "historical source", historicalCommit)
	if err := os.Remove(filepath.Join(engineDir, "ags-gh-linux-amd64")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("current tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, work, nil, "add", "-A")
	testGit(t, work, nil, "commit", "-m", "retire engine from current tree")
	originalHead = testGit(t, work, nil, "rev-parse", "HEAD")
	testGit(t, root, nil, "clone", "--bare", work, bare)
	testGit(t, bare, nil, "update-ref", "refs/pull/1/head", historicalCommit)
	testGit(t, bare, nil, "update-ref", "refs/ags/retention/"+historicalCommit, historicalCommit)
	return bare, originalHead, historicalCommit, blobOID, blob
}

func TestPrepareAndPublishRetiresExactBlobAndPreservesHistoricalLookup(t *testing.T) {
	ctx := context.Background()
	bare, originalHead, historicalCommit, blobOID, blob := testFixture(t)
	sum := sha256.Sum256(blob)
	intent := Intent{
		Schema: IntentSchema, OperationID: "fixture-retirement", Repository: "owner/repo",
		DefaultBranch: "main", ExpectedAncestor: historicalCommit,
		RecoveryArchive:       "off-host://fixture/recovery.bundle",
		RecoveryArchiveSHA256: strings.Repeat("a", 64), AllowSignatureRemoval: true,
		RetiredBlobs: []BlobSpec{{OID: blobOID, Bytes: int64(len(blob)), SHA256: hex.EncodeToString(sum[:])}},
	}
	workRoot := filepath.Join(t.TempDir(), "state")
	plan, err := Prepare(ctx, bare, workRoot, intent)
	if err != nil {
		t.Fatal(err)
	}
	if plan.OriginalHead != originalHead || plan.CleanHead == originalHead || plan.DefaultTree == "" || plan.CommitMap[historicalCommit] == historicalCommit {
		t.Fatalf("unexpected plan: original=%s clean=%s historical=%s", plan.OriginalHead, plan.CleanHead, plan.CommitMap[historicalCommit])
	}
	if exists, err := objectPhysicallyExists(ctx, plan.StagingGitDir, blobOID); err != nil || exists {
		t.Fatal("retired blob remained in staging")
	}
	if got := testGit(t, plan.StagingGitDir, nil, "show", "-s", "--format=%s", historicalCommit); got != "vendor engine" {
		t.Fatalf("historical lookup through replace ref failed: %q", got)
	}
	cmd := exec.Command("git", "--git-dir="+plan.StagingGitDir, "cat-file", "-e", historicalCommit+"^{object}")
	cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("old commit object remained physically reachable after staging gc")
	}
	beforeTree := testGit(t, bare, nil, "rev-parse", originalHead+"^{tree}")
	if err := Publish(ctx, bare, plan); err != nil {
		t.Fatal(err)
	}
	if exists, err := objectPhysicallyExists(ctx, bare, blobOID); err != nil || exists {
		t.Fatal("retired blob remained in active repository")
	}
	if got := testGit(t, bare, nil, "rev-parse", "refs/heads/main"); got != plan.CleanHead {
		t.Fatalf("main=%s want %s", got, plan.CleanHead)
	}
	if got := testGit(t, bare, nil, "rev-parse", "refs/pull/1/head"); got != plan.CommitMap[historicalCommit] {
		t.Fatalf("pull ref was not rewritten: %s", got)
	}
	mappedHistorical := plan.CommitMap[historicalCommit]
	if cmd := exec.Command("git", "--git-dir="+bare, "show-ref", "--verify", "--quiet", "refs/ags/retention/"+historicalCommit); cmd.Run() == nil {
		t.Fatal("old retention ref name survived the commit rewrite")
	}
	if got := testGit(t, bare, nil, "rev-parse", "refs/ags/retention/"+mappedHistorical); got != mappedHistorical {
		t.Fatalf("retention ref was not renamed with its new identity: %s", got)
	}
	if got := testGit(t, bare, nil, "rev-parse", plan.CleanHead+"^{tree}"); got != beforeTree {
		t.Fatal("current main tree changed")
	}
	if got := testGit(t, bare, nil, "show", "-s", "--format=%s", historicalCommit); got != "vendor engine" {
		t.Fatalf("historical alias failed after publication: %q", got)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	testGit(t, filepath.Dir(fresh), nil, "clone", "--no-local", bare, fresh)
	if got := testGit(t, fresh, nil, "rev-parse", "HEAD^{tree}"); got != beforeTree {
		t.Fatal("fresh clone current tree changed")
	}
	freshGit := filepath.Join(fresh, ".git")
	if exists, err := objectPhysicallyExists(ctx, freshGit, blobOID); err != nil || exists {
		t.Fatal("fresh full clone still contains retired blob")
	}
	testGit(t, bare, nil, "fsck", "--full")
}

func TestPrepareRejectsCurrentTreeUseAndRefDrift(t *testing.T) {
	ctx := context.Background()
	bare, _, historicalCommit, blobOID, blob := testFixture(t)
	sum := sha256.Sum256(blob)
	intent := Intent{
		Schema: IntentSchema, OperationID: "fixture-drift", Repository: "owner/repo",
		DefaultBranch: "main", ExpectedAncestor: historicalCommit,
		RecoveryArchive:       "off-host://fixture/recovery.bundle",
		RecoveryArchiveSHA256: strings.Repeat("b", 64), AllowSignatureRemoval: true,
		RetiredBlobs: []BlobSpec{{OID: blobOID, Bytes: int64(len(blob)), SHA256: hex.EncodeToString(sum[:])}},
	}
	plan, err := Prepare(ctx, bare, filepath.Join(t.TempDir(), "state"), intent)
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, bare, nil, "update-ref", "refs/heads/drift", historicalCommit)
	if err := Publish(ctx, bare, plan); err == nil || !strings.Contains(err.Error(), "refs drifted") {
		t.Fatalf("expected fail-closed ref drift, got %v", err)
	}
}
