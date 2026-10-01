package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/artifactretirement"
	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

type retirementProviderState map[string]map[string]bool

const retirementForceJournalSchema = "ags.artifact-retirement.forgejo-force-window.v1"

type retirementForceJournal struct {
	Schema            string `json:"schema"`
	OperationID       string `json:"operation_id"`
	Repository        string `json:"repository"`
	Branch            string `json:"branch"`
	PolicyFingerprint string `json:"policy_fingerprint"`
}

func (s *Service) RunConfiguredArtifactRetirement(ctx context.Context, intentPath string) (artifactretirement.Receipt, error) {
	var receipt artifactretirement.Receipt
	intentPath = strings.TrimSpace(intentPath)
	if intentPath == "" {
		return receipt, nil
	}
	if !filepath.IsAbs(intentPath) {
		return receipt, errors.New("artifact retirement intent path must be absolute")
	}
	if s == nil || s.Git == nil || s.DBForCtx(ctx) == nil {
		return receipt, errors.New("artifact retirement requires an owning primary")
	}
	var runErr error
	err := s.Git.WithMaintenance(ctx, func(lockedCtx context.Context) error {
		receipt, runErr = s.runConfiguredArtifactRetirement(lockedCtx, intentPath)
		return runErr
	})
	return receipt, err
}

func (s *Service) runConfiguredArtifactRetirement(ctx context.Context, intentPath string) (artifactretirement.Receipt, error) {
	var receipt artifactretirement.Receipt
	intent, err := artifactretirement.LoadIntent(intentPath)
	if err != nil {
		return receipt, err
	}
	intentSHA, err := artifactretirement.IntentSHA256(intent)
	if err != nil {
		return receipt, err
	}
	// Metadata only: ordinary Git admission is already exclusively owned.
	var repo db.Repository
	if err := s.DBForCtx(ctx).WithContext(ctx).Where("full_name = ?", intent.Repository).First(&repo).Error; err != nil {
		return receipt, err
	}
	if repo.DefaultBranch != intent.DefaultBranch {
		return receipt, errors.New("artifact retirement default branch does not match repository metadata")
	}
	repoPath, err := s.Git.GetRepoPath(ctx, intent.Repository)
	if err != nil {
		return receipt, err
	}
	gate, err := artifactretirement.AcquireStartupGate(filepath.Dir(filepath.Dir(repoPath)), intentPath, repo.ID, intent)
	if err != nil {
		return receipt, err
	}
	defer gate.Close()
	stateRoot := filepath.Join(filepath.Dir(intentPath), ".artifact-retirement", intent.OperationID)
	if err := artifactretirement.EnsurePrivateStateDirectory(stateRoot); err != nil {
		return receipt, err
	}
	receiptPath := filepath.Join(stateRoot, "receipt.json")
	if existing, err := artifactretirement.LoadReceipt(receiptPath); err == nil {
		if existing.OperationID != intent.OperationID || existing.IntentSHA256 != intentSHA || existing.Repository != intent.Repository {
			return receipt, errors.New("artifact retirement receipt does not match intent")
		}
		if err := artifactretirement.VerifyRetiredAbsent(ctx, repoPath, intent.RetiredBlobs); err != nil {
			return receipt, err
		}
		if err := artifactretirement.InstallRetiredBlobPolicy(repoPath, intent.RetiredBlobs); err != nil {
			return receipt, err
		}
		if err := gate.Complete(); err != nil {
			return receipt, err
		}
		return existing, nil
	} else if !os.IsNotExist(err) {
		return receipt, err
	}
	if err := s.preflightArtifactRetirement(ctx, repo.ID); err != nil {
		return receipt, err
	}
	planPath := filepath.Join(stateRoot, "plan.json")
	plan, err := artifactretirement.LoadPlan(planPath)
	if os.IsNotExist(err) {
		roots, rootErr := s.GitMaintenanceRoots(ctx, repo)
		if rootErr != nil {
			return receipt, fmt.Errorf("artifact retirement root inventory: %w", rootErr)
		}
		plan, err = artifactretirement.PrepareWithApplicationRoots(ctx, repoPath, stateRoot, intent, roots)
		if err != nil {
			return receipt, err
		}
		plan.BeforeDiskKiB, err = artifactretirement.DiskKiB(repoPath)
		if err != nil {
			return receipt, err
		}
		if err := artifactretirement.SavePlan(planPath, plan); err != nil {
			return receipt, err
		}
	} else if err != nil {
		return receipt, err
	}
	if plan.IntentSHA256 != intentSHA || plan.OperationID != intent.OperationID || plan.Repository != intent.Repository || plan.DefaultBranch != intent.DefaultBranch || plan.StagingGitDir != filepath.Join(stateRoot, "staging.git") {
		return receipt, errors.New("artifact retirement saved plan does not match intent")
	}
	if err := s.recoverArtifactRetirementForgejoForce(ctx, repo, plan, stateRoot); err != nil {
		return receipt, err
	}
	head, err := artifactretirement.CurrentHead(ctx, repoPath, intent.DefaultBranch)
	if err != nil {
		return receipt, err
	}
	if head != plan.OriginalHead && head != plan.CleanHead {
		return receipt, errors.New("artifact retirement live default branch is neither planned old nor clean head")
	}
	if head == plan.OriginalHead {
		if _, statErr := os.Stat(plan.StagingGitDir); os.IsNotExist(statErr) {
			before := plan.BeforeDiskKiB
			roots, rootErr := s.GitMaintenanceRoots(ctx, repo)
			if rootErr != nil {
				return receipt, rootErr
			}
			rebuilt, rebuildErr := artifactretirement.PrepareWithApplicationRoots(ctx, repoPath, stateRoot, intent, roots)
			if rebuildErr != nil {
				return receipt, rebuildErr
			}
			rebuilt.BeforeDiskKiB = before
			if rebuilt.OriginalHead != plan.OriginalHead || rebuilt.CleanHead != plan.CleanHead || rebuilt.DefaultTree != plan.DefaultTree || !sameRetirementRefs(rebuilt.OriginalRefs, plan.OriginalRefs) || !sameRetirementRefs(rebuilt.CleanRefs, plan.CleanRefs) {
				return receipt, errors.New("artifact retirement deterministic resume plan changed")
			}
			plan = rebuilt
			if err := artifactretirement.SavePlan(planPath, plan); err != nil {
				return receipt, err
			}
		} else if statErr != nil {
			return receipt, statErr
		}
	}

	started := time.Now().UTC()
	providerRequirements, err := s.retirementProviderCheckpoint(ctx, repo, plan, stateRoot, head == plan.OriginalHead)
	if err != nil {
		return receipt, err
	}
	if head == plan.OriginalHead {
		if err := artifactretirement.VerifyCurrentRefs(ctx, repoPath, plan.OriginalRefs); err != nil {
			return receipt, err
		}
	}
	// The durable interlock is recorded before the first external write. Removing
	// the environment setting cannot start an ordinary primary halfway through.
	if err := gate.MarkPending(); err != nil {
		return receipt, err
	}
	providerPlan := plan
	if head == plan.CleanHead {
		providerPlan.StagingGitDir = repoPath
	}
	providerState, err := s.rewriteArtifactRetirementProviders(ctx, repo, providerPlan, stateRoot, providerRequirements)
	if err != nil {
		return receipt, err
	}
	if err := s.validateArtifactRetirementProviderCoverage(ctx, repo.ID, plan, providerState); err != nil {
		return receipt, err
	}
	if head == plan.OriginalHead {
		if err := artifactretirement.Publish(ctx, repoPath, plan); err != nil {
			return receipt, err
		}
	} else {
		if err := artifactretirement.FinalizePublished(ctx, repoPath, plan); err != nil {
			return receipt, err
		}
	}
	if err := s.reconcileArtifactRetirementActiveFacts(ctx, repo.ID, plan, providerState); err != nil {
		return receipt, err
	}
	after, err := artifactretirement.DiskKiB(repoPath)
	if err != nil {
		return receipt, err
	}
	changedCommits := 0
	for old, cleaned := range plan.CommitMap {
		if old != cleaned {
			changedCommits++
		}
	}
	retired := make([]string, 0, len(intent.RetiredBlobs))
	for _, blob := range intent.RetiredBlobs {
		retired = append(retired, blob.OID)
	}
	receipt = artifactretirement.Receipt{
		Schema: artifactretirement.ReceiptSchema, OperationID: intent.OperationID, IntentSHA256: intentSHA, Repository: intent.Repository,
		StartedAt: started, FinishedAt: time.Now().UTC(), Status: "completed", Phase: "complete",
		OriginalHead: plan.OriginalHead, CleanHead: plan.CleanHead, ChangedRefs: len(plan.RefUpdates),
		ChangedCommits: changedCommits, SignatureRemovals: plan.SignatureRemovals, RetiredBlobs: retired,
		BeforeDiskKiB: plan.BeforeDiskKiB, AfterDiskKiB: after,
		RecoveryArchive: intent.RecoveryArchive, RecoveryArchiveSHA: intent.RecoveryArchiveSHA256,
	}
	if err := artifactretirement.SaveReceipt(receiptPath, receipt); err != nil {
		return artifactretirement.Receipt{}, err
	}
	if err := gate.Complete(); err != nil {
		return artifactretirement.Receipt{}, err
	}
	_ = os.RemoveAll(plan.StagingGitDir)
	return receipt, nil
}

func (s *Service) preflightArtifactRetirement(ctx context.Context, repositoryID uint) error {
	database := s.DBForCtx(ctx).WithContext(ctx)
	var crossRepositoryCount int64
	if err := database.Model(&db.PullRequest{}).Where("state = ? AND merged = ? AND ((repository_id = ? AND head_repository_id IS NOT NULL AND head_repository_id <> ? AND head_repository_id <> 0) OR (head_repository_id = ? AND repository_id <> ?))", "open", false, repositoryID, repositoryID, repositoryID, repositoryID).Count(&crossRepositoryCount).Error; err != nil {
		return errors.New("artifact retirement cross-repository preflight failed")
	}
	if crossRepositoryCount != 0 {
		return errors.New("artifact retirement requires coordinated migration for open cross-repository pull requests")
	}
	var actionCount int64
	if err := database.Model(&db.PullRequestActionIntent{}).
		Where("repository_id = ? AND state IN ?", repositoryID, forgejoActionActiveStates()).
		Count(&actionCount).Error; err != nil {
		return errors.New("artifact retirement action-intent preflight failed")
	}
	if actionCount != 0 {
		return errors.New("artifact retirement refuses active provider action intents")
	}
	if _, err := s.terminalizeClosedForgejoProjectionJobs(ctx, repositoryID); err != nil {
		return errors.New("artifact retirement closed projection-job reconciliation failed")
	}
	var projectionCount int64
	if err := database.Model(&db.PullRequestProjectionJob{}).
		Where("repository_id = ? AND phase NOT IN ?", repositoryID, forgejoProjectionTerminalPhases).
		Count(&projectionCount).Error; err != nil {
		return errors.New("artifact retirement projection-job preflight failed")
	}
	if projectionCount != 0 {
		return errors.New("artifact retirement refuses nonterminal projection jobs")
	}
	return nil
}

func retirementForceJournalPath(stateRoot string) string {
	return filepath.Join(stateRoot, "forgejo-force-window.json")
}

func canonicalRetirementDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func loadRetirementForceJournal(stateRoot string) (retirementForceJournal, bool, error) {
	var journal retirementForceJournal
	path := retirementForceJournalPath(stateRoot)
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return journal, false, nil
	}
	if err != nil {
		return journal, false, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 || st.Size() > 64*1024 {
		return journal, false, errors.New("artifact retirement force journal is unsafe")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return journal, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&journal); err != nil {
		return journal, false, errors.New("artifact retirement force journal is invalid")
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return journal, false, errors.New("artifact retirement force journal has trailing data")
	}
	if journal.Schema != retirementForceJournalSchema || journal.OperationID == "" || journal.Repository == "" ||
		journal.Branch == "" || !canonicalRetirementDigest(journal.PolicyFingerprint) {
		return journal, false, errors.New("artifact retirement force journal identity is invalid")
	}
	return journal, true, nil
}

func saveRetirementForceJournal(stateRoot string, journal retirementForceJournal) error {
	if journal.Schema != retirementForceJournalSchema || journal.OperationID == "" || journal.Repository == "" ||
		journal.Branch == "" || !canonicalRetirementDigest(journal.PolicyFingerprint) {
		return errors.New("artifact retirement force journal identity is invalid")
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateRoot, ".forgejo-force-window-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, retirementForceJournalPath(stateRoot))
}

func removeRetirementForceJournal(stateRoot string) error {
	err := os.Remove(retirementForceJournalPath(stateRoot))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Service) recoverArtifactRetirementForgejoForce(ctx context.Context, repo db.Repository, plan artifactretirement.Plan, stateRoot string) error {
	journal, exists, err := loadRetirementForceJournal(stateRoot)
	if err != nil || !exists {
		return err
	}
	if journal.OperationID != plan.OperationID || journal.Repository != repo.FullName || journal.Branch != plan.DefaultBranch {
		return errors.New("artifact retirement force journal does not match the current plan")
	}
	if s.ForgejoIntegration == nil {
		return errors.New("artifact retirement cannot restore Forgejo policy without the configured integration")
	}
	if err := s.ForgejoIntegration.SetArtifactRetirementForce(ctx, repo.FullName, journal.Branch, journal.PolicyFingerprint, false); err != nil {
		return fmt.Errorf("artifact retirement restore interrupted Forgejo force policy: %w", err)
	}
	if err := removeRetirementForceJournal(stateRoot); err != nil {
		return errors.New("artifact retirement restored Forgejo policy but could not retire its journal")
	}
	return nil
}

func (s *Service) rewriteArtifactRetirementForgejoBranch(
	ctx context.Context,
	repo db.Repository,
	plan artifactretirement.Plan,
	stateRoot, branch string,
	update artifactretirement.BranchUpdate,
) (bool, error) {
	if s.ForgejoIntegration == nil {
		return false, nil
	}
	actual, checked, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, repo.FullName, plan.StagingGitDir, branch)
	if err != nil {
		return true, err
	}
	if !checked || strings.TrimSpace(actual) == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(actual)) {
	case strings.ToLower(update.New):
		return true, nil
	case strings.ToLower(update.Old):
	default:
		return true, fmt.Errorf("artifact retirement Forgejo branch %s drifted from both planned heads", branch)
	}
	policy, required, err := s.ForgejoIntegration.ArtifactRetirementForcePolicy(ctx, repo.FullName, branch)
	if err != nil {
		return true, err
	}
	if !required {
		return s.ForgejoIntegration.RewriteBranchWithLease(ctx, repo.FullName, plan.StagingGitDir, branch, update.Old, update.New)
	}
	journal := retirementForceJournal{
		Schema: retirementForceJournalSchema, OperationID: plan.OperationID, Repository: repo.FullName,
		Branch: branch, PolicyFingerprint: policy.PolicyFingerprint,
	}
	if err := saveRetirementForceJournal(stateRoot, journal); err != nil {
		return true, err
	}
	if err := s.ForgejoIntegration.SetArtifactRetirementForce(ctx, repo.FullName, branch, policy.PolicyFingerprint, true); err != nil {
		return true, err
	}
	handled, pushErr := s.ForgejoIntegration.RewriteBranchWithLease(ctx, repo.FullName, plan.StagingGitDir, branch, update.Old, update.New)
	restoreErr := s.ForgejoIntegration.SetArtifactRetirementForce(ctx, repo.FullName, branch, policy.PolicyFingerprint, false)
	if restoreErr != nil {
		return true, fmt.Errorf("artifact retirement Forgejo force policy restoration failed: %w", restoreErr)
	}
	if err := removeRetirementForceJournal(stateRoot); err != nil {
		return true, errors.New("artifact retirement Forgejo policy restored but force journal cleanup failed")
	}
	if pushErr != nil {
		return handled, pushErr
	}
	return handled, nil
}

func (s *Service) rewriteArtifactRetirementProviders(ctx context.Context, repo db.Repository, plan artifactretirement.Plan, stateRoot string, requirements map[string]map[string]bool) (retirementProviderState, error) {
	state := retirementProviderState{}
	var err error
	updates := retirementBranchUpdates(plan)
	// Check the protected base before changing any easier work branch. A
	// provider without a supported maintenance capability must fail before
	// partial external graph publication, not halfway through a sorted loop.
	if requirements[ProjectionProviderForgejo][plan.DefaultBranch] {
		if update, changed := updates[plan.DefaultBranch]; changed {
			actual, handled, err := s.ForgejoIntegration.RemoteBranchSHA(ctx, repo.FullName, plan.StagingGitDir, plan.DefaultBranch)
			if err != nil || !handled {
				return nil, errors.New("artifact retirement protected-base observation unavailable")
			}
			if actual != update.Old && actual != update.New {
				return nil, errors.New("artifact retirement protected-base provider drift")
			}
			if actual == update.Old {
				if _, _, err := s.ForgejoIntegration.ArtifactRetirementForcePolicy(ctx, repo.FullName, plan.DefaultBranch); err != nil {
					return nil, fmt.Errorf("artifact retirement protected-base preflight: %w", err)
				}
			}
		}
	}
	providers := make([]string, 0, len(requirements))
	for provider := range requirements {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		branches := make([]string, 0, len(requirements[provider]))
		for branch := range requirements[provider] {
			branches = append(branches, branch)
		}
		sort.Strings(branches)
		for _, branch := range branches {
			update, changed := updates[branch]
			if !changed {
				continue
			}
			var handled bool
			switch provider {
			case ProjectionProviderForgejo:
				handled, err = s.rewriteArtifactRetirementForgejoBranch(ctx, repo, plan, stateRoot, branch, update)
			case ProjectionProviderGitHub:
				handled, err = s.GitHubIntegration.RewriteBranchWithLease(ctx, repo.FullName, plan.StagingGitDir, branch, update.Old, update.New)
			default:
				return nil, fmt.Errorf("artifact retirement does not support provider %s", provider)
			}
			if err != nil {
				return nil, err
			}
			if !handled {
				return nil, fmt.Errorf("artifact retirement required provider branch %s/%s is absent", provider, branch)
			}
			markRetirementProvider(state, provider, branch)
		}
	}
	return state, nil
}

func (s *Service) artifactRetirementProviderRequirements(ctx context.Context, repo db.Repository, plan artifactretirement.Plan) (map[string]map[string]bool, error) {
	required := map[string]map[string]bool{}
	add := func(provider, branch string) {
		if required[provider] == nil {
			required[provider] = map[string]bool{}
		}
		required[provider][branch] = true
	}
	if s.ForgejoIntegration != nil && s.ForgejoIntegration.ArtifactRetirementConfigured(repo.FullName) {
		add(ProjectionProviderForgejo, plan.DefaultBranch)
	}
	if s.GitHubIntegration != nil && s.GitHubIntegration.ArtifactRetirementConfigured(repo.FullName) {
		add(ProjectionProviderGitHub, plan.DefaultBranch)
	}
	updates := retirementBranchUpdates(plan)
	var refs []db.ProjectionRefState
	database := s.DBForCtx(ctx).WithContext(ctx)
	if err := database.Where("repository_id = ?", repo.ID).Find(&refs).Error; err != nil {
		return nil, errors.New("artifact retirement provider requirement query failed")
	}
	for _, row := range refs {
		if !strings.HasPrefix(row.Ref, "refs/heads/") {
			continue
		}
		branch := strings.TrimPrefix(row.Ref, "refs/heads/")
		update, changed := updates[branch]
		if !changed || row.AGSSHA != update.Old {
			continue
		}
		switch row.Provider {
		case ProjectionProviderForgejo, ProjectionProviderGitHub:
			add(row.Provider, branch)
		default:
			return nil, fmt.Errorf("artifact retirement does not support active provider %s", row.Provider)
		}
	}
	var projections []db.PullRequestProjection
	if err := database.Table("pull_request_projections").
		Joins("JOIN pull_requests ON pull_requests.id = pull_request_projections.pull_request_id").
		Where("pull_request_projections.repository_id = ? AND pull_requests.state = ? AND pull_requests.merged = ?", repo.ID, "open", false).
		Find(&projections).Error; err != nil {
		return nil, errors.New("artifact retirement open projection requirement query failed")
	}
	for _, projection := range projections {
		update, changed := updates[projection.SourceBranch]
		if !changed || projection.LastSyncedSHA != update.Old {
			continue
		}
		switch projection.Provider {
		case ProjectionProviderForgejo, ProjectionProviderGitHub:
			add(projection.Provider, projection.SourceBranch)
		default:
			return nil, fmt.Errorf("artifact retirement does not support open provider %s", projection.Provider)
		}
	}
	return required, nil
}

func markRetirementProvider(state retirementProviderState, provider, branch string) {
	if state[provider] == nil {
		state[provider] = map[string]bool{}
	}
	state[provider][branch] = true
}

func retirementBranchUpdates(plan artifactretirement.Plan) map[string]artifactretirement.BranchUpdate {
	result := map[string]artifactretirement.BranchUpdate{}
	for _, update := range artifactretirement.BranchUpdates(plan) {
		result[update.Branch] = update
	}
	return result
}

func (s *Service) validateArtifactRetirementProviderCoverage(ctx context.Context, repositoryID uint, plan artifactretirement.Plan, state retirementProviderState) error {
	updates := retirementBranchUpdates(plan)
	database := s.DBForCtx(ctx).WithContext(ctx)
	var refs []db.ProjectionRefState
	if err := database.Where("repository_id = ?", repositoryID).Find(&refs).Error; err != nil {
		return errors.New("artifact retirement projection-state preflight failed")
	}
	for _, row := range refs {
		if !strings.HasPrefix(row.Ref, "refs/heads/") {
			continue
		}
		branch := strings.TrimPrefix(row.Ref, "refs/heads/")
		update, changed := updates[branch]
		if !changed || row.AGSSHA != update.Old {
			continue
		}
		switch row.Provider {
		case ProjectionProviderForgejo, ProjectionProviderGitHub:
			if !state[row.Provider][branch] {
				return fmt.Errorf("artifact retirement provider branch %s/%s was not rewritten", row.Provider, branch)
			}
		default:
			return fmt.Errorf("artifact retirement does not support active provider %s", row.Provider)
		}
	}
	var projections []db.PullRequestProjection
	if err := database.Table("pull_request_projections").
		Joins("JOIN pull_requests ON pull_requests.id = pull_request_projections.pull_request_id").
		Where("pull_request_projections.repository_id = ? AND pull_requests.state = ? AND pull_requests.merged = ?", repositoryID, "open", false).
		Find(&projections).Error; err != nil {
		return errors.New("artifact retirement open projection preflight failed")
	}
	for _, projection := range projections {
		update, changed := updates[projection.SourceBranch]
		if !changed || projection.LastSyncedSHA != update.Old {
			continue
		}
		switch projection.Provider {
		case ProjectionProviderForgejo, ProjectionProviderGitHub:
			if !state[projection.Provider][projection.SourceBranch] {
				return fmt.Errorf("artifact retirement open projection %s/%s was not rewritten", projection.Provider, projection.SourceBranch)
			}
		default:
			return fmt.Errorf("artifact retirement does not support open provider %s", projection.Provider)
		}
	}
	return nil
}

func (s *Service) reconcileArtifactRetirementActiveFacts(ctx context.Context, repositoryID uint, plan artifactretirement.Plan, state retirementProviderState) error {
	database := s.DBForCtx(ctx).WithContext(ctx)
	updates := retirementBranchUpdates(plan)
	return database.Transaction(func(tx *gorm.DB) error {
		var prs []db.PullRequest
		if err := tx.Where("(repository_id = ? OR head_repository_id = ?) AND state = ? AND merged = ?", repositoryID, repositoryID, "open", false).Find(&prs).Error; err != nil {
			return err
		}
		openIDs := make([]uint, 0, len(prs))
		for _, pr := range prs {
			changes := map[string]any{}
			if mapped := plan.CommitMap[pr.HeadSHA]; mapped != "" && mapped != pr.HeadSHA {
				changes["head_sha"] = mapped
			}
			if mapped := plan.CommitMap[pr.BaseSHA]; mapped != "" && mapped != pr.BaseSHA {
				changes["base_sha"] = mapped
			}
			if len(changes) != 0 {
				result := tx.Model(&db.PullRequest{}).Where("id = ? AND head_sha = ? AND base_sha = ?", pr.ID, pr.HeadSHA, pr.BaseSHA).Updates(changes)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return errors.New("artifact retirement open PR coordinates changed during reconciliation")
				}
			}
			openIDs = append(openIDs, pr.ID)
		}
		if len(openIDs) > 0 {
			var projections []db.PullRequestProjection
			if err := tx.Where("pull_request_id IN ?", openIDs).Find(&projections).Error; err != nil {
				return err
			}
			for _, projection := range projections {
				update, changed := updates[projection.SourceBranch]
				if !changed || projection.LastSyncedSHA != update.Old || !state[projection.Provider][projection.SourceBranch] {
					continue
				}
				result := tx.Model(&db.PullRequestProjection{}).Where("id = ? AND last_synced_sha = ?", projection.ID, update.Old).Update("last_synced_sha", update.New)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return errors.New("artifact retirement open projection changed during reconciliation")
				}
			}
		}
		var refs []db.ProjectionRefState
		if err := tx.Where("repository_id = ?", repositoryID).Find(&refs).Error; err != nil {
			return err
		}
		for _, row := range refs {
			if !strings.HasPrefix(row.Ref, "refs/heads/") {
				continue
			}
			branch := strings.TrimPrefix(row.Ref, "refs/heads/")
			update, changed := updates[branch]
			if !changed || !state[row.Provider][branch] {
				continue
			}
			changes := map[string]any{}
			if row.AGSSHA == update.Old {
				changes["ags_sha"] = update.New
			}
			if row.ExternalSHA == update.Old {
				changes["external_sha"] = update.New
			}
			if len(changes) != 0 {
				result := tx.Model(&db.ProjectionRefState{}).Where("id = ? AND ags_sha = ? AND external_sha = ?", row.ID, row.AGSSHA, row.ExternalSHA).Updates(changes)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return errors.New("artifact retirement current projection changed during reconciliation")
				}
			}
		}
		return nil
	})
}
