package rest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestAccessGrantRoutesBootstrapWithoutAGSProfileAndRemainSecretSafe(t *testing.T) {
	const (
		sourceToken = "mat_access_grant_source_secret"
		workspaceID = "11111111-1111-4111-8111-111111111111"
		agentID     = "66666666-6666-4666-8666-666666666661"
		taskID      = "66666666-6666-4666-8666-666666666662"
		repository  = "operator/agent-git-service"
	)
	var sourceContext map[string]any
	if err := json.Unmarshal(restCurrentContextJSON(t), &sourceContext); err != nil {
		t.Fatal(err)
	}
	sourceContext["attribution"] = map[string]any{
		"source": "direct_human", "precise": true,
		"initiator":  map[string]any{"id": "maintainer-user-id", "name": "Operator"},
		"originator": map[string]any{"id": "issue-owner-id", "name": "Issue Owner"},
	}
	sourceContextJSON, err := json.Marshal(sourceContext)
	if err != nil {
		t.Fatal(err)
	}
	var receivedAuthorization string
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(sourceContextJSON)
	}))
	defer source.Close()

	h := testharness.New(t)
	executor := db.User{Login: "rest-runtime-executor", Name: "Runtime Executor", Type: db.TypeUser, Status: "active", UserKind: db.UserKindAgent}
	if err := h.DB.Create(&executor).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.DB.Create(&db.Repository{Name: "agent-git-service", FullName: repository, OwnerID: executor.ID, DefaultBranch: "main", Private: true, Visibility: "private"}).Error; err != nil {
		t.Fatal(err)
	}
	registry, err := executioncontext.New(executioncontext.Config{Enabled: true, Connectors: []executioncontext.ConnectorConfig{{
		SourceInstanceID: "multica-mini", Adapter: executioncontext.AdapterMulticaCurrentExecutionContextV1,
		AcceptedRuntimeEndpoints: []string{"http://primary.example.test:37134"}, EgressEndpoint: source.URL + "/api/integrations/current-execution-context",
		WorkspaceMappings: map[string]string{workspaceID: "primary-a"}, Timeout: "2s",
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Svc.ExecutionContextPuller = registry
	h.Svc.PrincipalSessions = restAccessGrantAuthority(t, executor.ID, workspaceID, repository)

	issueBody := []byte(`{
		"runtime_endpoint_hint":"http://primary.example.test:37134",
		"locator":{"workspace_id":"` + workspaceID + `","agent_id":"` + agentID + `","task_id":"` + taskID + `"},
		"source_token":"` + sourceToken + `","repository":"` + repository + `",
		"agent_id":"unbound-runtime-alias","policy_class":"unknown-class","operations":["ci.read","repo.admin"]
	}`)
	issueReq := httptest.NewRequest(http.MethodPost, "/api/v3/access-grants", bytes.NewReader(issueBody))
	issueReq.Header.Set("Content-Type", "application/json")
	issue := httptest.NewRecorder()
	h.Mux.ServeHTTP(issue, issueReq)
	if issue.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", issue.Code, issue.Body.String())
	}
	if issue.Header().Get("Cache-Control") != "no-store" || receivedAuthorization != "Bearer "+sourceToken {
		t.Fatalf("cache=%q source auth=%q", issue.Header().Get("Cache-Control"), receivedAuthorization)
	}
	if strings.Contains(issue.Body.String(), sourceToken) {
		t.Fatal("source token leaked in issue response")
	}
	var issued struct {
		GrantToken string `json:"grant_token"`
		Grant      struct {
			ID                   string   `json:"id"`
			AgentSelectorOutcome string   `json:"agent_selector_outcome"`
			PolicyClassOutcome   string   `json:"policy_class_outcome"`
			Warnings             []string `json:"warnings"`
		} `json:"grant"`
	}
	if err := json.Unmarshal(issue.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issued.GrantToken, "ags_grant_") || issued.Grant.ID == "" || issued.Grant.AgentSelectorOutcome != "ignored_unbound" || issued.Grant.PolicyClassOutcome != "ignored_unbound" {
		t.Fatalf("issued=%#v", issued)
	}
	if !containsString(issued.Grant.Warnings, "policy_class_ignored") {
		t.Fatalf("expected ignored policy_class warning: %#v", issued.Grant.Warnings)
	}

	current := accessGrantRESTRequest(t, h, http.MethodGet, "/api/v3/access-grants/current", issued.GrantToken, nil)
	if current.Code != http.StatusOK || current.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("current status=%d body=%s", current.Code, current.Body.String())
	}
	authorized := accessGrantRESTRequest(t, h, http.MethodPost, "/api/v3/access-grants/authorize", issued.GrantToken, map[string]any{
		"operation": "repo.read", "constraints": map[string]any{},
	})
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), `"provider_outcome":"not_applicable"`) {
		t.Fatalf("authorize status=%d body=%s", authorized.Code, authorized.Body.String())
	}
	missingMergeLocator := accessGrantRESTRequest(t, h, http.MethodPost, "/api/v3/access-grants/effects/pr.merge", issued.GrantToken, map[string]any{
		"ags_pr_number": 1, "provider_pr_number": 1, "expected_head_sha": strings.Repeat("a", 40), "merge_method": "rebase",
	})
	if missingMergeLocator.Code != http.StatusUnprocessableEntity {
		t.Fatalf("merge effect without invocation_id status=%d body=%s", missingMergeLocator.Code, missingMergeLocator.Body.String())
	}

	transport := accessGrantRESTRequest(t, h, http.MethodPost, "/api/v3/access-grants/transport-sessions", issued.GrantToken, map[string]any{
		"operation": "repo.read", "constraints": map[string]any{},
	})
	if transport.Code != http.StatusCreated || transport.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("transport status=%d body=%s", transport.Code, transport.Body.String())
	}
	if strings.Contains(transport.Body.String(), sourceToken) || strings.Contains(transport.Body.String(), issued.GrantToken) {
		t.Fatal("source or grant credential leaked in transport response")
	}
	var transported struct {
		Schema       string `json:"schema"`
		SessionToken string `json:"session_token"`
		Session      struct {
			ID             string `json:"id"`
			CredentialMode string `json:"credential_mode"`
			Principal      struct {
				ID uint `json:"id"`
			} `json:"principal"`
			WorkloadContext struct {
				Subject string `json:"subject"`
				AgentID string `json:"agent_id"`
			} `json:"workload_context"`
		} `json:"session"`
		Invocation struct {
			ID             string `json:"id"`
			ActorUserID    uint   `json:"actor_user_id"`
			ExecutorUserID uint   `json:"executor_user_id"`
			Operation      string `json:"operation"`
		} `json:"invocation"`
	}
	if err := json.Unmarshal(transport.Body.Bytes(), &transported); err != nil {
		t.Fatal(err)
	}
	if transported.Schema != "ags.access-grant-transport-session.v1" || !strings.HasPrefix(transported.SessionToken, "ags_sess_") ||
		transported.Session.ID == "" || transported.Session.CredentialMode != "access_grant_transport" || transported.Invocation.ID == "" ||
		transported.Invocation.Operation != "repo.read" || transported.Invocation.ExecutorUserID != executor.ID ||
		transported.Session.Principal.ID != executor.ID || transported.Invocation.ActorUserID == 0 || transported.Invocation.ActorUserID == executor.ID ||
		transported.Session.WorkloadContext.AgentID != agentID || transported.Session.WorkloadContext.Subject != "urn:multica:agent:"+agentID {
		t.Fatalf("transport receipt did not preserve canonical actor, executor, and Context Envelope: %#v", transported)
	}
	currentUser := accessGrantRESTRequest(t, h, http.MethodGet, "/api/v3/user", transported.SessionToken, nil)
	if currentUser.Code != http.StatusOK || strings.Contains(currentUser.Body.String(), transported.SessionToken) {
		t.Fatalf("transport current-user status=%d body=%s", currentUser.Code, currentUser.Body.String())
	}

	prTransport := accessGrantRESTRequest(t, h, http.MethodPost, "/api/v3/access-grants/transport-sessions", issued.GrantToken, map[string]any{
		"operation": "pr.create", "constraints": map[string]any{"base_ref": "main", "head_ref": "agent/access-grant-transport"},
	})
	if prTransport.Code != http.StatusCreated {
		t.Fatalf("pr transport status=%d body=%s", prTransport.Code, prTransport.Body.String())
	}
	var prTransported struct {
		SessionToken string `json:"session_token"`
		Session      struct {
			ID string `json:"id"`
		} `json:"session"`
		Invocation struct {
			ActorUserID    uint `json:"actor_user_id"`
			ExecutorUserID uint `json:"executor_user_id"`
		} `json:"invocation"`
	}
	if err := json.Unmarshal(prTransport.Body.Bytes(), &prTransported); err != nil {
		t.Fatal(err)
	}
	prResponse := accessGrantRESTRequest(t, h, http.MethodPost, "/api/v3/repos/operator/agent-git-service/pulls", prTransported.SessionToken, map[string]any{
		"title": "Access Grant transport PR", "body": "Implementation evidence.",
		"head": "agent/access-grant-transport", "base": "main",
	})
	if prResponse.Code != http.StatusCreated || strings.Contains(prResponse.Body.String(), sourceToken) ||
		strings.Contains(prResponse.Body.String(), issued.GrantToken) || strings.Contains(prResponse.Body.String(), prTransported.SessionToken) {
		t.Fatalf("transport PR create status=%d body=%s", prResponse.Code, prResponse.Body.String())
	}
	var storedPR db.PullRequest
	if err := h.DB.Preload("Author").First(&storedPR).Error; err != nil {
		t.Fatal(err)
	}
	if storedPR.AuthorID != prTransported.Invocation.ActorUserID || storedPR.AuthorID == executor.ID ||
		prTransported.Invocation.ExecutorUserID != executor.ID || storedPR.AgentSessionID == nil || *storedPR.AgentSessionID != prTransported.Session.ID {
		t.Fatalf("transport PR actor/executor projection is wrong: pr=%#v transport=%#v", storedPR, prTransported)
	}
	if !strings.Contains(string(storedPR.Body), "Multica: primary-a/MINI-1511") ||
		!strings.Contains(string(storedPR.Body), "AGS actor: fixture-ci-repair") ||
		!strings.Contains(string(storedPR.Body), "Initiated by: Operator (maintainer-user-id)") ||
		!strings.Contains(string(storedPR.Body), "Originated by: Issue Owner (issue-owner-id)") {
		t.Fatalf("transport PR body is missing immutable context attribution: %s", storedPR.Body)
	}
	var storedLink db.PullRequestMulticaLink
	if err := h.DB.First(&storedLink, "pull_request_id = ?", storedPR.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedLink.Source != db.MulticaLinkSourceAccessGrantSnapshot || storedLink.Confidence != db.MulticaLinkConfidenceAuthoritative ||
		storedLink.AccessGrantID != issued.Grant.ID || storedLink.SourceSnapshotID == "" || storedLink.IssueKey != "MINI-1511" || !storedLink.CompletionIntent {
		t.Fatalf("transport PR context link=%#v", storedLink)
	}
	attribution, err := h.Svc.PullRequestAttributionFor(context.Background(), storedPR)
	if err != nil {
		t.Fatal(err)
	}
	if attribution == nil || attribution.AGSActor.Type != "runtime_agent" || attribution.AGSActor.Initiator == nil ||
		attribution.AGSActor.Initiator.ID != "maintainer-user-id" || attribution.AGSActor.Originator == nil ||
		attribution.AGSActor.Originator.ID != "issue-owner-id" || attribution.DelegatedBy.Principal.ID != executor.ID {
		t.Fatalf("transport PR attribution=%#v", attribution)
	}

	revoked := accessGrantRESTRequest(t, h, http.MethodPost, "/api/v3/access-grants/revoke", issued.GrantToken, map[string]any{"reason": "transport proof complete"})
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke grant status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	for _, sessionToken := range []string{transported.SessionToken, prTransported.SessionToken} {
		afterRevoke := accessGrantRESTRequest(t, h, http.MethodGet, "/api/v3/user", sessionToken, nil)
		if afterRevoke.Code != http.StatusUnauthorized || strings.Contains(afterRevoke.Body.String(), sessionToken) {
			t.Fatalf("transport Session survived Grant revocation status=%d body=%s", afterRevoke.Code, afterRevoke.Body.String())
		}
	}
	var activeTransportSessions int64
	if err := h.DB.Model(&db.DelegatedAgentSession{}).
		Where("access_grant_id = ? AND credential_mode = ? AND revoked_at IS NULL", issued.Grant.ID, "access_grant_transport").
		Count(&activeTransportSessions).Error; err != nil {
		t.Fatal(err)
	}
	if activeTransportSessions != 0 {
		t.Fatalf("active transport Sessions after Grant revoke=%d", activeTransportSessions)
	}

	// A normal durable profile token is not a grant bearer and cannot enter the
	// grant surface even though the route itself has no AGS auth middleware.
	wrongCredential := accessGrantRESTRequest(t, h, http.MethodGet, "/api/v3/access-grants/current", h.Token, nil)
	if wrongCredential.Code != http.StatusUnauthorized || strings.Contains(wrongCredential.Body.String(), h.Token) {
		t.Fatalf("wrong credential status=%d body=%s", wrongCredential.Code, wrongCredential.Body.String())
	}

	for _, body := range []string{
		`{"source_token":"` + sourceToken + `","repository":"` + repository + `","locator":{"workspace_id":"` + workspaceID + `","agent_id":"` + agentID + `","task_id":"` + taskID + `"},"provider_token":"never"}`,
		`{"source_token":"` + sourceToken + `"}{}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v3/access-grants", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		h.Mux.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnprocessableEntity || strings.Contains(recorder.Body.String(), sourceToken) {
			t.Fatalf("closed request status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
}

// Workload roles are intentionally one-dimensional: maintainer/admin add only
// pr.merge to the standard collaboration envelope. Other privileged operations
// remain unavailable regardless of requested policy class or operation list.
func TestAccessGrantRolesOnlyAddPRMerge(t *testing.T) {
	const (
		sourceToken = "mat_access_grant_authorize_contract_secret"
		workspaceID = "11111111-1111-4111-8111-111111111111"
		agentID     = "66666666-6666-4666-8666-666666666661"
		taskID      = "66666666-6666-4666-8666-666666666662"
		repository  = "operator/authority-contract"
	)
	currentContext := restCurrentContextJSON(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(currentContext)
	}))
	defer source.Close()

	h := testharness.New(t)
	executor := db.User{Login: "rest-authorize-executor", Name: "REST Authorize Executor", Type: db.TypeUser, Status: "active", UserKind: db.UserKindAgent}
	if err := h.DB.Create(&executor).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{Name: "authority-contract", FullName: repository, OwnerID: executor.ID, DefaultBranch: "main", Private: true, Visibility: "private"}
	if err := h.DB.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	registry, err := executioncontext.New(executioncontext.Config{Enabled: true, Connectors: []executioncontext.ConnectorConfig{{
		SourceInstanceID: "multica-mini", Adapter: executioncontext.AdapterMulticaCurrentExecutionContextV1,
		AcceptedRuntimeEndpoints: []string{"http://primary.example.test:37134"}, EgressEndpoint: source.URL + "/api/integrations/current-execution-context",
		WorkspaceMappings: map[string]string{workspaceID: "primary-a"}, Timeout: "2s",
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Svc.ExecutionContextPuller = registry
	h.Svc.PrincipalSessions = restAccessGrantAuthority(t, executor.ID, workspaceID, repository)

	issueInput := func(role string, policyClass string, operations []string) map[string]any {
		body := map[string]any{
			"runtime_endpoint_hint": "http://primary.example.test:37134",
			"locator":               map[string]any{"workspace_id": workspaceID, "agent_id": agentID, "task_id": taskID},
			"source_token":          sourceToken, "repository": repository,
		}
		if role != "" {
			body["access_role"] = role
		}
		if policyClass != "" {
			body["policy_class"] = policyClass
		}
		if operations != nil {
			body["operations"] = operations
		}
		return body
	}
	first := accessGrantRESTRequest(t, h, http.MethodPost, "/api/v3/access-grants", "", issueInput("", "", nil))
	if first.Code != http.StatusCreated {
		t.Fatalf("bootstrap grant status=%d body=%s", first.Code, first.Body.String())
	}
	var bootstrap struct {
		Grant struct {
			Actor struct {
				ID    uint   `json:"id"`
				Login string `json:"login"`
			} `json:"actor"`
		} `json:"grant"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.Grant.Actor.ID == 0 || bootstrap.Grant.Actor.Login == "" {
		t.Fatalf("bootstrap actor=%#v", bootstrap.Grant.Actor)
	}
	if err := h.DB.Create(&db.Collaborator{RepositoryID: repo.ID, UserID: bootstrap.Grant.Actor.ID, Permission: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	roles := []string{"maintainer", "admin"}
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			issuedResponse := accessGrantRESTRequest(t, h, http.MethodPost, "/api/v3/access-grants", "", issueInput(role, "ignored.policy.class", []string{"pr.merge", "repo.create", "repo.admin", "git.force_push"}))
			if issuedResponse.Code != http.StatusCreated {
				t.Fatalf("role grant status=%d body=%s", issuedResponse.Code, issuedResponse.Body.String())
			}
			var issued struct {
				GrantToken string `json:"grant_token"`
				Grant      struct {
					EffectiveOperations []string `json:"effective_operations"`
					PolicyClassOutcome  string   `json:"policy_class_outcome"`
				} `json:"grant"`
			}
			if err := json.Unmarshal(issuedResponse.Body.Bytes(), &issued); err != nil {
				t.Fatal(err)
			}
			effective := make(map[string]bool, len(issued.Grant.EffectiveOperations))
			for _, operation := range issued.Grant.EffectiveOperations {
				effective[operation] = true
			}
			if issued.Grant.PolicyClassOutcome != "accepted_role" || !effective["pr.merge"] {
				t.Fatalf("role receipt=%#v", issued.Grant)
			}
			for _, operation := range []string{"repo.create", "repo.admin", "git.force_push"} {
				if effective[operation] {
					t.Fatalf("role must not add %s: %v", operation, issued.Grant.EffectiveOperations)
				}
			}
			for _, operation := range []string{"repo.read", "git.read", "git.push", "pr.create", "pr.read"} {
				if !effective[operation] {
					t.Fatalf("ordinary collaboration op missing: %s in %v", operation, issued.Grant.EffectiveOperations)
				}
			}
			for _, operation := range []string{"repo.create", "repo.admin", "git.force_push"} {
				response := accessGrantRESTRequest(t, h, http.MethodPost, "/api/v3/access-grants/authorize", issued.GrantToken, map[string]any{"operation": operation, "constraints": map[string]any{}})
				if response.Code != http.StatusForbidden {
					t.Fatalf("authorize %s status=%d body=%s", operation, response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestAccessGrantOpenAPIPathsAndSchemasAreClosed(t *testing.T) {
	h := testharness.New(t)
	// Upstream separates the extension document from GitHub-shaped APIs.
	// The fork's actual Access Grant paths and closed schemas are unchanged.
	recorder := h.DoRESTNoAuth(t, http.MethodGet, "/api/ext/v1/openapi.json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var document map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	routes := map[string]string{
		"/api/v3/access-grants":                             "post",
		"/api/v3/access-grants/renew":                       "post",
		"/api/v3/access-grants/current":                     "get",
		"/api/v3/access-grants/revoke":                      "post",
		"/api/v3/access-grants/authorize":                   "post",
		"/api/v3/access-grants/transport-sessions":          "post",
		"/api/v3/access-grants/effects/pr.merge":            "post",
		"/api/v3/access-grants/invocations/{invocation_id}": "get",
	}
	for route, method := range routes {
		path, ok := paths[route].(map[string]any)
		if !ok || path[method] == nil {
			t.Fatalf("missing %s %s", method, route)
		}
		operation := path[method].(map[string]any)
		if route != "/api/v3/access-grants" {
			security := operation["security"].([]any)
			entry := security[0].(map[string]any)
			if _, ok := entry["accessGrantAuth"]; !ok {
				t.Fatalf("%s does not use accessGrantAuth", route)
			}
		}
		if requestBody, ok := operation["requestBody"].(map[string]any); ok {
			content := requestBody["content"].(map[string]any)
			schema := content["application/json"].(map[string]any)["schema"].(map[string]any)
			assertOpenAPIObjectSchemasClosed(t, route+" request", schema)
			if route == "/api/v3/access-grants/effects/pr.merge" {
				required := stringSet(schema["required"].([]any))
				if !required["invocation_id"] {
					t.Fatalf("merge effect schema does not require caller-owned invocation_id: %#v", schema)
				}
			}
		}
	}
	components := document["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	for _, name := range []string{"AccessGrantIssueV1", "AccessGrantV1", "AccessGrantInvocationV1", "AccessGrantTransportInvocationV1", "AccessGrantTransportSessionReceiptV1"} {
		schema := schemas[name].(map[string]any)
		assertOpenAPIObjectSchemasClosed(t, name, schema)
	}
	mergeEffectPath := paths["/api/v3/access-grants/effects/pr.merge"].(map[string]any)["post"].(map[string]any)
	if summary := fmt.Sprint(mergeEffectPath["summary"]); !strings.Contains(summary, "terminal provider_attempt=not_attempted") || !strings.Contains(summary, "not HTTP 409") {
		t.Fatalf("merge effect summary does not publish terminal pre-dispatch semantics: %q", summary)
	}
	mergeResponses := mergeEffectPath["responses"].(map[string]any)
	if _, advertised := mergeResponses["409"]; advertised {
		t.Fatalf("merge effect must not advertise a post-dispatch-ambiguous 409: %#v", mergeResponses)
	}
	if _, ok := mergeResponses["200"]; !ok {
		t.Fatalf("merge effect terminal receipt response missing: %#v", mergeResponses)
	}
	authorizePath := paths["/api/v3/access-grants/authorize"].(map[string]any)["post"].(map[string]any)
	if summary := fmt.Sprint(authorizePath["summary"]); !strings.Contains(summary, "Admission-only") || !strings.Contains(summary, "provider_attempt=not_attempted") {
		t.Fatalf("authorize summary does not publish admission-only semantics: %q", summary)
	}
	authorizeVariants := openAPIRequestVariants(authorizePath)
	invocationVariants := schemas["AccessGrantInvocationV1"].(map[string]any)["oneOf"].([]any)
	transportPath := paths["/api/v3/access-grants/transport-sessions"].(map[string]any)["post"].(map[string]any)
	if summary := fmt.Sprint(transportPath["summary"]); !strings.Contains(summary, "repo.create") || !strings.Contains(summary, "repo.admin") || !strings.Contains(summary, "review.submit") || !strings.Contains(summary, "pr.merge") {
		t.Fatalf("transport summary does not publish high-risk exclusions: %q", summary)
	}
	transportVariants := openAPIRequestVariants(transportPath)
	authorizeInventory := openAPIOperationVariantInventory(t, authorizeVariants, "operation")
	invocationInventory := openAPIOperationVariantInventory(t, invocationVariants, "operation")
	transportInventory := openAPIOperationVariantInventory(t, transportVariants, "operation")

	expectedAuthorize := map[string]bool{}
	expectedInvocation := map[string]bool{}
	for _, registered := range sessionauthority.RegisteredOperations() {
		expectedInvocation[registered.Name] = true
		if registered.Name != "pr.merge" {
			expectedAuthorize[registered.Name] = true
		}
	}
	assertOpenAPIOperationClosure(t, "authorize", authorizeInventory, expectedAuthorize)
	assertOpenAPIOperationClosure(t, "invocation", invocationInventory, expectedInvocation)
	for _, operation := range []string{"repo.create", "repo.admin", "review.submit"} {
		if len(authorizeInventory[operation]) != 1 || len(invocationInventory[operation]) != 1 {
			t.Fatalf("%s must have one exact authorize and invocation variant: authorize=%d invocation=%d", operation, len(authorizeInventory[operation]), len(invocationInventory[operation]))
		}
		if len(transportInventory[operation]) != 0 {
			t.Fatalf("generic transport publishes forbidden %s variants: %#v", operation, transportInventory[operation])
		}
		assertElevatedOperationOpenAPISchema(t, operation, authorizeInventory[operation][0])
		assertElevatedOperationOpenAPISchema(t, operation, invocationInventory[operation][0])
	}
	if len(transportInventory["pr.merge"]) != 0 {
		t.Fatalf("generic transport publishes forbidden pr.merge variants: %#v", transportInventory["pr.merge"])
	}
	for operation := range transportInventory {
		if !expectedAuthorize[operation] {
			t.Fatalf("generic transport operation %q is outside authorize inventory", operation)
		}
	}

	transportResponses := transportPath["responses"].(map[string]any)
	transportResponseSchema := transportResponses["201"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	transportResponseProperties := transportResponseSchema["properties"].(map[string]any)
	sessionRef := transportResponseProperties["session"].(map[string]any)["$ref"]
	if sessionRef != "#/components/schemas/AccessGrantTransportSessionReceiptV1" {
		t.Fatalf("transport Session response is not bound to the closed receipt component: %#v", sessionRef)
	}
	invocationRef := transportResponseProperties["invocation"].(map[string]any)["$ref"]
	if invocationRef != "#/components/schemas/AccessGrantTransportInvocationV1" {
		t.Fatalf("transport invocation response is not bound to its restricted component: %#v", invocationRef)
	}
	transportInvocation := schemas["AccessGrantTransportInvocationV1"].(map[string]any)
	transportInvocationInventory := openAPIOperationVariantInventory(t, transportInvocation["oneOf"].([]any), "operation")
	assertOpenAPIOperationClosure(t, "transport request/invocation", transportInvocationInventory, operationInventorySet(transportInventory))
	transportReceipt := schemas["AccessGrantTransportSessionReceiptV1"].(map[string]any)
	transportReceiptVariants := transportReceipt["properties"].(map[string]any)["operation"].(map[string]any)["oneOf"].([]any)
	transportReceiptInventory := openAPIOperationVariantInventory(t, transportReceiptVariants, "name")
	for _, operation := range []string{"repo.create", "repo.admin", "review.submit", "pr.merge"} {
		if len(transportReceiptInventory[operation]) != 0 {
			t.Fatalf("transport receipt publishes forbidden %s variants", operation)
		}
	}
	assertOpenAPIOperationClosure(t, "transport request/receipt", transportReceiptInventory, operationInventorySet(transportInventory))
}

func assertOpenAPIObjectSchemasClosed(t *testing.T, path string, value any) {
	t.Helper()
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	if schema["type"] == "object" && schema["additionalProperties"] != false {
		t.Fatalf("%s contains an open object schema: %#v", path, schema)
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for name, child := range properties {
			assertOpenAPIObjectSchemasClosed(t, path+"."+name, child)
		}
	}
	if variants, ok := schema["oneOf"].([]any); ok {
		for index, variant := range variants {
			assertOpenAPIObjectSchemasClosed(t, fmt.Sprintf("%s.oneOf[%d]", path, index), variant)
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		assertOpenAPIObjectSchemasClosed(t, path+"[]", items)
	}
}

func openAPIRequestVariants(operation map[string]any) []any {
	requestBody := operation["requestBody"].(map[string]any)
	content := requestBody["content"].(map[string]any)
	schema := content["application/json"].(map[string]any)["schema"].(map[string]any)
	return schema["oneOf"].([]any)
}

func openAPIOperationVariantInventory(t *testing.T, variants []any, discriminator string) map[string][]map[string]any {
	t.Helper()
	inventory := map[string][]map[string]any{}
	for index, raw := range variants {
		variant, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("variant %d is not an object: %#v", index, raw)
		}
		properties := variant["properties"].(map[string]any)
		discriminatorSchema := properties[discriminator].(map[string]any)
		enum := discriminatorSchema["enum"].([]any)
		if len(enum) != 1 {
			t.Fatalf("variant %d %s enum=%#v, want one exact operation", index, discriminator, enum)
		}
		operation := fmt.Sprint(enum[0])
		inventory[operation] = append(inventory[operation], variant)
	}
	return inventory
}

func assertOpenAPIOperationClosure(t *testing.T, name string, actual map[string][]map[string]any, expected map[string]bool) {
	t.Helper()
	for operation := range expected {
		if len(actual[operation]) == 0 {
			t.Fatalf("%s inventory omits registered operation %q", name, operation)
		}
	}
	for operation := range actual {
		if !expected[operation] {
			t.Fatalf("%s inventory publishes unexpected operation %q", name, operation)
		}
	}
}

func operationInventorySet(inventory map[string][]map[string]any) map[string]bool {
	out := make(map[string]bool, len(inventory))
	for operation := range inventory {
		out[operation] = true
	}
	return out
}

func assertElevatedOperationOpenAPISchema(t *testing.T, operation string, variant map[string]any) {
	t.Helper()
	constraints := variant["properties"].(map[string]any)["constraints"].(map[string]any)
	if constraints["type"] != "object" || constraints["additionalProperties"] != false {
		t.Fatalf("%s constraints are not closed: %#v", operation, constraints)
	}
	properties := constraints["properties"].(map[string]any)
	required := stringSet(constraints["required"].([]any))
	expected := map[string]string{}
	switch operation {
	case "repo.create":
		expected = map[string]string{
			"target_repository": "string", "base_ref": "string", "source_base_sha": "string", "source_ref_digest": "string",
			"import_mode": "string", "visibility": "string",
		}
	case "repo.admin":
		expected = map[string]string{
			"target_repository": "string", "base_ref": "string", "source_base_sha": "string", "source_ref_digest": "string", "action": "string",
		}
	case "review.submit":
		expected = map[string]string{"pull_request_number": "integer", "forgejo_pull_request_number": "integer", "review_action": "string"}
	default:
		t.Fatalf("unsupported elevated operation %q", operation)
	}
	if len(properties) != len(expected) || len(required) != len(expected) {
		t.Fatalf("%s constraint inventory properties=%v required=%v", operation, properties, required)
	}
	for name, typ := range expected {
		property, ok := properties[name].(map[string]any)
		if !ok || property["type"] != typ || !required[name] {
			t.Fatalf("%s.%s schema=%#v required=%v", operation, name, property, required[name])
		}
	}
	assertOpenAPIPattern(t, operation, properties, "target_repository", `^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})/[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})$`)
	assertOpenAPIPattern(t, operation, properties, "source_base_sha", `^[a-f0-9]{40}$`)
	assertOpenAPIPattern(t, operation, properties, "source_ref_digest", `^[a-f0-9]{64}$`)
	if property, ok := properties["base_ref"].(map[string]any); ok {
		if property["pattern"] == nil || property["minLength"] != float64(1) || property["maxLength"] != float64(2048) {
			t.Fatalf("%s.base_ref is not an exact bounded ref schema: %#v", operation, property)
		}
	}
	assertOpenAPIEnum(t, operation, properties, "import_mode", []string{"ags-only", "ags-forgejo"})
	assertOpenAPIEnum(t, operation, properties, "visibility", []string{"private", "public", "internal"})
	assertOpenAPIEnum(t, operation, properties, "action", []string{"forgejo_onboard"})
	assertOpenAPIEnum(t, operation, properties, "review_action", []string{"approve", "request_changes", "comment"})
	for _, name := range []string{"pull_request_number", "forgejo_pull_request_number"} {
		if property, ok := properties[name].(map[string]any); ok {
			if property["minimum"] != float64(1) || property["maximum"] != float64(9007199254740991) {
				t.Fatalf("%s.%s is not a JSON-safe positive PR number: %#v", operation, name, property)
			}
		}
	}
}

func assertOpenAPIPattern(t *testing.T, operation string, properties map[string]any, name, expected string) {
	t.Helper()
	property, ok := properties[name].(map[string]any)
	if !ok {
		return
	}
	if property["pattern"] != expected {
		t.Fatalf("%s.%s pattern=%q, want %q", operation, name, property["pattern"], expected)
	}
}

func assertOpenAPIEnum(t *testing.T, operation string, properties map[string]any, name string, expected []string) {
	t.Helper()
	property, ok := properties[name].(map[string]any)
	if !ok {
		return
	}
	actual := stringSet(property["enum"].([]any))
	if len(actual) != len(expected) {
		t.Fatalf("%s.%s enum=%v, want %v", operation, name, actual, expected)
	}
	for _, value := range expected {
		if !actual[value] {
			t.Fatalf("%s.%s enum=%v, want %v", operation, name, actual, expected)
		}
	}
}

func stringSet(values []any) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		out[fmt.Sprint(value)] = true
	}
	return out
}

func restAccessGrantAuthority(t *testing.T, principalID uint, workspaceID, repository string) *sessionauthority.Set {
	t.Helper()
	authority, err := sessionauthority.New(sessionauthority.Config{
		Version: sessionauthority.CurrentVersion, ContractRevision: sessionauthority.ContractRevision,
		TrustedIssuers: []sessionauthority.TrustedIssuer{{ID: "multica-mini", Issuer: "multica", KeyIDs: []string{"source-key"}, Status: "active", TrustRevision: "trust-v1"}},
		PolicyClasses: []sessionauthority.PolicyClass{{
			ID: sessionauthority.DefaultDynamicPolicyClass, Status: "active", PolicyRevision: "class-v1",
			Operations: []string{"ci.read", "git.push", "git.read", "pr.create", "pr.rebase", "pr.read", "repo.read", "review.read"},
		}},
		TeamBindings: []sessionauthority.TeamBinding{{
			ID: "default", IssuerInstanceID: "multica-mini", WorkspaceID: workspaceID, TeamIdentityID: "default-team",
			PolicyClass: sessionauthority.DefaultDynamicPolicyClass, PrincipalID: principalID, Status: "active", BindingRevision: "binding-v1", EpochFloor: 1,
		}},
		Resources: []sessionauthority.ResourcePolicy{{
			ID: "repo", Target: "primary-a", Service: "ags", Repository: repository, Status: "active", MaxSessionTTL: "30m", PolicyRevision: "resource-v1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func accessGrantRESTRequest(t *testing.T, h *testharness.Harness, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, payload)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.Mux.ServeHTTP(recorder, req)
	return recorder
}
