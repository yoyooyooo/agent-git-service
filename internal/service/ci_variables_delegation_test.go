package service_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/githttp"
	"github.com/ngaut/agent-git-service/internal/graphql"
	"github.com/ngaut/agent-git-service/internal/oauth"
	"github.com/ngaut/agent-git-service/internal/rest"
	"github.com/ngaut/agent-git-service/internal/router"
	"github.com/ngaut/agent-git-service/internal/service"
)

// Issue real canonical access-grant transports. Rejection of an invalid or
// fabricated token would not prove that existing reviewer capabilities stay
// narrow when the executor itself is the repository owner.
func TestActionsVariablesExistingDelegatedReadersRemainDenied(t *testing.T) {
	svc, _, repo := setupAccessGrantService(t)
	var providerCalls atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		http.Error(w, "provider must not be reached by delegated reader", http.StatusInternalServerError)
	}))
	defer provider.Close()
	options := cibackend.BackendConfig{Kind: "forgejo", URL: provider.URL, TokenFile: "synthetic-not-read", AllowHTTP: true}
	backend, err := cibackend.NewHTTP(options, "synthetic-provider-token")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := cibackend.WithBackends(cibackend.Config{
		Default:      "native",
		Backends:     map[string]cibackend.BackendConfig{"fixture": options},
		Repositories: map[string]cibackend.Binding{repo.FullName: {Backend: "fixture", Repository: "ci/project"}},
	}, map[string]cibackend.Backend{"fixture": backend})
	if err != nil {
		t.Fatal(err)
	}
	svc.CI = registry
	mux := router.RegisterRoutes(chi.NewRouter(), &rest.Deps{Svc: svc}, githttp.New(svc.Git, svc),
		graphql.NewServer(svc), &oauth.Handler{Svc: svc}, "")
	grant, err := svc.IssueAccessGrant(context.Background(), accessGrantIssueInput("", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"ci.read", "review.read"} {
		t.Run(operation, func(t *testing.T) {
			constraints := map[string]any{}
			if operation == "review.read" {
				constraints = map[string]any{"pull_request_number": 1, "forgejo_pull_request_number": 1}
			}
			transport, err := svc.IssueAccessGrantTransportSession(context.Background(), grant.GrantToken,
				service.AccessGrantOperationInput{Operation: operation, Constraints: constraints})
			if err != nil {
				t.Fatal(err)
			}
			principal, session, err := svc.ResolveDelegatedSessionCredential(context.Background(), transport.SessionToken)
			if err != nil {
				t.Fatal(err)
			}
			ctx := service.ContextWithDelegatedSession(service.ContextWithUser(context.Background(), principal), session)
			// The same real token must work for its existing repository read.
			request := httptest.NewRequest(http.MethodGet, "/api/v3/repos/"+repo.FullName, nil)
			request.Header.Set("Authorization", "token "+transport.SessionToken)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("control repository read failed: status=%d body=%s", response.Code, response.Body.String())
			}
			for _, suffix := range []string{"", "/VAR_000"} {
				request = httptest.NewRequest(http.MethodGet, "/api/v3/repos/"+repo.FullName+"/actions/variables"+suffix, nil)
				request.Header.Set("Authorization", "token "+transport.SessionToken)
				response = httptest.NewRecorder()
				mux.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden {
					t.Fatalf("delegated variable read status=%d body=%s", response.Code, response.Body.String())
				}
			}
			if _, err := svc.CIRepositoryVariables(ctx, repo.FullName, 1, 30); !errors.Is(err, service.ErrForbidden) {
				t.Fatalf("direct service list widened delegated access: %v", err)
			}
			if _, err := svc.CIRepositoryVariable(ctx, repo.FullName, "VAR_000"); !errors.Is(err, service.ErrForbidden) {
				t.Fatalf("direct service get widened delegated access: %v", err)
			}
		})
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("delegated caller caused %d provider reads", providerCalls.Load())
	}
}
