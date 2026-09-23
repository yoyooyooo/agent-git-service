package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"github.com/ngaut/agent-git-service/internal/service"
)

func notificationDelivery(t *testing.T, state string) (multicaprojection.ExternalPRTerminalDelivery, string) {
	t.Helper()
	request := multicaprojection.ExternalPRLinkRequest{
		Provider:         "ags",
		IssueID:          "issue-id",
		WorkspaceID:      "workspace-id",
		Workspace:        "workspace-alpha",
		IssueKey:         "HUM-42",
		ExternalRepo:     "owner/repo",
		ExternalNumber:   7,
		ExternalURL:      "https://ags.example/owner/repo/pull/7",
		MergeProvider:    "forgejo",
		MergeRepo:        "forgejo/repo",
		MergeNumber:      42,
		MergeURL:         "https://forgejo.example/forgejo/repo/pulls/42",
		CompletionIntent: state == "merged",
		LinkConfidence:   "authoritative",
		State:            state,
	}
	if state == "merged" {
		request.MergedSHA = strings.Repeat("b", 40)
	}
	delivery, err := multicaprojection.NewExternalPRTerminalDelivery(multicaprojection.ExternalPRTerminalDeliveryInput{
		TargetInstance: "mini-prod",
		Request:        request,
		AGSPrID:        "owner/repo#7",
		ObservedAt:     time.Unix(10, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := multicaprojection.MarshalExternalPRTerminalDelivery(delivery)
	if err != nil {
		t.Fatal(err)
	}
	return delivery, payload
}

func TestMulticaExternalPRDispatcherUsesExistingClosedWireForBothStates(t *testing.T) {
	for _, tc := range []struct {
		state string
		path  string
	}{
		{state: "merged", path: multicaprojection.ExternalPRTerminalDeliveryMergedPath},
		{state: "closed", path: multicaprojection.ExternalPRTerminalDeliveryClosedPath},
	} {
		t.Run(tc.state, func(t *testing.T) {
			var gotPath, gotAuth, gotIdempotency string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotIdempotency = r.Header.Get("Idempotency-Key")
				gotBody, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusAccepted)
			}))
			defer server.Close()

			delivery, payload := notificationDelivery(t, tc.state)
			dispatcher, err := NewMulticaExternalPRDispatcher(MulticaExternalPRDispatcherConfig{
				TargetName:     service.OutboundTargetNameMulticaExternalPR,
				TargetInstance: "mini-prod",
				ServerURL:      server.URL,
				ServiceToken:   "service-secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			result := dispatcher.DispatchOutbound(context.Background(), service.OutboundDispatchCall{Delivery: db.OutboundDelivery{
				EventType:      service.OutboundEventMulticaExternalPRTerminal,
				TargetName:     service.OutboundTargetNameMulticaExternalPR,
				TargetType:     service.OutboundTargetTypeMulticaExternalPR,
				PayloadJSON:    db.LargeText(payload),
				IdempotencyKey: delivery.IdempotencyKey,
			}})
			wantBody, err := json.Marshal(delivery.Request)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Delivered || gotPath != tc.path || gotAuth != "Bearer service-secret" || gotIdempotency != delivery.IdempotencyKey || !bytes.Equal(gotBody, wantBody) {
				t.Fatalf("unexpected dispatch result=%#v path=%q auth=%q idempotency=%q body=%s want=%s", result, gotPath, gotAuth, gotIdempotency, gotBody, wantBody)
			}
			if bytes.Contains(gotBody, []byte(`"schema"`)) || bytes.Contains(gotBody, []byte(`"target"`)) {
				t.Fatalf("private wrapper leaked onto existing wire: %s", gotBody)
			}
		})
	}
}

func TestMulticaExternalPRDispatcherRejectsRedirectsWithoutFollowingOrForwarding(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var redirectedPosts int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/unexpected" {
					redirectedPosts++
					return
				}
				http.Redirect(w, r, "/unexpected", status)
			}))
			defer server.Close()

			delivery, payload := notificationDelivery(t, "merged")
			dispatcher, err := NewMulticaExternalPRDispatcher(MulticaExternalPRDispatcherConfig{
				TargetInstance: "mini-prod", ServerURL: server.URL, ServiceToken: "service-secret",
				Timeout: 15 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := dispatcher.DispatchOutbound(context.Background(), service.OutboundDispatchCall{Delivery: db.OutboundDelivery{
				EventType:      service.OutboundEventMulticaExternalPRTerminal,
				TargetName:     service.OutboundTargetNameMulticaExternalPR,
				TargetType:     service.OutboundTargetTypeMulticaExternalPR,
				PayloadJSON:    db.LargeText(payload),
				IdempotencyKey: delivery.IdempotencyKey,
			}})
			if result.Delivered || result.HTTPStatus != status || redirectedPosts != 0 {
				t.Fatalf("redirect was followed or accepted: result=%#v redirected_posts=%d", result, redirectedPosts)
			}
		})
	}
}

func TestMulticaExternalPRDispatcherRejectsCompletionIntentMismatchBeforeHTTP(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	for _, tc := range []struct {
		name  string
		state string
		claim bool
	}{
		{name: "merged without claim", state: "merged", claim: false},
		{name: "closed with claim", state: "closed", claim: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivery, _ := notificationDelivery(t, tc.state)
			delivery.Request.CompletionIntent = tc.claim
			payload, err := multicaprojection.MarshalExternalPRTerminalDelivery(delivery)
			if err != nil {
				t.Fatal(err)
			}
			dispatcher, err := NewMulticaExternalPRDispatcher(MulticaExternalPRDispatcherConfig{
				TargetInstance: "mini-prod", ServerURL: server.URL, ServiceToken: "service-secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			result := dispatcher.DispatchOutbound(context.Background(), service.OutboundDispatchCall{Delivery: db.OutboundDelivery{
				EventType:      service.OutboundEventMulticaExternalPRTerminal,
				TargetName:     service.OutboundTargetNameMulticaExternalPR,
				TargetType:     service.OutboundTargetTypeMulticaExternalPR,
				PayloadJSON:    db.LargeText(payload),
				IdempotencyKey: delivery.IdempotencyKey,
			}})
			if result.Delivered || result.Code != "payload_contract_invalid" || requests != 0 {
				t.Fatalf("invalid completion claim reached Multica: result=%#v requests=%d", result, requests)
			}
		})
	}
}

func TestMulticaExternalPRDispatcherRejectsMismatchedEventBeforeHTTP(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	delivery, payload := notificationDelivery(t, "merged")
	dispatcher, err := NewMulticaExternalPRDispatcher(MulticaExternalPRDispatcherConfig{
		TargetInstance: "mini-prod", ServerURL: server.URL, ServiceToken: "service-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := dispatcher.DispatchOutbound(context.Background(), service.OutboundDispatchCall{Delivery: db.OutboundDelivery{
		EventType:      service.OutboundEventPullRequestMerged,
		TargetName:     service.OutboundTargetNameMulticaExternalPR,
		TargetType:     service.OutboundTargetTypeMulticaExternalPR,
		PayloadJSON:    db.LargeText(payload),
		IdempotencyKey: delivery.IdempotencyKey,
	}})
	if result.Delivered || result.Code != "multica_target_mismatch" || requests != 0 {
		t.Fatalf("mismatched event was dispatched: result=%#v requests=%d", result, requests)
	}
}

func TestMulticaExternalPRDispatcherRejectsArbitraryURLWrongTargetAndMalformedEnvelope(t *testing.T) {
	for _, raw := range []string{"https://example.test/raw/path", "https://user:pass@example.test", "https://example.test/?token=secret", "ftp://example.test"} {
		if _, err := NewMulticaExternalPRDispatcher(MulticaExternalPRDispatcherConfig{TargetInstance: "mini-prod", ServerURL: raw, ServiceToken: "secret"}); err == nil {
			t.Fatalf("expected server URL rejection for %q", raw)
		}
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	dispatcher, err := NewMulticaExternalPRDispatcher(MulticaExternalPRDispatcherConfig{TargetInstance: "mini-prod", ServerURL: server.URL, ServiceToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		target  string
		payload string
		key     string
		code    string
	}{
		{name: "wrong target", target: service.OutboundTargetTypeFeishuWebhook, payload: `{}`, code: "multica_target_mismatch"},
		{name: "unknown field", target: service.OutboundTargetTypeMulticaExternalPR, payload: `{"schema":"ags.multica-external-pr-projection.v1","unknown":true}`, code: "payload_contract_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := dispatcher.DispatchOutbound(context.Background(), service.OutboundDispatchCall{Delivery: db.OutboundDelivery{
				EventType:      service.OutboundEventMulticaExternalPRTerminal,
				TargetName:     service.OutboundTargetNameMulticaExternalPR,
				TargetType:     tc.target,
				PayloadJSON:    db.LargeText(tc.payload),
				IdempotencyKey: tc.key,
			}})
			if result.Code != tc.code || result.Delivered || strings.Contains(result.Message, "secret") {
				t.Fatalf("malformed dispatch result=%#v", result)
			}
		})
	}
	if _, err := NewMulticaExternalPRDispatcher(MulticaExternalPRDispatcherConfig{TargetInstance: "mini-prod", ServerURL: server.URL, ServiceToken: "secret", Timeout: service.OutboundDeliveryLease}); err == nil {
		t.Fatal("dispatcher accepted timeout equal to lease")
	}
}
