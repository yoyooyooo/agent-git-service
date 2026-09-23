package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestTokenAuthRejectsLegacyDelegatedSessionCredentialMode(t *testing.T) {
	svc := setupTestService(t)
	if err := svc.DB.AutoMigrate(&db.Repository{}, &db.DelegatedAgentSession{}); err != nil {
		t.Fatal(err)
	}
	raw := "ags_sess_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	digest := sha256.Sum256([]byte(raw))
	principal := db.User{Login: "automation-principal", Name: "Automation Principal", Type: db.TypeUser, Status: db.UserStatusActive, UserKind: db.UserKindAgent}
	if err := svc.DB.Create(&principal).Error; err != nil {
		t.Fatal(err)
	}
	repo := db.Repository{Name: "project-kit", FullName: "operator/project-kit", OwnerID: principal.ID, Private: true, DefaultBranch: "main"}
	if err := svc.DB.Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
	session := db.DelegatedAgentSession{
		ID: "11111111-1111-1111-1111-111111111111", CredentialHash: hex.EncodeToString(digest[:]), CredentialPrefix: "sha256:fixture",
		PrincipalUserID: principal.ID, Issuer: "multica", AssertionVersion: 1, AssertionPurpose: "ags_session_exchange", AssertionJTI: "jti-auth",
		AssertionAudience: "urn:ags:workload-session-exchange:v1", IssuerWorkspaceID: "workspace-1", ExternalAgentID: "agent-1",
		ExternalAgentName: "example-implementer-a", ExternalTaskID: "task-1", TargetInstance: "primary-a", RepositoryID: repo.ID,
		GrantedCapabilities: []string{"repo:read"}, PolicyVersion: "2026-07-14.1", PolicySnapshotHash: "snapshot",
		CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := svc.DB.Create(&session).Error; err != nil {
		t.Fatal(err)
	}

	handler := TokenAuth(svc)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v3/user", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("legacy credential status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDelegatedSessionSurfaceIsAllowlisted(t *testing.T) {
	session := db.DelegatedAgentSession{ID: "session-1", OperationName: "pr.create", GrantedCapabilities: []string{"repo:read", "pr:create"}}
	handler := EnforceDelegatedSessionSurface(nil)(okHandler)
	tests := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/v3/user", http.StatusOK},
		{http.MethodGet, "/api/v3/repos/operator/project-kit", http.StatusOK},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/pulls/27", http.StatusOK},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/git/ref/heads/main", http.StatusOK},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/git/ref/heads/release/v2", http.StatusOK},
		{http.MethodPost, "/api/v3/repos/operator/project-kit/pulls", http.StatusOK},
		{http.MethodGet, "/api/v3/agent-sessions/current", http.StatusForbidden},
		{http.MethodPost, "/api/v3/agent-sessions/current/revoke", http.StatusForbidden},
		{http.MethodHead, "/api/v3/repos/operator/project-kit", http.StatusForbidden},
		{http.MethodGet, "/api/v3/user/tokens", http.StatusForbidden},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/issues", http.StatusForbidden},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/pulls", http.StatusForbidden},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/pulls/0", http.StatusForbidden},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/pulls/1/files", http.StatusForbidden},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/branches/main", http.StatusForbidden},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/git/refs/heads/main", http.StatusForbidden},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/git/ref/heads/../pulls/1", http.StatusForbidden},
		{http.MethodGet, "/api/v3/repos/operator/project-kit/git/ref/heads/main.lock", http.StatusForbidden},
		{http.MethodPatch, "/api/v3/repos/operator/project-kit/pulls/1", http.StatusForbidden},
		{http.MethodPut, "/api/v3/repos/operator/project-kit/pulls/1/merge", http.StatusForbidden},
		{http.MethodGet, "/api/v3/agent-sessions/session-1/lifecycle", http.StatusForbidden},
		{http.MethodPost, "/api/v3/agent-sessions/session-1/revoke", http.StatusForbidden},
		{http.MethodPost, "/api/v3/user/repos", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(service.ContextWithDelegatedSession(req.Context(), session))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDelegatedSessionRepoReadBranchProtectionSurface(t *testing.T) {
	handler := EnforceDelegatedSessionSurface(nil)(okHandler)
	tests := []struct {
		name      string
		operation string
		method    string
		path      string
		want      int
	}{
		{name: "exact named branch", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusOK},
		{name: "exact slash branch", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/release/v2/protection", want: http.StatusOK},
		{name: "exact slash branch containing protection", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/release/protection/v2/protection", want: http.StatusOK},
		{name: "protection subresource", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection/required_status_checks", want: http.StatusForbidden},
		{name: "trailing slash variant", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection/", want: http.StatusForbidden},
		{name: "branch list", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches", want: http.StatusForbidden},
		{name: "wildcard branch", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/*/protection", want: http.StatusForbidden},
		{name: "missing branch", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/protection", want: http.StatusForbidden},
		{name: "unsafe dot segment", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/../protection", want: http.StatusForbidden},
		{name: "unsafe lock suffix", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main.lock/protection", want: http.StatusForbidden},
		{name: "provider direct route", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection/provider", want: http.StatusForbidden},
		{name: "POST method", operation: "repo.read", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "PUT method", operation: "repo.read", method: http.MethodPut, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "PATCH method", operation: "repo.read", method: http.MethodPatch, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "DELETE method", operation: "repo.read", method: http.MethodDelete, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "HEAD method", operation: "repo.read", method: http.MethodHead, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "git read operation", operation: "git.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "git push operation", operation: "git.push", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "PR create operation", operation: "pr.create", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "PR read operation", operation: "pr.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "PR rebase operation", operation: "pr.rebase", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "PR merge operation", operation: "pr.merge", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "CI read operation", operation: "ci.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "review read operation", operation: "review.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
		{name: "unknown operation", operation: "repo.admin", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/branches/main/protection", want: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session := db.DelegatedAgentSession{ID: "session-1", OperationName: tc.operation, GrantedCapabilities: []string{"repo:read"}}
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(service.ContextWithDelegatedSession(req.Context(), session))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDelegatedSessionPRReadGraphQLSurfaceIsBound(t *testing.T) {
	handler := EnforceDelegatedSessionSurface(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Contains(body, []byte("PullRequestByNumber")) {
			t.Errorf("forwarded GraphQL body=%q err=%v", body, err)
			http.Error(w, "missing GraphQL body", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	query := `query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) {
		repository(owner: $owner, name: $repo) {
			pullRequest(number: $pr_number) { number title state }
		}
	}`
	session := db.DelegatedAgentSession{
		ID:                   "session-1",
		OperationName:        "pr.read",
		Repository:           db.Repository{ID: 41, FullName: "operator/project-kit"},
		OperationConstraints: map[string]string{"pull_request_number": "27"},
		GrantedCapabilities:  []string{"repo:read"},
	}
	for _, tc := range []struct {
		name             string
		path             string
		operationName    string
		sessionOperation string
		variables        string
		query            string
		rawBody          string
		want             int
	}{
		{name: "bound pull request query without operationName field", path: "/api/graphql", variables: `{"owner":"operator","repo":"project-kit","pr_number":27}`, query: query, want: http.StatusOK},
		{name: "compatibility GraphQL path", path: "/graphql", operationName: "PullRequestByNumber", variables: `{"owner":"operator","repo":"project-kit","pr_number":27}`, query: query, want: http.StatusOK},
		{name: "wrong repository", path: "/api/graphql", operationName: "PullRequestByNumber", variables: `{"owner":"operator","repo":"other","pr_number":27}`, query: query, want: http.StatusForbidden},
		{name: "wrong pull request", path: "/api/graphql", operationName: "PullRequestByNumber", variables: `{"owner":"operator","repo":"project-kit","pr_number":28}`, query: query, want: http.StatusForbidden},
		{name: "wrong operation name", path: "/api/graphql", operationName: "RepositoryQuery", variables: `{"owner":"operator","repo":"project-kit","pr_number":27}`, query: query, want: http.StatusForbidden},
		{name: "noncanonical variable type", path: "/api/graphql", operationName: "PullRequestByNumber", variables: `{"owner":"operator","repo":"project-kit","pr_number":27}`, query: strings.Replace(query, "$pr_number: Int!", "$pr_number: Float!", 1), want: http.StatusForbidden},
		{name: "wrong Session operation", path: "/api/graphql", operationName: "PullRequestByNumber", sessionOperation: "repo.read", variables: `{"owner":"operator","repo":"project-kit","pr_number":27}`, query: query, want: http.StatusForbidden},
		{name: "extra top-level resource", path: "/api/graphql", operationName: "PullRequestByNumber", variables: `{"owner":"operator","repo":"project-kit","pr_number":27}`, query: `query PullRequestByNumber($owner: String!, $repo: String!, $pr_number: Int!) { repository(owner: $owner, name: $repo) { pullRequest(number: $pr_number) { number } } viewer { login } }`, want: http.StatusForbidden},
		{name: "unknown JSON field", path: "/api/graphql", rawBody: `{"query":"query PullRequestByNumber { viewer { login } }","variables":{},"extensions":{}}`, want: http.StatusForbidden},
		{name: "malformed JSON", path: "/api/graphql", rawBody: `{"query":`, want: http.StatusForbidden},
		{name: "oversized body", path: "/api/graphql", rawBody: strings.Repeat(" ", delegatedGraphQLBodyLimit+1), want: http.StatusForbidden},
		{name: "mutation denied", path: "/api/graphql", operationName: "PullRequestByNumber", variables: `{"owner":"operator","repo":"project-kit","pr_number":27}`, query: `mutation PullRequestByNumber { deleteProjectV2(input: {}) { clientMutationId } }`, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operationName := ""
			if tc.operationName != "" {
				operationName = `"operationName":` + strconv.Quote(tc.operationName) + `,`
			}
			body := []byte(tc.rawBody)
			if tc.rawBody == "" {
				body = []byte(`{` + operationName + `"query":` + strconv.Quote(tc.query) + `,"variables":` + tc.variables + `}`)
			}
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			requestSession := session
			if tc.sessionOperation != "" {
				requestSession.OperationName = tc.sessionOperation
			}
			req = req.WithContext(service.ContextWithDelegatedSession(req.Context(), requestSession))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDelegatedSessionPRListGraphQLSurfaceIsRepositoryBound(t *testing.T) {
	handler := EnforceDelegatedSessionSurface(nil)(okHandler)
	query := `fragment pr on PullRequest { number title state url headRefName headRepositoryOwner { id login ... on User { name } } isCrossRepository isDraft createdAt }
	query PullRequestList(
		$owner: String!,
		$repo: String!,
		$limit: Int!,
		$endCursor: String,
		$baseBranch: String,
		$headBranch: String,
		$state: [PullRequestState!] = OPEN
	) {
		repository(owner: $owner, name: $repo) {
			pullRequests(
				states: $state,
				baseRefName: $baseBranch,
				headRefName: $headBranch,
				first: $limit,
				after: $endCursor,
				orderBy: {field: CREATED_AT, direction: DESC}
			) {
				totalCount
				nodes { ...pr }
				pageInfo { hasNextPage endCursor }
			}
		}
	}`
	session := db.DelegatedAgentSession{
		ID:            "session-1",
		OperationName: "pr.read",
		Repository:    db.Repository{ID: 41, FullName: "operator/project-kit"},
	}
	for _, tc := range []struct {
		name             string
		query            string
		variables        string
		operationName    string
		sessionOperation string
		want             int
	}{
		{name: "current gh list query", query: query, operationName: "PullRequestList", variables: `{"limit":30,"owner":"operator","repo":"project-kit","state":["OPEN"]}`, want: http.StatusOK},
		{name: "optional filters and pagination", query: query, variables: `{"limit":50,"owner":"operator","repo":"project-kit","state":["CLOSED","MERGED"],"baseBranch":"main","headBranch":"agent/read","endCursor":"cursor"}`, want: http.StatusOK},
		{name: "wrong repository", query: query, variables: `{"limit":30,"owner":"operator","repo":"other","state":["OPEN"]}`, want: http.StatusForbidden},
		{name: "unexpected variable", query: query, variables: `{"limit":30,"owner":"operator","repo":"project-kit","state":["OPEN"],"admin":true}`, want: http.StatusForbidden},
		{name: "wrong operation name field", query: query, operationName: "PullRequestByNumber", variables: `{"limit":30,"owner":"operator","repo":"project-kit","state":["OPEN"]}`, want: http.StatusForbidden},
		{name: "wrong Session operation", query: query, sessionOperation: "repo.read", variables: `{"limit":30,"owner":"operator","repo":"project-kit","state":["OPEN"]}`, want: http.StatusForbidden},
		{name: "extra top-level resource", query: strings.Replace(query, "\n\t}", "\n\tviewer { login }\n\t}", 1), variables: `{"limit":30,"owner":"operator","repo":"project-kit","state":["OPEN"]}`, want: http.StatusForbidden},
		{name: "changed sort order", query: strings.Replace(query, "direction: DESC", "direction: ASC", 1), variables: `{"limit":30,"owner":"operator","repo":"project-kit","state":["OPEN"]}`, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"query":` + strconv.Quote(tc.query) + `,"operationName":` + strconv.Quote(tc.operationName) + `,"variables":` + tc.variables + `}`)
			req := httptest.NewRequest(http.MethodPost, "/api/graphql", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			requestSession := session
			if tc.sessionOperation != "" {
				requestSession.OperationName = tc.sessionOperation
			}
			req = req.WithContext(service.ContextWithDelegatedSession(req.Context(), requestSession))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDelegatedSessionRESTSurfaceIsOperationScoped(t *testing.T) {
	handler := EnforceDelegatedSessionSurface(nil)(okHandler)
	for _, tc := range []struct {
		name      string
		operation string
		method    string
		path      string
		want      int
	}{
		{name: "repo read permits exact repository", operation: "repo.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit", want: http.StatusOK},
		{name: "repo read denies PR create", operation: "repo.read", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls", want: http.StatusForbidden},
		{name: "git read denies REST repository reads", operation: "git.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit", want: http.StatusForbidden},
		{name: "git push denies REST repository reads", operation: "git.push", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit", want: http.StatusForbidden},
		{name: "PR read permits numbered PR", operation: "pr.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27", want: http.StatusOK},
		{name: "PR read permits provider projection", operation: "pr.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27/provider/projection", want: http.StatusOK},
		{name: "PR read permits named head", operation: "pr.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/git/ref/heads/agent/read", want: http.StatusOK},
		{name: "PR read denies provider CI", operation: "pr.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27/provider/ci/runs", want: http.StatusForbidden},
		{name: "PR read permits PR list", operation: "pr.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls", want: http.StatusOK},
		{name: "review read permits exact verification read", operation: "review.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27", want: http.StatusOK},
		{name: "review read denies unrelated named head", operation: "review.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/git/ref/heads/agent/read", want: http.StatusForbidden},
		{name: "CI read permits exact verification read", operation: "ci.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27", want: http.StatusOK},
		{name: "CI read permits provider runs", operation: "ci.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27/provider/ci/runs", want: http.StatusOK},
		{name: "CI read permits provider run logs", operation: "ci.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27/provider/ci/runs/1017/logs", want: http.StatusOK},
		{name: "CI read denies provider projection", operation: "ci.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27/provider/projection", want: http.StatusForbidden},
		{name: "CI read denies unrelated named head", operation: "ci.read", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/git/ref/heads/agent/read", want: http.StatusForbidden},
		{name: "PR create permits exact verification read", operation: "pr.create", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27", want: http.StatusOK},
		{name: "PR comment permits issue comment POST", operation: "pr.comment", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/issues/27/comments", want: http.StatusOK},
		{name: "PR comment permits pull comment POST", operation: "pr.comment", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls/27/comments", want: http.StatusOK},
		{name: "PR comment denies PR create", operation: "pr.comment", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls", want: http.StatusForbidden},
		{name: "PR edit permits PATCH", operation: "pr.edit", method: http.MethodPatch, path: "/api/v3/repos/operator/project-kit/pulls/27", want: http.StatusOK},
		{name: "PR close permits PATCH", operation: "pr.close", method: http.MethodPatch, path: "/api/v3/repos/operator/project-kit/pulls/27", want: http.StatusOK},
		{name: "PR reopen permits PATCH", operation: "pr.reopen", method: http.MethodPatch, path: "/api/v3/repos/operator/project-kit/pulls/27", want: http.StatusOK},
		{name: "PR edit denies comment POST", operation: "pr.edit", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/issues/27/comments", want: http.StatusForbidden},
		{name: "review write permits review POST", operation: "review.write", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls/27/reviews", want: http.StatusOK},
		{name: "review write denies merge", operation: "review.write", method: http.MethodPut, path: "/api/v3/repos/operator/project-kit/pulls/27/merge", want: http.StatusForbidden},
		{name: "PR rebase permits exact action POST", operation: "pr.rebase", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls/27/actions/pr.rebase", want: http.StatusOK},
		{name: "PR rebase permits exact intent GET", operation: "pr.rebase", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27/actions/pr.rebase/intent-1", want: http.StatusOK},
		{name: "PR rebase permits Session Boundary Receipt GET", operation: "pr.rebase", method: http.MethodGet, path: "/api/v3/agent-sessions/current/authority-boundary-receipts/abr_123", want: http.StatusOK},
		{name: "repo read denies Session Boundary Receipt GET", operation: "repo.read", method: http.MethodGet, path: "/api/v3/agent-sessions/current/authority-boundary-receipts/abr_123", want: http.StatusForbidden},
		{name: "PR rebase permits verification read", operation: "pr.rebase", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27", want: http.StatusOK},
		{name: "PR rebase denies merge", operation: "pr.rebase", method: http.MethodPut, path: "/api/v3/repos/operator/project-kit/pulls/27/merge", want: http.StatusForbidden},
		{name: "PR rebase denies wrong action", operation: "pr.rebase", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls/27/actions/pr.merge", want: http.StatusForbidden},
		{name: "PR rebase denies zero PR", operation: "pr.rebase", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls/0/actions/pr.rebase", want: http.StatusForbidden},
		{name: "PR rebase denies empty intent", operation: "pr.rebase", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27/actions/pr.rebase/", want: http.StatusForbidden},
		{name: "PR merge Session denies retired action POST", operation: "pr.merge", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls/27/actions/pr.merge", want: http.StatusForbidden},
		{name: "PR merge Session denies verification read", operation: "pr.merge", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27", want: http.StatusForbidden},
		{name: "PR merge Session denies retired recovery read", operation: "pr.merge", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27/actions/pr.merge/99999999-9999-4999-8999-999999999999", want: http.StatusForbidden},
		{name: "PR merge denies empty recovery read", operation: "pr.merge", method: http.MethodGet, path: "/api/v3/repos/operator/project-kit/pulls/27/actions/pr.merge/", want: http.StatusForbidden},
		{name: "PR merge denies legacy merge route", operation: "pr.merge", method: http.MethodPut, path: "/api/v3/repos/operator/project-kit/pulls/27/merge", want: http.StatusForbidden},
		{name: "PR merge Session denies Human provider endpoint", operation: "pr.merge", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls/27/provider/merge", want: http.StatusForbidden},
		{name: "PR merge denies zero PR", operation: "pr.merge", method: http.MethodPost, path: "/api/v3/repos/operator/project-kit/pulls/0/actions/pr.merge", want: http.StatusForbidden},
		{name: "PR merge denies wrong method", operation: "pr.merge", method: http.MethodPut, path: "/api/v3/repos/operator/project-kit/pulls/27/actions/pr.merge", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := db.DelegatedAgentSession{ID: "session-1", OperationName: tc.operation, GrantedCapabilities: []string{"repo:read", "repo:write", "pr:create"}}
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(service.ContextWithDelegatedSession(req.Context(), session))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDelegatedSessionDeniedReadIsDurablyAudited(t *testing.T) {
	svc := setupTestService(t)
	if err := svc.DB.AutoMigrate(&db.AuditLogEntry{}); err != nil {
		t.Fatal(err)
	}
	principal := seedUserWithToken(t, svc, "delegated-reader", "")
	repository := db.Repository{ID: 41, FullName: "operator/project-kit"}
	session := db.DelegatedAgentSession{
		ID: "session-read-denial", PrincipalUserID: principal.ID, PrincipalUser: principal,
		PrincipalLogin: principal.Login, BindingID: "binding-reader", BindingRevision: "binding-reader-v1",
		IssuerInstanceID: "multica-mini", IssuerSubject: "subject-reader", CorrelationID: "correlation-read-denial",
		RepositoryID: repository.ID, Repository: repository, OperationName: "repo.read", PolicyVersion: "repo-policy-v1",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	handler := EnforceDelegatedSessionSurface(svc)(okHandler)
	req := httptest.NewRequest(http.MethodGet, "/api/v3/repos/operator/project-kit/issues", nil)
	req = req.WithContext(service.ContextWithDelegatedSession(service.ContextWithUser(req.Context(), principal), session))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	var audit db.AuditLogEntry
	if err := svc.DB.First(&audit, "action = ?", service.AuditActionDelegatedWriteDenied).Error; err != nil {
		t.Fatalf("load denial audit: %v", err)
	}
	for _, fact := range []string{
		`"session_id":"session-read-denial"`,
		`"binding_id":"binding-reader"`,
		`"operation":"GET /api/v3/repos/operator/project-kit/issues"`,
		`"outcome":"denied"`,
		`"reason":"surface_not_allowed"`,
	} {
		if !strings.Contains(audit.Details, fact) {
			t.Fatalf("denial audit missing %s: %s", fact, audit.Details)
		}
	}
}
