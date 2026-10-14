package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

func pullRequestProjectionIntegrityRef(number int) string {
	return fmt.Sprintf("refs/pull/%d/projection-integrity", number)
}

func forgejoPullRequestAuthorityIntegrityRef(number int) string {
	return fmt.Sprintf("refs/forgejo/pull/%d/authority-integrity", number)
}

func forgejoMergedPullRequestIntegrityRef(number int) string {
	return fmt.Sprintf("refs/forgejo/pull/%d/merge-integrity", number)
}

func forgejoPullRequestHasAuditedSupersession(pr forgejointegration.PullRequestSnapshot) bool {
	hasLabel := false
	for _, label := range pr.Labels {
		if strings.EqualFold(strings.TrimSpace(label), MergeIntegritySupersededLabel) {
			hasLabel = true
			break
		}
	}
	return hasLabel && hasMergeIntegritySupersessionMarker(pr.Body)
}

func forgejoPullRequestStateMatches(pr db.PullRequest, providerPR forgejointegration.PullRequestSnapshot) bool {
	providerState := strings.ToLower(strings.TrimSpace(providerPR.State))
	switch {
	case pr.Merged:
		return providerPR.Merged
	case pr.State == db.StateClosed:
		return providerState == "closed" && !providerPR.Merged
	default:
		return providerState == "open" && !providerPR.Merged
	}
}

func forgejoPullRequestHeadMatches(pr db.PullRequest, providerPR forgejointegration.PullRequestSnapshot) bool {
	head := strings.TrimSpace(pr.HeadSHA)
	providerHead := strings.TrimSpace(providerPR.HeadSHA)
	if head == "" || providerHead == "" {
		return true
	}
	return strings.EqualFold(head, providerHead)
}

// A provider may resolve a closed PR's head through its still-live source
// branch. Reusing that branch cannot change the historical merge decision.
// Merged PRs are compared by their immutable merge result; an abandoned PR's
// head is comparable only when the provider exposes its retained pull ref.
func forgejoPullRequestContentMismatch(pr db.PullRequest, providerPR forgejointegration.PullRequestSnapshot) string {
	if pr.Merged {
		if strings.TrimSpace(pr.MergeCommitSHA) == "" || strings.TrimSpace(providerPR.MergeCommitSHA) == "" {
			return fmt.Sprintf("AGS PR #%d or Forgejo PR #%d is merged without a recorded merge commit", pr.Number, providerPR.Number)
		}
		if !equalFoldTrim(pr.MergeCommitSHA, providerPR.MergeCommitSHA) {
			return fmt.Sprintf("AGS PR #%d merge commit %s differs from Forgejo PR #%d merge commit %s", pr.Number, pr.MergeCommitSHA, providerPR.Number, providerPR.MergeCommitSHA)
		}
		return ""
	}
	if pr.State == db.StateClosed && providerPR.HeadRef == pr.HeadRef {
		return ""
	}
	if !forgejoPullRequestHeadMatches(pr, providerPR) {
		return fmt.Sprintf("AGS PR #%d head %s differs from Forgejo PR #%d head %s", pr.Number, pr.HeadSHA, providerPR.Number, providerPR.HeadSHA)
	}
	return ""
}

func equalFoldTrim(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// forgejoPullRequestIntegrityMayResolve is the single gate before resolving
// refs/pull/N/projection-integrity. Open PRs also require projection
// last_synced_sha to equal both the AGS and provider heads, and ExternalRepo
// must match the configured Forgejo target.
func forgejoPullRequestIntegrityMayResolve(pr db.PullRequest, projection db.PullRequestProjection, providerPR forgejointegration.PullRequestSnapshot, configuredExternalRepo string) bool {
	if !forgejoPullRequestStateMatches(pr, providerPR) || forgejoPullRequestContentMismatch(pr, providerPR) != "" {
		return false
	}
	if forgejoPullRequestCoordinateMismatch(pr, projection, providerPR, configuredExternalRepo) != "" {
		return false
	}
	if pr.State != db.StateOpen || pr.Merged {
		return true
	}
	head := strings.TrimSpace(pr.HeadSHA)
	if head == "" {
		return false
	}
	return equalFoldTrim(projection.LastSyncedSHA, head) && equalFoldTrim(providerPR.HeadSHA, head)
}

func forgejoPullRequestCoordinateMismatch(pr db.PullRequest, projection db.PullRequestProjection, providerPR forgejointegration.PullRequestSnapshot, configuredExternalRepo string) string {
	if pr.ID == 0 || pr.Number == 0 || pr.RepositoryID == 0 {
		return "incomplete AGS PR identity"
	}
	if strings.TrimSpace(pr.HeadRef) == "" || strings.TrimSpace(pr.BaseRef) == "" {
		return "AGS PR missing head/base ref"
	}
	if projection.PullRequestID != 0 && projection.PullRequestID != pr.ID {
		return "projection does not belong to AGS PR"
	}
	if projection.ExternalNumber == 0 || strings.TrimSpace(projection.ExternalRepo) == "" {
		return "projection missing external coordinates"
	}
	configured := strings.TrimSpace(configuredExternalRepo)
	if configured == "" {
		return "configured Forgejo target is unavailable"
	}
	if strings.TrimSpace(projection.ExternalRepo) != configured {
		return fmt.Sprintf("projection external_repo %q does not match configured Forgejo target %q", projection.ExternalRepo, configured)
	}
	if strings.TrimSpace(projection.SourceBranch) != strings.TrimSpace(pr.HeadRef) {
		return fmt.Sprintf("projection source_branch %q does not match AGS head_ref %q", projection.SourceBranch, pr.HeadRef)
	}
	if strings.TrimSpace(projection.TargetBranch) != strings.TrimSpace(pr.BaseRef) {
		return fmt.Sprintf("projection target_branch %q does not match AGS base_ref %q", projection.TargetBranch, pr.BaseRef)
	}
	if providerPR.Number != 0 && providerPR.Number != projection.ExternalNumber {
		return fmt.Sprintf("provider PR number %d does not match projection %d", providerPR.Number, projection.ExternalNumber)
	}
	if strings.TrimSpace(providerPR.HeadRef) == "" || strings.TrimSpace(providerPR.BaseRef) == "" {
		return "provider PR missing head/base ref"
	}
	// A closed PR may expose its retained pull ref instead of its source branch.
	// No other branch or PR's synthetic ref is interchangeable with this mapping.
	retainedHead := fmt.Sprintf("refs/pull/%d/head", projection.ExternalNumber)
	if providerPR.HeadRef != pr.HeadRef && !((pr.State == db.StateClosed || pr.Merged) && providerPR.HeadRef == retainedHead) {
		return fmt.Sprintf("provider head_ref %q does not match AGS head_ref %q", providerPR.HeadRef, pr.HeadRef)
	}
	if strings.TrimSpace(providerPR.BaseRef) != strings.TrimSpace(pr.BaseRef) {
		return fmt.Sprintf("provider base_ref %q does not match AGS base_ref %q", providerPR.BaseRef, pr.BaseRef)
	}
	return ""
}

// Reconcile a stale cached base only after an exact provider observation agrees
// with the native PR. This repairs metadata, never retargets a provider or changes
// mapping identity/last-synced content. Both facts fence the compare-and-swap.
func (s *Service) reconcileObservedForgejoProjectionBase(ctx context.Context, pr db.PullRequest, projection db.PullRequestProjection, providerPR forgejointegration.PullRequestSnapshot, configuredExternalRepo string) (db.PullRequestProjection, error) {
	if projection.TargetBranch == pr.BaseRef {
		return projection, nil
	}
	candidate := projection
	candidate.TargetBranch = pr.BaseRef
	if projection.ID == 0 || providerPR.Number != projection.ExternalNumber ||
		!forgejoPullRequestStateMatches(pr, providerPR) || forgejoPullRequestContentMismatch(pr, providerPR) != "" ||
		forgejoPullRequestCoordinateMismatch(pr, candidate, providerPR, configuredExternalRepo) != "" {
		return projection, nil
	}
	native := s.DBForCtx(ctx).Model(&db.PullRequest{}).Select("1").
		Where("id = ? AND repository_id = ? AND head_repository_id = ? AND head_ref = ? AND base_ref = ? AND head_sha = ? AND state = ? AND merged = ?",
			pr.ID, pr.RepositoryID, pr.HeadRepositoryID, pr.HeadRef, pr.BaseRef, pr.HeadSHA, pr.State, pr.Merged)
	if pr.Merged {
		native = native.Where("merge_commit_sha = ?", pr.MergeCommitSHA)
	}
	result := s.DBForCtx(ctx).Model(&db.PullRequestProjection{}).
		Where("id = ? AND repository_id = ? AND pull_request_id = ? AND provider = ? AND external_repo = ? AND external_number = ?",
			projection.ID, projection.RepositoryID, pr.ID, ProjectionProviderForgejo, projection.ExternalRepo, projection.ExternalNumber).
		Where("source_branch = ? AND target_branch = ? AND state = ? AND last_synced_sha = ?",
			projection.SourceBranch, projection.TargetBranch, projection.State, projection.LastSyncedSHA).
		Where("EXISTS (?)", native).Update("target_branch", pr.BaseRef)
	if result.Error != nil {
		return projection, fmt.Errorf("reconcile observed Forgejo projection base: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return projection, fmt.Errorf("Forgejo projection base observation became stale: %w", ErrInvalidState)
	}
	return candidate, nil
}

func (s *Service) pullRequestHeadRepoFullName(ctx context.Context, pr db.PullRequest, fallback string) (string, error) {
	if pr.HeadRepositoryID != 0 && pr.HeadRepositoryID != pr.RepositoryID {
		if name := strings.TrimSpace(pr.HeadRepository.FullName); name != "" {
			return name, nil
		}
		var repo db.Repository
		if err := s.DBForCtx(ctx).Select("full_name").First(&repo, pr.HeadRepositoryID).Error; err != nil {
			return "", fmt.Errorf("lookup head repository %d: %w", pr.HeadRepositoryID, err)
		}
		if strings.TrimSpace(repo.FullName) == "" {
			return "", fmt.Errorf("head repository %d has empty full name", pr.HeadRepositoryID)
		}
		return repo.FullName, nil
	}
	if name := strings.TrimSpace(pr.HeadRepository.FullName); name != "" {
		return name, nil
	}
	if strings.TrimSpace(fallback) != "" {
		return strings.TrimSpace(fallback), nil
	}
	if name := strings.TrimSpace(pr.Repository.FullName); name != "" {
		return name, nil
	}
	return "", fmt.Errorf("missing head repository full name")
}

func (s *Service) tryHealStaleOpenPRHead(ctx context.Context, headRepoName string, pr db.PullRequest, projection db.PullRequestProjection, providerPR forgejointegration.PullRequestSnapshot) (db.PullRequest, bool, bool, error) {
	if s == nil || s.Git == nil || strings.TrimSpace(headRepoName) == "" || pr.State != db.StateOpen || pr.Merged {
		return pr, false, false, nil
	}
	tip, tipErr := s.Git.HeadSHA(ctx, headRepoName, pr.HeadRef)
	if tipErr != nil {
		return pr, false, false, nil
	}
	if !equalFoldTrim(tip, providerPR.HeadSHA) {
		return pr, false, false, nil
	}
	updated, syncErr := s.SyncPRHeadAfterPush(ctx, pr.ID, headRepoName)
	if syncErr != nil {
		return pr, false, false, nil
	}
	if !forgejoPullRequestHeadMatches(updated, providerPR) {
		return updated, false, false, nil
	}
	var fresh db.PullRequestProjection
	if projection.ID != 0 {
		if loadErr := s.DBForCtx(ctx).First(&fresh, projection.ID).Error; loadErr != nil {
			return updated, true, false, nil
		}
		projection = fresh
	}
	return updated, true, equalFoldTrim(projection.LastSyncedSHA, updated.HeadSHA), nil
}

// scanForgejoPullRequestIntegrity compares the complete AGS and Forgejo PR
// histories. It is reserved for the low-frequency audit path.
func (s *Service) scanForgejoPullRequestIntegrity(ctx context.Context, repoFullName string) (int, error) {
	return s.scanForgejoPullRequestIntegrityScope(ctx, repoFullName, true)
}

// scanForgejoActivePullRequestIntegrity reconciles only currently open AGS and
// Forgejo PRs. Missing or stale bulk candidates are confirmed with an exact
// provider read before drift is recorded.
func (s *Service) scanForgejoActivePullRequestIntegrity(ctx context.Context, repoFullName string) (int, error) {
	return s.scanForgejoPullRequestIntegrityScope(ctx, repoFullName, false)
}

func (s *Service) scanForgejoPullRequestIntegrityScope(ctx context.Context, repoFullName string, fullAudit bool) (int, error) {
	if s == nil || s.ForgejoIntegration == nil {
		return 0, nil
	}
	providerState := "open"
	if fullAudit {
		providerState = "all"
	}
	liveRows, handled, err := s.ForgejoIntegration.ListPullRequests(ctx, repoFullName, providerState)
	if err != nil {
		return 0, err
	}
	if !handled {
		return 0, nil
	}
	repo, err := s.projectionRepository(ctx, repoFullName)
	if err != nil {
		return 0, err
	}
	var prs []db.PullRequest
	prQuery := s.DBForCtx(ctx).Where("repository_id = ?", repo.ID)
	if !fullAudit {
		prQuery = prQuery.Where("state = ? AND merged = ?", db.StateOpen, false)
	}
	if err := prQuery.Order("number").Find(&prs).Error; err != nil {
		return 0, fmt.Errorf("list AGS pull requests for integrity scan: %w", err)
	}
	var projectionRows []db.PullRequestProjection
	if err := s.DBForCtx(ctx).
		Where("repository_id = ? AND provider = ?", repo.ID, ProjectionProviderForgejo).
		Find(&projectionRows).Error; err != nil {
		return 0, fmt.Errorf("list Forgejo pull request projections for integrity scan: %w", err)
	}
	projections := make(map[uint]db.PullRequestProjection, len(projectionRows))
	externalProjections := make(map[int]db.PullRequestProjection, len(projectionRows))
	for _, projection := range projectionRows {
		projections[projection.PullRequestID] = projection
		externalProjections[projection.ExternalNumber] = projection
	}
	live := make(map[int]forgejointegration.PullRequestSnapshot, len(liveRows))
	for _, row := range liveRows {
		live[row.Number] = row
	}
	configuredExternalRepo, err := s.ForgejoIntegration.ConfiguredTargetFullName(repoFullName)
	if err != nil {
		return 0, err
	}

	now := time.Now().UTC()
	recorded := 0
	for _, pr := range prs {
		ref := pullRequestProjectionIntegrityRef(pr.Number)
		projection, hasProjection := projections[pr.ID]
		required, policyErr := s.ForgejoIntegration.ValidatePullRequestProjection(repoFullName, pr.HeadRef, pr.BaseRef)
		if !hasProjection {
			if pr.State != db.StateOpen || pr.Merged {
				if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, ref, pr.HeadSHA, "", now); err != nil {
					return recorded, err
				}
				continue
			}
			if !required && policyErr == nil {
				if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, ref, pr.HeadSHA, "", now); err != nil {
					return recorded, err
				}
				continue
			}
			summary := fmt.Sprintf("AGS PR #%d has no Forgejo projection", pr.Number)
			if policyErr != nil {
				summary = fmt.Sprintf("%s: %v", summary, policyErr)
			}
			if err := s.recordPullRequestIntegrityFailure(ctx, repoFullName, ref, pr, forgejointegration.ProjectionFailurePullRequestProjectionMissing, pr.HeadSHA, "", summary, now); err != nil {
				return recorded, err
			}
			recorded++
			continue
		}
		if strings.TrimSpace(projection.ExternalRepo) != strings.TrimSpace(configuredExternalRepo) {
			summary := fmt.Sprintf("AGS PR #%d Forgejo mapping coordinates do not match: projection external_repo %q does not match configured Forgejo target %q", pr.Number, projection.ExternalRepo, configuredExternalRepo)
			if err := s.recordPullRequestIntegrityFailure(ctx, repoFullName, ref, pr, forgejointegration.ProjectionFailurePullRequestAuthorityMissing, pr.HeadSHA, projection.LastSyncedSHA, summary, now); err != nil {
				return recorded, err
			}
			recorded++
			continue
		}
		providerPR, found := live[projection.ExternalNumber]
		if !found || !forgejoPullRequestStateMatches(pr, providerPR) || !forgejoPullRequestHeadMatches(pr, providerPR) ||
			forgejoPullRequestContentMismatch(pr, providerPR) != "" || forgejoPullRequestCoordinateMismatch(pr, projection, providerPR, configuredExternalRepo) != "" {
			exactPR, exactFound, exactErr := s.ForgejoIntegration.ExactPullRequestSnapshot(ctx, repoFullName, projection.ExternalRepo, projection.ExternalNumber)
			if exactErr != nil {
				return recorded, fmt.Errorf("confirm Forgejo PR #%d projection for AGS PR #%d: %w", projection.ExternalNumber, pr.Number, exactErr)
			}
			providerPR, found = exactPR, exactFound
			if found {
				live[projection.ExternalNumber] = providerPR
			} else {
				delete(live, projection.ExternalNumber)
			}
		}
		if !found {
			summary := fmt.Sprintf("AGS PR #%d projection points to missing Forgejo PR #%d", pr.Number, projection.ExternalNumber)
			if err := s.recordPullRequestIntegrityFailure(ctx, repoFullName, ref, pr, forgejointegration.ProjectionFailurePullRequestProjectionMissing, pr.HeadSHA, projection.LastSyncedSHA, summary, now); err != nil {
				return recorded, err
			}
			recorded++
			continue
		}

		if pr.State == db.StateClosed && !pr.Merged && forgejoPullRequestHasAuditedSupersession(providerPR) {
			if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, ref, pr.HeadSHA, providerPR.HeadSHA, now); err != nil {
				return recorded, err
			}
			continue
		}

		if !forgejoPullRequestStateMatches(pr, providerPR) {
			summary := fmt.Sprintf("AGS PR #%d state=%s merged=%t differs from Forgejo PR #%d state=%s merged=%t", pr.Number, pr.State, pr.Merged, providerPR.Number, providerPR.State, providerPR.Merged)
			if err := s.recordPullRequestIntegrityFailure(ctx, repoFullName, ref, pr, forgejointegration.ProjectionFailurePullRequestStateDrift, pr.HeadSHA, providerPR.HeadSHA, summary, now); err != nil {
				return recorded, err
			}
			recorded++
			continue
		}
		projection, err = s.reconcileObservedForgejoProjectionBase(ctx, pr, projection, providerPR, configuredExternalRepo)
		if err != nil {
			return recorded, err
		}
		if mismatch := forgejoPullRequestCoordinateMismatch(pr, projection, providerPR, configuredExternalRepo); mismatch != "" {
			summary := fmt.Sprintf("AGS PR #%d Forgejo mapping coordinates do not match: %s", pr.Number, mismatch)
			if err := s.recordPullRequestIntegrityFailure(ctx, repoFullName, ref, pr, forgejointegration.ProjectionFailurePullRequestAuthorityMissing, pr.HeadSHA, providerPR.HeadSHA, summary, now); err != nil {
				return recorded, err
			}
			recorded++
			continue
		}
		if pr.State != db.StateOpen || pr.Merged {
			if mismatch := forgejoPullRequestContentMismatch(pr, providerPR); mismatch != "" {
				agsSHA, externalSHA := pr.HeadSHA, providerPR.HeadSHA
				if pr.Merged {
					agsSHA, externalSHA = pr.MergeCommitSHA, providerPR.MergeCommitSHA
				}
				if err := s.recordPullRequestIntegrityFailure(ctx, repoFullName, ref, pr, forgejointegration.ProjectionFailureSHADrift, agsSHA, externalSHA, mismatch, now); err != nil {
					return recorded, err
				}
				recorded++
				continue
			}
		} else if !forgejoPullRequestHeadMatches(pr, providerPR) {
			headRepoName, headRepoErr := s.pullRequestHeadRepoFullName(ctx, pr, repoFullName)
			if headRepoErr != nil {
				summary := fmt.Sprintf("AGS PR #%d head repository cannot be resolved for integrity heal: %v", pr.Number, headRepoErr)
				if err := s.recordPullRequestIntegrityFailure(ctx, repoFullName, ref, pr, forgejointegration.ProjectionFailurePullRequestAuthorityMissing, pr.HeadSHA, providerPR.HeadSHA, summary, now); err != nil {
					return recorded, err
				}
				recorded++
				continue
			}
			healedPR, healed, _, healErr := s.tryHealStaleOpenPRHead(ctx, headRepoName, pr, projection, providerPR)
			if healErr != nil {
				return recorded, healErr
			}
			if !healed {
				summary := fmt.Sprintf("AGS PR #%d head %s differs from Forgejo PR #%d head %s", pr.Number, pr.HeadSHA, providerPR.Number, providerPR.HeadSHA)
				if err := s.recordPullRequestIntegrityFailure(ctx, repoFullName, ref, pr, forgejointegration.ProjectionFailureSHADrift, pr.HeadSHA, providerPR.HeadSHA, summary, now); err != nil {
					return recorded, err
				}
				// Queue repair instead of only recording drift. Manual revalidate remains
				// a diagnostic escape hatch, not the normal recovery path.
				if _, enqueueErr := s.EnqueueForgejoPullRequestProjection(ctx, pr); enqueueErr != nil {
					// keep scan fail-soft; recorded failure already makes the drift durable
				}
				recorded++
				continue
			}
			pr = healedPR
			if projection.ID != 0 {
				var fresh db.PullRequestProjection
				if err := s.DBForCtx(ctx).First(&fresh, projection.ID).Error; err == nil {
					projection = fresh
				}
			}
		}
		if !forgejoPullRequestIntegrityMayResolve(pr, projection, providerPR, configuredExternalRepo) {
			continue
		}
		agsSHA, externalSHA := pr.HeadSHA, providerPR.HeadSHA
		if pr.Merged {
			agsSHA, externalSHA = pr.MergeCommitSHA, providerPR.MergeCommitSHA
		}
		if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, ref, agsSHA, externalSHA, now); err != nil {
			return recorded, err
		}
	}

	if !fullAudit {
		return recorded, nil
	}

	baseSHAs := map[string]string{}
	baseErrors := map[string]error{}
	for _, providerPR := range liveRows {
		// Reuse a newer exact read instead of auditing a contradicted list row.
		var present bool
		providerPR, present = live[providerPR.Number]
		if !present {
			continue
		}
		if !providerPR.Merged {
			continue
		}
		superseded := forgejoPullRequestHasAuditedSupersession(providerPR)
		authorityRef := forgejoPullRequestAuthorityIntegrityRef(providerPR.Number)
		if _, mapped := externalProjections[providerPR.Number]; mapped || superseded {
			if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, authorityRef, "", providerPR.MergeCommitSHA, now); err != nil {
				return recorded, err
			}
		} else {
			summary := fmt.Sprintf("Forgejo PR #%d is merged without an authoritative AGS PR projection", providerPR.Number)
			if err := s.recordProjectionFailure(ctx, projectionFailureRecord{
				Provider: ProjectionProviderForgejo, RepoFullName: repoFullName,
				Ref: authorityRef, Branch: providerPR.BaseRef,
				Type:      forgejointegration.ProjectionFailurePullRequestAuthorityMissing,
				Authority: ProjectionAuthorityAGS, ExternalSHA: providerPR.MergeCommitSHA,
				ErrorSummary: summary, OccurredAt: now,
			}); err != nil {
				return recorded, err
			}
			recorded++
		}

		if s.Git == nil {
			continue
		}
		mergeRef := forgejoMergedPullRequestIntegrityRef(providerPR.Number)
		mergeSHA := strings.TrimSpace(providerPR.MergeCommitSHA)
		if mergeSHA == "" {
			mergeSHA = strings.TrimSpace(providerPR.HeadSHA)
		}
		if superseded {
			if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, mergeRef, "", mergeSHA, now); err != nil {
				return recorded, err
			}
			continue
		}

		base := strings.TrimSpace(providerPR.BaseRef)
		if base == "" {
			base = strings.TrimSpace(repo.DefaultBranch)
		}
		baseSHA, ok := baseSHAs[base]
		baseErr := baseErrors[base]
		if !ok && baseErr == nil {
			baseSHA, baseErr = s.Git.HeadSHA(ctx, repoFullName, base)
			if baseErr != nil {
				baseErrors[base] = baseErr
			} else {
				baseSHAs[base] = baseSHA
			}
		}
		if baseErr != nil {
			summary := fmt.Sprintf("Forgejo PR #%d is recorded merged at %s but AGS base branch %s cannot be resolved: %v", providerPR.Number, mergeSHA, base, baseErr)
			if err := s.recordProjectionFailure(ctx, projectionFailureRecord{
				Provider: ProjectionProviderForgejo, RepoFullName: repoFullName,
				Ref: mergeRef, Branch: base,
				Type:      forgejointegration.ProjectionFailureMergedContentMissing,
				Authority: ProjectionAuthorityAGS, ExternalSHA: mergeSHA,
				ErrorSummary: summary, OccurredAt: now,
			}); err != nil {
				return recorded, err
			}
			recorded++
			continue
		}
		ancestor := false
		var ancestorErr error
		if mergeSHA != "" {
			ancestor, ancestorErr = s.Git.IsHistoricalAncestor(ctx, repoFullName, mergeSHA, baseSHA)
		}
		if ancestorErr == nil && ancestor {
			if err := s.ResolveProjectionRefState(ctx, repoFullName, ProjectionProviderForgejo, mergeRef, baseSHA, mergeSHA, now); err != nil {
				return recorded, err
			}
			continue
		}
		summary := fmt.Sprintf("Forgejo PR #%d is recorded merged at %s but that commit is not reachable from AGS %s (%s)", providerPR.Number, mergeSHA, base, baseSHA)
		if ancestorErr != nil {
			summary = fmt.Sprintf("%s: %v", summary, ancestorErr)
		}
		if err := s.recordProjectionFailure(ctx, projectionFailureRecord{
			Provider: ProjectionProviderForgejo, RepoFullName: repoFullName,
			Ref: mergeRef, Branch: base,
			Type:      forgejointegration.ProjectionFailureMergedContentMissing,
			Authority: ProjectionAuthorityAGS, AGSSHA: baseSHA, ExternalSHA: mergeSHA,
			ErrorSummary: summary, OccurredAt: now,
		}); err != nil {
			return recorded, err
		}
		recorded++
	}
	return recorded, nil
}

func (s *Service) recordPullRequestIntegrityFailure(ctx context.Context, repoFullName, ref string, pr db.PullRequest, failureType, agsSHA, externalSHA, summary string, occurredAt time.Time) error {
	return s.recordProjectionFailure(ctx, projectionFailureRecord{
		Provider: ProjectionProviderForgejo, RepoFullName: repoFullName,
		Ref: ref, Branch: pr.BaseRef, Type: failureType, Authority: ProjectionAuthorityAGS,
		AGSSHA: agsSHA, ExternalSHA: externalSHA, ErrorSummary: summary, OccurredAt: occurredAt,
	})
}
