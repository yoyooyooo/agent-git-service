package service

import (
	"fmt"
	"strings"
	"time"
)

func pullRequestMergedOutboundIntent(note PullRequestMergedNotification, target OutboundTarget, manualReplay bool) OutboundDeliveryIntent {
	mergeSHA := strings.TrimSpace(note.MergeCommitSHA)
	keySHA := mergeSHA
	if keySHA == "" {
		keySHA = "unknown"
	}
	return OutboundDeliveryIntent{
		EventType:      OutboundEventPullRequestMerged,
		TargetName:     strings.TrimSpace(target.Name),
		TargetType:     strings.TrimSpace(strings.ToLower(target.Type)),
		SubjectType:    "pull_request",
		SubjectKey:     fmt.Sprintf("%s#%d", strings.TrimSpace(note.RepoFullName), note.Number),
		IdempotencyKey: fmt.Sprintf("%s:%s:%d:%s:%s", OutboundEventPullRequestMerged, strings.TrimSpace(note.RepoFullName), note.Number, keySHA, strings.TrimSpace(target.Name)),
		PayloadVersion: "pr_merge_text_v1",
		PayloadJSON:    outboundTextPayloadJSON(renderPullRequestMergedOutboundText(note, manualReplay), manualReplay),
		RepoFullName:   strings.TrimSpace(note.RepoFullName),
		PRNumber:       note.Number,
		MergeSHA:       mergeSHA,
	}
}

func renderPullRequestMergedOutboundText(note PullRequestMergedNotification, manualReplay bool) string {
	lines := []string{"AGS PR merged"}
	if manualReplay {
		lines = append(lines, "Manual replay: true")
	}
	lines = append(lines,
		"Repo: "+note.RepoFullName,
		fmt.Sprintf("PR: #%d %s", note.Number, note.Title),
	)
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
		lines = append(lines, "Merge SHA: "+shortOutboundSHA(note.MergeCommitSHA))
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

func shortOutboundSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
