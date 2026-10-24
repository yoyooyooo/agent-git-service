package forgejointegration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProjectionDiagnosticPrincipalProbeFailureEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body, login, reason, source string
		status                            int
	}{
		{name: "unauthorized", status: 401, reason: "http_401", source: "http_status"},
		{name: "forbidden", status: 403, reason: "http_403", source: "http_status"},
		{name: "unavailable", status: 503, reason: "http_status", source: "http_status"},
		{name: "invalid JSON", status: 200, body: `{sentinel-body`, reason: "invalid_response", source: "response_validation"},
		{name: "owner mismatch", status: 200, body: `{"id":7,"login":"other"}`, login: "selected-owner", reason: "owner_mismatch", source: "response_validation"},
		{name: "invalid identity", status: 200, body: `{"id":0,"login":"other"}`, reason: "invalid_identity", source: "response_validation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.Method != http.MethodGet || r.Header.Get("Sudo") != "" {
					t.Error("identity probe wrote or impersonated")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			p := NewHTTPClient(server.URL, "").readPrincipal(context.Background(), tc.login)
			assertPrincipalProbeFailure(t, p, tc.reason, tc.source, tc.status)
			// A successful push must not erase the independent probe failure.
			d := projectionDiagnostic(context.Background(), PushRequest{}, p, nil)
			b, err := json.Marshal(d)
			if err != nil || d.Category != "none" || d.Principal.Probe != p.Probe || strings.Contains(string(b), "sentinel-body") || reads.Load() != 1 {
				t.Fatalf("unsafe or lost probe evidence: %s", b)
			}
		})
	}
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	c := NewHTTPClient(server.URL, "")
	assertPrincipalProbeFailure(t, c.readPrincipal(context.Background(), ""), "transport_failed", "transport", 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertPrincipalProbeFailure(t, c.readPrincipal(ctx, ""), "context_canceled", "transport", 0)
	assertPrincipalProbeFailure(t, unknownPrincipal(), "not_applicable", "none", 0)
}

func TestProjectionDiagnosticSSHInventoryFailureEvidence(t *testing.T) {
	for _, tc := range []struct {
		status               int
		body, reason, source string
		measured             int
	}{
		{401, "sentinel-body", "http_401", "http_status", 401},
		{403, "sentinel-body", "http_403", "http_status", 403},
		{503, "sentinel-body", "http_status", "http_status", 503},
		{200, "invalid sentinel-body", "invalid_response", "response_validation", 0},
		{200, "[]", "key_binding_not_found", "key_inventory", 0},
	} {
		t.Run(fmt.Sprint(tc.status, tc.reason), func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/users/selected-owner/keys" {
					t.Errorf("unexpected probe: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			p := selectedSSHPrincipal(context.Background(), NewHTTPClient(server.URL, ""), managedKey{Login: "selected-owner"})
			assertPrincipalProbeFailure(t, p, tc.reason, tc.source, tc.measured)
			b, _ := json.Marshal(p)
			if strings.Contains(string(b), "sentinel-body") || reads.Load() != 1 {
				t.Fatal("unsafe evidence or fallback probe")
			}
		})
	}
}

func assertPrincipalProbeFailure(t *testing.T, p Principal, reason, source string, status int) {
	t.Helper()
	if p.Status != "unknown" || p.ID != nil || p.Login != nil || p.Source != "none" || p.Probe == nil || p.Probe.ReasonCode != reason || p.Probe.EvidenceSource != source {
		t.Fatalf("missing safe probe evidence: %+v", p)
	}
	if status == 0 {
		if p.Probe.HTTPStatus != nil {
			t.Fatal("fabricated HTTP status")
		}
	} else if p.Probe.HTTPStatus == nil || *p.Probe.HTTPStatus != status {
		t.Fatalf("lost measured HTTP status: %+v", p.Probe)
	}
}
