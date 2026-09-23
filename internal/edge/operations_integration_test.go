package edge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/edge"
)

func TestOperationsProbeRealPeerAndExposeOnlyLoopbackDetails(t *testing.T) {
	f := newControlFixture(t)
	r, err := edge.OpenReadResources(context.Background(), "edge-fixture-test", writeReadConfig(t, f))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected probe")
		}
		w.WriteHeader(200)
	}))
	defer primary.Close()
	cfg := config.EdgeConfig{ID: "edge-fixture-test", PrimaryURL: primary.URL, CanonicalURL: "http://primary.example.test:6666", UnboundReads: "primary"}
	op, err := edge.StartOperations(context.Background(), cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	defer op.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !op.Snapshot().Ready {
		time.Sleep(20 * time.Millisecond)
	}
	if !op.Snapshot().Ready {
		t.Fatalf("probe failed: %+v", op.Snapshot())
	}
	s, err := edge.New(cfg, edge.WithReadRuntime(r.Runtime), edge.WithOperations(op))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "http://primary.example.test:6666/readyz", nil))
	if w.Code != 200 {
		t.Fatalf("readiness stayed static: %d %s", w.Code, w.Body.String())
	}
	var public map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &public)
	if public["available_bytes"] != nil || public["certificate_expires_at"] != nil {
		t.Fatal("public probe leaked private operations")
	}
	private := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://localhost/status", nil)
	req.RemoteAddr = "127.0.0.1:3344"
	s.DiagnosticsHandler().ServeHTTP(private, req)
	var details map[string]any
	if err := json.Unmarshal(private.Body.Bytes(), &details); err != nil {
		t.Fatal(err)
	}
	if details["health"] == nil || details["mirror"] == nil || details["telemetry"] == nil {
		t.Fatal("missing observations")
	}
}
