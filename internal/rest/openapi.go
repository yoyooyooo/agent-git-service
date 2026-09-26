package rest

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/ngaut/agent-git-service/internal/operationconstraints"
)

var extensionRestOpenAPISpec = mustBuildExtensionRestOpenAPISpec()
var githubCompatibleOpenAPISpec = mustBuildGitHubCompatibleOpenAPISpec()

const extensionOpenAPIPrefix = "/api/ext/v1"
const githubCompatibleOpenAPIPrefix = "/api/v3"

// OpenAPISpecBytes preserves the fork's immutable extension schema accessor.
// GitHub-compatible and canonical extension HTTP documents remain separated.
func OpenAPISpecBytes() []byte { return append([]byte(nil), extensionRestOpenAPISpec...) }

// GetGitHubCompatibleOpenAPI handles GET /api/v3/openapi.json.
func (d *Deps) GetGitHubCompatibleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(githubCompatibleOpenAPISpec)
}

// GetOpenAPI handles GET /api/ext/v1/openapi.json.
func (d *Deps) GetOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(extensionRestOpenAPISpec)
}

func mustBuildExtensionRestOpenAPISpec() []byte {
	body := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "agent-git-service Extension REST API",
			"version":     "1.0.0",
			"description": "Machine-readable contract for extension REST APIs under /api/ext/v1.",
		},
		"servers": []map[string]any{{"url": "/"}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"tokenAuth": map[string]any{
					"type":        "apiKey",
					"in":          "header",
					"name":        "Authorization",
					"description": "Use the GitHub-compatible Authorization header format: `token <access-token>`.",
				},
				"accessGrantAuth": map[string]any{
					"type":         "http",
					"scheme":       "bearer",
					"bearerFormat": "AGS task/repository access grant",
					"description":  "Use the one-time-returned task/repository grant. This is not a durable AGS profile or provider token.",
				},
			},
			"schemas": map[string]any{
				"DelegatedSessionLifecycleV1":          delegatedSessionLifecycleOpenAPISchema(),
				"AccessGrantIssueV1":                   accessGrantIssueOpenAPISchema(),
				"AccessGrantV1":                        accessGrantOpenAPISchema(),
				"AccessGrantInvocationV1":              accessGrantInvocationOpenAPISchema(),
				"AccessGrantTransportInvocationV1":     accessGrantTransportInvocationOpenAPISchema(),
				"AccessGrantTransportSessionReceiptV1": accessGrantTransportSessionReceiptOpenAPISchema(),
				"OutboundDeliveryV1":                   outboundDeliveryOpenAPISchema(),
				"MulticaExternalPRLinkRequestV1":       multicaExternalPRLinkRequestOpenAPISchema(),
			},
		},
		"x-agent-git-service": map[string]any{
			"api_surface": map[string]any{
				"extension_prefix":         extensionOpenAPIPrefix,
				"github_compatible_prefix": githubCompatibleOpenAPIPrefix,
			},
			"compatibility_deltas": []map[string]any{
				{
					"id":       "issues-list-omits-body",
					"path":     "/api/v3/repos/{owner}/{repo}/issues",
					"summary":  "List issues omits body content in REST list responses.",
					"evidence": "internal/service/issue.go: ListIssuesForREST returns issues for the REST list endpoint while omitting the body payload.",
				},
				{
					"id":       "branch-protection-monolithic",
					"path":     "/api/v3/repos/{owner}/{repo}/branches/{branch}/protection",
					"summary":  "Branch protection supports a monolithic protection document plus selected GitHub-style subresources, but not the full branch-protection subresource tree.",
					"evidence": "internal/router/router.go routes branch protection through wildcard branch handlers; internal/rest/handlers_branch.go dispatches selected subresources.",
				},
				{
					"id":       "extension-canonical-prefix",
					"path":     "/api/ext/v1/openapi.json",
					"summary":  "Extension APIs are published only under /api/ext/v1; /api/v3 is reserved for GitHub-compatible routes.",
					"evidence": "internal/router/router.go registers extension routes on /api/ext/v1.",
				},
			},
		},
		"paths": buildRESTOpenAPIPaths(),
	}
	out, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		panic(err)
	}
	return out
}

func mustBuildGitHubCompatibleOpenAPISpec() []byte {
	body := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "agent-git-service GitHub-Compatible REST API",
			"version":     "1.0.0",
			"description": "Machine-readable local compatibility contract for GitHub-shaped REST APIs under /api/v3. This document describes local compatibility behavior and is not a claim of strict GitHub.com parity.",
		},
		"servers": []map[string]any{{"url": "/"}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"tokenAuth": map[string]any{
					"type":        "apiKey",
					"in":          "header",
					"name":        "Authorization",
					"description": "Use the GitHub-compatible Authorization header format: `token <access-token>`.",
				},
			},
		},
		"x-agent-git-service": map[string]any{
			"api_surface": map[string]any{
				"github_compatible_prefix": githubCompatibleOpenAPIPrefix,
				"extension_prefix":         extensionOpenAPIPrefix,
			},
			"compatibility_deltas": []map[string]any{
				{
					"id":       "issues-list-omits-body",
					"path":     "/api/v3/repos/{owner}/{repo}/issues",
					"summary":  "List issues omits body content in REST list responses.",
					"evidence": "internal/service/issue.go: ListIssuesForREST returns issues for the REST list endpoint while omitting the body payload.",
				},
				{
					"id":       "branch-protection-monolithic",
					"path":     "/api/v3/repos/{owner}/{repo}/branches/{branch}/protection",
					"summary":  "Branch protection supports a monolithic protection document plus selected GitHub-style subresources, but not the full branch-protection subresource tree.",
					"evidence": "internal/router/router.go routes branch protection through wildcard branch handlers; internal/rest/handlers_branch.go dispatches selected subresources.",
				},
			},
			"route_families": []string{
				"discovery",
				"current user",
				"repositories",
				"issues",
				"pull requests",
				"branches and commits",
				"contents and Git database",
				"organizations and teams",
				"releases",
				"notifications",
				"search",
			},
		},
		"paths": buildGitHubCompatibleOpenAPIPaths(),
	}
	out, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		panic(err)
	}
	return out
}

func buildGitHubCompatibleOpenAPIPaths() map[string]any {
	return map[string]any{
		"/api/v3": map[string]any{
			"get": operation("getGitHubCompatibleAPIDiscovery", "Get GitHub-compatible REST discovery links.", nil, nil, nil, response(200, "API discovery document")),
		},
		"/api/v3/": map[string]any{
			"get": operation("getGitHubCompatibleAPIDiscoveryTrailingSlash", "Get GitHub-compatible REST discovery links.", nil, nil, nil, response(200, "API discovery document")),
		},
		"/api/v3/openapi.json": map[string]any{
			"get": operation("getGitHubCompatibleOpenAPISpec", "Get the published OpenAPI compatibility contract for GitHub-shaped REST APIs.", nil, nil, nil, response(200, "OpenAPI document")),
		},
		"/api/v3/meta": map[string]any{
			"get": operation("getServerMeta", "Get GitHub-compatible server metadata.", nil, nil, nil, response(200, "Server metadata returned")),
		},
		"/api/v3/rate_limit": map[string]any{
			"get": operation("getRateLimit", "Get GitHub-compatible local rate-limit state.", nil, nil, nil, response(200, "Rate-limit state returned")),
		},
		"/api/v3/user": map[string]any{
			"get": operation("getAuthenticatedUser", "Get the authenticated user.", auth(), nil, nil, response(200, "Authenticated user returned")),
		},
		"/api/v3/user/orgs": map[string]any{
			"get": operation("listUserOrgs", "List organizations for the authenticated user.", auth(), nil, nil, response(200, "Organization list returned")),
		},
		"/api/v3/user/repos": map[string]any{
			"get":  operation("listUserRepos", "List repositories for the authenticated user.", auth(), nil, nil, response(200, "Repository list returned")),
			"post": operation("createUserRepo", "Create a repository for the authenticated user.", auth(), nil, nil, response(201, "Repository created")),
		},
		"/api/v3/repos/{owner}/{repo}": map[string]any{
			"get":    operation("getRepo", "Get a repository.", nil, nil, pathParams(param("owner", "string"), param("repo", "string")), response(200, "Repository returned")),
			"patch":  operation("updateRepo", "Update a repository.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string")), response(200, "Repository updated")),
			"delete": operation("deleteRepo", "Delete a repository.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string")), response(204, "Repository deleted")),
		},
		"/api/v3/repos/{owner}/{repo}/issues": map[string]any{
			"get":  operation("listIssues", "List repository issues.", nil, nil, pathParams(param("owner", "string"), param("repo", "string")), response(200, "Issue list returned")),
			"post": operation("createIssue", "Create an issue.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string")), response(201, "Issue created")),
		},
		"/api/v3/repos/{owner}/{repo}/issues/{number}": map[string]any{
			"get":   operation("getIssue", "Get an issue.", nil, nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Issue returned")),
			"patch": operation("updateIssue", "Update an issue.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Issue updated")),
		},
		"/api/v3/repos/{owner}/{repo}/issues/{number}/comments": map[string]any{
			"get":  operation("listIssueComments", "List issue comments.", nil, nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Issue comments returned")),
			"post": operation("createIssueComment", "Create an issue comment.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(201, "Issue comment created")),
		},
		"/api/v3/repos/{owner}/{repo}/pulls": map[string]any{
			"get":  operation("listPullRequests", "List pull requests.", nil, nil, pathParams(param("owner", "string"), param("repo", "string")), response(200, "Pull request list returned")),
			"post": operation("createPullRequest", "Create a pull request.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string")), response(201, "Pull request created")),
		},
		"/api/v3/repos/{owner}/{repo}/pulls/{number}": map[string]any{
			"get":   operation("getPullRequest", "Get a pull request.", nil, nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Pull request returned")),
			"patch": operation("updatePullRequest", "Update a pull request.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Pull request updated")),
		},
	}
}

func buildRESTOpenAPIPaths() map[string]any {
	return map[string]any{
		"/api/ext/v1/client-runs": map[string]any{"post": operation("startClientRun", "Create one short-lived native actor run identity. Context association is best-effort, never a permission grant.", auth(), clientRunOpenAPIBody(), nil, clientRunOpenAPIResponses(true))},
		"/api/ext/v1/client-runs/{run_id}": map[string]any{
			"get":    operation("getClientRun", "Read the owned run receipt; current identifies the authenticating run.", auth(), nil, pathParams(param("run_id", "string")), clientRunOpenAPIResponses(false)),
			"delete": operation("revokeClientRun", "Revoke this actor's run without revoking the native parent; a run credential may revoke only itself.", auth(), nil, pathParams(param("run_id", "string")), response(204, "Run revoked")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/ci":                     map[string]any{"get": operation("getCIBackend", "Observe the independently configured CI backend and explicit required policy. Git hosting and merge authority remain separate.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string")), response(200, "Backend binding and required-check policy, without credentials"))},
		"/api/ext/v1/repos/{owner}/{repo}/pulls/{number}/context": map[string]any{"get": operation("getPullRequestRunLinks", "Read bounded run association and provider projection links. Missing association does not block PR collaboration.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Run receipts, provider links, head and observation completeness"))},
		"/api/ext/v1": map[string]any{
			"get": operation("getExtensionAPIDiscovery", "Get extension API discovery links.", nil, nil, nil, response(200, "extension API discovery document")),
		},
		"/api/ext/v1/": map[string]any{
			"get": operation("getExtensionAPIDiscoveryTrailingSlash", "Get extension API discovery links.", nil, nil, nil, response(200, "extension API discovery document")),
		},
		"/api/ext/v1/openapi.json": map[string]any{
			"get": operation("getRESTOpenAPISpec", "Get the published OpenAPI contract for extension REST APIs.", nil, nil, nil, response(200, "OpenAPI document")),
		},
		"/api/ext/v1/agents": map[string]any{
			"post": operation("createAgent", "Register a new agent identity.", nil, jsonBody(true, map[string]any{
				"prefix_login":      stringSchema("Optional login prefix for the created agent account."),
				"default_repo_name": stringSchema("Optional default repository name for the created agent."),
			}, nil), nil, response(201, "Agent created")),
		},
		"/api/ext/v1/agent-invites": map[string]any{
			"post": operation("createAgentInvite", "Create an invite token used to bind an agent to a user.", auth(), jsonBody(false, map[string]any{
				"repo_grants": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"repo_full_name": stringSchema("Repository full name to grant during bind."), "permission": stringSchema("Requested permission (read/write/admin).")}}},
				"team_grants": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"org": stringSchema("Organization login for a team grant."), "team_slug": stringSchema("Team slug to grant during bind."), "role": stringSchema("Team role (member/maintainer).")}}},
			}, nil), nil, response(201, "Agent invite created")),
		},
		"/api/ext/v1/agent-bindings/confirm": map[string]any{
			"post": operation("confirmAgentBinding", "Confirm an agent binding using an invite token.", auth(), jsonBody(true, map[string]any{
				"invite_token": stringSchema("Invite token issued by POST /api/ext/v1/agent-invites."),
			}, []string{"invite_token"}), nil, response(201, "Binding confirmed")),
		},
		"/api/ext/v1/agent-bindings/{agent_login}": map[string]any{
			"patch": operation("renameBoundAgent", "Rename a bound agent's display name.", auth(), jsonBody(true, map[string]any{
				"name": stringSchema("New display name for the bound agent."),
			}, []string{"name"}), pathParams(param("agent_login", "string")), response(200, "Agent renamed")),
			"delete": operation("unbindAgent", "Detach a bound agent and revoke only temporary human console switch sessions. The agent's long-lived token, account, memory repository, and independently managed grants are retained.", auth(), nil, pathParams(param("agent_login", "string")), response(200, "Agent unbound")),
		},
		"/api/ext/v1/agent-bindings/{agent_login}/reset-token": map[string]any{
			"post": operation("resetAgentToken", "Rotate the token for a bound agent login.", auth(), nil, pathParams(param("agent_login", "string")), response(200, "Token rotated")),
		},
		"/api/ext/v1/agent-bindings/{agent_login}/switch-session": map[string]any{
			"post": operation("switchAgentSession", "Create a temporary console session for a bound agent without rotating its existing tokens.", auth(), nil, pathParams(param("agent_login", "string")), response(200, "Switch session created")),
		},
		"/api/ext/v1/agent-bindings/{agent_login}/refresh-session": map[string]any{
			"post": operation("refreshAgentSwitchSession", "Refresh an active bound-agent switch session before it expires.", auth(), nil, pathParams(param("agent_login", "string")), response(200, "Switch session refreshed")),
		},
		"/api/v3/agent-sessions/{session_id}/lifecycle": map[string]any{
			"get": operation("getDelegatedAgentSessionLifecycle", "Read one canonical team-v4 delegated Session's closed, secret-free lifecycle projection as a durable site administrator.", auth(), nil, pathParams(param("session_id", "string")), delegatedSessionLifecycleOpenAPIResponses()),
		},
		"/api/v3/execution-context/intake": map[string]any{
			"post": operation("intakeExecutionContext", "Pull one task-token-bound context through an operator-registered fixed egress connector and persist a credential-free immutable snapshot. This does not grant AGS authority.", nil, jsonBody(true, map[string]any{
				"source_instance_id":    stringSchema("Optional stable registered source identity. When combined with the endpoint hint both selectors must resolve the same connector."),
				"runtime_endpoint_hint": stringSchema("Optional exact runtime-facing endpoint hint; it never controls AGS egress."),
				"locator": closedObjectSchema(map[string]any{
					"workspace_id": stringSchema("Current source workspace UUID."),
					"agent_id":     stringSchema("Current source agent UUID."),
					"task_id":      stringSchema("Current source task UUID."),
				}, []string{"workspace_id", "agent_id", "task_id"}),
				"source_token": map[string]any{"type": "string", "writeOnly": true, "description": "Current task-scoped source bearer. It is used only in request memory and is never persisted or returned."},
			}, []string{"locator", "source_token"}), nil, map[string]any{
				"201": map[string]any{"description": "Immutable credential-free context snapshot persisted"},
				"401": map[string]any{"description": "Source credential rejected or task no longer current"},
				"409": map[string]any{"description": "Connector selection ambiguous"},
				"422": map[string]any{"description": "Closed request, locator, or connector selection invalid"},
				"502": map[string]any{"description": "Source redirect or response contract rejected"},
				"503": map[string]any{"description": "Registry or source unavailable"},
			}),
		},
		"/api/v3/outbound/deliveries": map[string]any{
			"get": operation("listOutboundDeliveries", "List secret-safe durable outbound delivery rows and observable retry/terminal state.", auth(), nil, nil, map[string]any{
				"200": map[string]any{"description": "Durable outbound deliveries", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/OutboundDeliveryV1"}}}}},
			}),
		},
		"/api/v3/outbound/deliveries/{delivery_id}/retry": map[string]any{
			"post": operation("retryOutboundDelivery", "Retry one configured typed outbound delivery without accepting a caller-selected URL or provider.", auth(), nil, pathParams(param("delivery_id", "integer")), map[string]any{
				"200": map[string]any{"description": "Delivery state", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/OutboundDeliveryV1"}}}},
				"404": map[string]any{"description": "Delivery not found"},
			}),
		},
		"/api/v3/access-grants": map[string]any{
			"post": operation("issueAccessGrant", "Issue one task/repository-scoped grant from a fresh execution-context source observation. Selector and operation requests are fail-soft and cannot self-grant authority.", nil, accessGrantIssueOpenAPIBody(), nil, accessGrantIssueOpenAPIResponses()),
		},
		"/api/v3/access-grants/renew": map[string]any{
			"post": operation("renewAccessGrant", "Renew the same task/repository grant from a fresh matching source observation and atomically revoke the prior bearer.", accessGrantAuth(), accessGrantRenewOpenAPIBody(), nil, accessGrantIssueOpenAPIResponses()),
		},
		"/api/v3/access-grants/current": map[string]any{
			"get": operation("getCurrentAccessGrant", "Read the current secret-free grant receipt.", accessGrantAuth(), nil, nil, accessGrantReceiptOpenAPIResponses()),
		},
		"/api/v3/access-grants/revoke": map[string]any{
			"post": operation("revokeAccessGrant", "Revoke the current grant bearer.", accessGrantAuth(), accessGrantRevokeOpenAPIBody(), nil, accessGrantReceiptOpenAPIResponses()),
		},
		"/api/v3/access-grants/authorize": map[string]any{
			"post": operation("authorizeAccessGrantOperation", "Admission-only authorization that persists one exact operation invocation after fresh use-time authority validation. It never attempts a provider effect and returns provider_attempt=not_attempted.", accessGrantAuth(), accessGrantAuthorizeOpenAPIBody(), nil, accessGrantInvocationOpenAPIResponses()),
		},
		"/api/v3/access-grants/transport-sessions": map[string]any{
			"post": operation("issueAccessGrantTransportSession", "Derive one exact internal Git and GitHub-compatible API transport Session from the current Access Grant without accepting a Multica workload assertion. Generic transport excludes repo.create, repo.admin, review.submit, and pr.merge.", accessGrantAuth(), accessGrantTransportOpenAPIBody(), nil, accessGrantTransportSessionOpenAPIResponses()),
		},
		"/api/v3/access-grants/effects/pr.merge": map[string]any{
			"post": operation("executeAccessGrantPRMerge", "Execute at most one exact server-owned provider merge after use-time repo, policy, PR, head/base, CI and protected-authority checks. Definitive pre-dispatch fact conflicts return a terminal provider_attempt=not_attempted receipt, not HTTP 409.", accessGrantAuth(), accessGrantPRMergeOpenAPIBody(), nil, accessGrantPRMergeOpenAPIResponses()),
		},
		"/api/v3/access-grants/invocations/{invocation_id}": map[string]any{
			"get": operation("getAccessGrantInvocation", "Read or GET-only reconcile one exact provider-effect receipt, including terminal pre-dispatch conflicts. This route never repeats a provider POST.", accessGrantAuth(), nil, pathParams(param("invocation_id", "string")), accessGrantPRMergeOpenAPIResponses()),
		},
		"/api/ext/v1/oidc/device/code": map[string]any{
			"post": operation("createOIDCDeviceCode", "Start a generic OIDC device-code login flow.", nil, nil, nil, response(200, "Device code issued")),
		},
		"/api/ext/v1/oidc/session": map[string]any{
			"post": operation("exchangeOIDCSession", "Exchange generic OIDC session data for a local session.", nil, jsonBody(true, map[string]any{
				"device_code": stringSchema("OIDC device code previously issued to the client."),
			}, []string{"device_code"}), nil, response(200, "Session established")),
		},
		"/api/ext/v1/oidc/callback": map[string]any{
			"post": operation("handleOIDCCallback", "Handle the generic OIDC callback payload.", nil, jsonBody(true, map[string]any{
				"id_token": stringSchema("OIDC ID token returned from the login redirect flow."),
			}, []string{"id_token"}), nil, response(200, "Callback processed")),
		},
		"/api/ext/v1/oidc/lookup": map[string]any{
			"post": operation("lookupOIDCIdentity", "Resolve a generic OIDC identity to a local user.", nil, jsonBody(true, map[string]any{
				"id_token": stringSchema("OIDC ID token to validate and map to a local user."),
			}, []string{"id_token"}), nil, response(200, "Identity resolved")),
		},
		"/api/ext/v1/oauth/device/approve": map[string]any{
			"post": operation("approveOAuthDeviceCode", "Approve an OAuth device code for the authenticated human user.", auth(), jsonAndFormBody(true, map[string]any{
				"user_code": stringSchema("User code displayed to the human during the OAuth device flow."),
			}, []string{"user_code"}), nil, response(200, "Device code approved")),
		},
		"/api/ext/v1/oauth/device/reject": map[string]any{
			"post": operation("rejectOAuthDeviceCode", "Reject an OAuth device code for the authenticated human user.", auth(), jsonAndFormBody(true, map[string]any{
				"user_code": stringSchema("User code displayed to the human during the OAuth device flow."),
				"reason":    stringSchema("Optional rejection reason recorded in the device-code audit log."),
			}, []string{"user_code"}), nil, response(200, "Device code rejected")),
		},
		"/auth/connected/login": map[string]any{
			"get": operation("startConnectedLogin", "Redirect the browser to the configured connected login provider.", nil, nil, nil, response(302, "Redirect to connected login")),
		},
		"/auth/connected/callback": map[string]any{
			"get": operation("handleConnectedCallback", "Exchange a connected login authorization code for a local session.", nil, nil, queryParams(
				param("code", "string"),
				param("error", "string"),
				param("state", "string"),
			), map[string]any{
				"200": map[string]any{"description": "Direct agent callback without browser state returns durable token JSON; browser callback without console redirect returns a one-time local authorization code JSON."},
				"302": map[string]any{"description": "Browser callback redirects to the console with a one-time local authorization code and PKCE verifier cookie."},
			}),
		},
		"/api/v3/repos/{owner}/{repo}/projection/status": map[string]any{
			"get": operation("getProjectionStatus", "Inspect AGS-owned external projection ref state and Forgejo PR projection jobs for a repository.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string")), response(200, "Projection status returned")),
		},
		"/api/v3/repos/{owner}/{repo}/pulls/{number}/provider/projection": map[string]any{
			"get": operation("getPullRequestProviderProjection", "Read one exact live provider pull-request projection plus a secret-safe correlation receipt through AGS without exposing provider credentials.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Provider projection evidence returned")),
		},
		"/api/v3/repos/{owner}/{repo}/pulls/{number}/provider/ci/runs": map[string]any{
			"get": operation("getPullRequestProviderCIRuns", "Read provider CI runs plus a secret-safe correlation receipt bound to one exact AGS pull request and head through AGS.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Provider CI evidence returned")),
		},
		"/api/v3/repos/{owner}/{repo}/pulls/{number}/provider/merge": map[string]any{
			"post": operation("mergePullRequestViaProvider", "Authorize a Human through AGS and execute the authoritative mapped provider merge with the server-owned integration executor.", auth(), jsonBody(true, map[string]any{
				"expected_head_sha": map[string]any{"type": "string", "pattern": "^[a-f0-9]{40}$"},
				"merge_method":      map[string]any{"type": "string", "enum": []string{"merge", "rebase", "rebase-merge", "squash", "fast-forward-only"}},
			}, []string{"expected_head_sha", "merge_method"}), pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Provider merge accepted and receipt returned")),
			"get": operation("observePullRequestViaProvider", "Read-only merge/reconcile status for one AGS PR. Never POSTs a provider merge.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(200, "Provider merge observation returned")),
		},
		"/api/v3/repos/{owner}/{repo}/projection/forgejo/pulls/{number}/retry": map[string]any{
			"post": operation("retryForgejoPullRequestProjection", "Safely requeue/resume a retryable failed Forgejo PR projection job without manually creating a Forgejo PR.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string"), param("number", "integer")), response(202, "Projection retry queued")),
		},
		"/api/ext/v1/user/agents": map[string]any{
			"get": operation("listBoundAgents", "List agents bound to the authenticated user.", auth(), nil, nil, response(200, "Bound agents returned")),
		},
		"/api/ext/v1/viewer/summary": map[string]any{
			"get": operation("getViewerSummary", "Get the authenticated viewer's console summary.", auth(), nil, nil, response(200, "Viewer summary returned")),
		},
		"/api/ext/v1/user/orgs": map[string]any{
			"post": operation("createUserOrg", "Create a local organization for the authenticated user.", auth(), jsonBody(true, map[string]any{
				"login":                         stringSchema("Organization login."),
				"name":                          stringSchema("Optional organization display name."),
				"default_repository_permission": stringSchema("Default base repository permission for organization members."),
			}, []string{"login"}), nil, response(201, "Organization created")),
		},
		"/api/ext/v1/user/tokens": map[string]any{
			"get": operation("listTokens", "List local user tokens.", auth(), nil, nil, response(200, "Tokens returned")),
			"post": operation("createToken", "Create a local user token.", auth(), jsonBody(true, map[string]any{
				"name":       stringSchema("Optional token display name."),
				"expires_at": map[string]any{"type": "string", "format": "date-time", "description": "Optional RFC3339 expiration timestamp."},
			}, nil), nil, response(201, "Token created")),
			"delete": operation("deleteToken", "Delete a local user token.", auth(), jsonBody(true, map[string]any{
				"id":       map[string]any{"type": "integer", "minimum": 1},
				"token_id": map[string]any{"type": "integer", "minimum": 1},
				"token":    stringSchema("Raw token value when deleting by value instead of numeric id."),
			}, nil), nil, response(204, "Token deleted")),
		},
		"/api/ext/v1/notifications/summary": map[string]any{
			"get": operation("getNotificationsSummary", "Get the authenticated viewer's notification summary.", auth(), nil, nil, response(200, "Notifications summary returned")),
		},
		"/api/ext/v1/orgs/{org}/management-summary": map[string]any{
			"get": operation("getOrgManagementSummary", "Get a management summary for an organization.", auth(), nil, pathParams(
				param("org", "string"),
			), response(200, "Organization management summary returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/summary": map[string]any{
			"get": operation("getRepoSummary", "Get a console summary for a repository.", auth(), nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), response(200, "Repository summary returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/issues/{number}/thread": map[string]any{
			"get": operation("getIssueThread", "Get an issue thread aggregate.", auth(), nil, append(pathParams(
				param("owner", "string"),
				param("repo", "string"),
				param("number", "integer"),
			), queryParams(
				param("include", "string"),
				param("comments_page", "integer"),
				param("comments_per_page", "integer"),
			)...), response(200, "Issue thread aggregate returned")),
		},
		"/api/v3/repos/{owner}/{repo}/replication/identity": map[string]any{
			"get":  operation("getReplicationRegistration", "Observe replication identity without allocation. Explicit primary opt-in and native repository administrator required; never grants a peer access.", auth(), nil, pathParams(param("owner", "string"), param("repo", "string")), replicationRegistrationResponses()),
			"post": operation("registerReplicationRepository", "Allocate stable replication identity for the exact previously observed repository. No peer grants, listener changes, or Git writes. After an uncertain result inspect GET before retrying.", auth(), replicationRegistrationBody(), pathParams(param("owner", "string"), param("repo", "string")), replicationRegistrationResponses()),
		},
		"/api/ext/v1/repos/{owner}/{repo}/team-sharing/enable": map[string]any{
			"post": operation("enableRepoTeamSharing", "Enable team-based sharing for a repository.", auth(), nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), response(200, "Team sharing enabled")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/pages": map[string]any{
			"get": operation("listWikiPages", "List wiki pages for a repository.", nil, nil, append(pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), queryParams(
				param("path", "string"),
				param("recursive", "boolean"),
				param("label", "string"),
				param("labels", "string"),
				param("exclude_label", "string"),
				param("exclude_labels", "string"),
			)...), response(200, "Wiki pages returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/search": map[string]any{
			"get": operation("searchWikiPages", "Search wiki pages for a repository, with optional label filters.", nil, nil, append(pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), queryParams(
				param("q", "string"),
				param("limit", "integer"),
				param("offset", "integer"),
				param("label", "string"),
				param("labels", "string"),
				param("exclude_label", "string"),
				param("exclude_labels", "string"),
			)...), response(200, "Wiki search results returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/catalog": map[string]any{
			"get": operation("getWikiCatalog", "Get an aggregate wiki catalog for a repository.", nil, nil, append(pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), queryParams(
				param("include", "string"),
				param("path", "string"),
				param("recursive", "boolean"),
				param("labels", "string"),
				param("exclude_labels", "string"),
			)...), response(200, "Wiki catalog returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/pages/batch": map[string]any{
			"post": operation("batchGetWikiPages", "Get multiple wiki pages in one extension aggregate request.", auth(), jsonBody(true, map[string]any{
				"slugs":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Wiki page slugs to fetch."},
				"include":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional fields to include, such as body, labels, backlinks, or backlink_count."},
				"body_limit": map[string]any{"type": "integer", "minimum": 0, "description": "Maximum body characters to return per page."},
				"ref":        stringSchema("Optional full commit SHA from wiki history."),
			}, []string{"slugs"}), pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), response(200, "Wiki page batch returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/tree": map[string]any{
			"get": operation("listWikiTree", "List one directory view from the authoritative wiki tree.", nil, nil, append(pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), queryParams(
				param("path", "string"),
				param("ref", "string"),
			)...), response(200, "Wiki tree returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/state": map[string]any{
			"get": operation("getWikiState", "Get the authoritative wiki derived-index state for a repository.", auth(), nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), response(200, "Current wiki state")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/reconcile/request": map[string]any{
			"post": operation("requestWikiReconcile", "Request a wiki reconcile without running it synchronously.", auth(), nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), response(202, "Reconcile request recorded")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/reconcile": map[string]any{
			"post": operation("reconcileWiki", "Run the authoritative wiki reconcile synchronously and return the persisted result.", auth(), nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), response(200, "Reconcile completed")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/move": map[string]any{
			"post": operation("moveWikiPagePrefix", "Atomically move all wiki pages under one slug prefix to another prefix.", auth(), jsonBody(true, map[string]any{
				"from":     stringSchema("Source wiki slug prefix to move."),
				"to":       stringSchema("Destination wiki slug prefix."),
				"message":  stringSchema("Optional commit message recorded for the wiki move."),
				"if_match": mapSchema("Latest blob SHAs keyed by source wiki slug."),
			}, []string{"from", "to", "if_match"}), pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), response(200, "Wiki pages moved")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/pages/{slug}": map[string]any{
			"get": operation("getWikiPage", "Get a wiki page by slug, optionally at a full commit SHA from that page's history.", nil, nil, append(pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), queryParams(
				map[string]any{
					"name":        "ref",
					"in":          "query",
					"schema":      map[string]any{"type": "string", "pattern": "^[0-9a-fA-F]{40}$"},
					"description": "Optional full commit SHA from the requested page's history.",
				},
			)...), response(200, "Wiki page returned")),
			"put": operation("putWikiPage", "Create or replace a wiki page by slug.", auth(), jsonBody(true, map[string]any{
				"body":    stringSchema("Markdown body for the wiki page."),
				"message": stringSchema("Optional commit message recorded for the wiki update."),
				"sha":     stringSchema("Optional blob SHA precondition for optimistic concurrency control."),
			}, []string{"body"}), pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), response(200, "Wiki page written")),
			"delete": operation("deleteWikiPage", "Delete a wiki page by slug.", auth(), jsonBody(false, map[string]any{
				"message": stringSchema("Optional commit message recorded for the wiki deletion."),
			}, nil), pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), response(204, "Wiki page deleted")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/pages/{slug}/move": map[string]any{
			"post": operation("moveWikiPage", "Move a wiki page to a new slug and rewrite eligible inbound wiki references in the same commit.", auth(), jsonBody(true, map[string]any{
				"new_slug": stringSchema("Destination slug for the wiki page."),
				"message":  stringSchema("Optional commit message recorded for the wiki move."),
				"if_match": stringSchema("Latest source page commit SHA observed by the client."),
			}, []string{"new_slug", "if_match"}), pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), response(200, "Wiki page moved and inbound references rewritten")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/pages/{slug}/labels": map[string]any{
			"get": operation("listWikiPageLabels", "List labels attached to a wiki page.", nil, nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), response(200, "Wiki page labels returned")),
			"post": operation("addWikiPageLabels", "Add labels to a wiki page.", auth(), jsonBody(true, map[string]any{
				"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Repository label names to attach."},
			}, []string{"labels"}), pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), response(200, "Wiki page labels returned")),
			"put": operation("setWikiPageLabels", "Replace labels attached to a wiki page.", auth(), jsonBody(true, map[string]any{
				"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Repository label names that should remain attached."},
			}, []string{"labels"}), pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), response(200, "Wiki page labels returned")),
			"delete": operation("removeAllWikiPageLabels", "Remove all labels from a wiki page.", auth(), nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), response(204, "Wiki page labels removed")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/pages/{slug}/labels/{name}": map[string]any{
			"delete": operation("removeWikiPageLabel", "Remove one label from a wiki page.", auth(), nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
				param("name", "string"),
			), response(200, "Remaining wiki page labels returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/pages/{slug}/history": map[string]any{
			"get": operation("listWikiPageHistory", "List paginated revision history for a wiki page slug.", nil, nil, append(pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), queryParams(
				param("page", "integer"),
				param("per_page", "integer"),
			)...), response(200, "Wiki page history returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/compact": map[string]any{
			"post": operation("compactWikiHistory", "Temporarily disabled while the wiki catalog corruption incident is being contained and repaired.", auth(), jsonBody(false, map[string]any{
				"before": stringSchema("Reserved for future bounded compaction support. Currently rejected when non-empty."),
			}, nil), pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), response(409, "Wiki history compaction is temporarily disabled")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/compact/{jobID}": map[string]any{
			"get": operation("getWikiCompactionJob", "Get the current status for an async wiki history compaction job.", auth(), nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
				param("jobID", "string"),
			), response(200, "Wiki history compaction job returned")),
		},
		"/api/ext/v1/admin/wiki/repos/{owner}/{repo}/repair-locks": map[string]any{
			"post": operation("repairWikiLocks", "Inspect and clear stale wiki branch lock files for one repository.", auth(), jsonBody(false, map[string]any{
				"force": map[string]any{
					"type":        "boolean",
					"description": "When true, clear the lock even if it is still fresh.",
				},
			}, nil), pathParams(
				param("owner", "string"),
				param("repo", "string"),
			), response(200, "Wiki lock repair result returned")),
		},
		"/api/ext/v1/repos/{owner}/{repo}/wiki/pages/{slug}/backlinks": map[string]any{
			"get": operation("listWikiBacklinks", "List inbound wiki links for a page slug.", nil, nil, pathParams(
				param("owner", "string"),
				param("repo", "string"),
				wikiSlugParamSpec(),
			), response(200, "Wiki backlinks returned")),
		},
	}
}

func operation(id, summary string, security []map[string][]string, requestBody map[string]any, params []map[string]any, responses map[string]any) map[string]any {
	op := map[string]any{
		"operationId": id,
		"summary":     summary,
		"responses":   responses,
	}
	if len(security) > 0 {
		op["security"] = security
	}
	if requestBody != nil {
		op["requestBody"] = requestBody
	}
	if len(params) > 0 {
		op["parameters"] = params
	}
	return op
}

func auth() []map[string][]string {
	return []map[string][]string{{"tokenAuth": []string{}}}
}

func accessGrantAuth() []map[string][]string {
	return []map[string][]string{{"accessGrantAuth": []string{}}}
}

func pathParams(params ...map[string]any) []map[string]any {
	for _, param := range params {
		param["in"] = "path"
		param["required"] = true
	}
	return params
}

func queryParams(params ...map[string]any) []map[string]any {
	for _, param := range params {
		param["in"] = "query"
	}
	return params
}

func param(name, typ string) map[string]any {
	schema := map[string]any{"type": typ}
	if typ == "integer" {
		schema["minimum"] = 1
	}
	return map[string]any{
		"name":   name,
		"schema": schema,
	}
}

func wikiSlugParamSpec() map[string]any {
	p := param("slug", "string")
	p["description"] = "Wiki page slug as one path parameter. Encode nested slug separators as %2F, for example guides%2Fsetup."
	return p
}

func stringSchema(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
	}
}

func mapSchema(description string) map[string]any {
	return map[string]any{
		"type":                 "object",
		"description":          description,
		"additionalProperties": map[string]any{"type": "string"},
	}
}

func jsonBody(required bool, properties map[string]any, requiredProps []string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(requiredProps) > 0 {
		schema["required"] = requiredProps
	}
	return map[string]any{
		"required": required,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": schema,
			},
		},
	}
}

func jsonAndFormBody(required bool, properties map[string]any, requiredProps []string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(requiredProps) > 0 {
		schema["required"] = requiredProps
	}
	return map[string]any{
		"required": required,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": schema,
			},
			"application/x-www-form-urlencoded": map[string]any{
				"schema": schema,
			},
		},
	}
}

func accessGrantLocatorOpenAPISchema() map[string]any {
	return closedObjectSchema(map[string]any{
		"workspace_id": stringSchema("Current source workspace UUID."),
		"agent_id":     stringSchema("Current source agent UUID."),
		"task_id":      stringSchema("Current source task UUID."),
	}, []string{"workspace_id", "agent_id", "task_id"})
}

func accessGrantIssueOpenAPIBody() map[string]any {
	return closedJSONBody(true, map[string]any{
		"source_instance_id":    stringSchema("Optional stable registered source identity."),
		"runtime_endpoint_hint": stringSchema("Optional runtime endpoint hint used only for fixed-connector selection."),
		"locator":               accessGrantLocatorOpenAPISchema(),
		"source_token":          map[string]any{"type": "string", "writeOnly": true, "description": "Current task-scoped source bearer; never persisted or returned."},
		"repository":            map[string]any{"type": "string", "pattern": `^[^/\s]+/[^/\s]+$`},
		"agent_id":              stringSchema("Optional canonical actor selector. Unbound values are ignored with a warning."),
		"policy_class":          stringSchema("Optional policy-class request. Invalid or unbound values fall back to the legal default."),
		"operations": map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string"},
			"description": "Optional operation requests intersected with source, actor, resource and operation envelopes."},
	}, []string{"locator", "source_token", "repository"})
}

func accessGrantRenewOpenAPIBody() map[string]any {
	return closedJSONBody(true, map[string]any{
		"source_instance_id":    stringSchema("Optional stable registered source identity."),
		"runtime_endpoint_hint": stringSchema("Optional runtime endpoint hint used only for fixed-connector selection."),
		"locator":               accessGrantLocatorOpenAPISchema(),
		"source_token":          map[string]any{"type": "string", "writeOnly": true, "description": "Fresh current task-scoped source bearer; never persisted or returned."},
	}, []string{"locator", "source_token"})
}

func accessGrantRevokeOpenAPIBody() map[string]any {
	return closedJSONBody(false, map[string]any{
		"reason": map[string]any{"type": "string", "maxLength": 255, "description": "Optional secret-free audit reason."},
	}, nil)
}

type accessGrantOperationOpenAPIVariant struct {
	operation   string
	constraints map[string]any
	authorize   bool
	transport   bool
}

// accessGrantOperationOpenAPIInventory is the single closed schema inventory
// shared by authorize requests and invocation receipts. transport marks the
// strictly smaller generic Session surface; high-risk admission/effect
// operations must never be enabled there by schema reuse.
func accessGrantOperationOpenAPIInventory() []accessGrantOperationOpenAPIVariant {
	empty := func() map[string]any { return closedObjectSchema(map[string]any{}, nil) }
	positive := func() map[string]any {
		return map[string]any{"type": "integer", "minimum": 1, "maximum": operationconstraints.MaxJSONSafePositiveInteger}
	}
	branch := func() map[string]any {
		return map[string]any{
			"type": "string", "minLength": 1, "maxLength": 2048,
			"pattern":     `^(?!refs/(?!heads/))(?:refs/heads/)?(?!@(?:$|/))(?!\.)(?!.*\/\.)(?![^/]*\.lock(?:/|$))(?!.*\/[^/]*\.lock(?:/|$))(?!.*(?:\.\.|//|@\{|[ \\~^:?*\[\x00-\x1f\x7f]))(?!.*\.$)[^/]+(?:/[^/]+)*$`,
			"description": "Canonical branch or refs/heads branch accepted by the shared constraint kernel.",
		}
	}
	sha := func() map[string]any { return map[string]any{"type": "string", "pattern": `^[a-f0-9]{40}$`} }
	digest := func() map[string]any { return map[string]any{"type": "string", "pattern": `^[a-f0-9]{64}$`} }
	repository := func() map[string]any {
		return map[string]any{"type": "string", "pattern": `^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})/[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})$`}
	}
	ordinary := func(operation string, constraints map[string]any) accessGrantOperationOpenAPIVariant {
		return accessGrantOperationOpenAPIVariant{operation: operation, constraints: constraints, authorize: true, transport: true}
	}
	admissionOnly := func(operation string, constraints map[string]any) accessGrantOperationOpenAPIVariant {
		return accessGrantOperationOpenAPIVariant{operation: operation, constraints: constraints, authorize: true}
	}
	prNumber := closedObjectSchema(map[string]any{"pull_request_number": positive()}, []string{"pull_request_number"})
	reviewAction := map[string]any{"type": "string", "enum": []string{"approve", "request_changes", "comment"}}
	return []accessGrantOperationOpenAPIVariant{
		ordinary("repo.read", empty()),
		admissionOnly("repo.create", closedObjectSchema(map[string]any{
			"target_repository": repository(), "base_ref": branch(), "source_base_sha": sha(), "source_ref_digest": digest(),
			"import_mode": map[string]any{"type": "string", "enum": []string{"ags-only", "ags-forgejo"}},
			"visibility":  map[string]any{"type": "string", "enum": []string{"private", "public", "internal"}},
		}, []string{"target_repository", "base_ref", "source_base_sha", "source_ref_digest", "import_mode", "visibility"})),
		admissionOnly("repo.delete", empty()),
		admissionOnly("repo.admin", closedObjectSchema(map[string]any{
			"target_repository": repository(), "base_ref": branch(), "source_base_sha": sha(), "source_ref_digest": digest(),
			"action": map[string]any{"type": "string", "enum": []string{"forgejo_onboard"}},
		}, []string{"target_repository", "base_ref", "source_base_sha", "source_ref_digest", "action"})),
		ordinary("git.read", empty()),
		ordinary("git.push", empty()),
		admissionOnly("git.force_push", empty()),
		admissionOnly("protected_ref.write", empty()),
		admissionOnly("ref.delete", empty()),
		admissionOnly("branch_protection.write", empty()),
		admissionOnly("webhook.write", empty()),
		ordinary("pr.create", closedObjectSchema(map[string]any{"base_ref": branch(), "head_ref": branch()}, []string{"base_ref", "head_ref"})),
		ordinary("pr.comment", prNumber),
		ordinary("pr.edit", prNumber),
		ordinary("pr.close", prNumber),
		ordinary("pr.reopen", prNumber),
		ordinary("pr.read", closedObjectSchema(map[string]any{"pull_request_number": positive()}, []string{"pull_request_number"})),
		ordinary("pr.read", closedObjectSchema(map[string]any{"head_ref": branch()}, []string{"head_ref"})),
		ordinary("pr.rebase", closedObjectSchema(map[string]any{
			"pull_request_number": positive(), "forgejo_pull_request_number": positive(), "expected_head_sha": sha(), "expected_base_sha": sha(),
		}, []string{"pull_request_number", "forgejo_pull_request_number", "expected_head_sha", "expected_base_sha"})),
		{operation: "pr.merge", constraints: closedObjectSchema(map[string]any{
			"pull_request_number": positive(), "forgejo_pull_request_number": positive(), "expected_head_sha": sha(),
			"merge_method": map[string]any{"type": "string", "enum": []string{"merge", "rebase", "rebase-merge", "squash", "fast-forward-only"}},
		}, []string{"pull_request_number", "forgejo_pull_request_number", "expected_head_sha", "merge_method"})},
		ordinary("review.read", closedObjectSchema(map[string]any{
			"pull_request_number": positive(), "forgejo_pull_request_number": positive(),
		}, []string{"pull_request_number", "forgejo_pull_request_number"})),
		ordinary("review.write", closedObjectSchema(map[string]any{
			"pull_request_number": positive(), "review_action": reviewAction,
		}, []string{"pull_request_number", "review_action"})),
		ordinary("review.write", closedObjectSchema(map[string]any{
			"pull_request_number": positive(), "forgejo_pull_request_number": positive(), "review_action": reviewAction,
		}, []string{"pull_request_number", "forgejo_pull_request_number", "review_action"})),
		// Legacy alias kept for closed authorize/invocation inventory only.
		admissionOnly("review.submit", closedObjectSchema(map[string]any{
			"pull_request_number": positive(), "forgejo_pull_request_number": positive(),
			"review_action": reviewAction,
		}, []string{"pull_request_number", "forgejo_pull_request_number", "review_action"})),
		admissionOnly("review.dismiss", closedObjectSchema(map[string]any{
			"pull_request_number": positive(), "forgejo_pull_request_number": positive(),
		}, []string{"pull_request_number", "forgejo_pull_request_number"})),
		ordinary("ci.read", empty()),
		ordinary("ci.read", closedObjectSchema(map[string]any{"run_id": positive()}, []string{"run_id"})),
		ordinary("ci.read", closedObjectSchema(map[string]any{
			"pull_request_number": positive(), "forgejo_pull_request_number": positive(),
		}, []string{"pull_request_number", "forgejo_pull_request_number"})),
		ordinary("ci.read", closedObjectSchema(map[string]any{
			"pull_request_number": positive(), "forgejo_pull_request_number": positive(), "head_sha": sha(),
		}, []string{"pull_request_number", "forgejo_pull_request_number", "head_sha"})),
	}
}

func accessGrantOperationRequestOpenAPIBody(include func(accessGrantOperationOpenAPIVariant) bool) map[string]any {
	inventory := accessGrantOperationOpenAPIInventory()
	variants := make([]map[string]any, 0, len(inventory))
	for _, variant := range inventory {
		if !include(variant) {
			continue
		}
		variants = append(variants, closedObjectSchema(map[string]any{
			"operation":   map[string]any{"type": "string", "enum": []string{variant.operation}},
			"constraints": variant.constraints,
		}, []string{"operation", "constraints"}))
	}
	return map[string]any{
		"required": true,
		"content":  map[string]any{"application/json": map[string]any{"schema": map[string]any{"oneOf": variants}}},
	}
}

func accessGrantAuthorizeOpenAPIBody() map[string]any {
	return accessGrantOperationRequestOpenAPIBody(func(variant accessGrantOperationOpenAPIVariant) bool { return variant.authorize })
}

func accessGrantTransportOpenAPIBody() map[string]any {
	return accessGrantOperationRequestOpenAPIBody(func(variant accessGrantOperationOpenAPIVariant) bool { return variant.transport })
}

func accessGrantPRMergeOpenAPIBody() map[string]any {
	return closedJSONBody(true, map[string]any{
		"invocation_id":      map[string]any{"type": "string", "format": "uuid", "description": "Caller-owned canonical recovery locator predeclared before any effect POST; supported clients derive it deterministically from the exact effect facts and GET it first."},
		"ags_pr_number":      map[string]any{"type": "integer", "minimum": 1, "maximum": operationconstraints.MaxJSONSafePositiveInteger},
		"provider_pr_number": map[string]any{"type": "integer", "minimum": 1, "maximum": operationconstraints.MaxJSONSafePositiveInteger},
		"expected_head_sha":  map[string]any{"type": "string", "pattern": `^[a-f0-9]{40}$`},
		"merge_method":       map[string]any{"type": "string", "enum": []string{"merge", "rebase", "rebase-merge", "squash", "fast-forward-only"}},
	}, []string{"invocation_id", "ags_pr_number", "provider_pr_number", "expected_head_sha", "merge_method"})
}

func closedJSONBody(required bool, properties map[string]any, requiredProps []string) map[string]any {
	return map[string]any{
		"required": required,
		"content": map[string]any{"application/json": map[string]any{
			"schema": closedObjectSchema(properties, requiredProps),
		}},
	}
}

func accessGrantIssueOpenAPISchema() map[string]any {
	return closedObjectSchema(map[string]any{
		"schema":      map[string]any{"type": "string", "enum": []string{"ags.access-grant.issue.v1"}},
		"grant_token": map[string]any{"type": "string", "readOnly": true, "pattern": `^ags_grant_[A-Za-z0-9_-]+$`, "description": "Returned once and never persisted in plaintext."},
		"grant":       map[string]any{"$ref": "#/components/schemas/AccessGrantV1"},
	}, []string{"schema", "grant_token", "grant"})
}

func accessGrantOpenAPISchema() map[string]any {
	id := map[string]any{"type": "integer", "minimum": 1, "maximum": operationconstraints.MaxJSONSafePositiveInteger}
	timestamp := map[string]any{"type": "string", "format": "date-time"}
	actor := closedObjectSchema(map[string]any{
		"id": id, "login": map[string]any{"type": "string", "minLength": 1}, "name": map[string]any{"type": "string", "minLength": 1},
		"user_kind": map[string]any{"type": "string", "enum": []string{"agent"}},
	}, []string{"id", "login", "name", "user_kind"})
	executor := closedObjectSchema(map[string]any{
		"id": id, "login": map[string]any{"type": "string", "minLength": 1}, "user_kind": map[string]any{"type": "string", "minLength": 1},
	}, []string{"id", "login", "user_kind"})
	source := closedObjectSchema(map[string]any{
		"instance_id": map[string]any{"type": "string", "minLength": 1}, "snapshot_id": map[string]any{"type": "string", "format": "uuid"},
		"workspace_id": map[string]any{"type": "string", "minLength": 1}, "agent_id": map[string]any{"type": "string", "minLength": 1},
		"task_id": map[string]any{"type": "string", "minLength": 1}, "run_id": map[string]any{"type": "string", "minLength": 1},
		"issue_id": map[string]any{"type": "string"}, "runtime_id": map[string]any{"type": "string"},
		"context_digest": map[string]any{"type": "string", "pattern": `^sha256:[a-f0-9]{64}$`},
	}, []string{"instance_id", "snapshot_id", "workspace_id", "agent_id", "task_id", "run_id", "context_digest"})
	repository := closedObjectSchema(map[string]any{
		"id": id, "full_name": map[string]any{"type": "string", "pattern": `^[^/\s]+/[^/\s]+$`}, "target": map[string]any{"type": "string", "minLength": 1},
	}, []string{"id", "full_name", "target"})
	operations := map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string"}}
	return closedObjectSchema(map[string]any{
		"schema": map[string]any{"type": "string", "enum": []string{"ags.access-grant.v1"}},
		"id":     map[string]any{"type": "string", "format": "uuid"}, "actor": actor, "executor": executor, "source": source, "repository": repository,
		"agent_selector_outcome": map[string]any{"type": "string", "enum": []string{"default", "accepted", "ignored_invalid", "ignored_unbound"}},
		"default_policy_class":   map[string]any{"type": "string", "minLength": 1}, "requested_policy_class": map[string]any{"type": "string"},
		"policy_class_outcome":     map[string]any{"type": "string", "enum": []string{"default", "accepted_default", "accepted", "ignored_invalid", "ignored_unbound"}},
		"effective_policy_classes": operations, "requested_operations": operations, "effective_operations": operations,
		"warnings":              map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string"}},
		"association_status":    map[string]any{"type": "string", "enum": []string{"linked", "provisional", "unlinked", "conflict"}},
		"identity_kind":         map[string]any{"type": "string", "enum": []string{"human", "durable_agent", "temporary_agent"}},
		"collaboration_mode":    map[string]any{"type": "string", "enum": []string{"passthrough_transport"}},
		"authority_revision":    map[string]any{"type": "string", "pattern": `^sha256:[a-f0-9]{64}$`},
		"renewed_from_grant_id": map[string]any{"type": "string", "format": "uuid"},
		"state":                 map[string]any{"type": "string", "enum": []string{"active", "revoked", "expired"}},
		"created_at":            timestamp, "expires_at": timestamp, "last_used_at": timestamp, "revoked_at": timestamp,
		"revocation_reason": map[string]any{"type": "string", "maxLength": 255},
	}, []string{"schema", "id", "actor", "executor", "source", "repository", "agent_selector_outcome", "default_policy_class", "policy_class_outcome", "effective_policy_classes", "requested_operations", "effective_operations", "warnings", "association_status", "identity_kind", "collaboration_mode", "authority_revision", "state", "created_at", "expires_at"})
}

func accessGrantInvocationOpenAPISchema() map[string]any {
	return accessGrantInvocationOpenAPISchemaFor(func(accessGrantOperationOpenAPIVariant) bool { return true })
}

func accessGrantTransportInvocationOpenAPISchema() map[string]any {
	return accessGrantInvocationOpenAPISchemaFor(func(variant accessGrantOperationOpenAPIVariant) bool { return variant.transport })
}

func accessGrantInvocationOpenAPISchemaFor(include func(accessGrantOperationOpenAPIVariant) bool) map[string]any {
	sha := func() map[string]any { return map[string]any{"type": "string", "pattern": `^[a-f0-9]{40}$`} }
	timestamp := map[string]any{"type": "string", "format": "date-time"}
	positive := func() map[string]any {
		return map[string]any{"type": "integer", "minimum": 1, "maximum": operationconstraints.MaxJSONSafePositiveInteger}
	}
	common := map[string]any{
		"schema": map[string]any{"type": "string", "enum": []string{"ags.access-grant-invocation.v1"}},
		"id":     map[string]any{"type": "string", "format": "uuid"}, "grant_id": map[string]any{"type": "string", "format": "uuid"},
		"actor_user_id": positive(), "executor_user_id": positive(),
		"repository":         map[string]any{"type": "string", "pattern": `^[^/\s]+/[^/\s]+$`},
		"authority_revision": map[string]any{"type": "string", "pattern": `^sha256:[a-f0-9]{64}$`},
		"ags_pr_number":      positive(), "provider": map[string]any{"type": "string", "enum": []string{"forgejo"}},
		"provider_repository": map[string]any{"type": "string", "pattern": `^[^/\s]+/[^/\s]+$`}, "provider_pr_number": positive(),
		"expected_head_sha": sha(), "expected_base_sha": sha(), "base_ref": map[string]any{"type": "string", "minLength": 1},
		"effect_method":         map[string]any{"type": "string", "enum": []string{"merge", "rebase", "rebase-merge", "squash", "fast-forward-only"}},
		"state":                 map[string]any{"type": "string", "enum": []string{"planned", "dispatching", "completed", "denied", "conflict", "recovery_needed"}},
		"authorization_outcome": map[string]any{"type": "string", "enum": []string{"allowed", "denied"}},
		"provider_attempt":      map[string]any{"type": "string", "enum": []string{"not_attempted", "attempted"}},
		"provider_outcome":      map[string]any{"type": "string", "enum": []string{"not_applicable", "not_attempted", "outcome_unknown", "confirmed", "reconciled_after_error", "merged_fact_drift", "confirmed_not_merged"}},
		"provider_merged":       map[string]any{"type": "boolean"}, "provider_merge_sha": sha(), "denial_code": map[string]any{"type": "string"},
		"created_at": timestamp, "finished_at": timestamp,
	}
	required := []string{"schema", "id", "grant_id", "actor_user_id", "executor_user_id", "repository", "operation", "constraints", "authority_revision", "state", "authorization_outcome", "provider_attempt", "provider_outcome", "provider_merged", "created_at"}
	inventory := accessGrantOperationOpenAPIInventory()
	variants := make([]map[string]any, 0, len(inventory))
	for _, variant := range inventory {
		if !include(variant) {
			continue
		}
		properties := make(map[string]any, len(common)+2)
		for key, value := range common {
			properties[key] = value
		}
		properties["operation"] = map[string]any{"type": "string", "enum": []string{variant.operation}}
		properties["constraints"] = variant.constraints
		variants = append(variants, closedObjectSchema(properties, required))
	}
	return map[string]any{"oneOf": variants}
}

func accessGrantIssueOpenAPIResponses() map[string]any {
	return map[string]any{
		"201": map[string]any{"description": "New access grant issued", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/AccessGrantIssueV1"}}}},
		"401": map[string]any{"description": "Source or current grant credential rejected"}, "403": map[string]any{"description": "Authority denied"},
		"409": map[string]any{"description": "Authority or source facts changed"}, "422": map[string]any{"description": "Closed request or locator invalid"},
		"502": map[string]any{"description": "Source response rejected"}, "503": map[string]any{"description": "Grant authority unavailable"},
	}
}

func accessGrantReceiptOpenAPIResponses() map[string]any {
	return map[string]any{
		"200": map[string]any{"description": "Secret-free access grant receipt", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/AccessGrantV1"}}}},
		"401": map[string]any{"description": "Grant credential rejected"}, "403": map[string]any{"description": "Grant denied"},
		"409": map[string]any{"description": "Grant authority changed"}, "503": map[string]any{"description": "Grant authority unavailable"},
	}
}

func accessGrantTransportSessionReceiptOpenAPISchema() map[string]any {
	uuid := func() map[string]any { return map[string]any{"type": "string", "format": "uuid"} }
	positive := func() map[string]any {
		return map[string]any{"type": "integer", "minimum": 1, "maximum": operationconstraints.MaxJSONSafePositiveInteger}
	}
	inventory := accessGrantOperationOpenAPIInventory()
	operationVariants := make([]map[string]any, 0, len(inventory))
	for _, variant := range inventory {
		if !variant.transport {
			continue
		}
		properties := map[string]any{"name": map[string]any{"type": "string", "enum": []string{variant.operation}}}
		required := []string{"name"}
		if len(variant.constraints["properties"].(map[string]any)) > 0 {
			properties["constraints"] = variant.constraints
			required = append(required, "constraints")
		}
		operationVariants = append(operationVariants, closedObjectSchema(properties, required))
	}

	principal := closedObjectSchema(map[string]any{"id": positive(), "login": map[string]any{"type": "string", "minLength": 1}}, []string{"id", "login"})
	actor := closedObjectSchema(map[string]any{
		"provider": map[string]any{"type": "string", "enum": []string{"multica"}}, "workspace_id": uuid(), "workspace": map[string]any{"type": "string", "minLength": 1},
		"agent_id": uuid(), "agent_name": map[string]any{"type": "string", "minLength": 1}, "task_id": uuid(), "run_id": uuid(),
		"issue_id": uuid(), "issue_key": map[string]any{"type": "string", "pattern": `^[A-Z][A-Z0-9]*-[1-9][0-9]*$`},
	}, []string{"provider", "workspace_id", "workspace", "agent_id", "agent_name", "task_id", "run_id"})
	workload := closedObjectSchema(map[string]any{
		"schema": map[string]any{"type": "string", "enum": []string{"workload.context.v1"}}, "issuer_instance_id": map[string]any{"type": "string", "minLength": 1},
		"subject": map[string]any{"type": "string", "pattern": `^urn:multica:agent:[0-9a-f-]{36}$`}, "correlation_id": uuid(),
		"workspace_id": uuid(), "agent_id": uuid(), "squad_id": uuid(), "issue_id": uuid(),
		"issue_key": map[string]any{"type": "string", "pattern": `^[A-Z][A-Z0-9]*-[1-9][0-9]*$`}, "task_id": uuid(), "run_id": uuid(),
		"trigger_id": uuid(), "runtime_id": uuid(), "role": map[string]any{"type": "string", "minLength": 1},
	}, []string{"schema", "issuer_instance_id", "subject", "correlation_id", "workspace_id", "agent_id", "task_id", "run_id"})
	target := closedObjectSchema(map[string]any{
		"instance": map[string]any{"type": "string", "minLength": 1}, "repository": map[string]any{"type": "string", "pattern": `^[^/\s]+/[^/\s]+$`},
	}, []string{"instance", "repository"})
	resource := closedObjectSchema(map[string]any{
		"service": map[string]any{"type": "string", "enum": []string{"ags"}}, "repository": map[string]any{"type": "string", "pattern": `^[^/\s]+/[^/\s]+$`},
	}, []string{"service", "repository"})
	basis := closedObjectSchema(map[string]any{
		"trust_revision":            map[string]any{"type": "string", "pattern": `^sha256:[a-f0-9]{64}$`},
		"native_grant_revision":     map[string]any{"type": "string", "pattern": `^grant-rev:[a-f0-9]{64}$`},
		"resource_policy_revision":  map[string]any{"type": "string", "minLength": 1},
		"policy_class_revision":     map[string]any{"type": "string", "minLength": 1},
		"requested_operation_scope": map[string]any{"type": "string", "minLength": 1},
	}, []string{"trust_revision", "native_grant_revision", "resource_policy_revision", "policy_class_revision", "requested_operation_scope"})
	timestamp := map[string]any{"type": "string", "format": "date-time"}
	return closedObjectSchema(map[string]any{
		"contract_revision": map[string]any{"type": "string", "enum": []string{"2026-07-24.team-authority-v4"}}, "id": uuid(), "principal": principal,
		"team_identity_id": map[string]any{"type": "string", "minLength": 1}, "team_binding_revision": map[string]any{"type": "string", "minLength": 1}, "policy_class": map[string]any{"type": "string", "minLength": 1},
		"membership_epoch": positive(), "credential_mode": map[string]any{"type": "string", "enum": []string{"access_grant_transport"}},
		"actor": actor, "workload_context": workload, "trace_quality": map[string]any{"type": "string", "enum": []string{"complete", "trace_degraded"}},
		"missing_fields": map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"squad_id", "issue_id", "issue_key", "trigger_id", "runtime_id"}}},
		"target":         target, "resource": resource, "operation": map[string]any{"oneOf": operationVariants},
		"effective_ttl":       map[string]any{"type": "string", "pattern": `^[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h)(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h))*$`},
		"authorization_basis": basis, "granted_capabilities": map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1}},
		"policy_version": map[string]any{"type": "string", "minLength": 1}, "state": map[string]any{"type": "string", "enum": []string{"active", "revoked", "expired"}},
		"created_at": timestamp, "expires_at": timestamp, "last_used_at": timestamp, "revoked_at": timestamp,
		"revoked_by_user_id": positive(), "revocation_reason": map[string]any{"type": "string", "maxLength": 255},
	}, []string{"contract_revision", "id", "principal", "team_identity_id", "team_binding_revision", "policy_class", "membership_epoch", "credential_mode", "actor", "workload_context", "trace_quality", "missing_fields", "target", "resource", "operation", "effective_ttl", "authorization_basis", "granted_capabilities", "policy_version", "state", "created_at", "expires_at"})
}

func accessGrantTransportSessionOpenAPIResponses() map[string]any {
	schema := closedObjectSchema(map[string]any{
		"schema":        map[string]any{"type": "string", "enum": []string{"ags.access-grant-transport-session.v1"}},
		"session_token": map[string]any{"type": "string", "readOnly": true, "pattern": `^ags_sess_[A-Za-z0-9_-]+$`, "description": "Returned once to the internal CLI adapter and never included in public command output."},
		"session":       map[string]any{"$ref": "#/components/schemas/AccessGrantTransportSessionReceiptV1"},
		"invocation":    map[string]any{"$ref": "#/components/schemas/AccessGrantTransportInvocationV1"},
	}, []string{"schema", "session_token", "session", "invocation"})
	return map[string]any{
		"201": map[string]any{"description": "Internal transport Session issued from the Access Grant", "content": map[string]any{"application/json": map[string]any{"schema": schema}}},
		"401": map[string]any{"description": "Grant credential rejected"}, "403": map[string]any{"description": "Operation denied"},
		"409": map[string]any{"description": "Grant authority changed"}, "422": map[string]any{"description": "Closed operation request invalid"},
		"503": map[string]any{"description": "Grant transport authority unavailable"},
	}
}

func accessGrantInvocationOpenAPIResponses() map[string]any {
	return map[string]any{
		"200": map[string]any{"description": "Secret-free exact invocation/effect receipt", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/AccessGrantInvocationV1"}}}},
		"401": map[string]any{"description": "Grant credential rejected"}, "403": map[string]any{"description": "Operation denied"},
		"409": map[string]any{"description": "Authority or exact operation facts changed"}, "422": map[string]any{"description": "Closed operation request invalid"},
		"503": map[string]any{"description": "Grant or provider authority unavailable"},
	}
}

func accessGrantPRMergeOpenAPIResponses() map[string]any {
	return map[string]any{
		"200": map[string]any{"description": "Exact provider-effect receipt, including terminal pre-dispatch conflict receipts", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/AccessGrantInvocationV1"}}}},
		"401": map[string]any{"description": "Grant credential rejected"}, "403": map[string]any{"description": "Operation denied or locator belongs to another grant"},
		"422": map[string]any{"description": "Closed request or invocation locator collision invalid"},
		"503": map[string]any{"description": "Grant or provider authority unavailable"},
	}
}

func delegatedSessionLifecycleOpenAPISchema() map[string]any {
	uuid := func() map[string]any { return map[string]any{"type": "string", "format": "uuid"} }
	timestamp := func() map[string]any { return map[string]any{"type": "string", "format": "date-time"} }
	principal := closedObjectSchema(map[string]any{
		"id": map[string]any{"type": "integer", "minimum": 1, "maximum": operationconstraints.MaxJSONSafePositiveInteger}, "login": map[string]any{"type": "string", "minLength": 1},
	}, []string{"id", "login"})
	target := closedObjectSchema(map[string]any{
		"instance": map[string]any{"type": "string", "minLength": 1}, "repository": map[string]any{"type": "string", "minLength": 3},
	}, []string{"instance", "repository"})
	resource := closedObjectSchema(map[string]any{
		"service": map[string]any{"type": "string", "enum": []string{"ags"}}, "repository": map[string]any{"type": "string", "minLength": 3},
	}, []string{"service", "repository"})
	operationSchema := closedObjectSchema(map[string]any{
		"name": map[string]any{"type": "string", "enum": []string{"repo.read", "git.read", "git.push", "pr.create", "pr.read", "pr.rebase", "pr.merge", "ci.read", "review.read"}},
	}, []string{"name"})
	basis := closedObjectSchema(map[string]any{
		"trust_revision":            map[string]any{"type": "string", "minLength": 1},
		"native_grant_revision":     map[string]any{"type": "string", "pattern": `^grant-rev:[a-f0-9]{64}$`},
		"resource_policy_revision":  map[string]any{"type": "string", "minLength": 1},
		"policy_class_revision":     map[string]any{"type": "string", "minLength": 1},
		"requested_operation_scope": map[string]any{"type": "string", "minLength": 1},
	}, []string{"trust_revision", "native_grant_revision", "resource_policy_revision", "policy_class_revision", "requested_operation_scope"})
	authority := closedObjectSchema(map[string]any{
		"contract_revision": map[string]any{"type": "string", "enum": []string{"2026-07-24.team-authority-v4"}},
		"principal":         principal, "team_identity_id": uuid(), "team_binding_revision": map[string]any{"type": "string", "minLength": 1},
		"policy_class": map[string]any{"type": "string", "minLength": 1}, "membership_epoch": map[string]any{"type": "integer", "minimum": 1, "maximum": operationconstraints.MaxJSONSafePositiveInteger},
		"issuer_instance_id": map[string]any{"type": "string", "minLength": 1}, "target": target, "resource": resource,
		"operation": operationSchema, "authorization_basis": basis,
	}, []string{"contract_revision", "principal", "team_identity_id", "team_binding_revision", "policy_class", "membership_epoch", "issuer_instance_id", "target", "resource", "operation", "authorization_basis"})
	provenance := closedObjectSchema(map[string]any{
		"schema": map[string]any{"type": "string", "enum": []string{"workload.context.v1"}}, "workspace_id": uuid(), "agent_id": uuid(),
		"squad_id": uuid(), "issue_id": uuid(), "issue_key": map[string]any{"type": "string", "pattern": `^[A-Z][A-Z0-9]*-[1-9][0-9]*$`},
		"task_id": uuid(), "run_id": uuid(), "correlation_id": uuid(), "trigger_id": uuid(), "runtime_id": uuid(),
	}, []string{"schema", "workspace_id", "agent_id", "task_id", "run_id", "correlation_id"})
	return closedObjectSchema(map[string]any{
		"schema": map[string]any{"type": "string", "enum": []string{"ags.delegated-session-lifecycle.v1"}}, "session_id": uuid(),
		"state": map[string]any{"type": "string", "enum": []string{"active", "revoked", "expired"}}, "created_at": timestamp(), "expires_at": timestamp(),
		"revoked_at": timestamp(), "expiry_audited_at": timestamp(), "authority": authority, "provenance": provenance,
		"claim_limit": map[string]any{"type": "string", "enum": []string{"This readback proves the persisted lifecycle state and expiry-audited marker for one delegated Session; it does not expose authentication material or prove external provider effects."}},
	}, []string{"schema", "session_id", "state", "created_at", "expires_at", "authority", "provenance", "claim_limit"})
}

func outboundDeliveryOpenAPISchema() map[string]any {
	return closedObjectSchema(map[string]any{
		"id":              map[string]any{"type": "integer", "minimum": 1},
		"idempotency_key": map[string]any{"type": "string", "minLength": 1},
		"event_type":      map[string]any{"type": "string", "enum": []string{"pull_request_merged", "projection_drift", "multica_incident", "multica_external_pr_terminal"}},
		"target_name":     map[string]any{"type": "string", "minLength": 1},
		"target_type":     map[string]any{"type": "string", "enum": []string{"feishu_webhook", "multica_external_pr"}},
		"subject_type":    map[string]any{"type": "string"}, "subject_key": map[string]any{"type": "string"},
		"payload_version": map[string]any{"type": "string"},
		"status":          map[string]any{"type": "string", "enum": []string{"pending", "delivering", "delivered", "retry_wait", "dead_letter"}},
		"attempt_count":   map[string]any{"type": "integer", "minimum": 0}, "max_attempts": map[string]any{"type": "integer", "minimum": 1},
		"lease_owner": map[string]any{"type": "string"}, "lease_expires_at": map[string]any{"type": "string", "format": "date-time", "nullable": true},
		"next_attempt_at":  map[string]any{"type": "string", "format": "date-time", "nullable": true},
		"last_attempt_at":  map[string]any{"type": "string", "format": "date-time", "nullable": true},
		"delivered_at":     map[string]any{"type": "string", "format": "date-time", "nullable": true},
		"last_http_status": map[string]any{"type": "integer"}, "last_error_code": map[string]any{"type": "string"}, "last_error": map[string]any{"type": "string"},
		"repo_full_name": map[string]any{"type": "string"}, "pr_number": map[string]any{"type": "integer"},
		"external_repo": map[string]any{"type": "string"}, "external_number": map[string]any{"type": "integer"}, "merge_sha": map[string]any{"type": "string"},
		"created_at": map[string]any{"type": "string", "format": "date-time"}, "updated_at": map[string]any{"type": "string", "format": "date-time"},
	}, []string{"id", "idempotency_key", "event_type", "target_name", "target_type", "status", "attempt_count", "max_attempts", "created_at", "updated_at"})
}

func multicaExternalPRLinkRequestOpenAPISchema() map[string]any {
	schema := closedObjectSchema(map[string]any{
		"provider": map[string]any{"type": "string", "enum": []string{"ags"}}, "issue_id": map[string]any{"type": "string", "minLength": 1},
		"workspace_id": map[string]any{"type": "string", "minLength": 1}, "workspace": map[string]any{"type": "string", "minLength": 1},
		"issue_key":       map[string]any{"type": "string", "minLength": 1},
		"external_repo":   map[string]any{"type": "string", "minLength": 1},
		"external_number": map[string]any{"type": "integer", "minimum": 1}, "external_url": map[string]any{"type": "string", "format": "uri", "pattern": `^https?://`},
		"merge_provider": map[string]any{"type": "string", "enum": []string{"forgejo"}}, "merge_repo": map[string]any{"type": "string", "minLength": 1},
		"merge_number": map[string]any{"type": "integer", "minimum": 1}, "merge_url": map[string]any{"type": "string", "format": "uri", "pattern": `^https?://`},
		"merged_sha":              map[string]any{"type": "string", "pattern": `^(|[0-9a-f]{40})$`},
		"target_instance":         map[string]any{"type": "string", "pattern": `^[a-z0-9][a-z0-9.-]{0,63}$`},
		"canonical_repository_id": map[string]any{"type": "string", "pattern": `^sha256:[0-9a-f]{64}$`}, "canonical_repository": map[string]any{"type": "string", "minLength": 1},
		"provider_binding_id": map[string]any{"type": "string", "pattern": `^sha256:[0-9a-f]{64}$`}, "provider_binding_revision": map[string]any{"type": "string", "pattern": `^sha256:[0-9a-f]{64}$`}, "provider_repository": map[string]any{"type": "string", "minLength": 1},
		"expected_head_sha": map[string]any{"type": "string", "pattern": `^[0-9a-f]{40}$`}, "expected_base_sha": map[string]any{"type": "string", "pattern": `^[0-9a-f]{40}$`}, "base_ref": map[string]any{"type": "string", "minLength": 1},
		"delegated_merge_method": map[string]any{"type": "string", "enum": []string{"merge", "rebase", "rebase-merge", "squash", "fast-forward-only"}}, "projection_facts_revision": map[string]any{"type": "string", "pattern": `^sha256:[0-9a-f]{64}$`},
		"completion_intent": map[string]any{"type": "boolean"}, "link_confidence": map[string]any{"type": "string", "enum": []string{"authoritative"}},
		"state": map[string]any{"type": "string", "enum": []string{"closed", "merged"}}, "idempotency_key": map[string]any{"type": "string", "minLength": 1},
	}, []string{"provider", "issue_id", "workspace_id", "workspace", "issue_key", "external_repo", "external_number", "external_url", "merge_provider", "merge_repo", "merge_number", "merge_url", "merged_sha", "completion_intent", "link_confidence", "state", "idempotency_key"})
	schema["description"] = "Canonical closed ExternalPRLinkRequest wire. Runtime invariants: state=merged requires completion_intent=true and a lowercase 40-hex merged_sha; state=closed requires completion_intent=false and an empty merged_sha. Projection binding fields target_instance, canonical_repository_id, canonical_repository, provider_binding_id, provider_binding_revision, provider_repository, expected_head_sha, expected_base_sha, base_ref, delegated_merge_method, and projection_facts_revision must be supplied as one complete group or omitted."
	return schema
}

func closedObjectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func delegatedSessionLifecycleOpenAPIResponses() map[string]any {
	return map[string]any{
		"200": map[string]any{"description": "Delegated Session lifecycle returned", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/DelegatedSessionLifecycleV1"}}}},
		"401": map[string]any{"description": "Authentication required"},
		"403": map[string]any{"description": "Current durable principal is not an active site administrator"},
		"404": map[string]any{"description": "Delegated Session not found"},
		"422": map[string]any{"description": "Session is not a canonical team-v4 lifecycle row"},
	}
}

func response(code int, description string) map[string]any {
	return map[string]any{
		strconv.Itoa(code): map[string]any{
			"description": description,
		},
	}
}
