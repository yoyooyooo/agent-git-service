package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	applog "github.com/ngaut/agent-git-service/internal/logging"
)

// PullRequestMergeNotifier delivers compact pull-request merge facts to an
// external notification backend.
type PullRequestMergeNotifier interface {
	NotifyPullRequestMerged(context.Context, PullRequestMergedNotification) error
}

// PullRequestMergedNotification is the stable event payload for a PR that has
// become merged in AGS, either by AGS itself or by an accepted projection
// callback such as Forgejo.
type PullRequestMergedNotification struct {
	RepoFullName    string
	Number          int
	Title           string
	URL             string
	ForgejoURL      string
	GitLabURL       string
	GitHubURL       string
	MulticaIssueKey string
	MulticaIssueURL string
	BaseRef         string
	HeadRef         string
	MergeCommitSHA  string
	MergedByLogin   string
	MergeMethod     string
	Source          string
	MergedAt        time.Time
}

func (s *Service) emitPullRequestMergedNotification(ctx context.Context, pr db.PullRequest, source, mergeMethod string) {
	if s == nil || (s.PullRequestMergeNotifier == nil && len(s.OutboundEventTargets[OutboundEventPullRequestMerged]) == 0) {
		return
	}
	note := s.buildPullRequestMergedNotification(ctx, pr, source, mergeMethod)
	if s.Ctx == nil {
		s.sendPullRequestMergedNotification(ctx, note)
		return
	}
	bgCtx := s.ServerCtx()
	bgCtx = applog.CloneContext(bgCtx, ctx)
	if tenantDB, ok := DBFromContext(ctx); ok {
		bgCtx = ContextWithDB(bgCtx, tenantDB)
	}
	applog.AddAttrs(bgCtx,
		slog.String("repo", note.RepoFullName),
		slog.Int("pr_number", note.Number),
		slog.String("source", note.Source),
	)
	s.Wg.Add(1)
	go func() {
		defer s.Wg.Done()
		s.sendPullRequestMergedNotification(bgCtx, note)
	}()
}

func (s *Service) sendPullRequestMergedNotification(ctx context.Context, note PullRequestMergedNotification) {
	if len(s.OutboundEventTargets[OutboundEventPullRequestMerged]) > 0 && s.OutboundDispatcher != nil {
		s.sendPullRequestMergedOutbound(ctx, note)
		return
	}
	if s.PullRequestMergeNotifier == nil {
		return
	}
	if err := s.PullRequestMergeNotifier.NotifyPullRequestMerged(ctx, note); err != nil {
		slog.WarnContext(ctx, "pull request merge notification failed", "repo", note.RepoFullName, "pr_number", note.Number, "source", note.Source, "error", err)
		return
	}
	slog.InfoContext(ctx, "pull request merge notification sent", "repo", note.RepoFullName, "pr_number", note.Number, "source", note.Source)
}

func (s *Service) sendPullRequestMergedOutbound(ctx context.Context, note PullRequestMergedNotification) {
	for _, target := range s.OutboundEventTargets[OutboundEventPullRequestMerged] {
		target.Name = strings.TrimSpace(target.Name)
		target.Type = strings.TrimSpace(strings.ToLower(target.Type))
		if target.Name == "" || target.Type == "" {
			continue
		}
		intent := pullRequestMergedOutboundIntent(note, target, false)
		delivery, created, err := s.EnqueueOutboundDelivery(ctx, intent)
		if err != nil {
			slog.WarnContext(ctx, "pull request merge outbound enqueue failed", "repo", note.RepoFullName, "pr_number", note.Number, "target", target.Name, "error", err)
			continue
		}
		if !created && delivery.Status == OutboundDeliveryStatusDelivered {
			slog.InfoContext(ctx, "pull request merge outbound already delivered", "repo", note.RepoFullName, "pr_number", note.Number, "target", target.Name, "delivery_id", delivery.ID)
			continue
		}
		if err := s.DeliverOutboundDeliveryNow(ctx, delivery.ID); err != nil {
			slog.WarnContext(ctx, "pull request merge outbound delivery failed", "repo", note.RepoFullName, "pr_number", note.Number, "target", target.Name, "delivery_id", delivery.ID, "error", err)
			continue
		}
		slog.InfoContext(ctx, "pull request merge outbound delivery attempted", "repo", note.RepoFullName, "pr_number", note.Number, "target", target.Name, "delivery_id", delivery.ID)
	}
}

func (s *Service) buildPullRequestMergedNotification(ctx context.Context, pr db.PullRequest, source, mergeMethod string) PullRequestMergedNotification {
	repoFullName := strings.TrimSpace(pr.Repository.FullName)
	if repoFullName == "" && pr.RepositoryID != 0 {
		if repo, err := s.GetRepoByID(ctx, fmt.Sprint(pr.RepositoryID)); err == nil {
			repoFullName = repo.FullName
		}
	}
	mergedAt := time.Time{}
	if pr.MergedAt != nil {
		mergedAt = *pr.MergedAt
	}
	url := ""
	if repoFullName != "" && pr.Number != 0 {
		url = s.absAGSURL(fmt.Sprintf("/%s/pull/%d", repoFullName, pr.Number))
	}
	forgejoURL := ""
	gitLabURL := ""
	gitHubURL := ""
	if pr.ID != 0 {
		forgejoURL = s.pullRequestProjectionURL(ctx, pr.ID, ProjectionProviderForgejo)
		gitLabURL = s.pullRequestProjectionURL(ctx, pr.ID, ProjectionProviderGitLab)
		gitHubURL = s.pullRequestProjectionURL(ctx, pr.ID, ProjectionProviderGitHub)
	}
	multicaRef := s.multicaIssueRefForPR(ctx, repoFullName, pr)
	return PullRequestMergedNotification{
		RepoFullName:    repoFullName,
		Number:          pr.Number,
		Title:           pr.Title,
		URL:             url,
		ForgejoURL:      forgejoURL,
		GitLabURL:       gitLabURL,
		GitHubURL:       gitHubURL,
		MulticaIssueKey: multicaRef.IssueKey,
		MulticaIssueURL: multicaRef.URL,
		BaseRef:         pr.BaseRef,
		HeadRef:         pr.HeadRef,
		MergeCommitSHA:  pr.MergeCommitSHA,
		MergedByLogin:   pr.MergedByLogin,
		MergeMethod:     strings.TrimSpace(strings.ToLower(mergeMethod)),
		Source:          strings.TrimSpace(source),
		MergedAt:        mergedAt,
	}
}

func (s *Service) pullRequestProjectionURL(ctx context.Context, prID uint, provider string) string {
	if s == nil || prID == 0 {
		return ""
	}
	var projection db.PullRequestProjection
	if err := s.DBForCtx(ctx).
		Select("external_url").
		Where("pull_request_id = ? AND provider = ? AND external_url <> ''", prID, strings.TrimSpace(provider)).
		Order("updated_at DESC, id DESC").
		Limit(1).
		Find(&projection).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(projection.ExternalURL)
}
