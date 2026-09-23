package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/service"
)

func TestFeishuPullRequestMergeNotifierSendsTextPayload(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method=%s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("content-type=%q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	notifier, err := NewFeishuPullRequestMergeNotifier([]FeishuWebhookConfig{{
		Enabled:    true,
		ID:         "frontend_ci_debug",
		Name:       "前端CI测试",
		WebhookURL: server.URL,
	}})
	if err != nil {
		t.Fatalf("new notifier: %v", err)
	}
	err = notifier.NotifyPullRequestMerged(context.Background(), service.PullRequestMergedNotification{
		RepoFullName:    "octo/repo",
		Number:          12,
		Title:           "Fix checkout",
		URL:             "http://ags/octo/repo/pull/12",
		ForgejoURL:      "http://forgejo.local/octo/repo/pulls/42",
		GitLabURL:       "http://gitlab.local/octo/repo/-/merge_requests/7",
		GitHubURL:       "https://github.com/example-org/project-kit/pull/8",
		MulticaIssueKey: "HUM-60",
		MulticaIssueURL: "https://multica.ai/workspace-alpha/issues/HUM-60",
		BaseRef:         "main",
		HeadRef:         "agent/fix-checkout",
		MergeCommitSHA:  "abcdef1234567890",
		MergedByLogin:   "example-owner",
		MergeMethod:     "merge",
		Source:          "ags",
		MergedAt:        time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	if payload["msg_type"] != "text" {
		t.Fatalf("msg_type=%v", payload["msg_type"])
	}
	content, ok := payload["content"].(map[string]any)
	if !ok {
		t.Fatalf("content missing: %#v", payload)
	}
	text := content["text"].(string)
	for _, want := range []string{"AGS PR merged", "octo/repo", "#12 Fix checkout", "example-owner", "main", "agent/fix-checkout", "abcdef123456", "AGS URL: http://ags/octo/repo/pull/12", "Forgejo PR: http://forgejo.local/octo/repo/pulls/42", "GitLab MR: http://gitlab.local/octo/repo/-/merge_requests/7", "GitHub PR: https://github.com/example-org/project-kit/pull/8", "Multica: HUM-60 https://multica.ai/workspace-alpha/issues/HUM-60"} {
		if !strings.Contains(text, want) {
			t.Fatalf("payload missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"Notification target", "前端CI测试"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("payload should not include %q:\n%s", unwanted, text)
		}
	}
}
