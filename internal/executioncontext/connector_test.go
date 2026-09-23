package executioncontext

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testWorkspaceID = "11111111-1111-4111-8111-111111111111"
	testAgentID     = "66666666-6666-4666-8666-666666666661"
	testTaskID      = "66666666-6666-4666-8666-666666666662"
	testRunID       = "66666666-6666-4666-8666-666666666663"
	testIssueID     = "66666666-6666-4666-8666-666666666664"
	testRuntimeID   = "66666666-6666-4666-8666-666666666665"
	testDaemonID    = "019ea042-2ade-745d-b177-a84287e0ee3e"
	testToken       = "mat_connector_test_secret_value"
)

func TestRegistryPullUsesOnlyFixedEgressAndNormalizesSnapshot(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != currentExecutionContextPath {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Fatalf("authorization mismatch")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(validContextJSON(t, nil))
	}))
	defer server.Close()

	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	result, err := registry.Pull(context.Background(), PullRequest{
		RuntimeEndpointHint: "http://mini-runtime:37134",
		Locator:             testLocator(),
		SourceToken:         testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if result.SourceRef.SourceInstanceID != "multica-mini" || result.SourceRef.WorkspaceRef != "primary-a" || result.SourceRef.TaskID != testTaskID || result.SourceRef.RunID != testRunID {
		t.Fatalf("source ref=%#v", result.SourceRef)
	}
	if !strings.HasPrefix(result.ContextDigest, "sha256:") || len(result.ContextDigest) != 71 {
		t.Fatalf("digest=%q", result.ContextDigest)
	}
	if strings.Contains(string(result.ContextJSON), testToken) {
		t.Fatal("canonical context contains source token")
	}
	var roundTrip CurrentContext
	if err := json.Unmarshal(result.ContextJSON, &roundTrip); err != nil || roundTrip.Task.ID != testTaskID {
		t.Fatalf("canonical context=%s err=%v", result.ContextJSON, err)
	}
}

func TestRegistryPullAcceptsMinimalV2WithClaimGeneration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(validContextJSON(t, map[string]any{"schema": MulticaCurrentExecutionContextSchemaV2, "minimal_v2": true}))
	}))
	defer server.Close()

	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	result, err := registry.Pull(context.Background(), PullRequest{
		RuntimeEndpointHint: "http://mini-runtime:37134",
		Locator:             testLocator(),
		SourceToken:         testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceRef.RunID != testRunID {
		t.Fatalf("source ref run_id=%q", result.SourceRef.RunID)
	}
	if result.Context.Schema != MulticaCurrentExecutionContextSchemaV2 {
		t.Fatalf("schema=%q", result.Context.Schema)
	}
	if result.Context.Claim == nil || result.Context.Claim.Generation != testRunID || result.Context.Claim.TaskID != testTaskID {
		t.Fatalf("claim=%#v", result.Context.Claim)
	}
	if result.Context.Run.ID != testRunID || result.Context.Run.TaskID != testTaskID {
		t.Fatalf("run=%#v", result.Context.Run)
	}
	if result.Context.Workspace.Name != "" || result.Context.Agent.Name != "" || result.Context.Task.MaxAttempts != 0 {
		t.Fatalf("display fields leaked into v2 parse: %#v", result.Context)
	}
}

func TestRegistryPullAcceptsServerDerivedDaemonID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(validContextJSON(t, map[string]any{
			"schema": MulticaCurrentExecutionContextSchemaV2, "minimal_v2": true, "daemon_id": testDaemonID,
		}))
	}))
	defer server.Close()

	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	result, err := registry.Pull(context.Background(), PullRequest{
		RuntimeEndpointHint: "http://mini-runtime:37134", Locator: testLocator(), SourceToken: testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SourceRef.RuntimeID != testRuntimeID || result.SourceRef.DaemonID != testDaemonID ||
		result.Context.Runtime == nil || result.Context.Runtime.DaemonID != testDaemonID {
		t.Fatalf("runtime/daemon facts = source:%#v context:%#v", result.SourceRef, result.Context.Runtime)
	}
}

func TestRegistryRejectsInvalidDaemonID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(validContextJSON(t, map[string]any{
			"schema": MulticaCurrentExecutionContextSchemaV2, "minimal_v2": true, "daemon_id": "client-controlled-name",
		}))
	}))
	defer server.Close()
	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	_, err := registry.Pull(context.Background(), PullRequest{
		RuntimeEndpointHint: "http://mini-runtime:37134", Locator: testLocator(), SourceToken: testToken,
	})
	if !errors.Is(err, ErrSourceContextInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestRegistryRejectsV2WithoutClaimGeneration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		body := validContextJSON(t, map[string]any{"schema": MulticaCurrentExecutionContextSchemaV2, "minimal_v2": true})
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		delete(payload, "claim")
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(encoded)
	}))
	defer server.Close()
	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	_, err := registry.Pull(context.Background(), PullRequest{
		RuntimeEndpointHint: "http://mini-runtime:37134",
		Locator:             testLocator(),
		SourceToken:         testToken,
	})
	if !errors.Is(err, ErrSourceContextInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestRegistryRejectsClaimRunDualReadMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(validContextJSON(t, map[string]any{
			"schema":           MulticaCurrentExecutionContextSchemaV2,
			"minimal_v2":       true,
			"claim_generation": "11111111-1111-4111-8111-111111111111",
		}))
	}))
	defer server.Close()
	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	_, err := registry.Pull(context.Background(), PullRequest{
		RuntimeEndpointHint: "http://mini-runtime:37134",
		Locator:             testLocator(),
		SourceToken:         testToken,
	})
	if !errors.Is(err, ErrSourceContextInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestRegistryRejectsV2DisplayEnrichment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		body := validContextJSON(t, map[string]any{"schema": MulticaCurrentExecutionContextSchemaV2, "minimal_v2": true})
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		payload["agent"] = map[string]any{"id": testAgentID, "name": "leaked-display-name", "status": "working"}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(encoded)
	}))
	defer server.Close()
	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	_, err := registry.Pull(context.Background(), PullRequest{
		RuntimeEndpointHint: "http://mini-runtime:37134",
		Locator:             testLocator(),
		SourceToken:         testToken,
	})
	if !errors.Is(err, ErrSourceContextInvalid) {
		t.Fatalf("err=%v", err)
	}
}

func TestRegistryV1ProjectsClaimOntoNormalizedSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(validContextJSON(t, nil))
	}))
	defer server.Close()
	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	result, err := registry.Pull(context.Background(), PullRequest{
		RuntimeEndpointHint: "http://mini-runtime:37134",
		Locator:             testLocator(),
		SourceToken:         testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Context.Claim == nil || result.Context.Claim.Generation != testRunID || result.Context.Claim.TaskID != testTaskID {
		t.Fatalf("v1 dual-read claim missing: %#v", result.Context.Claim)
	}
	var wire map[string]any
	if err := json.Unmarshal(result.ContextJSON, &wire); err != nil {
		t.Fatal(err)
	}
	claim, ok := wire["claim"].(map[string]any)
	if !ok || claim["generation"] != testRunID {
		t.Fatalf("normalized snapshot claim=%#v", wire["claim"])
	}
	if _, present := wire["details_available"]; present {
		t.Fatalf("unexpected top-level details_available: %#v", wire)
	}
}

func TestRegistryMinimalV2CanonicalOmitsDetailsAvailableFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(validContextJSON(t, map[string]any{"schema": MulticaCurrentExecutionContextSchemaV2, "minimal_v2": true}))
	}))
	defer server.Close()
	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	result, err := registry.Pull(context.Background(), PullRequest{
		RuntimeEndpointHint: "http://mini-runtime:37134",
		Locator:             testLocator(),
		SourceToken:         testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.ContextJSON), `"details_available"`) {
		t.Fatalf("canonical snapshot synthesized details_available: %s", result.ContextJSON)
	}
}

func TestRegistryResolutionFailsClosedWithoutOutboundRequest(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("should not be called")
	})}
	registry, err := New(Config{Enabled: true, Connectors: []ConnectorConfig{
		testConnector("http://fixed-one.example", "multica-one", "http://shared-runtime:37133"),
		testConnector("http://fixed-two.example", "multica-two", "http://shared-runtime:37133"),
	}}, client)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		input  PullRequest
		wanted error
	}{
		{name: "unmatched", input: PullRequest{RuntimeEndpointHint: "http://unknown-runtime:37133", Locator: testLocator(), SourceToken: testToken}, wanted: ErrConnectorNotFound},
		{name: "ambiguous", input: PullRequest{RuntimeEndpointHint: "http://shared-runtime:37133", Locator: testLocator(), SourceToken: testToken}, wanted: ErrConnectorAmbiguous},
		{name: "path alias", input: PullRequest{RuntimeEndpointHint: "http://shared-runtime:37133/alias", Locator: testLocator(), SourceToken: testToken}, wanted: ErrConnectorNotFound},
		{name: "selector disagreement", input: PullRequest{SourceInstanceID: "multica-one", RuntimeEndpointHint: "http://other-runtime:37133", Locator: testLocator(), SourceToken: testToken}, wanted: ErrConnectorNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := registry.Pull(context.Background(), tc.input); !errors.Is(err, tc.wanted) {
				t.Fatalf("error=%v want=%v", err, tc.wanted)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("outbound calls=%d", calls.Load())
	}
}

func TestRegistryRejectsRedirectWithoutForwardingCredential(t *testing.T) {
	var sinkCalls atomic.Int64
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Fatal("redirect sink received authorization")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+currentExecutionContextPath, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	registry := newTestRegistry(t, []ConnectorConfig{testConnector(redirect.URL, "multica-mini", "http://mini-runtime:37134")})
	_, err := registry.Pull(context.Background(), PullRequest{RuntimeEndpointHint: "http://mini-runtime:37134", Locator: testLocator(), SourceToken: testToken})
	if !errors.Is(err, ErrSourceRedirectRejected) {
		t.Fatalf("error=%v", err)
	}
	if sinkCalls.Load() != 0 {
		t.Fatalf("redirect sink calls=%d", sinkCalls.Load())
	}
}

func TestRegistryErrorsNeverEchoCredentialOrSourceBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		wanted error
	}{
		{name: "credential rejected", status: http.StatusUnauthorized, body: testToken, wanted: ErrSourceCredentialRejected},
		{name: "source error", status: http.StatusInternalServerError, body: "upstream reflected " + testToken, wanted: ErrSourceUnavailable},
		{name: "invalid payload", status: http.StatusOK, body: `{"schema":"bad","source_token":"` + testToken + `"}`, wanted: ErrSourceContextInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
			_, err := registry.Pull(context.Background(), PullRequest{RuntimeEndpointHint: "http://mini-runtime:37134", Locator: testLocator(), SourceToken: testToken})
			if !errors.Is(err, tc.wanted) {
				t.Fatalf("error=%v want=%v", err, tc.wanted)
			}
			if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), tc.body) {
				t.Fatalf("credential/source body leaked in error: %v", err)
			}
		})
	}
}

func TestRegistryRejectsTerminalOrMismatchedContext(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]any
	}{
		{name: "terminal task", overrides: map[string]any{"task_status": "completed", "run_status": "completed"}},
		{name: "wrong task", overrides: map[string]any{"task_id": "00000000-0000-4000-8000-000000000001"}},
		{name: "unmapped workspace", overrides: map[string]any{"workspace_id": "00000000-0000-4000-8000-000000000002"}},
		{name: "reflected bearer", overrides: map[string]any{"agent_name": testToken}},
		{name: "unknown field", overrides: map[string]any{"unknown_top_level": "not-allowed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(validContextJSON(t, tc.overrides))
			}))
			defer server.Close()
			registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
			_, err := registry.Pull(context.Background(), PullRequest{RuntimeEndpointHint: "http://mini-runtime:37134", Locator: testLocator(), SourceToken: testToken})
			if !errors.Is(err, ErrSourceContextInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRegistryRenewalRequiresResubmittedCurrentlyValidTaskToken(t *testing.T) {
	currentToken := "mat_current_task_token_one"
	terminal := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if terminal || r.Header.Get("Authorization") != "Bearer "+currentToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(validContextJSON(t, nil))
	}))
	defer server.Close()
	registry := newTestRegistry(t, []ConnectorConfig{testConnector(server.URL, "multica-mini", "http://mini-runtime:37134")})
	request := PullRequest{RuntimeEndpointHint: "http://mini-runtime:37134", Locator: testLocator(), SourceToken: currentToken}
	if _, err := registry.Pull(context.Background(), request); err != nil {
		t.Fatalf("initial pull: %v", err)
	}
	oldToken := currentToken
	currentToken = "mat_current_task_token_two"
	request.SourceToken = oldToken
	if _, err := registry.Pull(context.Background(), request); !errors.Is(err, ErrSourceCredentialRejected) {
		t.Fatalf("stale renewal error=%v", err)
	}
	request.SourceToken = currentToken
	if _, err := registry.Pull(context.Background(), request); err != nil {
		t.Fatalf("fresh renewal: %v", err)
	}
	terminal = true
	if _, err := registry.Pull(context.Background(), request); !errors.Is(err, ErrSourceCredentialRejected) {
		t.Fatalf("terminal renewal error=%v", err)
	}
}

func TestNormalizeConfigRejectsUnsafeRegistryValues(t *testing.T) {
	base := testConnector("http://fixed.example", "multica-mini", "http://mini-runtime:37134")
	for _, tc := range []struct {
		name   string
		mutate func(*ConnectorConfig)
	}{
		{name: "duplicate source", mutate: func(*ConnectorConfig) {}},
		{name: "egress userinfo", mutate: func(c *ConnectorConfig) {
			c.EgressEndpoint = "http://user:pass@fixed.example" + currentExecutionContextPath
		}},
		{name: "egress query", mutate: func(c *ConnectorConfig) { c.EgressEndpoint += "?target=other" }},
		{name: "wrong egress path", mutate: func(c *ConnectorConfig) { c.EgressEndpoint = "http://fixed.example/private" }},
		{name: "runtime path", mutate: func(c *ConnectorConfig) { c.AcceptedRuntimeEndpoints = []string{"http://mini-runtime:37134/alias"} }},
		{name: "secret source id", mutate: func(c *ConnectorConfig) { c.SourceInstanceID = "mat_secret" }},
		{name: "unsupported adapter", mutate: func(c *ConnectorConfig) { c.Adapter = "dynamic_url" }},
		{name: "missing workspace map", mutate: func(c *ConnectorConfig) { c.WorkspaceMappings = nil }},
		{name: "excessive timeout", mutate: func(c *ConnectorConfig) { c.Timeout = "31s" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			candidate.AcceptedRuntimeEndpoints = append([]string(nil), base.AcceptedRuntimeEndpoints...)
			candidate.WorkspaceMappings = map[string]string{testWorkspaceID: "primary-a"}
			tc.mutate(&candidate)
			connectors := []ConnectorConfig{candidate}
			if tc.name == "duplicate source" {
				connectors = append(connectors, base)
			}
			if _, err := NormalizeConfig(Config{Enabled: true, Connectors: connectors}); err == nil {
				t.Fatal("unsafe registry config accepted")
			}
		})
	}
}

func newTestRegistry(t *testing.T, connectors []ConnectorConfig) *Registry {
	t.Helper()
	registry, err := New(Config{Enabled: true, Connectors: connectors}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func testConnector(serverURL, sourceID, runtimeEndpoint string) ConnectorConfig {
	return ConnectorConfig{
		SourceInstanceID:         sourceID,
		Adapter:                  AdapterMulticaCurrentExecutionContextV1,
		AcceptedRuntimeEndpoints: []string{runtimeEndpoint},
		EgressEndpoint:           strings.TrimRight(serverURL, "/") + currentExecutionContextPath,
		WorkspaceMappings:        map[string]string{testWorkspaceID: "primary-a"},
		Timeout:                  "2s",
	}
}

func testLocator() Locator {
	return Locator{WorkspaceID: testWorkspaceID, AgentID: testAgentID, TaskID: testTaskID}
}

func validContextJSON(t *testing.T, overrides map[string]any) []byte {
	t.Helper()
	workspaceID := testWorkspaceID
	taskID := testTaskID
	taskStatus := "running"
	runStatus := "running"
	agentName := "fixture-ci-repair"
	if value, ok := overrides["workspace_id"].(string); ok {
		workspaceID = value
	}
	if value, ok := overrides["task_id"].(string); ok {
		taskID = value
	}
	if value, ok := overrides["task_status"].(string); ok {
		taskStatus = value
	}
	if value, ok := overrides["run_status"].(string); ok {
		runStatus = value
	}
	if value, ok := overrides["agent_name"].(string); ok {
		agentName = value
	}
	schema := MulticaCurrentExecutionContextSchema
	if value, ok := overrides["schema"].(string); ok {
		schema = value
	}
	claimGeneration := testRunID
	if value, ok := overrides["claim_generation"].(string); ok {
		claimGeneration = value
	}
	minimalV2, _ := overrides["minimal_v2"].(bool)
	runtime := map[string]any{"id": testRuntimeID}
	if value, ok := overrides["daemon_id"].(string); ok {
		runtime["daemon_id"] = value
	}
	payload := map[string]any{
		"schema":      schema,
		"observed_at": "2026-08-06T02:12:30.123456789Z",
		"workspace":   map[string]any{"id": workspaceID, "slug": "primary-a"},
		"agent":       map[string]any{"id": testAgentID},
		"task": map[string]any{
			"id": taskID, "status": taskStatus, "attempt": 1,
		},
		"run": map[string]any{
			"id": testRunID, "task_id": taskID,
		},
		"claim": map[string]any{
			"generation": claimGeneration, "task_id": taskID,
		},
		"issue":   map[string]any{"id": testIssueID, "key": "MINI-1511"},
		"runtime": runtime,
		"trigger": map[string]any{"kind": "issue_assignment", "id": testIssueID},
		"attribution": map[string]any{
			"source": "direct_human", "precise": true,
		},
	}
	if !minimalV2 {
		payload["workspace"] = map[string]any{"id": workspaceID, "name": "primary-a", "slug": "primary-a"}
		payload["agent"] = map[string]any{"id": testAgentID, "name": agentName, "status": "working"}
		payload["task"] = map[string]any{
			"id": taskID, "status": taskStatus, "attempt": 1, "max_attempts": 2,
			"created_at": "2026-08-06T02:12:05Z", "dispatched_at": "2026-08-06T02:12:05Z", "started_at": "2026-08-06T02:12:06Z",
		}
		payload["run"] = map[string]any{
			"id": testRunID, "task_id": taskID, "status": runStatus, "attempt": 1, "max_attempts": 2,
			"created_at": "2026-08-06T02:12:05Z", "dispatched_at": "2026-08-06T02:12:05Z", "started_at": "2026-08-06T02:12:06Z",
		}
		delete(payload, "claim")
		payload["issue"] = map[string]any{"id": testIssueID, "key": "MINI-1511", "title": "live proof", "status": "todo", "created_at": "2026-08-06T02:12:05Z", "updated_at": "2026-08-06T02:12:05Z"}
		runtime = map[string]any{"id": testRuntimeID, "name": "Pi (mini)", "provider": "pi", "status": "online", "details_available": true}
		if value, ok := overrides["daemon_id"].(string); ok {
			runtime["daemon_id"] = value
		}
		payload["runtime"] = runtime
		payload["attribution"] = map[string]any{
			"source": "direct_human", "precise": true,
			"initiator":  map[string]any{"id": "f2698085-7f70-4a87-93e1-ef5957e8b102", "name": "operator"},
			"originator": map[string]any{"id": "f2698085-7f70-4a87-93e1-ef5957e8b102", "name": "operator"},
			"evidence":   map[string]any{"kind": "issue_assignment", "ref_id": testIssueID},
		}
	}
	if value, ok := overrides["unknown_top_level"]; ok {
		payload["unknown_top_level"] = value
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
