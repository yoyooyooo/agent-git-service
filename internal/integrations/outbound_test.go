package integrations

import "testing"

func TestOutboundConfigResolveEventTargets(t *testing.T) {
	cfg := Config{
		Outbound: OutboundConfig{
			Targets: map[string]OutboundTargetConfig{
				"department_official": {
					Type:           OutboundTargetTypeFeishuWebhook,
					Enabled:        true,
					Name:           "部门正式群",
					WebhookURLFile: "/run/secrets/feishu-webhook",
				},
			},
			Events: map[string]OutboundEventConfig{
				OutboundEventPullRequestMerged: {Enabled: true, Targets: []string{"department_official"}},
			},
		},
	}

	event, targets, ok, err := cfg.ResolveOutboundEventTargets(OutboundEventPullRequestMerged)
	if err != nil {
		t.Fatalf("ResolveOutboundEventTargets: %v", err)
	}
	if !ok || !event.Enabled || len(targets) != 1 || targets[0].ID != "department_official" || targets[0].Target.Name != "部门正式群" {
		t.Fatalf("unexpected outbound resolution: ok=%v event=%#v targets=%#v", ok, event, targets)
	}
}

func TestOutboundConfigFallsBackToLegacyNotifications(t *testing.T) {
	cfg := Config{
		Notifications: NotificationsConfig{
			Targets: map[string]NotificationTargetConfig{
				"department_official": {
					Type:           NotificationTargetTypeFeishuWebhook,
					Enabled:        true,
					Name:           "部门正式群",
					WebhookURLFile: "/run/secrets/feishu-webhook",
				},
			},
			Events: map[string]NotificationEventConfig{
				NotificationEventPullRequestMerged: {Enabled: true, Targets: []string{"department_official"}},
			},
		},
	}

	_, targets, ok, err := cfg.ResolveOutboundEventTargets(OutboundEventPullRequestMerged)
	if err != nil {
		t.Fatalf("ResolveOutboundEventTargets legacy fallback: %v", err)
	}
	if !ok || len(targets) != 1 || targets[0].ID != "department_official" || targets[0].Target.Type != OutboundTargetTypeFeishuWebhook {
		t.Fatalf("unexpected legacy fallback: ok=%v targets=%#v", ok, targets)
	}
}
