package forgejointegration

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifyWebhookSignatureAcceptsHubSHA256(t *testing.T) {
	body := []byte(`{"action":"closed"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if err := VerifyWebhookSignature("secret", sig, body); err != nil {
		t.Fatalf("VerifyWebhookSignature: %v", err)
	}
	if err := VerifyWebhookSignature("secret", "sha256=bad", body); err == nil {
		t.Fatal("expected bad signature to fail")
	}
}

func TestParseMergedPullRequestWebhook(t *testing.T) {
	payload := []byte(`{
	  "action": "closed",
	  "repository": {"full_name": "example-owner/demo", "default_branch": "main"},
	  "pull_request": {
	    "number": 7,
	    "merged": true,
	    "html_url": "http://forgejo/example-owner/demo/pulls/7",
	    "head": {"ref": "agent/demo"},
	    "base": {"ref": "main"}
	  }
	}`)
	event, ok, err := ParseMergedPullRequestWebhook(payload)
	if err != nil {
		t.Fatalf("ParseMergedPullRequestWebhook: %v", err)
	}
	if !ok {
		t.Fatal("expected merged PR event")
	}
	if event.RepoFullName != "example-owner/demo" || event.PRNumber != 7 || event.BaseBranch != "main" || event.HeadBranch != "agent/demo" {
		t.Fatalf("unexpected event=%#v", event)
	}
}

func TestParseClosedPullRequestWebhookIncludesUnmergedClose(t *testing.T) {
	payload := []byte(`{
	  "action": "closed",
	  "repository": {"full_name": "example-owner/demo", "default_branch": "main"},
	  "pull_request": {
	    "number": 7,
	    "merged": false,
	    "html_url": "http://forgejo/example-owner/demo/pulls/7",
	    "head": {"ref": "agent/demo"},
	    "base": {"ref": "main"}
	  }
	}`)
	event, ok, err := ParseClosedPullRequestWebhook(payload)
	if err != nil {
		t.Fatalf("ParseClosedPullRequestWebhook: %v", err)
	}
	if !ok {
		t.Fatal("expected closed unmerged PR event")
	}
	if event.Merged || event.RepoFullName != "example-owner/demo" || event.PRNumber != 7 || event.BaseBranch != "main" || event.HeadBranch != "agent/demo" {
		t.Fatalf("unexpected event=%#v", event)
	}
}

func TestParsePullRequestActionLabelWebhook(t *testing.T) {
	payload := []byte(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/demo", "default_branch": "main"},
	  "pull_request": {
	    "number": 7,
	    "html_url": "http://forgejo/forgejo/demo/pulls/7",
	    "head": {"ref": "agent/demo", "sha": "abc123"},
	    "base": {"ref": "main"},
	    "labels": [{"name": "ags/action-rebase"}]
	  },
	  "label": {"name": "ags/action-rebase"},
	  "sender": {"login": "operator"}
	}`)
	event, ok, err := ParsePullRequestActionLabelWebhook(payload)
	if err != nil {
		t.Fatalf("ParsePullRequestActionLabelWebhook: %v", err)
	}
	if !ok {
		t.Fatal("expected action label event")
	}
	if event.RepoFullName != "forgejo/demo" || event.PRNumber != 7 || event.LabelName != AGSActionRebaseLabel || event.SenderLogin != "operator" || event.HeadBranch != "agent/demo" || event.HeadSHA != "abc123" {
		t.Fatalf("unexpected event=%#v", event)
	}
}

func TestParsePullRequestActionLabelWebhookIgnoresActionRemoval(t *testing.T) {
	payload := []byte(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/demo"},
	  "pull_request": {"number": 7, "labels": []},
	  "label": {"name": "ags/action-rebase"},
	  "sender": {"login": "operator"}
	}`)
	_, ok, err := ParsePullRequestActionLabelWebhook(payload)
	if err != nil {
		t.Fatalf("ParsePullRequestActionLabelWebhook: %v", err)
	}
	if ok {
		t.Fatal("action-label removal must not retrigger handling")
	}
}

func TestParsePullRequestActionLabelWebhookIgnoresStatusLabel(t *testing.T) {
	payload := []byte(`{
	  "action": "label_updated",
	  "repository": {"full_name": "forgejo/demo"},
	  "pull_request": {"number": 7, "labels": [{"name": "ags/status-rebasing"}]},
	  "label": {"name": "ags/status-rebasing"},
	  "sender": {"login": "operator"}
	}`)
	_, ok, err := ParsePullRequestActionLabelWebhook(payload)
	if err != nil {
		t.Fatalf("ParsePullRequestActionLabelWebhook: %v", err)
	}
	if ok {
		t.Fatal("status labels must not trigger action handling")
	}
}

func TestParseDeletedBranchWebhook(t *testing.T) {
	payload := []byte(`{
	  "ref": "agent/lane-d/probe-delete-after",
	  "ref_type": "branch",
	  "repository": {"full_name": "example-owner/demo"},
	  "pusher_type": "user"
	}`)
	event, ok, err := ParseDeletedBranchWebhook(payload)
	if err != nil {
		t.Fatalf("ParseDeletedBranchWebhook: %v", err)
	}
	if !ok {
		t.Fatal("expected deleted branch event")
	}
	if event.RepoFullName != "example-owner/demo" || event.BranchName != "agent/lane-d/probe-delete-after" {
		t.Fatalf("unexpected event=%#v", event)
	}
}
