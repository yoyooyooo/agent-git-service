package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"gorm.io/gorm/clause"
)

const multicaPRLinkTokenSource = "task_token"

var multicaPRLinkTokenPattern = regexp.MustCompile(`(?is)\n?\s*<!--\s*multica-external-pr-link-token:\s*([^\s<]+)\s*-->\s*`)

func extractMulticaPRLinkToken(body string) (cleanBody, token string) {
	match := multicaPRLinkTokenPattern.FindStringSubmatch(body)
	if len(match) != 2 {
		return body, ""
	}
	cleaned := multicaPRLinkTokenPattern.ReplaceAllString(body, "\n")
	return strings.TrimSpace(cleaned), strings.TrimSpace(match[1])
}

func enrichBodyWithAuthoritativeMulticaLink(body string, claims multicaprojection.PRLinkClaims) string {
	trimmed := strings.TrimSpace(body)
	lines := make([]string, 0, 3)
	if trimmed != "" {
		lines = append(lines, trimmed)
	}
	marker := strings.ToUpper(strings.TrimSpace(claims.IssueKey))
	workspace := strings.Trim(strings.ToLower(strings.TrimSpace(claims.Workspace)), "/")
	if workspace != "" && marker != "" {
		marker = workspace + "/" + marker
	}
	if marker != "" && !strings.Contains(strings.ToUpper(trimmed), strings.ToUpper(marker)) {
		lines = append(lines, "Multica: "+marker)
	}
	if url := strings.TrimSpace(claims.IssueURL); url != "" && !strings.Contains(trimmed, url) {
		lines = append(lines, "Multica issue: "+url)
	}
	return strings.Join(lines, "\n\n")
}

func (s *Service) bindAuthoritativeMulticaLink(ctx context.Context, pr db.PullRequest, token string) (db.PullRequestMulticaLink, bool, error) {
	if strings.TrimSpace(token) == "" {
		return db.PullRequestMulticaLink{}, false, nil
	}
	if s == nil || s.MulticaProjection == nil {
		return db.PullRequestMulticaLink{}, false, fmt.Errorf("multica link token was supplied but Multica projection is not configured")
	}
	claims, err := s.MulticaProjection.VerifyPRLinkToken(token)
	if err != nil {
		return db.PullRequestMulticaLink{}, false, fmt.Errorf("verify Multica PR link token: %w", err)
	}
	workspace := strings.Trim(strings.ToLower(claims.Workspace), "/")
	issueURL := claims.IssueURL
	if issueURL == "" && workspace != "" {
		issueURL = multicaprojection.IssueURL(s.MulticaProjection.AppURL(), workspace, claims.IssueKey)
	}
	link := authoritativeMulticaLink(pr.ID, pr.RepositoryID, claims, issueURL)
	dbConn := s.DBForCtx(ctx)
	if err := dbConn.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "pull_request_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"repository_id", "workspace", "workspace_id", "issue_id", "issue_key", "issue_url",
			"task_id", "agent_id", "run_id", "access_grant_id", "source_snapshot_id",
			"assertion_issuer", "assertion_version", "assertion_purpose", "assertion_audience", "assertion_jti",
			"confidence", "source", "completion_intent", "updated_at",
		}),
	}).Create(&link).Error; err != nil {
		return db.PullRequestMulticaLink{}, false, err
	}
	return link, true, nil
}

func authoritativeMulticaLink(prID, repositoryID uint, claims multicaprojection.PRLinkClaims, issueURL string) db.PullRequestMulticaLink {
	return db.PullRequestMulticaLink{
		PullRequestID: prID, RepositoryID: repositoryID,
		Workspace: strings.Trim(strings.ToLower(claims.Workspace), "/"), WorkspaceID: claims.WorkspaceID,
		IssueID: claims.IssueID, IssueKey: strings.ToUpper(claims.IssueKey), IssueURL: issueURL,
		TaskID: claims.TaskID, AgentID: claims.AgentID, RunID: claims.RunID,
		Confidence: db.MulticaLinkConfidenceAuthoritative, Source: db.MulticaLinkSourceTaskToken,
		CompletionIntent: true,
	}
}

func (s *Service) authoritativeMulticaLinkForPR(ctx context.Context, prID uint) (db.PullRequestMulticaLink, bool) {
	if s == nil || s.DB == nil || prID == 0 {
		return db.PullRequestMulticaLink{}, false
	}
	var link db.PullRequestMulticaLink
	err := s.DBForCtx(ctx).Where("pull_request_id = ? AND confidence = ?", prID, db.MulticaLinkConfidenceAuthoritative).First(&link).Error
	if err != nil {
		return db.PullRequestMulticaLink{}, false
	}
	return link, true
}

func multicaIssueRefFromLink(link db.PullRequestMulticaLink) multicaprojection.IssueRef {
	return multicaprojection.IssueRef{
		Workspace:   link.Workspace,
		WorkspaceID: link.WorkspaceID,
		IssueID:     link.IssueID,
		IssueKey:    link.IssueKey,
		URL:         link.IssueURL,
	}
}

func (s *Service) dispatchMulticaRegisterPullRequestLink(ctx context.Context, pr db.PullRequest, state string, forgejoRepo string, forgejoNumber int, forgejoURL string, mergedSHA string) {
	if s == nil || s.MulticaProjection == nil {
		return
	}
	link, ok := s.authoritativeMulticaLinkForPR(ctx, pr.ID)
	if !ok {
		return
	}
	request := multicaprojection.ExternalPRLinkRequest{
		Provider:         s.MulticaProjection.ExternalPRProvider(),
		IssueID:          link.IssueID,
		WorkspaceID:      link.WorkspaceID,
		Workspace:        link.Workspace,
		IssueKey:         link.IssueKey,
		ExternalRepo:     pr.Repository.FullName,
		ExternalNumber:   pr.Number,
		ExternalURL:      s.absAGSURL(fmt.Sprintf("/%s/pull/%d", pr.Repository.FullName, pr.Number)),
		MergeProvider:    "forgejo",
		MergeRepo:        forgejoRepo,
		MergeNumber:      forgejoNumber,
		MergeURL:         forgejoURL,
		MergedSHA:        mergedSHA,
		CompletionIntent: link.CompletionIntent,
		LinkConfidence:   link.Confidence,
		State:            state,
	}
	if facts, ok := s.delegatedPRMergeProjectionFacts(ctx, pr, forgejoRepo, forgejoNumber); ok {
		request.TargetInstance = facts.TargetInstance
		request.CanonicalRepositoryID = facts.CanonicalRepositoryID
		request.CanonicalRepository = facts.CanonicalRepository
		request.ProviderBindingID = facts.ProviderBindingID
		request.ProviderBindingRevision = facts.ProviderBindingRevision
		request.ProviderRepository = facts.ProviderRepository
		request.ExpectedHeadSHA = facts.ExpectedHeadSHA
		request.ExpectedBaseSHA = facts.ExpectedBaseSHA
		request.BaseRef = facts.BaseRef
		request.DelegatedMergeMethod = facts.MergeMethod
		request.ProjectionFactsRevision = facts.ProjectionFactsRevision
	}
	s.Wg.Add(1)
	go func() {
		defer s.Wg.Done()
		registerCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.MulticaProjection.RegisterPullRequestLink(registerCtx, request); err != nil {
			slog.WarnContext(ctx, "register Multica PR link failed", "repo", pr.Repository.FullName, "pr_number", pr.Number, "issue_id", link.IssueID, "error", err)
		}
	}()
}

type delegatedPRMergeProjectionFacts struct {
	TargetInstance          string `json:"target_instance"`
	CanonicalRepositoryID   string `json:"canonical_repository_id"`
	CanonicalRepository     string `json:"canonical_repository"`
	ProviderBindingID       string `json:"provider_binding_id"`
	ProviderBindingRevision string `json:"provider_binding_revision"`
	ProviderRepository      string `json:"provider_repository"`
	AGSPRNumber             int    `json:"ags_pr_number"`
	ProviderPRNumber        int    `json:"provider_pr_number"`
	ExpectedHeadSHA         string `json:"expected_head_sha"`
	ExpectedBaseSHA         string `json:"expected_base_sha"`
	BaseRef                 string `json:"base_ref"`
	MergeMethod             string `json:"merge_method"`
	ProjectionFactsRevision string `json:"-"`
}

func (s *Service) delegatedPRMergeProjectionFacts(ctx context.Context, pr db.PullRequest, forgejoRepo string, forgejoNumber int) (delegatedPRMergeProjectionFacts, bool) {
	if s == nil || s.ForgejoIntegration == nil || s.MulticaProjection == nil || s.Git == nil || pr.ID == 0 || pr.RepositoryID == 0 ||
		pr.Number <= 0 || forgejoNumber <= 0 || !canonicalActionSHA(pr.HeadSHA) {
		return delegatedPRMergeProjectionFacts{}, false
	}
	binding, err := s.ForgejoIntegration.ProviderMergeBindingForBase(pr.Repository.FullName, pr.RepositoryID, s.MulticaProjection.TargetInstance(), pr.BaseRef)
	if err != nil || binding.ProviderRepository != strings.TrimSpace(forgejoRepo) || binding.BaseRef != pr.BaseRef {
		return delegatedPRMergeProjectionFacts{}, false
	}
	baseSHA, err := s.Git.HeadSHA(ctx, pr.Repository.FullName, pr.BaseRef)
	if err != nil || !canonicalActionSHA(baseSHA) {
		return delegatedPRMergeProjectionFacts{}, false
	}
	facts := delegatedPRMergeProjectionFacts{
		TargetInstance: binding.TargetInstance, CanonicalRepositoryID: binding.CanonicalRepositoryID,
		CanonicalRepository: binding.CanonicalRepository, ProviderBindingID: binding.ProviderBindingID,
		ProviderBindingRevision: binding.ProviderBindingRevision, ProviderRepository: binding.ProviderRepository,
		AGSPRNumber: pr.Number, ProviderPRNumber: forgejoNumber, ExpectedHeadSHA: pr.HeadSHA,
		ExpectedBaseSHA: baseSHA, BaseRef: binding.BaseRef, MergeMethod: binding.MergeMethod,
	}
	encoded, _ := json.Marshal(facts)
	digest := sha256.Sum256(encoded)
	facts.ProjectionFactsRevision = "sha256:" + hex.EncodeToString(digest[:])
	return facts, true
}
