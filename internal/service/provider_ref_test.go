package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
)

func TestProviderGitRefReadsExactLiveBranchWithoutNativeGitOrPR(t *testing.T) {
	calls := 0
	sha := strings.Repeat("c", 40)
	svc, ctx, repo := newProviderInventoryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		branch := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/provider-owner/mirror/branches/")
		if r.Method != http.MethodGet || (branch != "main" && branch != "feature/nested") {
			t.Errorf("not an exact provider branch GET: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"name":%q,"commit":{"id":%q}}`, branch, sha)
	})
	for _, ref := range []string{"refs/heads/main", "refs/heads/feature/nested"} {
		got, err := svc.ReadProviderGitRef(ctx, repo.FullName, ref)
		if err != nil || got.SHA != sha || got.Ref != ref || got.ExternalRepo != "provider-owner/mirror" || got.ObservedAt.IsZero() {
			t.Fatalf("live provider ref: %+v %v", got, err)
		}
	}
	if svc.Git != nil || calls != 2 {
		t.Fatal("fixture must prove there is no native Git fallback")
	}
	for _, ref := range []string{"main", "refs/tags/v1", "refs/heads/", "refs/heads/../main", "refs/heads/main\n"} {
		if _, err := svc.ReadProviderGitRef(ctx, repo.FullName, ref); !errors.Is(err, ErrValidation) {
			t.Fatalf("invalid ref accepted: %v", err)
		}
	}
	delegated := ContextWithDelegatedSession(ctx, db.DelegatedAgentSession{ID: "single-pr", OperationName: "pr.read"})
	if _, err := svc.ReadProviderGitRef(delegated, repo.FullName, "refs/heads/main"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("PR authority widened to refs: %v", err)
	}
	if _, err := svc.ReadProviderGitRef(t.Context(), repo.FullName, "refs/heads/main"); !errors.Is(err, ErrUnauthorized) || calls != 2 {
		t.Fatalf("denial reached provider: %v calls=%d", err, calls)
	}
}

func TestProviderGitRefRejectsMissingAndMismatchedProviderBranch(t *testing.T) {
	body := ""
	status := http.StatusNotFound
	svc, ctx, repo := newProviderInventoryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	if _, err := svc.ReadProviderGitRef(ctx, repo.FullName, "refs/heads/main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing branch: %v", err)
	}
	status = http.StatusOK
	for _, invalid := range []string{
		`null`, `{}`, `{"name":"main","commit":{"id":"short"}}`,
		fmt.Sprintf(`{"name":"other","commit":{"id":%q}}`, strings.Repeat("a", 40)),
		fmt.Sprintf(`{"name":"main","commit":{"id":%q,"sha":%q}}`, strings.Repeat("a", 40), strings.Repeat("b", 40)),
	} {
		body = invalid
		if _, err := svc.ReadProviderGitRef(ctx, repo.FullName, "refs/heads/main"); !errors.Is(err, ErrProviderInventoryUnavailable) {
			t.Fatalf("invalid provider branch accepted: %v", err)
		}
	}
}
