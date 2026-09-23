package edge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
)

func TestDiagnosticsKeepCredentialsOutAndRejectPublicSocketPeers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("raw-upstream-sensitive-body"))
	}))
	defer upstream.Close()
	s, err := New(config.EdgeConfig{ID: "diag", PrimaryURL: upstream.URL, CanonicalURL: "http://primary.example.test:6666", UnboundReads: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 70; i++ {
		r := httptest.NewRequest("GET", "http://primary.example.test:6666/api/v3/user?token=secret-query-value", nil)
		r.Header.Set("Authorization", "Bearer secret-authorization-value")
		r.Header.Set(requestIDHeader, "attacker-chosen-id")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Header().Get(requestIDHeader) == "attacker-chosen-id" {
			t.Fatal("trusted caller id")
		}
	}
	get := func(addr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://localhost/status", nil)
		r.RemoteAddr = addr
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		s.DiagnosticsHandler().ServeHTTP(w, r)
		return w
	}
	if w := get("10.0.0.2:1234"); w.Code != 404 {
		t.Fatal("forwarded address bypassed socket policy")
	}
	w := get("127.0.0.1:1234")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, bad := range []string{"secret-query-value", "secret-authorization-value", "raw-upstream-sensitive-body", "attacker-chosen-id"} {
		if strings.Contains(w.Body.String(), bad) {
			t.Fatal("diagnostics leaked", bad)
		}
	}
	var result struct {
		Telemetry TelemetrySnapshot `json:"telemetry"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Telemetry.Requests != 70 || len(result.Telemetry.Recent) != 64 || result.Telemetry.Failures != 70 {
		t.Fatalf("unbounded/incorrect counters: %+v", result.Telemetry)
	}
	if result.Telemetry.Recent[0].Stage != "primary_proxy" {
		t.Fatal("missing failing stage")
	}
	metrics := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://localhost/metrics", nil)
	r.RemoteAddr = "127.0.0.1:1"
	s.DiagnosticsHandler().ServeHTTP(metrics, r)
	if !strings.Contains(metrics.Body.String(), "ags_edge_request_failures_total 70") {
		t.Fatal(metrics.Body.String())
	}
}

func TestOperationalReadinessExpiresAndDefaultRemainsClosed(t *testing.T) {
	o := &Operations{health: OperationsHealth{Ready: true, CheckedAt: time.Now().Add(-2 * time.Minute), Reason: "operational"}}
	if o.Snapshot().Ready || o.Snapshot().Reason != "checks_stale" {
		t.Fatal("stale checks remain green")
	}
	o.health.CheckedAt = time.Now()
	if !o.Snapshot().Ready {
		t.Fatal("fresh checks not observed")
	}
	for _, addr := range []string{"0.0.0.0:16667", "192.0.2.20:16667", "localhost:16667", "127.0.0.1:0"} {
		if _, err := config.NormalizeEdge(config.EdgeConfig{ID: "diag", PrimaryURL: "http://127.0.0.1:1", DiagnosticsAddr: addr}); err == nil {
			t.Fatal("unsafe diagnostic bind", addr)
		}
	}
}
