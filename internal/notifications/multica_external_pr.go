package notifications

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"github.com/ngaut/agent-git-service/internal/service"
)

// MulticaExternalPRDispatcherConfig is server-owned configuration for the
// typed terminal projection target. It has no caller-provided URL or path.
type MulticaExternalPRDispatcherConfig struct {
	TargetName     string
	TargetInstance string
	ServerURL      string
	ServiceToken   string // resolved from service_token_file by integrations.LoadFile
	Timeout        time.Duration
}

// MulticaExternalPRDispatcher delivers the private durable wrapper through the
// existing closed ExternalPRLinkRequest wire. It never sends the wrapper.
type MulticaExternalPRDispatcher struct {
	targetName     string
	targetInstance string
	serverURL      string
	serviceToken   string
	client         *http.Client
}

func NewMulticaExternalPRDispatcher(cfg MulticaExternalPRDispatcherConfig) (*MulticaExternalPRDispatcher, error) {
	cfg.TargetName = strings.TrimSpace(cfg.TargetName)
	cfg.TargetInstance = strings.TrimSpace(cfg.TargetInstance)
	cfg.ServerURL = strings.TrimRight(strings.TrimSpace(cfg.ServerURL), "/")
	cfg.ServiceToken = strings.TrimSpace(cfg.ServiceToken)
	if cfg.TargetName == "" {
		cfg.TargetName = service.OutboundTargetNameMulticaExternalPR
	}
	if cfg.TargetInstance == "" || cfg.ServiceToken == "" {
		return nil, fmt.Errorf("Multica external PR dispatcher requires target instance and service token")
	}
	parsed, err := url.Parse(cfg.ServerURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("Multica server_url must be an origin without path, query, or credentials")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("Multica server_url must use http or https")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Timeout >= service.OutboundDeliveryLease {
		return nil, fmt.Errorf("Multica external PR dispatcher timeout must be less than outbound delivery lease")
	}
	return &MulticaExternalPRDispatcher{
		targetName:     cfg.TargetName,
		targetInstance: cfg.TargetInstance,
		serverURL:      cfg.ServerURL,
		serviceToken:   cfg.ServiceToken,
		client: &http.Client{
			Timeout: cfg.Timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// DispatchOutbound implements service.OutboundDispatcher for exactly one
// typed target. The private wrapper and its complete request are validated
// before any network call.
func (d *MulticaExternalPRDispatcher) DispatchOutbound(ctx context.Context, call service.OutboundDispatchCall) service.OutboundDeliveryResult {
	if d == nil {
		return service.OutboundDeliveryResult{Code: "multica_dispatcher_missing", Message: "Multica dispatcher is not configured"}
	}
	if strings.TrimSpace(call.Delivery.EventType) != service.OutboundEventMulticaExternalPRTerminal || strings.TrimSpace(call.Delivery.TargetName) != d.targetName || strings.TrimSpace(strings.ToLower(call.Delivery.TargetType)) != service.OutboundTargetTypeMulticaExternalPR {
		return service.OutboundDeliveryResult{Code: "multica_target_mismatch", Message: "delivery event or target is not the configured Multica typed target"}
	}
	delivery, err := multicaprojection.DecodeExternalPRTerminalDelivery([]byte(call.Delivery.PayloadJSON))
	if err != nil {
		return service.OutboundDeliveryResult{Code: "payload_contract_invalid", Message: "decode Multica terminal delivery failed"}
	}
	if delivery.Target.InstanceID != d.targetInstance || delivery.IdempotencyKey != strings.TrimSpace(call.Delivery.IdempotencyKey) {
		return service.OutboundDeliveryResult{Code: "payload_contract_invalid", Message: "Multica terminal delivery target or idempotency binding is invalid"}
	}
	path, err := multicaprojection.ExternalPRTerminalPath(delivery.Request.State)
	if err != nil {
		return service.OutboundDeliveryResult{Code: "payload_contract_invalid", Message: "Multica terminal delivery state is invalid"}
	}
	body, err := multicaprojection.MarshalExternalPRLinkRequest(delivery.Request)
	if err != nil {
		return service.OutboundDeliveryResult{Code: "payload_contract_invalid", Message: "encode closed Multica external PR request failed"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.serverURL+path, bytes.NewReader(body))
	if err != nil {
		return service.OutboundDeliveryResult{Code: "request_invalid", Message: "create Multica terminal request failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.serviceToken)
	req.Header.Set("Idempotency-Key", delivery.IdempotencyKey)
	resp, err := d.client.Do(req)
	if err != nil {
		return service.OutboundDeliveryResult{Retryable: true, Code: "network_error", Message: "post Multica external PR projection failed"}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return service.OutboundDeliveryResult{Delivered: true, HTTPStatus: resp.StatusCode}
	}
	retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	code := fmt.Sprintf("multica_http_%d", resp.StatusCode)
	return service.OutboundDeliveryResult{Retryable: retryable, HTTPStatus: resp.StatusCode, Code: code, Message: "Multica external PR projection rejected"}
}
