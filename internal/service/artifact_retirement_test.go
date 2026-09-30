package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/artifactretirement"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func retirementServiceGit(t *testing.T, repoPath string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir=" + repoPath}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func retirementServiceFixture(t *testing.T) (*Service, db.Repository, db.User) {
	t.Helper()
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "retirement.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := database.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	store, err := gitstore.New(filepath.Join(t.TempDir(), "gitrepos"))
	if err != nil {
		t.Fatal(err)
	}
	owner := db.User{Login: "retirement-owner", Name: "retirement-owner", Type: db.TypeUser}
	if err := database.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{Name: "repo", FullName: "retirement-owner/repo", OwnerID: owner.ID, DefaultBranch: "main"}
	if err := database.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background(), repo.FullName, "main", true); err != nil {
		t.Fatal(err)
	}
	return &Service{DB: database, Git: store, BaseURL: "http://localhost"}, repo, owner
}

func TestRunConfiguredArtifactRetirementRequiresAbsoluteIntentPath(t *testing.T) {
	if _, err := (&Service{}).RunConfiguredArtifactRetirement(context.Background(), "relative-retirement.json"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative intent path was accepted: %v", err)
	}
}

func TestRunConfiguredArtifactRetirementRewritesHistoryAndIsIdempotent(t *testing.T) {
	svc, repo, owner := retirementServiceFixture(t)
	ctx := context.Background()
	engine := []byte(strings.Repeat("legacy-engine\n", 4096))
	engineCommit, err := svc.Git.WriteFile(ctx, repo.FullName, "main", "packages/ags-cli/libexec/ags-gh-linux-amd64", "vendor engine", engine)
	if err != nil {
		t.Fatal(err)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, repo.FullName)
	if err != nil {
		t.Fatal(err)
	}
	blobOID := retirementServiceGit(t, repoPath, "rev-parse", engineCommit+":packages/ags-cli/libexec/ags-gh-linux-amd64")
	if _, err := svc.Git.DeleteFileFromRepo(ctx, repo.FullName, "main", "packages/ags-cli/libexec/ags-gh-linux-amd64", "remove engine"); err != nil {
		t.Fatal(err)
	}
	originalHead := retirementServiceGit(t, repoPath, "rev-parse", "refs/heads/main")
	originalTree := retirementServiceGit(t, repoPath, "rev-parse", originalHead+"^{tree}")
	if err := svc.Git.CreateBranch(ctx, repo.FullName, "feature", "main"); err != nil {
		t.Fatal(err)
	}
	featureHead, err := svc.Git.WriteFile(ctx, repo.FullName, "feature", "feature.txt", "feature work", []byte("feature\n"))
	if err != nil {
		t.Fatal(err)
	}
	authCtx := ContextWithUser(ctx, owner)
	pr, err := svc.CreatePR(authCtx, CreatePRInput{
		RepoFullName: repo.FullName, Title: "active work", HeadRef: "feature", BaseRef: "main", AuthorLogin: owner.Login,
	})
	if err != nil {
		t.Fatal(err)
	}
	review := db.PullRequestReview{PullRequestID: pr.ID, AuthorLogin: owner.Login, State: "APPROVED", CommitSHA: featureHead}
	if err := svc.DB.Create(&review).Error; err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(engine)
	intent := artifactretirement.Intent{
		Schema:                artifactretirement.IntentSchema,
		OperationID:           "service-fixture-retirement",
		Repository:            repo.FullName,
		DefaultBranch:         "main",
		ExpectedAncestor:      engineCommit,
		RecoveryArchive:       "off-host://fixture/recovery.bundle",
		RecoveryArchiveSHA256: strings.Repeat("a", 64),
		AllowSignatureRemoval: true,
		RetiredBlobs: []artifactretirement.BlobSpec{{
			OID: blobOID, Bytes: int64(len(engine)), SHA256: hex.EncodeToString(sum[:]),
		}},
	}
	intentDir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(intentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	intentPath := filepath.Join(intentDir, "retirement.json")
	data, _ := json.Marshal(intent)
	if err := os.WriteFile(intentPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	receipt, err := svc.RunConfiguredArtifactRetirement(ctx, intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "completed" || receipt.OriginalHead != originalHead || receipt.CleanHead == originalHead || receipt.ChangedCommits == 0 || receipt.AfterDiskKiB <= 0 {
		t.Fatalf("receipt: %+v", receipt)
	}
	if got := retirementServiceGit(t, repoPath, "rev-parse", receipt.CleanHead+"^{tree}"); got != originalTree {
		t.Fatal("current tree changed")
	}
	var migratedPR db.PullRequest
	if err := svc.DB.First(&migratedPR, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if migratedPR.HeadSHA == featureHead || migratedPR.BaseSHA == originalHead {
		t.Fatalf("active PR coordinates were not moved to the cleaned graph: head=%s base=%s", migratedPR.HeadSHA, migratedPR.BaseSHA)
	}
	var historicalReview db.PullRequestReview
	if err := svc.DB.First(&historicalReview, review.ID).Error; err != nil {
		t.Fatal(err)
	}
	if historicalReview.CommitSHA != featureHead {
		t.Fatal("historical review identity was relabelled as a cleaned commit")
	}
	cmd := exec.Command("git", "--git-dir="+repoPath, "cat-file", "-e", blobOID+"^{object}")
	cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("retired blob remains physically available")
	}
	commit, err := svc.Git.GetCommit(ctx, repo.FullName, engineCommit)
	if err != nil || commit.Message != "vendor engine" {
		t.Fatalf("historical commit alias failed: %+v %v", commit, err)
	}
	maintenance, err := svc.Git.MaintainStorage(ctx, repo.FullName, func(scanCtx context.Context) ([]string, error) {
		return svc.GitMaintenanceRoots(scanCtx, repo)
	})
	if err != nil || maintenance.Phase != "complete" {
		t.Fatalf("post-retirement automatic maintenance rejected renamed retention refs: %+v %v", maintenance, err)
	}
	again, err := svc.RunConfiguredArtifactRetirement(ctx, intentPath)
	if err != nil || again.CleanHead != receipt.CleanHead || !again.FinishedAt.Equal(receipt.FinishedAt) {
		t.Fatalf("idempotent resume: %+v %v", again, err)
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	cmd = exec.Command("git", "clone", "--no-local", repoPath, fresh)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fresh clone: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "--git-dir="+filepath.Join(fresh, ".git"), "cat-file", "-e", blobOID+"^{object}")
	cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("fresh complete clone still contains retired blob")
	}
}

func TestArtifactRetirementResumeRevalidatesProviderBranch(t *testing.T) {
	svc, repo, _ := retirementServiceFixture(t)
	ctx := context.Background()
	engine := []byte(strings.Repeat("legacy-engine\n", 2048))
	engineCommit, err := svc.Git.WriteFile(ctx, repo.FullName, "main", "packages/ags-cli/libexec/ags-gh-linux-amd64", "vendor engine", engine)
	if err != nil {
		t.Fatal(err)
	}
	repoPath, err := svc.Git.GetRepoPath(ctx, repo.FullName)
	if err != nil {
		t.Fatal(err)
	}
	blobOID := retirementServiceGit(t, repoPath, "rev-parse", engineCommit+":packages/ags-cli/libexec/ags-gh-linux-amd64")
	if _, err := svc.Git.DeleteFileFromRepo(ctx, repo.FullName, "main", "packages/ags-cli/libexec/ags-gh-linux-amd64", "remove engine"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(engine)
	intent := artifactretirement.Intent{
		Schema: artifactretirement.IntentSchema, OperationID: "resume-provider-readback", Repository: repo.FullName,
		DefaultBranch: "main", ExpectedAncestor: engineCommit,
		RecoveryArchive: "off-host://fixture/recovery.bundle", RecoveryArchiveSHA256: strings.Repeat("b", 64),
		AllowSignatureRemoval: true,
		RetiredBlobs:          []artifactretirement.BlobSpec{{OID: blobOID, Bytes: int64(len(engine)), SHA256: hex.EncodeToString(sum[:])}},
	}
	intentDir := filepath.Join(t.TempDir(), "config")
	if err := os.MkdirAll(intentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	intentPath := filepath.Join(intentDir, "retirement.json")
	data, _ := json.Marshal(intent)
	if err := os.WriteFile(intentPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(intentDir, ".artifact-retirement", intent.OperationID)
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	plan, err := artifactretirement.Prepare(ctx, repoPath, stateRoot, intent)
	if err != nil {
		t.Fatal(err)
	}
	plan.BeforeDiskKiB, err = artifactretirement.DiskKiB(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := artifactretirement.SavePlan(filepath.Join(stateRoot, "plan.json"), plan); err != nil {
		t.Fatal(err)
	}

	enabled := true
	providerHead := plan.OriginalHead
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: "https://forgejo.example",
		RepoMap: map[string]forgejointegration.RepoMapping{
			repo.FullName: {Owner: "mirror", Repo: "repo", Enabled: &enabled},
		},
	}, nil, func(_ context.Context, req forgejointegration.PushRequest) error {
		if req.ForceWithLeaseSHA != plan.OriginalHead || req.ForceWithLeaseRef != "refs/heads/main" {
			t.Fatalf("unsafe provider rewrite: %+v", req)
		}
		providerHead = plan.CleanHead
		return nil
	}, func(context.Context, string, string, string) (string, error) {
		return providerHead, nil
	})
	if _, err := svc.rewriteArtifactRetirementProviders(ctx, repo, plan); err != nil {
		t.Fatal(err)
	}
	if err := artifactretirement.Publish(ctx, repoPath, plan); err != nil {
		t.Fatal(err)
	}

	// Simulate a third-party provider rewrite after local publication but
	// before the application reconciliation/receipt was durably recorded.
	providerHead = strings.Repeat("3", 40)
	if _, err := svc.RunConfiguredArtifactRetirement(ctx, intentPath); err == nil {
		t.Fatal("resume trusted stale DB provider evidence")
	}
	if providerHead != strings.Repeat("3", 40) {
		t.Fatalf("resume overwrote third-party provider head: %s", providerHead)
	}
}
