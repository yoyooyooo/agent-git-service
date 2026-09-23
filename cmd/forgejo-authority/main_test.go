package main

import (
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/integrations"
)

func TestValidateAuthorityConfigRequiresProjectionDriftTarget(t *testing.T) {
	if err := validateAuthorityConfig(integrations.Config{}); err == nil || !strings.Contains(err.Error(), "projection_drift") {
		t.Fatalf("expected missing target failure, got %v", err)
	}

	cfg := integrations.Config{Outbound: integrations.OutboundConfig{
		Targets: map[string]integrations.OutboundTargetConfig{
			"ops": {Type: integrations.OutboundTargetTypeFeishuWebhook, Enabled: true, WebhookURL: "https://example.invalid/hook"},
		},
		Events: map[string]integrations.OutboundEventConfig{
			integrations.OutboundEventProjectionDrift: {Enabled: true, Targets: []string{"ops"}},
		},
	}}
	if err := validateAuthorityConfig(cfg); err != nil {
		t.Fatalf("expected valid projection target: %v", err)
	}
}
