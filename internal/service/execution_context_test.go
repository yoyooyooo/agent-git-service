package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/testharness"
)

func TestIntakeExecutionContextPersistsImmutableCredentialFreeSnapshot(t *testing.T) {
	svc := newExecutionContextTestService(t)
	pullResult := testExecutionContextPullResult(t)
	puller := &staticExecutionContextPuller{result: pullResult}
	svc.ExecutionContextPuller = puller
	token := "mat_service_intake_secret_value"

	result, err := svc.IntakeExecutionContext(context.Background(), service.ExecutionContextIntakeInput{
		RuntimeEndpointHint: "http://primary.example.test:37134",
		Locator: executioncontext.Locator{
			WorkspaceID: pullResult.SourceRef.WorkspaceID,
			AgentID:     pullResult.SourceRef.AgentID,
			TaskID:      pullResult.SourceRef.TaskID,
		},
		SourceToken: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	if puller.observedToken != token {
		t.Fatal("service did not pass the request-scoped source token to the puller")
	}
	if result.Schema != service.ExecutionContextIntakeSchema || result.SnapshotID == "" || result.ContextDigest != pullResult.ContextDigest || result.SourceRef.SourceInstanceID != "multica-mini" {
		t.Fatalf("result=%#v", result)
	}

	record, sourceRef, current, err := svc.GetExecutionContextSnapshot(context.Background(), result.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Schema != executioncontext.SnapshotSchema || record.ContextDigest != pullResult.ContextDigest || sourceRef.TaskID != pullResult.SourceRef.TaskID || current.Run.ID != pullResult.Context.Run.ID {
		t.Fatalf("record=%#v source_ref=%#v context=%#v", record, sourceRef, current)
	}
	stored := strings.Join([]string{
		record.ID, record.Schema, record.SourceInstanceID, record.SourceRefJSON,
		record.ContextSchema, record.ContextDigest, record.ContextJSON,
		record.ExternalWorkspaceID, record.ExternalAgentID, record.ExternalTaskID,
		record.ExternalRunID, record.ExternalIssueID, record.ExternalRuntimeID,
	}, "\n")
	tokenDigest := sha256.Sum256([]byte(token))
	if strings.Contains(stored, token) || strings.Contains(stored, hex.EncodeToString(tokenDigest[:])) {
		t.Fatal("snapshot persisted source token or its authenticating hash")
	}
	responseJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(responseJSON), token) || strings.Contains(string(responseJSON), hex.EncodeToString(tokenDigest[:])) {
		t.Fatal("intake response leaked source token or its authenticating hash")
	}

	if err := svc.DB.Model(&record).Update("context_digest", "sha256:"+strings.Repeat("0", 64)).Error; err == nil {
		t.Fatal("immutable snapshot update succeeded")
	}
	if err := svc.DB.Delete(&record).Error; err == nil {
		t.Fatal("immutable snapshot delete succeeded")
	}
	var count int64
	if err := svc.DB.Model(&db.ExecutionContextSnapshot{}).Where("id = ?", record.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("snapshot count=%d err=%v", count, err)
	}
}

func TestIntakeExecutionContextFailureCreatesNoSnapshotAndLeaksNoToken(t *testing.T) {
	svc := newExecutionContextTestService(t)
	svc.ExecutionContextPuller = &staticExecutionContextPuller{err: executioncontext.ErrSourceCredentialRejected}
	token := "mat_rejected_service_secret"
	_, err := svc.IntakeExecutionContext(context.Background(), service.ExecutionContextIntakeInput{
		RuntimeEndpointHint: "http://primary.example.test:37134",
		Locator: executioncontext.Locator{
			WorkspaceID: "11111111-1111-4111-8111-111111111111",
			AgentID:     "66666666-6666-4666-8666-666666666661",
			TaskID:      "66666666-6666-4666-8666-666666666662",
		},
		SourceToken: token,
	})
	if !errors.Is(err, executioncontext.ErrSourceCredentialRejected) {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked token: %v", err)
	}
	var count int64
	if err := svc.DB.Model(&db.ExecutionContextSnapshot{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("snapshot count=%d err=%v", count, err)
	}
}

func TestIntakeExecutionContextUnavailableWithoutRegistry(t *testing.T) {
	svc := newExecutionContextTestService(t)
	_, err := svc.IntakeExecutionContext(context.Background(), service.ExecutionContextIntakeInput{})
	if !errors.Is(err, service.ErrExecutionContextIntakeUnavailable) {
		t.Fatalf("error=%v", err)
	}
}

type staticExecutionContextPuller struct {
	result        executioncontext.PullResult
	err           error
	observedToken string
}

func (p *staticExecutionContextPuller) Pull(_ context.Context, input executioncontext.PullRequest) (executioncontext.PullResult, error) {
	p.observedToken = input.SourceToken
	return p.result, p.err
}

func testExecutionContextPullResult(t *testing.T) executioncontext.PullResult {
	t.Helper()
	current := executioncontext.CurrentContext{
		Schema:      executioncontext.MulticaCurrentExecutionContextSchema,
		ObservedAt:  "2026-08-06T02:12:30.123456789Z",
		Workspace:   executioncontext.Workspace{ID: "11111111-1111-4111-8111-111111111111", Name: "primary-a", Slug: "primary-a"},
		Agent:       executioncontext.Agent{ID: "66666666-6666-4666-8666-666666666661", Name: "fixture-ci-repair", Status: "working"},
		Task:        executioncontext.Task{ID: "66666666-6666-4666-8666-666666666662", Status: "running", Attempt: 1, MaxAttempts: 2},
		Run:         executioncontext.Run{ID: "66666666-6666-4666-8666-666666666663", TaskID: "66666666-6666-4666-8666-666666666662", Status: "running", Attempt: 1, MaxAttempts: 2},
		Issue:       &executioncontext.Issue{ID: "66666666-6666-4666-8666-666666666664", Key: "MINI-1511", Title: "live proof", Status: "todo"},
		Runtime:     &executioncontext.Runtime{ID: "66666666-6666-4666-8666-666666666665", Name: "Pi (mini)", Provider: "pi", Status: "online", DetailsAvailable: true},
		Attribution: &executioncontext.Attribution{Source: "direct_human", Precise: true},
	}
	canonical, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	digestBytes := sha256.Sum256(canonical)
	return executioncontext.PullResult{
		SourceRef: executioncontext.SourceRef{
			Schema: executioncontext.SourceRefSchema, SourceInstanceID: "multica-mini",
			Adapter:     executioncontext.AdapterMulticaCurrentExecutionContextV1,
			WorkspaceID: current.Workspace.ID, WorkspaceRef: "primary-a", AgentID: current.Agent.ID,
			TaskID: current.Task.ID, RunID: current.Run.ID, IssueID: current.Issue.ID,
			RuntimeID: current.Runtime.ID, ObservedAt: current.ObservedAt,
		},
		Context: current, ContextJSON: canonical,
		ContextDigest: fmt.Sprintf("sha256:%x", digestBytes),
	}
}

func newExecutionContextTestService(t *testing.T) *service.Service {
	t.Helper()
	svc, cleanup := testharness.NewService(t, testharness.ServiceConfig{})
	t.Cleanup(cleanup)
	return svc
}
