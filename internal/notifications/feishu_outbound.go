package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/service"
)

// FeishuOutboundDispatcher delivers outbound text payloads to named Feishu
// webhook targets and maps Feishu/provider failures into structured outbound
// results.
type FeishuOutboundDispatcher struct {
	targets map[string]feishuWebhookTarget
}

func NewFeishuOutboundDispatcher(configs []FeishuWebhookConfig) (*FeishuOutboundDispatcher, error) {
	targets := map[string]feishuWebhookTarget{}
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
		id := strings.TrimSpace(cfg.ID)
		if id == "" {
			id = strings.TrimSpace(cfg.Name)
		}
		if id == "" {
			return nil, fmt.Errorf("feishu outbound target id required")
		}
		targets[id] = feishuWebhookTarget{
			id:          id,
			name:        strings.TrimSpace(cfg.Name),
			description: strings.TrimSpace(cfg.Description),
			webhookURL:  webhookURL,
			client:      &http.Client{Timeout: timeout},
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no enabled feishu webhook targets")
	}
	return &FeishuOutboundDispatcher{targets: targets}, nil
}

func (d *FeishuOutboundDispatcher) DispatchOutbound(ctx context.Context, call service.OutboundDispatchCall) service.OutboundDeliveryResult {
	if d == nil || len(d.targets) == 0 {
		return service.OutboundDeliveryResult{Retryable: false, Code: "feishu_not_configured", Message: "feishu outbound dispatcher not configured"}
	}
	target, ok := d.targets[strings.TrimSpace(call.Delivery.TargetName)]
	if !ok {
		return service.OutboundDeliveryResult{Retryable: false, Code: "feishu_target_missing", Message: "feishu outbound target not found: " + call.Delivery.TargetName}
	}
	text, err := outboundTextFromPayload(string(call.Delivery.PayloadJSON))
	if err != nil {
		return service.OutboundDeliveryResult{Retryable: false, Code: "payload_invalid", Message: err.Error()}
	}
	return postFeishuTextResult(ctx, target.client, target.webhookURL, text)
}

func outboundTextFromPayload(payload string) (string, error) {
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return "", fmt.Errorf("decode outbound text payload: %w", err)
	}
	if strings.TrimSpace(decoded.Text) == "" {
		return "", fmt.Errorf("outbound text payload is empty")
	}
	return decoded.Text, nil
}

func postFeishuTextResult(ctx context.Context, client *http.Client, webhookURL, text string) service.OutboundDeliveryResult {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return service.OutboundDeliveryResult{Retryable: false, Code: "payload_invalid", Message: "marshal feishu payload: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return service.OutboundDeliveryResult{Retryable: false, Code: "request_invalid", Message: "create feishu request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return service.OutboundDeliveryResult{Retryable: true, Code: "network_error", Message: "post feishu webhook: " + err.Error()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return service.OutboundDeliveryResult{Retryable: retryable, HTTPStatus: resp.StatusCode, Code: fmt.Sprintf("http_%d", resp.StatusCode), Message: truncateText(string(respBody), 500)}
	}
	code, msg, ok := feishuResponseCode(respBody)
	if !ok || code == 0 {
		return service.OutboundDeliveryResult{Delivered: true, HTTPStatus: resp.StatusCode}
	}
	codeName := fmt.Sprintf("feishu_%.0f", code)
	return service.OutboundDeliveryResult{Retryable: code == 11232, HTTPStatus: resp.StatusCode, Code: codeName, Message: truncateText(firstNonEmpty(msg, string(bytes.TrimSpace(respBody))), 500)}
}

func feishuResponseCode(body []byte) (float64, string, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return 0, "", false
	}
	var decoded map[string]any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return 0, "", false
	}
	if code, ok := numericField(decoded, "code"); ok {
		return code, stringField(decoded, "msg"), true
	}
	if code, ok := numericField(decoded, "StatusCode"); ok {
		return code, stringField(decoded, "StatusMessage"), true
	}
	return 0, "", false
}

func stringField(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}
