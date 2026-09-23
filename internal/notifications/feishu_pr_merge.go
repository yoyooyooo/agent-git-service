package notifications

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/service"
)

// FeishuPullRequestMergeNotifier sends AGS pull-request merge notifications to
// configured Feishu webhook targets.
type FeishuPullRequestMergeNotifier struct {
	dispatcher *FeishuTextDispatcher
}

func NewFeishuPullRequestMergeNotifier(configs []FeishuWebhookConfig) (*FeishuPullRequestMergeNotifier, error) {
	dispatcher, err := NewFeishuTextDispatcher(configs)
	if err != nil {
		return nil, err
	}
	return &FeishuPullRequestMergeNotifier{dispatcher: dispatcher}, nil
}

func (n *FeishuPullRequestMergeNotifier) NotifyPullRequestMerged(ctx context.Context, note service.PullRequestMergedNotification) error {
	if n == nil || n.dispatcher == nil {
		return nil
	}
	return n.dispatcher.SendText(ctx, renderPullRequestMergedText(note, n.dispatcher.TargetLabels()))
}

func renderPullRequestMergedText(note service.PullRequestMergedNotification, targets []string) string {
	lines := []string{
		"AGS PR merged",
		"Repo: " + note.RepoFullName,
		fmt.Sprintf("PR: #%d %s", note.Number, note.Title),
	}
	if note.MergedByLogin != "" {
		lines = append(lines, "Merged by: "+note.MergedByLogin)
	}
	if note.Source != "" {
		lines = append(lines, "Source: "+note.Source)
	}
	if note.MergeMethod != "" {
		lines = append(lines, "Merge method: "+note.MergeMethod)
	}
	if note.BaseRef != "" {
		lines = append(lines, "Base: "+note.BaseRef)
	}
	if note.HeadRef != "" {
		lines = append(lines, "Head: "+note.HeadRef)
	}
	if note.MergeCommitSHA != "" {
		lines = append(lines, "Merge SHA: "+shortSHA(note.MergeCommitSHA))
	}
	if note.URL != "" {
		lines = append(lines, "AGS URL: "+note.URL)
	}
	if note.ForgejoURL != "" {
		lines = append(lines, "Forgejo PR: "+note.ForgejoURL)
	}
	if note.GitLabURL != "" {
		lines = append(lines, "GitLab MR: "+note.GitLabURL)
	}
	if note.GitHubURL != "" {
		lines = append(lines, "GitHub PR: "+note.GitHubURL)
	}
	if note.MulticaIssueURL != "" {
		label := strings.TrimSpace(note.MulticaIssueKey)
		if label == "" {
			label = "issue"
		}
		lines = append(lines, "Multica: "+label+" "+note.MulticaIssueURL)
	}
	if !note.MergedAt.IsZero() {
		lines = append(lines, "Merged at: "+note.MergedAt.UTC().Format(time.RFC3339))
	}
	return strings.Join(lines, "\n")
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
