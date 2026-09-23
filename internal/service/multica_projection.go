package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"gorm.io/gorm"
)

func (s *Service) projectPullRequestCreatedToMultica(ctx context.Context, repoFullName, headSHA string, pr db.PullRequest, forgejo forgejointegration.PullRequestResult, gitLab gitlabintegration.ShadowMergeRequestResult, gitLabHandled bool) {
	if s == nil || s.MulticaProjection == nil {
		return
	}
	issueRef := s.multicaIssueRefForPR(ctx, repoFullName, pr)
	if issueRef.IssueKey == "" {
		return
	}
	req := multicaprojection.Request{
		IssueKey:      issueRef.IssueKey,
		Workspace:     issueRef.Workspace,
		WorkspaceID:   issueRef.WorkspaceID,
		RepoFullName:  repoFullName,
		AGSPRNumber:   pr.Number,
		AGSPRURL:      s.absAGSURL(fmt.Sprintf("/%s/pull/%d", repoFullName, pr.Number)),
		HeadBranch:    pr.HeadRef,
		HeadSHA:       headSHA,
		ForgejoNumber: forgejo.Number,
		ForgejoURL:    forgejo.URL,
		CIState:       "pending",
		MergeState:    "open",
		MulticaURL:    issueRef.URL,
	}
	if gitLabHandled {
		req.GitLabProject = gitLab.ProjectPath
		req.GitLabMRNumber = gitLab.IID
		req.GitLabMRURL = gitLab.WebURL
	} else {
		s.addGitLabProjectionToMulticaRequest(ctx, pr.ID, &req)
	}
	s.addGitHubProjectionToMulticaRequest(ctx, pr.ID, &req)
	s.addExternalPRMetadataToMulticaRequest(ctx, pr, &req)
	s.dispatchMulticaProjection(ctx, "AGS PR created", req)
	s.dispatchMulticaRegisterPullRequestLink(ctx, pr, "open", forgejo.ExternalRepo, forgejo.Number, forgejo.URL, "")
	s.scheduleForgejoCIProjection(ctx, req)
}

func (s *Service) ProjectPullRequestSyncedToMultica(ctx context.Context, pr db.PullRequest) {
	if s == nil || s.MulticaProjection == nil {
		return
	}
	issueRef := s.multicaIssueRefForPR(ctx, pr.Repository.FullName, pr)
	if issueRef.IssueKey == "" {
		return
	}
	projection, ok := s.forgejoProjectionForPullRequest(ctx, pr.ID)
	if !ok {
		return
	}
	req := multicaprojection.Request{
		IssueKey:      issueRef.IssueKey,
		Workspace:     issueRef.Workspace,
		WorkspaceID:   issueRef.WorkspaceID,
		RepoFullName:  pr.Repository.FullName,
		AGSPRNumber:   pr.Number,
		AGSPRURL:      s.absAGSURL(fmt.Sprintf("/%s/pull/%d", pr.Repository.FullName, pr.Number)),
		HeadBranch:    pr.HeadRef,
		HeadSHA:       pr.HeadSHA,
		ForgejoNumber: projection.ExternalNumber,
		ForgejoURL:    projection.ExternalURL,
		CIState:       "pending",
		MergeState:    "open",
		MulticaURL:    issueRef.URL,
	}
	s.addGitLabProjectionToMulticaRequest(ctx, pr.ID, &req)
	s.addGitHubProjectionToMulticaRequest(ctx, pr.ID, &req)
	s.addExternalPRMetadataToMulticaRequest(ctx, pr, &req)
	s.dispatchMulticaProjection(ctx, "AGS PR synchronized", req)
	s.dispatchMulticaRegisterPullRequestLink(ctx, pr, "open", projection.ExternalRepo, projection.ExternalNumber, projection.ExternalURL, "")
	s.scheduleForgejoCIProjection(ctx, req)
}

func (s *Service) projectPullRequestClosedToMultica(ctx context.Context, pr db.PullRequest) {
	if s == nil || s.MulticaProjection == nil {
		return
	}
	repoFullName := pr.Repository.FullName
	if repoFullName == "" {
		repo, err := s.GetRepoByID(ctx, fmt.Sprint(pr.RepositoryID))
		if err != nil {
			slog.WarnContext(ctx, "lookup PR repo for Multica close projection failed", "pull_request_id", pr.ID, "error", err)
			return
		}
		repoFullName = repo.FullName
	}
	issueRef := s.multicaIssueRefForPR(ctx, repoFullName, pr)
	if issueRef.IssueKey == "" {
		return
	}
	req := multicaprojection.Request{
		IssueKey:     issueRef.IssueKey,
		Workspace:    issueRef.Workspace,
		WorkspaceID:  issueRef.WorkspaceID,
		RepoFullName: repoFullName,
		AGSPRNumber:  pr.Number,
		AGSPRURL:     s.absAGSURL(fmt.Sprintf("/%s/pull/%d", repoFullName, pr.Number)),
		HeadBranch:   pr.HeadRef,
		HeadSHA:      pr.HeadSHA,
		CIState:      "canceled",
		MergeState:   "closed",
		MulticaURL:   issueRef.URL,
	}
	forgejoRepo := ""
	forgejoNumber := 0
	if projection, ok := s.forgejoProjectionForPullRequest(ctx, pr.ID); ok {
		req.ForgejoNumber = projection.ExternalNumber
		req.ForgejoURL = projection.ExternalURL
		forgejoRepo = projection.ExternalRepo
		forgejoNumber = projection.ExternalNumber
	}
	s.addGitLabProjectionToMulticaRequest(ctx, pr.ID, &req)
	s.addGitHubProjectionToMulticaRequest(ctx, pr.ID, &req)
	s.addExternalPRMetadataToMulticaRequest(ctx, pr, &req)
	if forgejoNumber > 0 {
		observedAt := time.Now().UTC()
		if pr.ClosedAt != nil && !pr.ClosedAt.IsZero() {
			observedAt = pr.ClosedAt.UTC()
		}
		if err := s.enqueueMulticaExternalPRTerminalDelivery(ctx, pr, ProjectionProviderForgejo, forgejoRepo, forgejoNumber, ProjectionStateClosed, false, "", observedAt); err != nil {
			slog.WarnContext(ctx, "enqueue Multica PR close terminal delivery failed", "repo", repoFullName, "pr_number", pr.Number, "error", err)
		}
	}
}

func (s *Service) multicaIssueRefForPR(ctx context.Context, repoFullName string, pr db.PullRequest) multicaprojection.IssueRef {
	if s == nil || s.MulticaProjection == nil {
		return multicaprojection.IssueRef{}
	}
	if link, ok := s.authoritativeMulticaLinkForPR(ctx, pr.ID); ok {
		return multicaIssueRefFromLink(link)
	}
	if strings.TrimSpace(repoFullName) == "" {
		repoFullName = pr.Repository.FullName
	}
	return s.MulticaProjection.ResolveIssueRef(repoFullName, string(pr.Body), pr.Title, pr.HeadRef)
}

func (s *Service) dispatchMulticaProjection(ctx context.Context, label string, req multicaprojection.Request) {
	if s == nil || s.MulticaProjection == nil {
		return
	}
	s.Wg.Add(1)
	go func() {
		defer s.Wg.Done()
		projectionCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		req := req
		s.applyCurrentPullRequestStateToMulticaRequest(projectionCtx, &req)
		if err := s.MulticaProjection.Project(projectionCtx, req); err != nil {
			slog.WarnContext(ctx, "project state to Multica failed", "label", label, "repo", req.RepoFullName, "pr_number", req.AGSPRNumber, "issue", req.IssueKey, "error", err)
			return
		}
		slog.InfoContext(ctx, "projected state to Multica", "label", label, "repo", req.RepoFullName, "pr_number", req.AGSPRNumber, "issue", req.IssueKey, "merge_state", req.MergeState)
	}()
}

func (s *Service) addGitLabProjectionToMulticaRequest(ctx context.Context, prID uint, req *multicaprojection.Request) {
	projection, ok := s.projectionForPullRequest(ctx, prID, ProjectionProviderGitLab)
	if !ok {
		return
	}
	req.GitLabProject = projection.ExternalRepo
	req.GitLabMRNumber = projection.ExternalNumber
	req.GitLabMRURL = projection.ExternalURL
}

func (s *Service) addGitHubProjectionToMulticaRequest(ctx context.Context, prID uint, req *multicaprojection.Request) {
	projection, ok := s.projectionForPullRequest(ctx, prID, ProjectionProviderGitHub)
	if !ok {
		return
	}
	req.GitHubRepo = projection.ExternalRepo
	req.GitHubPRNumber = projection.ExternalNumber
	req.GitHubPRURL = projection.ExternalURL
}

func (s *Service) addExternalPRMetadataToMulticaRequest(ctx context.Context, pr db.PullRequest, req *multicaprojection.Request) {
	if s == nil || req == nil {
		return
	}
	provider := "ags"
	if s.MulticaProjection != nil {
		provider = s.MulticaProjection.ExternalPRProvider()
	}
	req.ExternalPRProvider = provider
	if link, ok := s.authoritativeMulticaLinkForPR(ctx, pr.ID); ok {
		req.ExternalPRLinkConfidence = link.Confidence
		req.ExternalPRLinkSource = link.Source
	}
}

func (s *Service) projectionForPullRequest(ctx context.Context, prID uint, provider string) (db.PullRequestProjection, bool) {
	var projection db.PullRequestProjection
	if err := s.DBForCtx(ctx).
		Where("pull_request_id = ? AND provider = ?", prID, provider).
		First(&projection).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return db.PullRequestProjection{}, false
		}
		slog.WarnContext(ctx, "lookup PR projection for Multica failed", "provider", provider, "pull_request_id", prID, "error", err)
		return db.PullRequestProjection{}, false
	}
	return projection, true
}

func (s *Service) forgejoProjectionForPullRequest(ctx context.Context, prID uint) (db.PullRequestProjection, bool) {
	return s.projectionForPullRequest(ctx, prID, ProjectionProviderForgejo)
}

func (s *Service) scheduleForgejoCIProjection(ctx context.Context, base multicaprojection.Request) {
	if s == nil || s.ForgejoIntegration == nil || s.MulticaProjection == nil || base.ForgejoNumber == 0 || base.IssueKey == "" {
		return
	}
	s.Wg.Add(1)
	go func() {
		defer s.Wg.Done()
		pollCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		projectedRunning := false
		foundRun := false
		for {
			run, ok, err := s.ForgejoIntegration.LatestWorkflowRunForPullRequest(pollCtx, base.RepoFullName, base.ForgejoNumber, base.HeadSHA)
			if err != nil {
				slog.WarnContext(ctx, "poll Forgejo CI for Multica failed", "repo", base.RepoFullName, "forgejo_pr", base.ForgejoNumber, "error", err)
				return
			}
			if ok {
				foundRun = true
				ciState, terminal := forgejoRunCIState(run.Status)
				if terminal || (!projectedRunning && ciState == "running") {
					req := base
					req.CIState = ciState
					req.CIRunURL = forgejoRunURL(run)
					s.applyCurrentPullRequestStateToMulticaRequest(pollCtx, &req)
					s.dispatchMulticaProjection(ctx, "Forgejo CI "+ciState, req)
					projectedRunning = true
				}
				if terminal {
					return
				}
			}
			select {
			case <-pollCtx.Done():
				if !foundRun {
					req := base
					req.CIState = "no_run"
					s.dispatchMulticaProjection(ctx, "Forgejo CI no_run", req)
				}
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Service) applyCurrentPullRequestStateToMulticaRequest(ctx context.Context, req *multicaprojection.Request) {
	if s == nil || req == nil || strings.TrimSpace(req.RepoFullName) == "" || req.AGSPRNumber == 0 {
		return
	}
	pr, err := s.GetPR(ctx, req.RepoFullName, req.AGSPRNumber)
	if err != nil {
		slog.WarnContext(ctx, "lookup PR state for Multica CI projection failed", "repo", req.RepoFullName, "pr_number", req.AGSPRNumber, "error", err)
		return
	}
	if pr.Merged {
		req.MergeState = "merged"
		if req.MergedSHA == "" {
			req.MergedSHA = pr.MergeCommitSHA
		}
		if req.MergedAt == "" && pr.MergedAt != nil {
			req.MergedAt = pr.MergedAt.Format(time.RFC3339)
		}
		req.AllowStatusSet = true
		return
	}
	if pr.State == db.StateClosed {
		req.MergeState = "closed"
	}
}

func forgejoRunCIState(status string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success":
		return "passed", true
	case "failure", "cancelled", "canceled", "error", "timed_out", "skipped":
		return "failed", true
	case "", "queued", "waiting":
		return "pending", false
	default:
		return "running", false
	}
}

func forgejoRunURL(run forgejointegration.WorkflowRun) string {
	if strings.TrimSpace(run.HTMLURL) != "" {
		return run.HTMLURL
	}
	return strings.TrimSpace(run.URL)
}
func (s *Service) absAGSURL(path string) string {
	base := strings.TrimRight(s.BaseURL, "/")
	if base == "" {
		return path
	}
	return base + path
}
