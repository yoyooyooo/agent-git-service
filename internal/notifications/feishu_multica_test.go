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

func TestFeishuMulticaIncidentNotifierSendsTextPayload(t *testing.T) {
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

	notifier, err := NewFeishuMulticaIncidentNotifier(FeishuWebhookConfig{Enabled: true, WebhookURL: server.URL})
	if err != nil {
		t.Fatalf("new notifier: %v", err)
	}
	err = notifier.NotifyMulticaIncident(context.Background(), service.MulticaIncidentNotification{
		RepoFullName:      "octo/repo",
		IssueNumber:       7,
		IssueTitle:        "[multica-failure] HUM-1 lane-a timeout",
		IssueURL:          "http://ags/api/v3/repos/octo/repo/issues/7",
		MulticaIssueKey:   "HUM-1",
		MulticaIssueTitle: "Fix checkout",
		AgentName:         "lane-a",
		FailureReason:     "timeout",
		TaskID:            "task-1",
		EventCount30d:     2,
		TotalEventCount:   3,
		OccurredAt:        time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC),
		Error:             "runtime timed out",
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
	for _, want := range []string{"octo/repo", "HUM-1", "lane-a", "timeout", "task-1", "runtime timed out"} {
		if !strings.Contains(text, want) {
			t.Fatalf("payload missing %q:\n%s", want, text)
		}
	}
}

func TestFeishuMulticaIncidentNotifierReportsWebhookError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":19024,"msg":"bad webhook"}`))
	}))
	defer server.Close()

	notifier, err := NewFeishuMulticaIncidentNotifier(FeishuWebhookConfig{Enabled: true, WebhookURL: server.URL})
	if err != nil {
		t.Fatalf("new notifier: %v", err)
	}
	err = notifier.NotifyMulticaIncident(context.Background(), service.MulticaIncidentNotification{RepoFullName: "octo/repo"})
	if err == nil || !strings.Contains(err.Error(), "19024") {
		t.Fatalf("expected feishu error, got %v", err)
	}
}
