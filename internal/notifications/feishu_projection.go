package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/service"
)

type FeishuProjectionNotifier struct {
	webhookURL string
	client     *http.Client
	dispatcher *FeishuTextDispatcher
}

func NewFeishuProjectionNotifier(cfg FeishuWebhookConfig) (*FeishuProjectionNotifier, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	webhookURL := strings.TrimSpace(cfg.WebhookURL)
	if webhookURL == "" && strings.TrimSpace(cfg.WebhookURLFile) != "" {
		data, err := os.ReadFile(strings.TrimSpace(cfg.WebhookURLFile))
		if err != nil {
			return nil, fmt.Errorf("read feishu webhook url file: %w", err)
		}
		webhookURL = strings.TrimSpace(string(data))
	}
	if webhookURL == "" {
		return nil, fmt.Errorf("feishu webhook url required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &FeishuProjectionNotifier{webhookURL: webhookURL, client: &http.Client{Timeout: timeout}}, nil
}

func NewFeishuProjectionNotifierForTargets(configs []FeishuWebhookConfig) (*FeishuProjectionNotifier, error) {
	dispatcher, err := NewFeishuTextDispatcher(configs)
	if err != nil {
		return nil, err
	}
	return &FeishuProjectionNotifier{dispatcher: dispatcher}, nil
}

func (n *FeishuProjectionNotifier) SetHTTPClient(client *http.Client) {
	if client != nil {
		n.client = client
	}
}

func (n *FeishuProjectionNotifier) NotifyProjectionDrift(ctx context.Context, note service.ProjectionDriftNotification) error {
	if n == nil {
		return nil
	}
	return n.sendText(ctx, renderProjectionDriftText(note))
}

func (n *FeishuProjectionNotifier) NotifyProjectionDriftResolved(ctx context.Context, note service.ProjectionDriftNotification) error {
	if n == nil {
		return nil
	}
	return n.sendText(ctx, renderProjectionDriftResolvedText(note))
}

func (n *FeishuProjectionNotifier) sendText(ctx context.Context, text string) error {
	if n.dispatcher != nil {
		return n.dispatcher.SendText(ctx, text)
	}
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal feishu payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create feishu request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("post feishu webhook: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("feishu webhook status %d: %s", resp.StatusCode, truncateText(string(respBody), 500))
	}
	return validateFeishuResponse(respBody)
}

func renderProjectionDriftText(note service.ProjectionDriftNotification) string {
	branch := note.Branch
	if branch == "" {
		branch = strings.TrimPrefix(note.Ref, "refs/heads/")
	}
	lines := []string{
		projectionDriftFeishuTitle(note.Provider, false),
		"Notification emitted at: " + time.Now().UTC().Format(time.RFC3339),
		"Repo: " + note.RepoFullName,
		"Ref: " + note.Ref,
		"Type: " + note.Type,
		"Authority: " + note.Authority,
		"AGS SHA: " + shortProjectionSHA(note.AGSSHA),
		"Forgejo SHA: " + shortProjectionSHA(note.ForgejoSHA),
		"First seen: " + note.FirstSeenAt.UTC().Format(time.RFC3339),
		"Last seen: " + note.LastSeenAt.UTC().Format(time.RFC3339),
	}
	if branch != "" {
		lines = append(lines, "Branch: "+branch)
	}
	if strings.TrimSpace(note.ErrorSummary) != "" {
		lines = append(lines, "Error: "+truncateText(strings.TrimSpace(note.ErrorSummary), 300))
	}
	if hint := projectionDriftHint(note); hint != "" {
		lines = append(lines, "Suggested action: "+hint)
	}
	return strings.Join(lines, "\n")
}

func renderProjectionDriftResolvedText(note service.ProjectionDriftNotification) string {
	branch := note.Branch
	if branch == "" {
		branch = strings.TrimPrefix(note.Ref, "refs/heads/")
	}
	resolvedAt := note.ResolvedAt
	if resolvedAt.IsZero() {
		resolvedAt = note.LastSeenAt
	}
	lines := []string{
		projectionDriftFeishuTitle(note.Provider, true),
		"Notification emitted at: " + time.Now().UTC().Format(time.RFC3339),
		"Repo: " + note.RepoFullName,
		"Ref: " + note.Ref,
		"Type: " + note.Type,
		"Authority: " + note.Authority,
		"AGS SHA: " + shortProjectionSHA(note.AGSSHA),
		"Forgejo SHA: " + shortProjectionSHA(note.ForgejoSHA),
		"First seen: " + note.FirstSeenAt.UTC().Format(time.RFC3339),
		"Last seen: " + note.LastSeenAt.UTC().Format(time.RFC3339),
		"Resolved at: " + resolvedAt.UTC().Format(time.RFC3339),
	}
	if branch != "" {
		lines = append(lines, "Branch: "+branch)
	}
	return strings.Join(lines, "\n")
}

func projectionDriftHint(note service.ProjectionDriftNotification) string {
	switch strings.TrimSpace(note.Type) {
	case forgejointegration.ProjectionFailureSourceBranchCleanupPending:
		branch := strings.TrimSpace(note.Branch)
		if branch == "" {
			branch = strings.TrimPrefix(strings.TrimSpace(note.Ref), "refs/heads/")
		}
		if branch == "" {
			return "merged source branch cleanup is pending; delete the stale AGS source branch, do not restore the Forgejo branch"
		}
		return "merged source branch cleanup is pending; delete AGS refs/heads/" + branch + ", do not restore the Forgejo branch"
	}
	if strings.Contains(note.Type, "protected_branch") {
		branch := strings.TrimSpace(note.Branch)
		if branch == "" {
			branch = strings.TrimPrefix(strings.TrimSpace(note.Ref), "refs/heads/")
		}
		if branch == "" {
			return "Forgejo protected branch rejected the AGS projection push; keep branch protection intact, then either run the authorized projection repair/seed path or adjust the projection mechanism, not human direct push"
		}
		return "Forgejo protected branch rejected the AGS projection push for " + branch + "; keep branch protection intact, then run the authorized projection repair/seed path or adjust the projection mechanism, not human direct push"
	}
	return ""
}

func shortProjectionSHA(sha string) string {
	if sha == "" {
		return "unknown"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func projectionDriftFeishuTitle(provider string, resolved bool) string {
	if strings.EqualFold(strings.TrimSpace(provider), service.ProjectionProviderGitLab) {
		if resolved {
			return "AGS GitLab projection drift resolved"
		}
		return "AGS GitLab projection drift"
	}
	if strings.EqualFold(strings.TrimSpace(provider), service.ProjectionProviderGitHub) {
		if resolved {
			return "AGS GitHub projection drift resolved"
		}
		return "AGS GitHub projection drift"
	}
	if resolved {
		return "AGS Forgejo projection drift resolved"
	}
	return "AGS Forgejo projection drift"
}
