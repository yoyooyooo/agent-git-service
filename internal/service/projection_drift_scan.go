package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gittransport"
)

// ScanForgejoProjectionDrift performs the bounded operational reconciliation:
// refs plus currently open pull requests. Historical provider state is handled
// by AuditForgejoProjectionDrift on its separate, lower-frequency schedule.
func (s *Service) ScanForgejoProjectionDrift(ctx context.Context, limit int) (int, error) {
	return s.scanConfiguredForgejoProjectionDrift(ctx, limit, false)
}

// AuditForgejoProjectionDrift performs the full historical integrity audit.
func (s *Service) AuditForgejoProjectionDrift(ctx context.Context, limit int) (int, error) {
	return s.scanConfiguredForgejoProjectionDrift(ctx, limit, true)
}

func (s *Service) scanConfiguredForgejoProjectionDrift(ctx context.Context, limit int, fullAudit bool) (int, error) {
	if s == nil || s.ForgejoIntegration == nil || s.Git == nil {
		return 0, nil
	}
	repos := s.ForgejoIntegration.ConfiguredRepoFullNames()
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if len(repos) > limit {
		repos = repos[:limit]
	}
	recorded := 0
	var errs []error
	for _, repoFullName := range repos {
		n, err := s.scanForgejoRepoDrift(ctx, repoFullName, fullAudit)
		recorded += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return recorded, joinServiceErrors(errs)
}

func (s *Service) scanForgejoRepoDrift(ctx context.Context, repoFullName string, fullAudit bool) (int, error) {
	repoFullName = strings.TrimSpace(repoFullName)
	if repoFullName == "" {
		return 0, nil
	}
	repo, err := s.projectionRepository(ctx, repoFullName)
	if err != nil {
		return 0, fmt.Errorf("lookup repo for projection drift scan %s: %w", repoFullName, err)
	}
	if skipForgejoProjectionDriftScan(repo) {
		if err := s.resolveAbsentForgejoProjectionStates(ctx, repoFullName, time.Now().UTC(), map[string]struct{}{}); err != nil {
			return 0, err
		}
		return 0, nil
	}
	repoPath, err := s.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		return 0, fmt.Errorf("lookup repo path for projection drift scan %s: %w", repoFullName, err)
	}
	remoteURL, err := s.ForgejoIntegration.RemoteURLForRepo(repoFullName)
	if err != nil {
		return 0, fmt.Errorf("resolve Forgejo remote for projection drift scan %s: %w", repoFullName, err)
	}
	agsRefs, err := s.Git.ListBranches(ctx, repoFullName)
	if err != nil {
		return 0, fmt.Errorf("list AGS heads for projection drift scan %s: %w", repoFullName, err)
	}
	forgejoRefs, err := lsRemoteHeads(ctx, repoPath, remoteURL)
	if err != nil {
		return 0, fmt.Errorf("list Forgejo heads for projection drift scan %s: %w", repoFullName, err)
	}
	now := time.Now().UTC()
	recorded := 0
	seen := map[string]struct{}{}
	presentRefs := map[string]struct{}{}
	for _, branch := range agsRefs {
		branchName := strings.TrimSpace(branch.Name)
		ref := "refs/heads/" + branchName
		if ref == "refs/heads/" {
			continue
		}
		presentRefs[ref] = struct{}{}
		if !s.ForgejoIntegration.MirrorBranchEnabled(branchName) {
			continue
		}
		seen[ref] = struct{}{}
		forgejoSHA := forgejoRefs[ref]
		if forgejoSHA == branch.SHA {
			_ = s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, ref, branch.SHA, forgejoSHA, now)
			continue
		}
		failureType := forgejointegration.ProjectionFailureSHADrift
		summary := "Forgejo ref SHA differs from AGS authoritative ref"
		if forgejoSHA == "" {
			pending, err := s.isMergedForgejoSourceBranchCleanupPending(ctx, repo.ID, branchName)
			if err != nil {
				return recorded, fmt.Errorf("classify merged source branch cleanup %s: %w", ref, err)
			}
			if pending {
				failureType = forgejointegration.ProjectionFailureSourceBranchCleanupPending
				summary = "Merged Forgejo PR source branch is deleted on Forgejo but still exists on AGS; delete the AGS source branch instead of restoring Forgejo"
			} else {
				summary = "Forgejo ref missing for AGS authoritative ref"
			}
		}
		if err := s.recordProjectionFailure(ctx, projectionFailureRecord{
			Provider:     ProjectionProviderForgejo,
			RepoFullName: repoFullName,
			Ref:          ref,
			Branch:       branch.Name,
			Type:         failureType,
			Authority:    ProjectionAuthorityAGS,
			AGSSHA:       branch.SHA,
			ExternalSHA:  forgejoSHA,
			ErrorSummary: summary,
			OccurredAt:   now,
		}); err != nil {
			return recorded, err
		}
		recorded++
	}
	for ref := range forgejoRefs {
		presentRefs[ref] = struct{}{}
	}
	for ref, forgejoSHA := range forgejoRefs {
		if _, ok := seen[ref]; ok {
			continue
		}
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if !s.ForgejoIntegration.MirrorBranchEnabled(branch) {
			continue
		}
		if err := s.recordProjectionFailure(ctx, projectionFailureRecord{
			Provider:     ProjectionProviderForgejo,
			RepoFullName: repoFullName,
			Ref:          ref,
			Branch:       branch,
			Type:         forgejointegration.ProjectionFailureSHADrift,
			Authority:    ProjectionAuthorityAGS,
			ExternalSHA:  forgejoSHA,
			ErrorSummary: "Forgejo has extra ref absent from AGS authoritative heads",
			OccurredAt:   now,
		}); err != nil {
			return recorded, err
		}
		recorded++
	}
	if err := s.resolveAbsentForgejoProjectionStates(ctx, repoFullName, now, presentRefs); err != nil {
		return recorded, err
	}
	if !fullAudit {
		prIntegrityRecorded, err := s.scanForgejoActivePullRequestIntegrity(ctx, repoFullName)
		recorded += prIntegrityRecorded
		if err != nil {
			return recorded, fmt.Errorf("scan active Forgejo pull-request integrity for %s: %w", repoFullName, err)
		}
		return recorded, nil
	}
	integrityRecorded, err := s.scanMergedPullRequestIntegrity(ctx, repoFullName)
	recorded += integrityRecorded
	if err != nil {
		return recorded, fmt.Errorf("scan merged pull-request integrity for %s: %w", repoFullName, err)
	}
	prIntegrityRecorded, err := s.scanForgejoPullRequestIntegrity(ctx, repoFullName)
	recorded += prIntegrityRecorded
	if err != nil {
		return recorded, fmt.Errorf("audit Forgejo pull-request integrity for %s: %w", repoFullName, err)
	}
	return recorded, nil
}

func skipForgejoProjectionDriftScan(repo db.Repository) bool {
	return repo.Fork || repo.ParentID != nil
}

func (s *Service) isMergedForgejoSourceBranchCleanupPending(ctx context.Context, repoID uint, branch string) (bool, error) {
	branch = strings.TrimSpace(branch)
	if repoID == 0 || branch == "" || strings.HasPrefix(branch, "refs/") || branch == "main" || branch == "master" {
		return false, nil
	}
	var openCount int64
	if err := s.DBForCtx(ctx).Model(&db.PullRequest{}).
		Where("repository_id = ? AND head_ref = ? AND state = ? AND merged = ?", repoID, branch, db.StateOpen, false).
		Count(&openCount).Error; err != nil {
		return false, err
	}
	if openCount > 0 {
		return false, nil
	}
	var prs []db.PullRequest
	if err := s.DBForCtx(ctx).Model(&db.PullRequest{}).
		Joins("JOIN pull_request_projections p ON p.pull_request_id = pull_requests.id").
		Where("pull_requests.repository_id = ? AND pull_requests.head_ref = ? AND pull_requests.merged = ?", repoID, branch, true).
		Where("p.provider = ? AND p.source_branch = ? AND p.state IN ?", ProjectionProviderForgejo, branch, []string{ProjectionStateClosed, ProjectionStateMerged}).
		Order("pull_requests.merged_at DESC, pull_requests.id DESC").
		Limit(1).
		Find(&prs).Error; err != nil {
		return false, err
	}
	if len(prs) == 0 {
		return false, nil
	}
	return sourceBranchDeletionAllowed(prs[0], branch), nil
}

func (s *Service) resolveAbsentForgejoProjectionStates(ctx context.Context, repoFullName string, resolvedAt time.Time, presentRefs map[string]struct{}) error {
	repoID := s.projectionRepositoryID(ctx, repoFullName)
	if repoID == 0 {
		return nil
	}
	var states []db.ProjectionRefState
	if err := s.DBForCtx(ctx).
		Where("provider = ? AND repository_id = ? AND status = ? AND ref LIKE ?", ProjectionProviderForgejo, repoID, ProjectionStatusActive, "refs/heads/%").
		Find(&states).Error; err != nil {
		return err
	}
	for _, state := range states {
		if _, ok := presentRefs[strings.TrimSpace(state.Ref)]; ok {
			continue
		}
		if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, state.Ref, "", "", resolvedAt); err != nil {
			return err
		}
	}
	return nil
}

func lsRemoteHeads(ctx context.Context, repoPath, remoteURL string) (map[string]string, error) {
	out, err := gittransport.Run(ctx, remoteURL, "", "-C", repoPath, "ls-remote", "--heads", remoteURL)
	if err != nil {
		return nil, fmt.Errorf("git ls-remote --heads: %w", err)
	}
	refs := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			refs[fields[1]] = fields[0]
		}
	}
	return refs, scanner.Err()
}

func joinServiceErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			parts = append(parts, err.Error())
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New(strings.Join(parts, "; "))
}
