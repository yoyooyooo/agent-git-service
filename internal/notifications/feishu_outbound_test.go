package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestFeishuOutboundDispatcherClassifiesFrequencyLimitAsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":11232,"data":{},"msg":"frequency limited psm[lark.oapi.app_platform_runtime]appID[1500]"}`))
	}))
	defer server.Close()

	dispatcher, err := NewFeishuOutboundDispatcher([]FeishuWebhookConfig{{ID: "department_official", Enabled: true, WebhookURL: server.URL}})
	if err != nil {
		t.Fatalf("NewFeishuOutboundDispatcher: %v", err)
	}
	result := dispatcher.DispatchOutbound(context.Background(), service.OutboundDispatchCall{Delivery: db.OutboundDelivery{TargetName: "department_official", PayloadJSON: db.LargeText(`{"text":"hello"}`)}})
	if result.Delivered || !result.Retryable || result.Code != "feishu_11232" || result.Message == "" {
		t.Fatalf("expected retryable feishu_11232, got %#v", result)
	}
}

func TestFeishuOutboundDispatcherDeliversSuccessfulText(t *testing.T) {
	var hit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{},"msg":"success"}`))
	}))
	defer server.Close()

	dispatcher, err := NewFeishuOutboundDispatcher([]FeishuWebhookConfig{{ID: "department_official", Enabled: true, WebhookURL: server.URL}})
	if err != nil {
		t.Fatalf("NewFeishuOutboundDispatcher: %v", err)
	}
	result := dispatcher.DispatchOutbound(context.Background(), service.OutboundDispatchCall{Delivery: db.OutboundDelivery{TargetName: "department_official", PayloadJSON: db.LargeText(`{"text":"hello"}`)}})
	if !hit || !result.Delivered || result.Retryable || result.Code != "" {
		t.Fatalf("expected delivered result, got hit=%v result=%#v", hit, result)
	}
}
