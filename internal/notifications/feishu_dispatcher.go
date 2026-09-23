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
)

type feishuWebhookTarget struct {
	id          string
	name        string
	description string
	webhookURL  string
	client      *http.Client
}

// FeishuTextDispatcher sends one text message to one or more configured
// Feishu custom bot webhooks.
type FeishuTextDispatcher struct {
	targets []feishuWebhookTarget
}

func NewFeishuTextDispatcher(configs []FeishuWebhookConfig) (*FeishuTextDispatcher, error) {
	targets := make([]feishuWebhookTarget, 0, len(configs))
	for _, cfg := range configs {
		if !cfg.Enabled {
			continue
		}
		webhookURL, err := resolveFeishuWebhookURL(cfg)
		if err != nil {
			return nil, fmt.Errorf("feishu target %q: %w", feishuTargetLabel(cfg), err)
		}
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		targets = append(targets, feishuWebhookTarget{
			id:          strings.TrimSpace(cfg.ID),
			name:        strings.TrimSpace(cfg.Name),
			description: strings.TrimSpace(cfg.Description),
			webhookURL:  webhookURL,
			client:      &http.Client{Timeout: timeout},
		})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no enabled feishu webhook targets")
	}
	return &FeishuTextDispatcher{targets: targets}, nil
}

func (d *FeishuTextDispatcher) TargetLabels() []string {
	if d == nil {
		return nil
	}
	labels := make([]string, 0, len(d.targets))
	for _, target := range d.targets {
		labels = append(labels, feishuTargetDisplay(target.id, target.name))
	}
	return labels
}

func (d *FeishuTextDispatcher) SendText(ctx context.Context, text string) error {
	if d == nil || len(d.targets) == 0 {
		return nil
	}
	var errs []string
	for _, target := range d.targets {
		if err := postFeishuText(ctx, target.client, target.webhookURL, text); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", feishuTargetDisplay(target.id, target.name), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("feishu webhook delivery failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func resolveFeishuWebhookURL(cfg FeishuWebhookConfig) (string, error) {
	webhookURL := strings.TrimSpace(cfg.WebhookURL)
	if webhookURL == "" && strings.TrimSpace(cfg.WebhookURLFile) != "" {
		data, err := os.ReadFile(strings.TrimSpace(cfg.WebhookURLFile))
		if err != nil {
			return "", fmt.Errorf("read webhook url file: %w", err)
		}
		webhookURL = strings.TrimSpace(string(data))
	}
	if webhookURL == "" {
		return "", fmt.Errorf("webhook url required")
	}
	return webhookURL, nil
}

func postFeishuText(ctx context.Context, client *http.Client, webhookURL, text string) error {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create feishu request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
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

func feishuTargetLabel(cfg FeishuWebhookConfig) string {
	return feishuTargetDisplay(strings.TrimSpace(cfg.ID), strings.TrimSpace(cfg.Name))
}

func feishuTargetDisplay(id, name string) string {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if name != "" && id != "" {
		return name + "(" + id + ")"
	}
	if name != "" {
		return name
	}
	if id != "" {
		return id
	}
	return "unnamed"
}
