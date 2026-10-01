package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

type retirementProviderFixture struct {
	force       bool
	fingerprint string
}

func (f *retirementProviderFixture) EnsureRepository(context.Context, string, string, bool) error {
	return nil
}
func (f *retirementProviderFixture) EnsurePullRequest(context.Context, forgejointegration.PullRequestRequest) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (f *retirementProviderFixture) UpdatePullRequestState(context.Context, string, string, int, string) (forgejointegration.PullRequestResult, error) {
	return forgejointegration.PullRequestResult{}, nil
}
func (f *retirementProviderFixture) InspectRepositoryAuthority(context.Context, string, string, string, string, string) (forgejointegration.RepositoryAuthorityState, error) {
	return forgejointegration.RepositoryAuthorityState{
		BaseBranchProtected: true, ForcePushBlocked: !f.force,
		IntegrationBotCollaborator: true, IntegrationBotAuthorized: true,
		IntegrationBotMergeAuthorized: true,
	}, nil
}
func (f *retirementProviderFixture) ApplyRepositoryAuthority(context.Context, string, string, string, string, string, string, forgejointegration.RepositoryAuthorityState) error {
	return nil
}
func (f *retirementProviderFixture) InspectArtifactRetirementProtection(context.Context, string, string, string) (forgejointegration.ArtifactRetirementProtectionState, error) {
	return forgejointegration.ArtifactRetirementProtectionState{Exists: true, ForcePushEnabled: f.force, PolicyFingerprint: f.fingerprint}, nil
}
func (f *retirementProviderFixture) SetArtifactRetirementForce(_ context.Context, _, _, _, fingerprint string, enabled bool) error {
	if fingerprint != f.fingerprint {
		return errors.New("fixture policy fingerprint drift")
	}
	f.force = enabled
	return nil
}

func TestArtifactRetirementPreflightRetiresClosedPullRequestProjectionJobs(t *testing.T) {
	svc, repo, owner := retirementServiceFixture(t)
	ctx := context.Background()
	if err := svc.Git.CreateBranch(ctx, repo.FullName, "closed-work", "main"); err != nil {
		t.Fatal(err)
	}
	head, err := svc.Git.WriteFile(ctx, repo.FullName, "closed-work", "closed.txt", "closed work", []byte("closed\n"))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := svc.CreatePR(ContextWithUser(ctx, owner), CreatePRInput{RepoFullName: repo.FullName, Title: "closed work", HeadRef: "closed-work", BaseRef: "main", AuthorLogin: owner.Login})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := svc.DB.Model(&db.PullRequest{}).Where("id = ?", pr.ID).Updates(map[string]any{"state": db.StateClosed, "closed_at": &now}).Error; err != nil {
		t.Fatal(err)
	}
	job := db.PullRequestProjectionJob{PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo, Trigger: ForgejoProjectionTriggerPullRequest, RepoFullName: repo.FullName, AGSPRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef, HeadSHA: head, Phase: ForgejoProjectionPhasePreflight, Attempt: 9}
	if err := svc.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.preflightArtifactRetirement(ctx, repo.ID); err != nil {
		t.Fatalf("closed historical projection blocked retirement: %v", err)
	}
	var got db.PullRequestProjectionJob
	if err := svc.DB.First(&got, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Phase != ForgejoProjectionPhaseFailedTerminal || got.LastErrorType != forgejointegration.ProjectionFailurePullRequestStateDrift || got.FinishedAt == nil {
		t.Fatalf("closed projection job not terminalized by retirement preflight: %+v", got)
	}
}

func TestRunConfiguredArtifactRetirementRequiresAbsoluteIntentPath(t *testing.T) {
	if _, err := (&Service{}).RunConfiguredArtifactRetirement(context.Background(), "relative-retirement.json"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative intent path was accepted: %v", err)
	}
}

func retirementRecoveryDigest(t *testing.T, path string) string {
	t.Helper()
	digest, err := artifactretirement.ReferenceSnapshotSHA256(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestArtifactRetirementProviderRequirementsIgnoreDriftEvidenceButKeepOpenProjection(t *testing.T) {
	svc, repo, owner := retirementServiceFixture(t)
	ctx := context.Background()
	enabled := true
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true,
		RepoMap: map[string]forgejointegration.RepoMapping{
			repo.FullName: {Owner: "mirror", Repo: "repo", Enabled: &enabled, BaseBranch: "main"},
		},
	}, nil, nil)

	oldMain, newMain := strings.Repeat("1", 40), strings.Repeat("2", 40)
	oldDrift, newDrift := strings.Repeat("3", 40), strings.Repeat("4", 40)
	oldOpen, newOpen := strings.Repeat("5", 40), strings.Repeat("6", 40)
	plan := artifactretirement.Plan{
		Repository: repo.FullName, DefaultBranch: "main",
		RefUpdates: []artifactretirement.RefUpdate{
			{Ref: "refs/heads/main", Old: oldMain, New: newMain},
			{Ref: "refs/heads/stale-drift", Old: oldDrift, New: newDrift},
			{Ref: "refs/heads/open-work", Old: oldOpen, New: newOpen},
		},
	}
	now := time.Now().UTC()
	if err := svc.DB.Create(&db.ProjectionRefState{
		Provider: ProjectionProviderForgejo, RepositoryID: repo.ID, RepoFullName: repo.FullName,
		Ref: "refs/heads/stale-drift", Branch: "stale-drift", Type: forgejointegration.ProjectionFailureSHADrift,
		Status: ProjectionStatusActive, Authority: ProjectionAuthorityAGS, AGSSHA: oldDrift,
		ExternalSHA: strings.Repeat("7", 40), FirstSeenAt: now, LastSeenAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	pr := db.PullRequest{
		Number: 901, RepositoryID: repo.ID, Title: "open retirement projection", State: db.StateOpen,
		AuthorID: owner.ID, HeadRef: "open-work", HeadSHA: oldOpen, BaseRef: "main", BaseSHA: oldMain,
	}
	if err := svc.DB.Create(&pr).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.DB.Create(&db.PullRequestProjection{
		PullRequestID: pr.ID, RepositoryID: repo.ID, Provider: ProjectionProviderForgejo,
		ExternalRepo: "mirror/repo", ExternalNumber: 42, SourceBranch: "open-work", TargetBranch: "main",
		State: ProjectionStateOpen, LastSyncedSHA: oldOpen,
	}).Error; err != nil {
		t.Fatal(err)
	}

	required, err := svc.artifactRetirementProviderRequirements(ctx, repo, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !required[ProjectionProviderForgejo]["main"] || !required[ProjectionProviderForgejo]["open-work"] {
		t.Fatalf("current authority missing from requirements: %#v", required)
	}
	if required[ProjectionProviderForgejo]["stale-drift"] {
		t.Fatalf("drift evidence gained provider write authority: %#v", required)
	}
	if err := svc.validateArtifactRetirementProviderCoverage(ctx, repo.ID, plan, retirementProviderState{
		ProjectionProviderForgejo: {"main": true, "open-work": true},
	}); err != nil {
		t.Fatalf("drift evidence blocked retirement coverage: %v", err)
	}
	var preserved db.ProjectionRefState
	if err := svc.DB.Where("repository_id = ? AND ref = ?", repo.ID, "refs/heads/stale-drift").First(&preserved).Error; err != nil {
		t.Fatal(err)
	}
	if preserved.Status != ProjectionStatusActive || preserved.AGSSHA != oldDrift {
		t.Fatalf("drift evidence was mutated by requirement evaluation: %+v", preserved)
	}
}

func TestArtifactRetirementResumeRevalidatesLegacyProviderCheckpoint(t *testing.T) {
	svc, repo, _ := retirementServiceFixture(t)
	ctx := context.Background()
	enabled := true
	svc.ForgejoIntegration = forgejointegration.New(forgejointegration.Config{
		Enabled: true,
		RepoMap: map[string]forgejointegration.RepoMapping{
			repo.FullName: {Owner: "mirror", Repo: "repo", Enabled: &enabled, BaseBranch: "main"},
		},
	}, nil, nil)
	plan := artifactretirement.Plan{
		IntentSHA256: strings.Repeat("a", 64), Repository: repo.FullName, DefaultBranch: "main",
		RefUpdates: []artifactretirement.RefUpdate{
			{Ref: "refs/heads/main", Old: strings.Repeat("1", 40), New: strings.Repeat("2", 40)},
			{Ref: "refs/heads/stale-drift", Old: strings.Repeat("3", 40), New: strings.Repeat("4", 40)},
		},
	}
	stateRoot := t.TempDir()
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := retirementProvidersCheckpoint{
		Schema: "ags.artifact-retirement.providers.v1", IntentSHA256: plan.IntentSHA256, RepositoryID: repo.ID,
		Required: map[string]map[string]bool{ProjectionProviderForgejo: {"main": true, "stale-drift": true}},
	}
	path := filepath.Join(stateRoot, "providers.json")
	if err := artifactretirement.SaveCheckpoint(path, legacy); err != nil {
		t.Fatal(err)
	}
	required, err := svc.retirementProviderCheckpoint(ctx, repo, plan, stateRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	if !required[ProjectionProviderForgejo]["main"] || required[ProjectionProviderForgejo]["stale-drift"] {
		t.Fatalf("legacy drift authority survived recomputation: %#v", required)
	}
	var saved retirementProvidersCheckpoint
	if err := artifactretirement.LoadCheckpoint(path, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Schema != retirementProviderCheckpointSchema || saved.Required[ProjectionProviderForgejo]["stale-drift"] {
		t.Fatalf("legacy checkpoint not upgraded safely: %#v", saved)
	}
}

func TestArtifactRetirementProtectedBaseFailurePrecedesAllExternalWrites(t *testing.T) {
	calls, pushes := 0, 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Error("capability preflight attempted provider mutation")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer api.Close()
	old, newSHA := strings.Repeat("1", 40), strings.Repeat("2", 40)
	integration := forgejointegration.NewWithGitCapabilities(forgejointegration.Config{Enabled: true, BaseURL: api.URL, AuthorityPolicyEnabled: true, IntegrationBot: "fixture-bot", RepoMap: map[string]forgejointegration.RepoMapping{"owner/repo": {Owner: "owner", Repo: "repo", BaseBranch: "main"}}}, nil,
		func(context.Context, forgejointegration.PushRequest) error { pushes++; return nil },
		func(context.Context, string, string, string) (string, error) { return old, nil })
	svc := &Service{ForgejoIntegration: integration}
	plan := artifactretirement.Plan{Repository: "owner/repo", DefaultBranch: "main", StagingGitDir: "/isolated/fixture.git", RefUpdates: []artifactretirement.RefUpdate{{Ref: "refs/heads/a-feature", Old: old, New: newSHA}, {Ref: "refs/heads/main", Old: old, New: newSHA}}}
	_, err := svc.rewriteArtifactRetirementProviders(context.Background(), db.Repository{FullName: "owner/repo"}, plan, t.TempDir(), map[string]map[string]bool{ProjectionProviderForgejo: {"a-feature": true, "main": true}})
	if err == nil || !strings.Contains(err.Error(), "protected-base preflight") || pushes != 0 || calls == 0 {
		t.Fatalf("capability failure allowed partial publication: %v pushes=%d calls=%d", err, pushes, calls)
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
	review := db.PullRequestReview{PullRequestID: pr.ID, AuthorLogin: "independent-reviewer", State: "APPROVED", CommitSHA: featureHead}
	if err := svc.DB.Create(&review).Error; err != nil {
		t.Fatal(err)
	}
	if approvals, _, err := svc.currentPRReviewState(ctx, pr); err != nil || approvals != 1 {
		t.Fatalf("fixture review not valid for original head: %d %v", approvals, err)
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
		RecoveryRefsSHA256:    retirementRecoveryDigest(t, repoPath),
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
	oldClone := filepath.Join(t.TempDir(), "old-checkout")
	oldCloneCmd := exec.Command("git", "clone", "--no-local", repoPath, oldClone)
	if output, err := oldCloneCmd.CombinedOutput(); err != nil {
		t.Fatalf("old clone fixture: %v %s", err, output)
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
	oldComparison, err := svc.Git.Compare(ctx, repo.FullName, originalHead, featureHead)
	if err != nil {
		t.Fatal(err)
	}
	newComparison, err := svc.Git.Compare(ctx, repo.FullName, migratedPR.BaseSHA, migratedPR.HeadSHA)
	if err != nil || oldComparison.AheadBy != newComparison.AheadBy || oldComparison.BehindBy != newComparison.BehindBy || oldComparison.MergeBaseSHA != newComparison.MergeBaseSHA || len(oldComparison.Files) != 1 || oldComparison.Files[0].Filename != "feature.txt" {
		t.Fatalf("old/new PR content comparison differs: old=%+v new=%+v err=%v", oldComparison, newComparison, err)
	}
	if preserved, err := svc.Git.IsHistoricalAncestor(ctx, repo.FullName, originalHead, migratedPR.HeadSHA); err != nil || !preserved {
		t.Fatalf("historical ancestry not preserved: %v %v", preserved, err)
	}
	if authorized, _ := svc.Git.IsAncestor(ctx, repo.FullName, originalHead, migratedPR.HeadSHA); authorized {
		t.Fatal("historical alias silently authorized literal ancestry")
	}
	oldDiff, err := svc.Git.DiffRaw(ctx, repo.FullName, originalHead, featureHead)
	if err != nil || !strings.Contains(oldDiff, "feature.txt") {
		t.Fatalf("historical PR diff missing: %q %v", oldDiff, err)
	}
	if approvals, _, err := svc.currentPRReviewState(ctx, migratedPR); err != nil || approvals != 0 {
		t.Fatalf("original approval authorized cleaned head: %d %v", approvals, err)
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
	if err != nil || maintenance.Phase != "complete" || maintenance.MissingApplicationObjects != 0 || maintenance.Status != "completed" {
		t.Fatalf("post-retirement automatic maintenance rejected renamed retention refs: %+v %v", maintenance, err)
	}
	again, err := svc.RunConfiguredArtifactRetirement(ctx, intentPath)
	if err != nil || again.CleanHead != receipt.CleanHead || !again.FinishedAt.Equal(receipt.FinishedAt) {
		t.Fatalf("idempotent resume: %+v %v", again, err)
	}
	changedIntent := intent
	changedIntent.RecoveryArchiveSHA256 = strings.Repeat("c", 64)
	changedData, _ := json.Marshal(changedIntent)
	if err := os.WriteFile(intentPath, changedData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunConfiguredArtifactRetirement(ctx, intentPath); err == nil || !strings.Contains(err.Error(), "receipt does not match intent") {
		t.Fatalf("changed intent reused a completion receipt: %v", err)
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
	pushOld := exec.Command("git", "-C", oldClone, "push", "origin", "HEAD:refs/heads/stale-checkout")
	if out, err := pushOld.CombinedOutput(); err == nil || !strings.Contains(string(out), "retired artifact") {
		t.Fatalf("stale checkout reintroduced retired history: %v %s", err, out)
	}
	if err := artifactretirement.VerifyRetiredAbsent(ctx, repoPath, intent.RetiredBlobs); err != nil {
		t.Fatal("rejected push left retired blobs in store", err)
	}
	pushNew := exec.Command("git", "-C", fresh, "push", "origin", "HEAD:refs/heads/clean-checkout")
	if out, err := pushNew.CombinedOutput(); err != nil {
		t.Fatalf("clean checkout push failed: %v %s", err, out)
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
		RecoveryArchive: "off-host://fixture/recovery.bundle", RecoveryArchiveSHA256: strings.Repeat("b", 64), RecoveryRefsSHA256: retirementRecoveryDigest(t, repoPath),
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
	tamperedIntent := intent
	tamperedIntent.RecoveryArchiveSHA256 = strings.Repeat("c", 64)
	tamperedData, _ := json.Marshal(tamperedIntent)
	if err := os.WriteFile(intentPath, tamperedData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunConfiguredArtifactRetirement(ctx, intentPath); err == nil || !strings.Contains(err.Error(), "saved plan does not match intent") {
		t.Fatalf("changed intent reused an existing plan: %v", err)
	}
	if err := os.WriteFile(intentPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	enabled := true
	providerHead := plan.OriginalHead
	svc.ForgejoIntegration = forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, BaseURL: "https://forgejo.example",
		RepoMap: map[string]forgejointegration.RepoMapping{
			repo.FullName: {Owner: "mirror", Repo: "repo", Enabled: &enabled, BaseBranch: "protected-other"},
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
	required, err := svc.retirementProviderCheckpoint(ctx, repo, plan, stateRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.rewriteArtifactRetirementProviders(ctx, repo, plan, stateRoot, required); err != nil {
		t.Fatal(err)
	}
	if err := artifactretirement.Publish(ctx, repoPath, plan); err != nil {
		t.Fatal(err)
	}

	// Simulate a third-party provider rewrite after local publication but
	// before the application reconciliation/receipt was durably recorded.
	providerHead = strings.Repeat("3", 40)
	if _, err := svc.RunConfiguredArtifactRetirement(ctx, intentPath); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("resume did not reject actual provider drift: %v", err)
	}
	if providerHead != strings.Repeat("3", 40) {
		t.Fatalf("resume overwrote third-party provider head: %s", providerHead)
	}
}

func TestArtifactRetirementForgejoForceWindowRestoresOnSuccessAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pushError bool
	}{
		{name: "success"},
		{name: "push_failure", pushError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &retirementProviderFixture{fingerprint: strings.Repeat("a", 64)}
			oldSHA, newSHA := strings.Repeat("1", 40), strings.Repeat("2", 40)
			providerHead := oldSHA
			enabled := true
			integration := forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
				Enabled: true, IntegrationBot: "ags-bot",
				RepoMap: map[string]forgejointegration.RepoMapping{
					"owner/repo": {Owner: "mirror", Repo: "repo", Enabled: &enabled, BaseBranch: "main"},
				},
			}, fixture, func(_ context.Context, req forgejointegration.PushRequest) error {
				if !fixture.force {
					t.Fatal("provider push ran without the durable force window")
				}
				if req.ForceWithLeaseRef != "refs/heads/main" || req.ForceWithLeaseSHA != oldSHA {
					t.Fatalf("unsafe provider lease: %+v", req)
				}
				if tc.pushError {
					return errors.New("fixture push failed")
				}
				providerHead = newSHA
				return nil
			}, func(context.Context, string, string, string) (string, error) {
				return providerHead, nil
			})
			svc := &Service{ForgejoIntegration: integration}
			repo := db.Repository{FullName: "owner/repo", DefaultBranch: "main"}
			plan := artifactretirement.Plan{
				OperationID: "force-window-fixture", Repository: repo.FullName, DefaultBranch: "main",
				StagingGitDir: t.TempDir(),
			}
			stateRoot := t.TempDir()
			handled, err := svc.rewriteArtifactRetirementForgejoBranch(context.Background(), repo, plan, stateRoot, "main",
				artifactretirement.BranchUpdate{Branch: "main", Old: oldSHA, New: newSHA})
			if !handled {
				t.Fatal("mapped Forgejo branch was not handled")
			}
			if tc.pushError && err == nil {
				t.Fatal("provider push failure was hidden")
			}
			if !tc.pushError && err != nil {
				t.Fatal(err)
			}
			if fixture.force {
				t.Fatal("temporary force policy survived the operation")
			}
			if _, exists, loadErr := loadRetirementForceJournal(stateRoot); loadErr != nil || exists {
				t.Fatalf("force journal survived a restored policy: exists=%v err=%v", exists, loadErr)
			}
			if !tc.pushError && providerHead != newSHA {
				t.Fatalf("provider head=%s want %s", providerHead, newSHA)
			}
		})
	}
}

func TestArtifactRetirementRecoversInterruptedForgejoForceWindow(t *testing.T) {
	fixture := &retirementProviderFixture{force: true, fingerprint: strings.Repeat("b", 64)}
	enabled := true
	integration := forgejointegration.NewWithGitCapabilities(forgejointegration.Config{
		Enabled: true, IntegrationBot: "ags-bot",
		RepoMap: map[string]forgejointegration.RepoMapping{
			"owner/repo": {Owner: "mirror", Repo: "repo", Enabled: &enabled, BaseBranch: "main"},
		},
	}, fixture, func(context.Context, forgejointegration.PushRequest) error {
		t.Fatal("recovery must restore policy before any provider push")
		return nil
	}, func(context.Context, string, string, string) (string, error) {
		return strings.Repeat("1", 40), nil
	})
	svc := &Service{ForgejoIntegration: integration}
	repo := db.Repository{FullName: "owner/repo", DefaultBranch: "main"}
	plan := artifactretirement.Plan{OperationID: "interrupted-force-window", Repository: repo.FullName, DefaultBranch: "main"}
	stateRoot := t.TempDir()
	if err := saveRetirementForceJournal(stateRoot, retirementForceJournal{
		Schema: retirementForceJournalSchema, OperationID: plan.OperationID, Repository: repo.FullName,
		Branch: "main", PolicyFingerprint: fixture.fingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.recoverArtifactRetirementForgejoForce(context.Background(), repo, plan, stateRoot); err != nil {
		t.Fatal(err)
	}
	if fixture.force {
		t.Fatal("recovery left force-push enabled")
	}
	if _, exists, err := loadRetirementForceJournal(stateRoot); err != nil || exists {
		t.Fatalf("recovery journal was not retired: exists=%v err=%v", exists, err)
	}
}
