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

	"github.com/ngaut/agent-git-service/internal/service"
)

// FeishuWebhookConfig configures Feishu custom bot webhook delivery.
type FeishuWebhookConfig struct {
	ID             string
	Name           string
	Description    string
	Enabled        bool
	WebhookURL     string
	WebhookURLFile string
	Timeout        time.Duration
}

// FeishuMulticaIncidentNotifier sends Multica incident summaries to a Feishu
// custom bot webhook. It intentionally sends a compact incident projection, not
// raw event logs.
type FeishuMulticaIncidentNotifier struct {
	webhookURL string
	client     *http.Client
	dispatcher *FeishuTextDispatcher
}

// NewFeishuMulticaIncidentNotifier constructs a Multica incident notifier.
// Disabled config returns nil.
func NewFeishuMulticaIncidentNotifier(cfg FeishuWebhookConfig) (*FeishuMulticaIncidentNotifier, error) {
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
	return &FeishuMulticaIncidentNotifier{
		webhookURL: webhookURL,
		client:     &http.Client{Timeout: timeout},
	}, nil
}

func NewFeishuMulticaIncidentNotifierForTargets(configs []FeishuWebhookConfig) (*FeishuMulticaIncidentNotifier, error) {
	dispatcher, err := NewFeishuTextDispatcher(configs)
	if err != nil {
		return nil, err
	}
	return &FeishuMulticaIncidentNotifier{dispatcher: dispatcher}, nil
}

// SetHTTPClient overrides the HTTP client for tests.
func (n *FeishuMulticaIncidentNotifier) SetHTTPClient(client *http.Client) {
	if client != nil {
		n.client = client
	}
}

// NotifyMulticaIncident implements service.MulticaIncidentNotifier.
func (n *FeishuMulticaIncidentNotifier) NotifyMulticaIncident(ctx context.Context, note service.MulticaIncidentNotification) error {
	if n == nil {
		return nil
	}
	text := renderMulticaIncidentText(note)
	if n.dispatcher != nil {
		return n.dispatcher.SendText(ctx, text)
	}
	payload := map[string]any{
		"msg_type": "text",
		"content": map[string]string{
			"text": text,
		},
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
	if err := validateFeishuResponse(respBody); err != nil {
		return err
	}
	return nil
}

func renderMulticaIncidentText(note service.MulticaIncidentNotification) string {
	lines := []string{
		"Multica task failure incident",
		"Repo: " + note.RepoFullName,
		fmt.Sprintf("Incident: #%d %s", note.IssueNumber, note.IssueTitle),
	}
	if note.IssueURL != "" {
		lines = append(lines, "AGS issue: "+note.IssueURL)
	}
	if note.MulticaIssueKey != "" {
		line := "Multica: " + note.MulticaIssueKey
		if note.MulticaIssueTitle != "" {
			line += " " + note.MulticaIssueTitle
		}
		if note.MulticaIssueURL != "" {
			line += " " + note.MulticaIssueURL
		}
		lines = append(lines, line)
	}
	if note.AgentName != "" || note.AgentID != "" {
		lines = append(lines, "Agent: "+firstNonEmpty(note.AgentName, note.AgentID))
	}
	lines = append(lines, "Reason: "+note.FailureReason)
	if note.RuntimeProvider != "" || note.RuntimeID != "" {
		lines = append(lines, "Runtime: "+strings.TrimSpace(note.RuntimeProvider+" "+note.RuntimeID))
	}
	lines = append(lines, "Task: "+note.TaskID)
	if note.Attempt > 0 || note.MaxAttempts > 0 {
		lines = append(lines, fmt.Sprintf("Attempt: %d/%d", note.Attempt, note.MaxAttempts))
	}
	lines = append(lines, fmt.Sprintf("Window: %d recent / %d total", note.EventCount30d, note.TotalEventCount))
	if !note.OccurredAt.IsZero() {
		lines = append(lines, "Occurred: "+note.OccurredAt.UTC().Format(time.RFC3339))
	}
	if strings.TrimSpace(note.Error) != "" {
		lines = append(lines, "Error: "+truncateText(strings.TrimSpace(note.Error), 800))
	}
	return strings.Join(lines, "\n")
}

func validateFeishuResponse(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return nil
	}
	if code, ok := numericField(decoded, "code"); ok && code != 0 {
		return fmt.Errorf("feishu webhook code %.0f: %s", code, truncateText(string(trimmed), 500))
	}
	if code, ok := numericField(decoded, "StatusCode"); ok && code != 0 {
		return fmt.Errorf("feishu webhook status code %.0f: %s", code, truncateText(string(trimmed), 500))
	}
	return nil
}

func numericField(values map[string]any, key string) (float64, bool) {
	value, ok := values[key]
	if !ok {
		return 0, false
	}
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	default:
		return 0, false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truncateText(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	if max <= 1 {
		return value[:max]
	}
	return value[:max-1] + "…"
}
