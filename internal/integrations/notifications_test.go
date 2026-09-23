package integrations

import (
	"strings"
	"testing"
)

func TestNotificationsConfigResolveEventTargets(t *testing.T) {
	cfg := NotificationsConfig{
		Targets: map[string]NotificationTargetConfig{
			"frontend_ci_debug": {
				Type:           NotificationTargetTypeFeishuWebhook,
				Enabled:        true,
				Name:           "前端CI测试",
				Description:    "debug robot",
				WebhookURLFile: "/run/secrets/feishu-webhook",
			},
		},
		Events: map[string]NotificationEventConfig{
			NotificationEventPullRequestMerged: {
				Enabled: true,
				Targets: []string{"frontend_ci_debug"},
			},
		},
	}

	event, targets, ok, err := cfg.ResolveEventTargets(NotificationEventPullRequestMerged)
	if err != nil {
		t.Fatalf("ResolveEventTargets: %v", err)
	}
	if !ok || !event.Enabled || len(targets) != 1 {
		t.Fatalf("unexpected resolution: ok=%v event=%#v targets=%#v", ok, event, targets)
	}
	if targets[0].ID != "frontend_ci_debug" || targets[0].Target.Name != "前端CI测试" {
		t.Fatalf("target mismatch: %#v", targets[0])
	}
}

func TestNotificationsConfigRejectsEnabledEventWithoutTargets(t *testing.T) {
	cfg := NotificationsConfig{
		Events: map[string]NotificationEventConfig{
			NotificationEventPullRequestMerged: {Enabled: true},
		},
	}

	_, _, _, err := cfg.ResolveEventTargets(NotificationEventPullRequestMerged)
	if err == nil || !strings.Contains(err.Error(), "no targets") {
		t.Fatalf("expected no targets error, got %v", err)
	}
}

func TestNotificationsConfigRejectsMissingTarget(t *testing.T) {
	cfg := NotificationsConfig{
		Events: map[string]NotificationEventConfig{
			NotificationEventPullRequestMerged: {Enabled: true, Targets: []string{"missing"}},
		},
	}

	_, _, _, err := cfg.ResolveEventTargets(NotificationEventPullRequestMerged)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected missing target error, got %v", err)
	}
}
