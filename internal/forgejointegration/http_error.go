package forgejointegration

import (
	"fmt"
	"net/http"

	"github.com/ngaut/agent-git-service/internal/gittransport"
)

// ProviderHTTPError intentionally has no URL, body, headers or arbitrary cause.
// Method and RouteKind are selected from this module's fixed operation catalog.
type ProviderHTTPError struct {
	Status            int
	Method, RouteKind string
}

func (e *ProviderHTTPError) Error() string {
	return fmt.Sprintf("forgejo %s %s returned status %d (response withheld)", e.Method, e.RouteKind, e.Status)
}
func providerHTTPError(operation string, status int) error {
	method, route := http.MethodGet, "unknown"
	switch operation {
	case "get repository", "lookup repository", "verify repository", "lookup repository authority":
		route = "repository"
	case "lookup branch protection":
		route = "branch_protection"
	case "list authority webhooks":
		route = "webhook"
	case "get default branch":
		route = "branch"
	case "create repository":
		method, route = http.MethodPost, "repository"
	case "lookup organization":
		route = "organization"
	case "lookup authenticated user":
		route = "current_user"
	case "list pull requests", "get pull request":
		route = "pull_request"
	case "create pull request":
		method, route = http.MethodPost, "pull_request"
	case "update pull request", "edit pull request base", "update pull request state":
		method, route = http.MethodPatch, "pull_request"
	case "merge pull request":
		method, route = http.MethodPost, "pull_merge"
	case "list issue labels", "list labels", "list authority labels":
		route = "issue_label"
	case "add issue labels":
		method, route = http.MethodPost, "issue_label"
	case "remove issue label":
		method, route = http.MethodDelete, "issue_label"
	case "lookup integration bot permission", "lookup collaborator permission":
		route = "collaborator_permission"
	case "create issue comment":
		method, route = http.MethodPost, "issue_comment"
	case "list issue comments":
		route = "issue_comment"
	case "list workflow runs":
		route = "workflow_run"
	case "get workflow run logs":
		route = "workflow_log"
	}
	return &ProviderHTTPError{Status: status, Method: method, RouteKind: route}
}
func httpFailure(e *ProviderHTTPError) gittransport.Failure {
	category, reason := "unknown", "http_status"
	switch e.Status {
	case 401:
		category, reason = "auth_failed", "http_401"
	case 403:
		category, reason = "http_forbidden", "http_403"
	case 404:
		category, reason = "target_missing_or_inaccessible", "http_404"
	case 409:
		category, reason = "cas_conflict", "http_conflict"
	}
	status := e.Status
	return gittransport.Failure{Category: category, ReasonCode: reason, EvidenceSource: "http_status", HTTPStatus: &status, RetryAdvice: "inspect_before_new_plan"}
}

func authorityHTTPError(method string, status int) error {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
	default:
		method = "unknown"
	}
	return &ProviderHTTPError{Status: status, Method: method, RouteKind: "authority"}
}
