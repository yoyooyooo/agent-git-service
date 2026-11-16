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

func TestRepoFlagCollectsRepeatableSelectors(t *testing.T) {
	var repos repoFlag
	for _, value := range []string{"jackie/lumi", " ci/widget "} {
		if err := repos.Set(value); err != nil {
			t.Fatalf("set %q: %v", value, err)
		}
	}
	if got := repos.String(); got != "jackie/lumi,ci/widget" {
		t.Fatalf("unexpected selectors: %q", got)
	}
	if err := repos.Set("  "); err == nil {
		t.Fatal("empty -repo was accepted")
	}
}
