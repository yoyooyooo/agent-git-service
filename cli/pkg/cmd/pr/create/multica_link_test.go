package create

import "testing"

func TestMulticaPRLinkTokenTargetAllowedRequiresExplicitAGSHost(t *testing.T) {
	t.Setenv("AGS_URL", "")
	t.Setenv("AGENT_GIT_SERVICE_URL", "")
	t.Setenv("MULTICA_EXTERNAL_PR_LINK_TOKEN_ALLOWED_HOSTS", "")
	if multicaPRLinkTokenTargetAllowed("github.com") {
		t.Fatalf("multicaPRLinkTokenTargetAllowed() allowed github.com without explicit AGS host")
	}
}

func TestMulticaPRLinkTokenTargetAllowedMatchesAGSURL(t *testing.T) {
	t.Setenv("AGS_URL", "http://primary.example.test:6666")
	t.Setenv("AGENT_GIT_SERVICE_URL", "")
	t.Setenv("MULTICA_EXTERNAL_PR_LINK_TOKEN_ALLOWED_HOSTS", "")
	if !multicaPRLinkTokenTargetAllowed("primary.example.test:6666") {
		t.Fatalf("multicaPRLinkTokenTargetAllowed() did not match AGS_URL host")
	}
	if multicaPRLinkTokenTargetAllowed("github.com") {
		t.Fatalf("multicaPRLinkTokenTargetAllowed() should not match unrelated host")
	}
}

func TestMulticaPRLinkTokenTargetAllowedMatchesAGSURLHostnameWithoutPort(t *testing.T) {
	t.Setenv("AGS_URL", "http://localhost:6666")
	t.Setenv("AGENT_GIT_SERVICE_URL", "")
	t.Setenv("MULTICA_EXTERNAL_PR_LINK_TOKEN_ALLOWED_HOSTS", "")
	if !multicaPRLinkTokenTargetAllowed("localhost") {
		t.Fatalf("multicaPRLinkTokenTargetAllowed() did not match AGS_URL hostname without port")
	}
	if !multicaPRLinkTokenTargetAllowed("localhost:6666") {
		t.Fatalf("multicaPRLinkTokenTargetAllowed() did not match AGS_URL host with port")
	}
	if multicaPRLinkTokenTargetAllowed("github.com") {
		t.Fatalf("multicaPRLinkTokenTargetAllowed() should not match unrelated host")
	}
}

func TestMulticaPRLinkTokenTargetAllowedMatchesAllowedHosts(t *testing.T) {
	t.Setenv("AGS_URL", "")
	t.Setenv("AGENT_GIT_SERVICE_URL", "")
	t.Setenv("MULTICA_EXTERNAL_PR_LINK_TOKEN_ALLOWED_HOSTS", "primary.example.test:6666, ags.local")
	if !multicaPRLinkTokenTargetAllowed("http://ags.local/owner/repo") {
		t.Fatalf("multicaPRLinkTokenTargetAllowed() did not match allowed host list")
	}
}
