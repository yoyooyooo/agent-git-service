package forgejointegration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestIntegrationVerifyAndParseMergedPullRequestWebhook(t *testing.T) {
	body := []byte(`{"action":"closed","repository":{"full_name":"example-owner/demo"},"pull_request":{"number":1,"merged":true,"base":{"ref":"main"}}}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	integration := New(Config{Enabled: true, WebhookSecret: "secret"}, &fakeClient{}, nil)
	event, ok, err := integration.VerifyAndParseMergedPullRequestWebhook(sig, body)
	if err != nil {
		t.Fatalf("VerifyAndParseMergedPullRequestWebhook: %v", err)
	}
	if !ok || event.RepoFullName != "example-owner/demo" || event.BaseBranch != "main" {
		t.Fatalf("event=%#v ok=%v", event, ok)
	}
}

func TestRemoteURLForRepoUsesMapping(t *testing.T) {
	integration := New(Config{
		Enabled:      true,
		BaseURL:      "http://forgejo.local",
		Token:        "secret-token",
		DefaultOwner: "ci",
		RepoMap: map[string]RepoMapping{
			"example-owner/demo": {Owner: "forgejo", Repo: "demo-ci"},
		},
	}, &fakeClient{}, nil)
	url, err := integration.RemoteURLForRepo("example-owner/demo")
	if err != nil {
		t.Fatalf("RemoteURLForRepo: %v", err)
	}
	if url != "http://x-access-token:secret-token@forgejo.local/forgejo/demo-ci.git" {
		t.Fatalf("url=%q", url)
	}
}
