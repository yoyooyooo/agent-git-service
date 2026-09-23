package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
)

// PullRequestIntegrationPolicy declares which provider convergence is required
// for one synchronous dispatch. GitLab remains an optional shadow provider.
type PullRequestIntegrationPolicy struct {
	RequireForgejo bool
	ForgejoRewrite *ForgejoPullRequestRewritePolicy
	// HumanLeaseRewrite projects a Human/CLI rebase with force-with-lease
	// and skips delegated action-intent admission.
	HumanLeaseRewrite bool
}

// ForgejoPullRequestRewritePolicy carries the exact, previously accepted
// Forgejo PR identity and old head used for one lease-protected rebase
// projection. Callers must obtain these values before mutating the AGS head.
type ForgejoPullRequestRewritePolicy struct {
	ExternalRepo       string
	ExternalNumber     int
	ExpectedOldHeadSHA string
}

// PullRequestProviderResult is one provider's structured dispatch outcome.
type PullRequestProviderResult struct {
	Provider       string
	Required       bool
	Attempted      bool
	Projected      bool
	DesiredSHA     string
	ObservedSHA    string
	ExternalRepo   string
	ExternalNumber int
	ExternalURL    string
	Err            error
}

// PullRequestIntegrationResult keeps required and optional provider outcomes
// separate so callers cannot mistake an optional shadow failure for Forgejo
// convergence, or a required Forgejo failure for success.
type PullRequestIntegrationResult struct {
	Forgejo PullRequestProviderResult
	GitLab  PullRequestProviderResult
	GitHub  PullRequestProviderResult
}

func (r PullRequestIntegrationResult) RequiredError() error {
	var errs []error
	for _, provider := range []PullRequestProviderResult{r.Forgejo, r.GitLab, r.GitHub} {
		if provider.Required && provider.Err != nil {
			errs = append(errs, fmt.Errorf("required %s projection: %w", provider.Provider, provider.Err))
		}
	}
	return errors.Join(errs...)
}

// DispatchPullRequestIntegrations ensures external PR/MR mirrors after an AGS
// PR is created. Required Forgejo policy is inferred from repository config.
func (s *Service) DispatchPullRequestIntegrations(ctx context.Context, pr db.PullRequest) (PullRequestIntegrationResult, error) {
	return s.DispatchPullRequestIntegrationsWithPolicy(ctx, pr, PullRequestIntegrationPolicy{})
}

// DispatchPullRequestIntegrationsWithPolicy performs synchronous provider
// dispatch while preserving per-provider outcomes. Only required-provider
// failures are returned as the action error; optional failures remain visible
// in the result and logs.
func (s *Service) DispatchPullRequestIntegrationsWithPolicy(ctx context.Context, pr db.PullRequest, policy PullRequestIntegrationPolicy) (PullRequestIntegrationResult, error) {
	result := PullRequestIntegrationResult{
		Forgejo: PullRequestProviderResult{Provider: ProjectionProviderForgejo, Required: policy.RequireForgejo, DesiredSHA: strings.TrimSpace(pr.HeadSHA)},
		GitLab:  PullRequestProviderResult{Provider: ProjectionProviderGitLab, DesiredSHA: strings.TrimSpace(pr.HeadSHA)},
		GitHub:  PullRequestProviderResult{Provider: ProjectionProviderGitHub, DesiredSHA: strings.TrimSpace(pr.HeadSHA)},
	}
	if s == nil {
		if policy.RequireForgejo {
			result.Forgejo.Err = fmt.Errorf("service is nil")
		}
		return result, result.RequiredError()
	}
	repoFullName := pr.Repository.FullName
	if repoFullName == "" {
		repo, err := s.GetRepoByID(ctx, fmt.Sprint(pr.RepositoryID))
		if err != nil {
			return result, fmt.Errorf("lookup PR repo: %w", err)
		}
		repoFullName = repo.FullName
	}
	repoPath, err := s.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		return result, fmt.Errorf("lookup AGS repo path for PR integrations: %w", err)
	}
	headSHA := strings.TrimSpace(pr.HeadSHA)
	if headSHA == "" {
		headSHA, _ = s.Git.HeadSHA(ctx, repoFullName, pr.HeadRef)
	}
	result.Forgejo.DesiredSHA = headSHA
	result.GitLab.DesiredSHA = headSHA
	result.GitHub.DesiredSHA = headSHA

	forgejoRequired := policy.RequireForgejo
	if s.ForgejoIntegration != nil {
		configuredRequired, _ := s.ForgejoIntegration.ValidatePullRequestProjection(repoFullName, pr.HeadRef, pr.BaseRef)
		forgejoRequired = forgejoRequired || configuredRequired
	}
	result.Forgejo.Required = forgejoRequired
	result.Forgejo.Attempted = s.ForgejoIntegration != nil
	forgejoResult, forgejoHandled, forgejoErr := s.ensureForgejoPullRequestWithPolicyAndAdmission(ctx, repoFullName, repoPath, headSHA, pr, policy)
	result.Forgejo.Projected = forgejoHandled && forgejoErr == nil
	result.Forgejo.ExternalRepo = forgejoResult.ExternalRepo
	result.Forgejo.ExternalNumber = forgejoResult.Number
	result.Forgejo.ExternalURL = forgejoResult.URL
	result.Forgejo.ObservedSHA = strings.TrimSpace(forgejoResult.HeadSHA)
	result.Forgejo.Err = forgejoErr
	if result.Forgejo.ObservedSHA == "" {
		var projectionErr *forgejointegration.ProjectionError
		if errors.As(forgejoErr, &projectionErr) && projectionErr != nil {
			result.Forgejo.ObservedSHA = strings.TrimSpace(projectionErr.ActualSHA)
		}
	}
	if forgejoRequired && !forgejoHandled && forgejoErr == nil {
		result.Forgejo.Err = fmt.Errorf("required Forgejo projection did not confirm an external pull request")
	}
	if result.Forgejo.Err != nil {
		slog.WarnContext(ctx, "ensure Forgejo PR failed", "repo", repoFullName, "pr_number", pr.Number, "required", forgejoRequired, "error", result.Forgejo.Err)
	}

	result.GitLab.Attempted = s.GitLabIntegration != nil
	if s.GitLabIntegration != nil && policy.ForgejoRewrite != nil && !policy.HumanLeaseRewrite {
		if err := s.revalidateCurrentDelegatedProviderWrite(ctx, policy.ForgejoRewrite.ExternalRepo, policy.ForgejoRewrite.ExternalNumber); err != nil {
			result.GitLab.Err = err
			return result, fmt.Errorf("rebase GitLab projection admission: %w", err)
		}
	} else if _, delegated := DelegatedSessionIDFromContext(ctx); delegated && s.GitLabIntegration != nil {
		if _, err := s.RevalidateDelegatedSession(ctx, pr.RepositoryID, "pr.create", "pr:create", map[string]string{"head_ref": pr.HeadRef, "base_ref": pr.BaseRef}); err != nil {
			result.GitLab.Err = err
			return result, fmt.Errorf("delegated GitLab projection admission: %w", err)
		}
	}
	gitLabResult, gitLabHandled, gitLabErr := s.ensureGitLabShadowMergeRequest(ctx, repoFullName, repoPath, headSHA, pr)
	result.GitLab.Projected = gitLabHandled && gitLabErr == nil
	result.GitLab.ExternalRepo = gitLabResult.ProjectPath
	result.GitLab.ExternalNumber = gitLabResult.IID
	result.GitLab.ExternalURL = gitLabResult.WebURL
	result.GitLab.Err = gitLabErr
	if gitLabErr != nil {
		slog.WarnContext(ctx, "ensure GitLab shadow MR failed", "repo", repoFullName, "pr_number", pr.Number, "required", false, "error", gitLabErr)
	}

	result.GitHub.Attempted = s.GitHubIntegration != nil
	if s.GitHubIntegration != nil && policy.ForgejoRewrite != nil && !policy.HumanLeaseRewrite {
		if err := s.revalidateCurrentDelegatedProviderWrite(ctx, policy.ForgejoRewrite.ExternalRepo, policy.ForgejoRewrite.ExternalNumber); err != nil {
			result.GitHub.Err = err
			return result, fmt.Errorf("rebase GitHub projection admission: %w", err)
		}
	} else if _, delegated := DelegatedSessionIDFromContext(ctx); delegated && s.GitHubIntegration != nil {
		if _, err := s.RevalidateDelegatedSession(ctx, pr.RepositoryID, "pr.create", "pr:create", map[string]string{"head_ref": pr.HeadRef, "base_ref": pr.BaseRef}); err != nil {
			result.GitHub.Err = err
			return result, fmt.Errorf("delegated GitHub projection admission: %w", err)
		}
	}
	gitHubResult, gitHubHandled, gitHubErr := s.ensureGitHubShadowPullRequest(ctx, repoFullName, repoPath, headSHA, pr)
	result.GitHub.Projected = gitHubHandled && gitHubErr == nil
	result.GitHub.ExternalRepo = gitHubResult.TargetRepo
	result.GitHub.ExternalNumber = gitHubResult.Number
	result.GitHub.ExternalURL = gitHubResult.WebURL
	result.GitHub.Err = gitHubErr
	if gitHubErr != nil {
		slog.WarnContext(ctx, "ensure GitHub shadow PR failed", "repo", repoFullName, "pr_number", pr.Number, "required", false, "error", gitHubErr)
	}
	if forgejoHandled || gitLabHandled || gitHubHandled {
		s.projectPullRequestCreatedToMultica(ctx, repoFullName, headSHA, pr, forgejoResult, gitLabResult, gitLabHandled)
	}
	return result, result.RequiredError()
}

// ProjectRebasedPullRequestBranch lease-pushes a Human/CLI rebase to Forgejo.
// oldHeadSHA is the projection last-synced SHA captured before the AGS rebase.
func (s *Service) ProjectRebasedPullRequestBranch(ctx context.Context, pr db.PullRequest, oldHeadSHA string) error {
	if s == nil || exactGitSHA(pr.HeadSHA, oldHeadSHA) {
		return nil
	}
	projection, ok := s.forgejoProjectionForPullRequest(ctx, pr.ID)
	if !ok || strings.TrimSpace(projection.ExternalRepo) == "" || projection.ExternalNumber <= 0 {
		_, err := s.DispatchPullRequestIntegrations(ctx, pr)
		return err
	}
	oldHeadSHA = strings.TrimSpace(oldHeadSHA)
	if oldHeadSHA == "" {
		oldHeadSHA = strings.TrimSpace(projection.LastSyncedSHA)
	}
	if oldHeadSHA == "" || exactGitSHA(pr.HeadSHA, oldHeadSHA) {
		_, err := s.DispatchPullRequestIntegrations(ctx, pr)
		return err
	}
	_, err := s.DispatchPullRequestIntegrationsWithPolicy(ctx, pr, PullRequestIntegrationPolicy{
		RequireForgejo:    true,
		HumanLeaseRewrite: true,
		ForgejoRewrite: &ForgejoPullRequestRewritePolicy{
			ExternalRepo:       projection.ExternalRepo,
			ExternalNumber:     projection.ExternalNumber,
			ExpectedOldHeadSHA: oldHeadSHA,
		},
	})
	return err
}

func (s *Service) ensureForgejoPullRequest(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest) (forgejointegration.PullRequestResult, bool, error) {
	return s.ensureForgejoPullRequestWithPolicy(ctx, repoFullName, repoPath, headSHA, pr, nil)
}

func (s *Service) ensureForgejoPullRequestWithPolicy(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest, rewrite *ForgejoPullRequestRewritePolicy) (forgejointegration.PullRequestResult, bool, error) {
	return s.ensureForgejoPullRequestWithPolicyAndAdmission(ctx, repoFullName, repoPath, headSHA, pr, PullRequestIntegrationPolicy{ForgejoRewrite: rewrite})
}

func (s *Service) ensureForgejoPullRequestWithPolicyAndAdmission(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest, policy PullRequestIntegrationPolicy) (forgejointegration.PullRequestResult, bool, error) {
	rewrite := policy.ForgejoRewrite
	var beforeWrite func(context.Context) error
	if rewrite != nil && !policy.HumanLeaseRewrite {
		beforeWrite = func(checkCtx context.Context) error {
			return s.revalidateCurrentDelegatedProviderWrite(checkCtx, rewrite.ExternalRepo, rewrite.ExternalNumber)
		}
	} else if rewrite == nil {
		if _, delegated := DelegatedSessionIDFromContext(ctx); delegated {
			beforeWrite = func(checkCtx context.Context) error {
				_, err := s.RevalidateDelegatedSession(checkCtx, pr.RepositoryID, "pr.create", "pr:create", map[string]string{"head_ref": pr.HeadRef, "base_ref": pr.BaseRef})
				return err
			}
		}
	}
	res, handled, err := s.ensureForgejoPullRequestExternalWithPolicyAndAuthority(ctx, repoFullName, repoPath, headSHA, pr, rewrite, beforeWrite)
	if err != nil || !handled {
		return res, handled, err
	}
	// The integration returns only after both the remote branch and exact mapped
	// Forgejo PR head have been independently observed at headSHA.
	if err := s.recordForgejoPullRequestProjection(ctx, repoFullName, headSHA, pr, res); err != nil {
		return forgejointegration.PullRequestResult{}, false, err
	}
	return res, true, nil
}

func (s *Service) ensureForgejoPullRequestExternal(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest) (forgejointegration.PullRequestResult, bool, error) {
	return s.ensureForgejoPullRequestExternalWithPolicyAndAuthority(ctx, repoFullName, repoPath, headSHA, pr, nil, nil)
}

func (s *Service) ensureForgejoPullRequestExternalWithAuthority(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest, beforeWrite func(context.Context) error) (forgejointegration.PullRequestResult, bool, error) {
	return s.ensureForgejoPullRequestExternalWithPolicyAndAuthority(ctx, repoFullName, repoPath, headSHA, pr, nil, beforeWrite)
}

func (s *Service) ensureForgejoPullRequestExternalWithPolicy(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest, rewrite *ForgejoPullRequestRewritePolicy) (forgejointegration.PullRequestResult, bool, error) {
	return s.ensureForgejoPullRequestExternalWithPolicyAndAuthority(ctx, repoFullName, repoPath, headSHA, pr, rewrite, nil)
}

func (s *Service) ensureForgejoPullRequestExternalWithPolicyAndAuthority(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest, rewrite *ForgejoPullRequestRewritePolicy, beforeWrite func(context.Context) error) (forgejointegration.PullRequestResult, bool, error) {
	if s.ForgejoIntegration == nil {
		return forgejointegration.PullRequestResult{}, false, nil
	}
	body := s.forgejoPullRequestBody(ctx, repoFullName, pr)
	request := forgejointegration.PullRequestSyncRequest{
		RepoFullName: repoFullName,
		RepoPath:     repoPath,
		Head:         pr.HeadRef,
		Base:         pr.BaseRef,
		Title:        pr.Title,
		Body:         body,
		HeadSHA:      headSHA,
		BeforeWrite:  beforeWrite,
	}
	if rewrite != nil {
		request.RewriteLease = &forgejointegration.PullRequestRewriteLease{
			ExternalRepo: rewrite.ExternalRepo, ExternalNumber: rewrite.ExternalNumber,
			ExpectedOldHeadSHA: rewrite.ExpectedOldHeadSHA,
		}
	}
	res, err := s.ForgejoIntegration.EnsurePullRequest(ctx, request)
	if err != nil {
		return res, false, fmt.Errorf("ensure Forgejo PR: %w", err)
	}
	if res.Number == 0 || res.ExternalRepo == "" {
		return forgejointegration.PullRequestResult{}, false, nil
	}
	return res, true, nil
}

func (s *Service) recordForgejoPullRequestProjection(ctx context.Context, repoFullName, headSHA string, pr db.PullRequest, res forgejointegration.PullRequestResult) error {
	if err := s.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID:  pr.ID,
		RepositoryID:   pr.RepositoryID,
		Provider:       ProjectionProviderForgejo,
		ExternalRepo:   res.ExternalRepo,
		ExternalNumber: res.Number,
		ExternalURL:    res.URL,
		SourceBranch:   pr.HeadRef,
		TargetBranch:   pr.BaseRef,
		State:          ProjectionStateOpen,
		LastSyncedSHA:  headSHA,
	}); err != nil {
		return fmt.Errorf("record Forgejo projection: %w", err)
	}
	slog.InfoContext(ctx, "forgejo pull request projection recorded", "repo", repoFullName, "pr_number", pr.Number, "external_number", res.Number)
	return nil
}

func (s *Service) forgejoPullRequestBody(ctx context.Context, repoFullName string, pr db.PullRequest) string {
	agsURL := ""
	if repoFullName != "" && pr.Number != 0 {
		agsURL = s.absAGSURL(fmt.Sprintf("/%s/pull/%d", repoFullName, pr.Number))
	}
	intro := fmt.Sprintf("Created from AGS PR #%d.", pr.Number)
	if agsURL != "" {
		intro = fmt.Sprintf("Created from AGS PR #%d: %s", pr.Number, agsURL)
	}
	sections := []string{intro}
	if attribution, err := s.PullRequestAttributionFor(ctx, pr); err != nil {
		slog.WarnContext(ctx, "load delegated PR actor projection", "pr_number", pr.Number, "error", err)
	} else if attribution != nil {
		principal := forgejoProvenanceValue(attribution.DelegatedBy.Principal.Login)
		delegator := principal
		if attribution.DelegatedBy.Human != nil && strings.TrimSpace(attribution.DelegatedBy.Human.Login) != "" {
			delegator = forgejoProvenanceValue(attribution.DelegatedBy.Human.Login)
		}
		workspace := forgejoProvenanceValue(attribution.AGSActor.Workspace)
		if workspace == "" {
			workspace = forgejoProvenanceValue(attribution.AGSActor.WorkspaceID)
		} else if workspaceID := forgejoProvenanceValue(attribution.AGSActor.WorkspaceID); workspaceID != "" && workspaceID != workspace {
			workspace += " (" + workspaceID + ")"
		}
		lines := []string{
			"<!-- ags-delegated-workload:start -->",
			"Delegated workload:",
			"- Actor: " + forgejoProvenanceValue(attribution.AGSActor.AgentName) + " [Multica Agent]",
			"- Agent ID: " + forgejoProvenanceValue(attribution.AGSActor.AgentID),
			"- Delegated by: " + delegator + " via AGS principal " + principal,
			"- Target: Multica workspace " + workspace + " -> AGS " + forgejoProvenanceValue(attribution.AGSActor.TargetInstance),
			"- Task: " + forgejoProvenanceValue(attribution.AGSActor.TaskID),
		}
		if attribution.AGSActor.RunID != "" {
			lines = append(lines, "- Run: "+forgejoProvenanceValue(attribution.AGSActor.RunID))
		}
		if attribution.AGSActor.IssueKey != "" || attribution.AGSActor.IssueID != "" {
			issue := forgejoProvenanceValue(attribution.AGSActor.IssueKey)
			if attribution.AGSActor.IssueID != "" {
				if issue != "" {
					issue += " (" + forgejoProvenanceValue(attribution.AGSActor.IssueID) + ")"
				} else {
					issue = forgejoProvenanceValue(attribution.AGSActor.IssueID)
				}
			}
			lines = append(lines, "- Issue: "+issue)
		}
		lines = append(lines, "- Session: "+forgejoProvenanceValue(attribution.AGSActor.SessionID))
		lines = append(lines, "<!-- ags-delegated-workload:end -->")
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if body := strings.TrimSpace(string(pr.Body)); body != "" {
		sections = append(sections, body)
	}
	if link, ok := s.authoritativeMulticaLinkForPR(ctx, pr.ID); ok {
		issueRef := link.IssueKey
		if strings.TrimSpace(link.Workspace) != "" {
			issueRef = strings.TrimSpace(link.Workspace) + "/" + issueRef
		}
		issueURL := strings.TrimSpace(link.IssueURL)
		if issueURL == "" && s.MulticaProjection != nil && strings.TrimSpace(link.Workspace) != "" {
			issueURL = multicaprojection.IssueURL(s.MulticaProjection.AppURL(), link.Workspace, link.IssueKey)
		}
		lines := []string{"External links:"}
		if agsURL != "" {
			lines = append(lines, "- AGS PR: "+agsURL)
		}
		if strings.TrimSpace(issueRef) != "" {
			lines = append(lines, "- Multica issue: "+strings.TrimSpace(issueRef))
		}
		if issueURL != "" {
			lines = append(lines, "- Multica issue URL: "+issueURL)
		}
		lines = append(lines, "- Multica link: authoritative via "+link.Source)
		if s.MulticaProjection != nil && s.MulticaProjection.CompletionOnMergeEnabled() {
			lines = append(lines, "- Completion integration: Multica-owned on merge")
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

func forgejoProvenanceValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func (s *Service) ensureGitLabShadowMergeRequest(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest) (gitlabintegration.ShadowMergeRequestResult, bool, error) {
	if s.GitLabIntegration == nil {
		return gitlabintegration.ShadowMergeRequestResult{}, false, nil
	}
	var beforeWrite func(context.Context) error
	if _, actionBound := forgejoActionBindingFromContext(ctx); actionBound {
		// GitLab branch push and MR ensure are part of the exact rebase action
		// generation. Each external write reloads the bound job+intent rather
		// than relying on the earlier dispatch admission.
		beforeWrite = func(checkCtx context.Context) error {
			return s.revalidateBoundActionProviderWrite(checkCtx)
		}
	} else if sessionID, delegated := DelegatedSessionIDFromContext(ctx); delegated {
		var coordinate db.DelegatedAgentSession
		if err := s.DBForCtx(ctx).Select("id", "operation_name").First(&coordinate, "id = ?", sessionID).Error; err != nil {
			return gitlabintegration.ShadowMergeRequestResult{}, false, err
		}
		operation, capability := coordinate.OperationName, "pr:create"
		if strings.TrimSpace(operation) == "" {
			operation = "pr.create"
		}
		if operation == "pr.rebase" {
			// Historical/unbound rebase work must never fall back to Session-only
			// authority at an external provider seam.
			beforeWrite = func(context.Context) error {
				return fmt.Errorf("GitLab rebase projection has no exact action binding: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
			}
		} else {
			beforeWrite = func(checkCtx context.Context) error {
				_, err := s.RevalidateDelegatedSession(checkCtx, pr.RepositoryID, operation, capability, map[string]string{"head_ref": pr.HeadRef, "base_ref": pr.BaseRef})
				return err
			}
		}
	}
	res, handled, err := s.GitLabIntegration.EnsureShadowMergeRequest(ctx, gitlabintegration.ShadowMergeRequestRequest{
		RepoFullName: repoFullName,
		RepoPath:     repoPath,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      headSHA,
		BaseBranch:   pr.BaseRef,
		Title:        pr.Title,
		Body:         string(pr.Body),
		AGSNumber:    pr.Number,
		BeforeWrite:  beforeWrite,
	})
	if err != nil {
		return gitlabintegration.ShadowMergeRequestResult{}, false, fmt.Errorf("ensure GitLab shadow MR: %w", err)
	}
	if !handled {
		return gitlabintegration.ShadowMergeRequestResult{}, false, nil
	}
	if err := s.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID:  pr.ID,
		RepositoryID:   pr.RepositoryID,
		Provider:       ProjectionProviderGitLab,
		ExternalRepo:   res.ProjectPath,
		ExternalNumber: res.IID,
		ExternalURL:    res.WebURL,
		SourceBranch:   res.SourceBranch,
		TargetBranch:   res.TargetBranch,
		State:          ProjectionStateOpen,
		LastSyncedSHA:  headSHA,
	}); err != nil {
		return gitlabintegration.ShadowMergeRequestResult{}, false, fmt.Errorf("record GitLab projection: %w", err)
	}
	slog.InfoContext(ctx, "gitlab shadow merge request projection recorded", "repo", repoFullName, "pr_number", pr.Number, "external_number", res.IID)
	return res, true, nil
}

func (s *Service) ensureGitHubShadowPullRequest(ctx context.Context, repoFullName, repoPath, headSHA string, pr db.PullRequest) (githubintegration.ShadowPullRequestResult, bool, error) {
	if s.GitHubIntegration == nil {
		return githubintegration.ShadowPullRequestResult{}, false, nil
	}
	var beforeWrite func(context.Context) error
	if _, actionBound := forgejoActionBindingFromContext(ctx); actionBound {
		beforeWrite = func(checkCtx context.Context) error {
			return s.revalidateBoundActionProviderWrite(checkCtx)
		}
	} else if sessionID, delegated := DelegatedSessionIDFromContext(ctx); delegated {
		var coordinate db.DelegatedAgentSession
		if err := s.DBForCtx(ctx).Select("id", "operation_name").First(&coordinate, "id = ?", sessionID).Error; err != nil {
			return githubintegration.ShadowPullRequestResult{}, false, err
		}
		operation, capability := coordinate.OperationName, "pr:create"
		if strings.TrimSpace(operation) == "" {
			operation = "pr.create"
		}
		if operation == "pr.rebase" {
			beforeWrite = func(context.Context) error {
				return fmt.Errorf("GitHub rebase projection has no exact action binding: %w", delegatedUseTimeDenied(DelegatedDenialConstraintMismatch))
			}
		} else {
			beforeWrite = func(checkCtx context.Context) error {
				_, err := s.RevalidateDelegatedSession(checkCtx, pr.RepositoryID, operation, capability, map[string]string{"head_ref": pr.HeadRef, "base_ref": pr.BaseRef})
				return err
			}
		}
	}
	res, handled, err := s.GitHubIntegration.EnsureShadowPullRequest(ctx, githubintegration.ShadowPullRequestRequest{
		RepoFullName: repoFullName,
		RepoPath:     repoPath,
		HeadBranch:   pr.HeadRef,
		HeadSHA:      headSHA,
		BaseBranch:   pr.BaseRef,
		Title:        pr.Title,
		Body:         string(pr.Body),
		AGSNumber:    pr.Number,
		BeforeWrite:  beforeWrite,
	})
	if err != nil {
		return githubintegration.ShadowPullRequestResult{}, false, fmt.Errorf("ensure GitHub shadow PR: %w", err)
	}
	if !handled {
		return githubintegration.ShadowPullRequestResult{}, false, nil
	}
	if err := s.UpsertPullRequestProjection(ctx, db.PullRequestProjection{
		PullRequestID:  pr.ID,
		RepositoryID:   pr.RepositoryID,
		Provider:       ProjectionProviderGitHub,
		ExternalRepo:   res.TargetRepo,
		ExternalNumber: res.Number,
		ExternalURL:    res.WebURL,
		SourceBranch:   res.SourceBranch,
		TargetBranch:   res.TargetBranch,
		State:          ProjectionStateOpen,
		LastSyncedSHA:  headSHA,
	}); err != nil {
		return githubintegration.ShadowPullRequestResult{}, false, fmt.Errorf("record GitHub projection: %w", err)
	}
	slog.InfoContext(ctx, "github shadow pull request projection recorded", "repo", repoFullName, "pr_number", pr.Number, "external_number", res.Number)
	return res, true, nil
}
