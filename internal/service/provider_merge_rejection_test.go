package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/graphql"
	"github.com/ngaut/agent-git-service/internal/service"
)

func TestNativeProviderMergePreservesDefiniteRejection(t *testing.T) {
	svc, _, input, provider := setupAccessGrantMerge(t, "unknown")
	human := addHumanMergeActor(t, svc, "merge-rejection-human")
	ctx := service.ContextWithUser(context.Background(), human)
	provider.mergeError = &forgejointegration.ProviderHTTPError{Status: http.StatusMethodNotAllowed, Method: http.MethodPost, RouteKind: "pull_merge"}
	if err := svc.Git.CreateBranch(ctx, accessGrantTestRepo, "feature/access-grant", "main"); err != nil {
		t.Fatal(err)
	}

	_, err := svc.MergeStandardPR(ctx, accessGrantTestRepo, input.AGSPRNumber, "rebase", "", input.ExpectedHeadSHA)
	var providerErr *forgejointegration.ProviderHTTPError
	if !errors.Is(err, service.ErrInvalidState) || !errors.As(err, &providerErr) || providerErr.Status != http.StatusMethodNotAllowed {
		t.Fatalf("expected definitive native rejection with typed 405, got %v", err)
	}
	if strings.Contains(err.Error(), "outcome is unknown") || !strings.Contains(err.Error(), "required checks") {
		t.Fatalf("rejection diagnostic=%v", err)
	}
	for range 2 {
		observed, err := svc.ObserveHumanProviderMerge(ctx, accessGrantTestRepo, input.AGSPRNumber, input.ExpectedHeadSHA)
		if err != nil || observed.ProviderMerged {
			t.Fatalf("rejected merge observation=%#v err=%v", observed, err)
		}
	}
	var pr db.PullRequest
	if err := svc.DB.First(&pr, "number = ?", input.AGSPRNumber).Error; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 || pr.Merged || pr.State != db.StateOpen {
		t.Fatalf("writes=%d pr=%#v; denial/readback must not retry or create terminal facts", calls, pr)
	}
}

func TestHumanProviderMergeErrorUsesOnlyExactConfirmedSuccess(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		merged    bool
		wrongHead bool
		wantMerge bool
	}{
		{name: "405 then exact merged readback", status: http.StatusMethodNotAllowed, merged: true, wantMerge: true},
		{name: "transport failure remains unknown"},
		{name: "503 remains unknown", status: http.StatusServiceUnavailable},
		{name: "405 wrong head remains unknown", status: http.StatusMethodNotAllowed, merged: true, wrongHead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, input, provider := setupAccessGrantMerge(t, "unknown")
			human := addHumanMergeActor(t, svc, "merge-readback-human")
			ctx := service.ContextWithUser(context.Background(), human)
			if tc.status != 0 {
				provider.mergeError = &forgejointegration.ProviderHTTPError{Status: tc.status, Method: http.MethodPost, RouteKind: "pull_merge"}
			}
			if tc.merged {
				snapshot := provider.snapshot
				snapshot.State, snapshot.Merged, snapshot.MergeCommitSHA = "closed", true, input.ExpectedHeadSHA
				if tc.wrongHead {
					snapshot.HeadSHA = strings.Repeat("b", 40)
				}
				provider.mergeSnapshot = &snapshot
			}
			receipt, err := svc.ExecuteHumanProviderMerge(ctx, accessGrantTestRepo, input.AGSPRNumber, service.HumanProviderMergeInput{ExpectedHeadSHA: input.ExpectedHeadSHA, MergeMethod: "rebase"})
			if tc.wantMerge {
				if err != nil || !receipt.ProviderMerged || receipt.ProviderMergeSHA != input.ExpectedHeadSHA {
					t.Fatalf("receipt=%#v err=%v", receipt, err)
				}
			} else if !errors.Is(err, service.ErrConflict) || receipt.ProviderMerged {
				t.Fatalf("unconfirmed result=%#v err=%v", receipt, err)
			}
			provider.mu.Lock()
			calls := provider.mergeCalls
			provider.mu.Unlock()
			if calls != 1 {
				t.Fatalf("provider merge calls=%d", calls)
			}
		})
	}
}

func TestHumanProviderMergeDoesNotAcceptDifferentReadback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*forgejointegration.PullRequestSnapshot)
	}{
		{name: "another head", mutate: func(p *forgejointegration.PullRequestSnapshot) { p.HeadSHA = strings.Repeat("b", 40) }},
		{name: "another PR number", mutate: func(p *forgejointegration.PullRequestSnapshot) { p.Number++ }},
		{name: "missing PR number", mutate: func(p *forgejointegration.PullRequestSnapshot) { p.Number = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, input, provider := setupAccessGrantMerge(t, "success")
			human := addHumanMergeActor(t, svc, "merge-different-readback-human")
			ctx := service.ContextWithUser(context.Background(), human)
			provider.snapshot.State = "closed"
			provider.snapshot.Merged = true
			provider.snapshot.MergeCommitSHA = input.ExpectedHeadSHA
			tc.mutate(&provider.snapshot)
			observed, err := svc.ObserveHumanProviderMerge(ctx, accessGrantTestRepo, input.AGSPRNumber, input.ExpectedHeadSHA)
			if err != nil || observed.ProviderMerged || observed.Outcome != "recovery_needed" {
				t.Fatalf("different-PR observation=%#v err=%v", observed, err)
			}
			receipt, err := svc.ExecuteHumanProviderMerge(ctx, accessGrantTestRepo, input.AGSPRNumber, service.HumanProviderMergeInput{ExpectedHeadSHA: input.ExpectedHeadSHA, MergeMethod: "rebase"})
			if err == nil || receipt.ProviderMerged {
				t.Fatalf("different-PR retry=%#v err=%v", receipt, err)
			}
			provider.mu.Lock()
			calls := provider.mergeCalls
			provider.mu.Unlock()
			if calls != 0 {
				t.Fatalf("readback sent %d provider writes", calls)
			}
		})
	}
}

func TestHumanProviderMergeRejectsWrongPreflightPRNumber(t *testing.T) {
	svc, _, input, provider := setupAccessGrantMerge(t, "success")
	human := addHumanMergeActor(t, svc, "merge-preflight-number-human")
	provider.snapshot.Number++
	receipt, err := svc.ExecuteHumanProviderMerge(service.ContextWithUser(context.Background(), human), accessGrantTestRepo, input.AGSPRNumber, service.HumanProviderMergeInput{ExpectedHeadSHA: input.ExpectedHeadSHA, MergeMethod: "rebase"})
	if !errors.Is(err, service.ErrConflict) || receipt.ProviderMerged {
		t.Fatalf("wrong-number preflight=%#v err=%v", receipt, err)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 0 {
		t.Fatalf("wrong-number preflight sent %d provider writes", calls)
	}
}

func TestGraphQLProviderMergePreservesDefiniteRejection(t *testing.T) {
	svc, _, input, provider := setupAccessGrantMerge(t, "unknown")
	human := addHumanMergeActor(t, svc, "merge-graphql-rejection-human")
	ctx := service.ContextWithUser(context.Background(), human)
	provider.mergeError = &forgejointegration.ProviderHTTPError{Status: http.StatusMethodNotAllowed, Method: http.MethodPost, RouteKind: "pull_merge"}
	if err := svc.Git.CreateBranch(ctx, accessGrantTestRepo, "feature/access-grant", "main"); err != nil {
		t.Fatal(err)
	}
	var pr db.PullRequest
	if err := svc.DB.First(&pr, "number = ?", input.AGSPRNumber).Error; err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"query":     `mutation($input: MergePullRequestInput!) { mergePullRequest(input: $input) { pullRequest { merged } } }`,
		"variables": map[string]any{"input": map[string]any{"pullRequestId": fmt.Sprintf("PullRequest_%d", pr.ID), "mergeMethod": "REBASE", "expectedHeadOid": input.ExpectedHeadSHA}},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/graphql", bytes.NewReader(body)).WithContext(ctx)
	graphql.NewServer(svc).Handler(recorder, request)
	var result struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || len(result.Errors) != 1 || (len(result.Data) != 0 && string(result.Data) != "null") {
		t.Fatalf("GraphQL rejection status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	message := result.Errors[0].Message
	if !strings.Contains(message, "provider rejected merge") || !strings.Contains(message, "status 405") || strings.Contains(message, "outcome is unknown") {
		t.Fatalf("GraphQL rejection lost diagnostic: %s", message)
	}
	provider.mu.Lock()
	calls := provider.mergeCalls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider merge invocations=%d", calls)
	}
}
