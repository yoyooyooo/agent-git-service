package rest_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestExecutionContextIntakeRoutePullsWithoutAGSAuthAndPersistsNoCredential(t *testing.T) {
	const sourceToken = "mat_rest_intake_secret_value"
	var receivedAuthorization string
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(restCurrentContextJSON(t))
	}))
	defer source.Close()

	h := testharness.New(t)
	registry, err := executioncontext.New(executioncontext.Config{Enabled: true, Connectors: []executioncontext.ConnectorConfig{{
		SourceInstanceID: "multica-mini", Adapter: executioncontext.AdapterMulticaCurrentExecutionContextV1,
		AcceptedRuntimeEndpoints: []string{"http://primary.example.test:37134"},
		EgressEndpoint:           source.URL + "/api/integrations/current-execution-context",
		WorkspaceMappings:        map[string]string{"11111111-1111-4111-8111-111111111111": "primary-a"}, Timeout: "2s",
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Svc.ExecutionContextPuller = registry
	body := []byte(`{
		"runtime_endpoint_hint":"http://primary.example.test:37134",
		"locator":{"workspace_id":"11111111-1111-4111-8111-111111111111","agent_id":"66666666-6666-4666-8666-666666666661","task_id":"66666666-6666-4666-8666-666666666662"},
		"source_token":"` + sourceToken + `"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v3/execution-context/intake", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.Mux.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control=%q", recorder.Header().Get("Cache-Control"))
	}
	if receivedAuthorization != "Bearer "+sourceToken {
		t.Fatal("source did not receive the current task token")
	}
	if strings.Contains(recorder.Body.String(), sourceToken) {
		t.Fatal("intake response leaked source token")
	}
	var response service.ExecutionContextIntakeResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Schema != service.ExecutionContextIntakeSchema || response.SnapshotID == "" || response.SourceRef.SourceInstanceID != "multica-mini" || response.ContextSnapshot.Task.ID != "66666666-6666-4666-8666-666666666662" {
		t.Fatalf("response=%#v", response)
	}
	var record db.ExecutionContextSnapshot
	if err := h.DB.First(&record, "id = ?", response.SnapshotID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(record.SourceRefJSON+record.ContextJSON, sourceToken) {
		t.Fatal("persisted snapshot leaked source token")
	}
}

func TestExecutionContextIntakeRouteRejectsClosedOrSecretEchoingRequests(t *testing.T) {
	h := testharness.New(t)
	secret := "mat_never_echo_this_value"
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"runtime_endpoint_hint":"http://primary.example.test:37134","locator":{"workspace_id":"11111111-1111-4111-8111-111111111111","agent_id":"66666666-6666-4666-8666-666666666661","task_id":"66666666-6666-4666-8666-666666666662"},"source_token":"` + secret + `","operation":"pr.merge"}`},
		{name: "trailing json", body: `{"source_token":"` + secret + `"}{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v3/execution-context/intake", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			h.Mux.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), secret) {
				t.Fatal("validation response echoed source token")
			}
		})
	}
}

func restCurrentContextJSON(t *testing.T) []byte {
	t.Helper()
	payload := map[string]any{
		"schema": "multica.current-execution-context.v1", "observed_at": "2026-08-06T02:12:30.123456789Z",
		"workspace":   map[string]any{"id": "11111111-1111-4111-8111-111111111111", "name": "primary-a", "slug": "primary-a"},
		"agent":       map[string]any{"id": "66666666-6666-4666-8666-666666666661", "name": "fixture-ci-repair", "status": "working"},
		"task":        map[string]any{"id": "66666666-6666-4666-8666-666666666662", "status": "running", "attempt": 1, "max_attempts": 2},
		"run":         map[string]any{"id": "66666666-6666-4666-8666-666666666663", "task_id": "66666666-6666-4666-8666-666666666662", "status": "running", "attempt": 1, "max_attempts": 2},
		"issue":       map[string]any{"id": "66666666-6666-4666-8666-666666666664", "key": "MINI-1511", "title": "live proof", "status": "todo"},
		"runtime":     map[string]any{"id": "66666666-6666-4666-8666-666666666665", "name": "Pi (mini)", "provider": "pi", "status": "online", "details_available": true},
		"trigger":     map[string]any{"kind": "issue_assignment", "id": "66666666-6666-4666-8666-666666666664"},
		"attribution": map[string]any{"source": "direct_human", "precise": true},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
