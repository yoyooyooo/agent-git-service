package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/artifactretirement"
	"github.com/ngaut/agent-git-service/internal/db"
	"gorm.io/gorm"
)

type retirementProviderState map[string]map[string]bool

func (s *Service) RunConfiguredArtifactRetirement(ctx context.Context, intentPath string) (artifactretirement.Receipt, error) {
	var receipt artifactretirement.Receipt
	intentPath = strings.TrimSpace(intentPath)
	if intentPath == "" {
		return receipt, nil
	}
	intent, err := artifactretirement.LoadIntent(intentPath)
	if err != nil {
		return receipt, err
	}
	repo, err := s.GetRepo(ctx, intent.Repository)
	if err != nil {
		return receipt, err
	}
	if repo.DefaultBranch != intent.DefaultBranch {
		return receipt, errors.New("artifact retirement default branch does not match repository metadata")
	}
	repoPath, err := s.Git.GetRepoPath(ctx, intent.Repository)
	if err != nil {
		return receipt, err
	}
	stateRoot := filepath.Join(filepath.Dir(intentPath), ".artifact-retirement", intent.OperationID)
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return receipt, err
	}
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		return receipt, err
	}
	receiptPath := filepath.Join(stateRoot, "receipt.json")
	if existing, err := artifactretirement.LoadReceipt(receiptPath); err == nil {
		if existing.OperationID != intent.OperationID || existing.Repository != intent.Repository {
			return receipt, errors.New("artifact retirement receipt does not match intent")
		}
		if err := artifactretirement.VerifyRetiredAbsent(ctx, repoPath, intent.RetiredBlobs); err != nil {
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
		plan, err = artifactretirement.Prepare(ctx, repoPath, stateRoot, intent)
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
	if plan.OperationID != intent.OperationID || plan.Repository != intent.Repository || plan.DefaultBranch != intent.DefaultBranch {
		return receipt, errors.New("artifact retirement saved plan does not match intent")
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
			rebuilt, rebuildErr := artifactretirement.Prepare(ctx, repoPath, stateRoot, intent)
			if rebuildErr != nil {
				return receipt, rebuildErr
			}
			rebuilt.BeforeDiskKiB = before
			if rebuilt.OriginalHead != plan.OriginalHead || rebuilt.CleanHead != plan.CleanHead || rebuilt.DefaultTree != plan.DefaultTree {
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
	providerState := retirementProviderState{}
	if head == plan.OriginalHead {
		providerState, err = s.rewriteArtifactRetirementProviders(ctx, repo, plan)
		if err != nil {
			return receipt, err
		}
		if err := s.validateArtifactRetirementProviderCoverage(ctx, repo.ID, plan, providerState); err != nil {
			return receipt, err
		}
		if err := artifactretirement.Publish(ctx, repoPath, plan); err != nil {
			return receipt, err
		}
	} else {
		// Publication is atomic at the ref transaction. A restart after it
		// resumes cleanup/DB reconciliation instead of attempting a second rewrite.
		if err := artifactretirement.FinalizePublished(ctx, repoPath, plan); err != nil {
			return receipt, err
		}
		providerState, err = s.inferArtifactRetirementProviderCoverage(ctx, repo.ID, plan)
		if err != nil {
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
		Schema: artifactretirement.ReceiptSchema, OperationID: intent.OperationID, Repository: intent.Repository,
		StartedAt: started, FinishedAt: time.Now().UTC(), Status: "completed", Phase: "complete",
		OriginalHead: plan.OriginalHead, CleanHead: plan.CleanHead, ChangedRefs: len(plan.RefUpdates),
		ChangedCommits: changedCommits, SignatureRemovals: plan.SignatureRemovals, RetiredBlobs: retired,
		BeforeDiskKiB: plan.BeforeDiskKiB, AfterDiskKiB: after,
		RecoveryArchive: intent.RecoveryArchive, RecoveryArchiveSHA: intent.RecoveryArchiveSHA256,
	}
	if err := artifactretirement.SaveReceipt(receiptPath, receipt); err != nil {
		return artifactretirement.Receipt{}, err
	}
	_ = os.RemoveAll(plan.StagingGitDir)
	return receipt, nil
}

func (s *Service) preflightArtifactRetirement(ctx context.Context, repositoryID uint) error {
	database := s.DBForCtx(ctx).WithContext(ctx)
	var actionCount int64
	if err := database.Model(&db.PullRequestActionIntent{}).
		Where("repository_id = ? AND state IN ?", repositoryID, forgejoActionActiveStates()).
		Count(&actionCount).Error; err != nil {
		return errors.New("artifact retirement action-intent preflight failed")
	}
	if actionCount != 0 {
		return errors.New("artifact retirement refuses active provider action intents")
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

func (s *Service) rewriteArtifactRetirementProviders(ctx context.Context, repo db.Repository, plan artifactretirement.Plan) (retirementProviderState, error) {
	state := retirementProviderState{}
	requirements, err := s.artifactRetirementProviderRequirements(ctx, repo, plan)
	if err != nil {
		return nil, err
	}
	updates := retirementBranchUpdates(plan)
	for provider, branches := range requirements {
		for branch := range branches {
			update, changed := updates[branch]
			if !changed {
				continue
			}
			var handled bool
			switch provider {
			case ProjectionProviderForgejo:
				handled, err = s.ForgejoIntegration.RewriteBranchWithLease(ctx, repo.FullName, plan.StagingGitDir, branch, update.Old, update.New)
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

// On a resume after local publication, current projection rows are the durable
// proof of branches that had already converged before publication.
func (s *Service) inferArtifactRetirementProviderCoverage(ctx context.Context, repositoryID uint, plan artifactretirement.Plan) (retirementProviderState, error) {
	state := retirementProviderState{}
	updates := retirementBranchUpdates(plan)
	var refs []db.ProjectionRefState
	if err := s.DBForCtx(ctx).WithContext(ctx).Where("repository_id = ?", repositoryID).Find(&refs).Error; err != nil {
		return nil, errors.New("artifact retirement projection-state resume failed")
	}
	for _, row := range refs {
		if !strings.HasPrefix(row.Ref, "refs/heads/") {
			continue
		}
		branch := strings.TrimPrefix(row.Ref, "refs/heads/")
		update, changed := updates[branch]
		if !changed {
			continue
		}
		if row.AGSSHA == update.Old || row.AGSSHA == update.New {
			markRetirementProvider(state, row.Provider, branch)
		}
	}
	return state, nil
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
				if err := tx.Model(&db.PullRequest{}).Where("id = ? AND head_sha = ? AND base_sha = ?", pr.ID, pr.HeadSHA, pr.BaseSHA).Updates(changes).Error; err != nil {
					return err
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
				if err := tx.Model(&db.PullRequestProjection{}).Where("id = ? AND last_synced_sha = ?", projection.ID, update.Old).Update("last_synced_sha", update.New).Error; err != nil {
					return err
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
				if err := tx.Model(&db.ProjectionRefState{}).Where("id = ?", row.ID).Updates(changes).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}
