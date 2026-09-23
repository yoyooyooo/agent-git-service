package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
)

const ExecutionContextIntakeSchema = "ags.execution-context-intake.v1"

var ErrExecutionContextIntakeUnavailable = errors.New("execution context intake is unavailable")

// ExecutionContextIntakeInput contains one request-scoped source credential.
// Callers must clear SourceToken after the call. Service and persistence code
// never return, log, hash, or store it.
type ExecutionContextIntakeInput struct {
	SourceInstanceID    string
	RuntimeEndpointHint string
	Locator             executioncontext.Locator
	SourceToken         string
}

// ExecutionContextIntakeResult is the credential-free receipt for one durable,
// immutable source observation. It grants no AGS identity or operation.
type ExecutionContextIntakeResult struct {
	Schema          string                          `json:"schema"`
	SnapshotID      string                          `json:"snapshot_id"`
	SourceRef       executioncontext.SourceRef      `json:"source_ref"`
	ContextDigest   string                          `json:"context_digest"`
	ContextSnapshot executioncontext.CurrentContext `json:"context_snapshot"`
	CreatedAt       time.Time                       `json:"created_at"`
	// Provisional is true when Multica CEC was unavailable and locator facts
	// were recorded without source verification. It never grants extra ops.
	Provisional bool `json:"-"`
}

// IntakeExecutionContext pulls one task-token-bound source observation through
// the configured registry, then stores only canonical credential-free JSON.
func (s *Service) IntakeExecutionContext(ctx context.Context, input ExecutionContextIntakeInput) (ExecutionContextIntakeResult, error) {
	if s.ExecutionContextPuller == nil {
		return ExecutionContextIntakeResult{}, ErrExecutionContextIntakeUnavailable
	}
	pulled, err := s.ExecutionContextPuller.Pull(ctx, executioncontext.PullRequest{
		SourceInstanceID: input.SourceInstanceID, RuntimeEndpointHint: input.RuntimeEndpointHint,
		Locator: input.Locator, SourceToken: input.SourceToken,
	})
	input.SourceToken = ""
	if err != nil {
		return ExecutionContextIntakeResult{}, err
	}
	sourceRefJSON, err := json.Marshal(pulled.SourceRef)
	if err != nil {
		return ExecutionContextIntakeResult{}, ErrExecutionContextIntakeUnavailable
	}
	observedAt, err := time.Parse(time.RFC3339Nano, pulled.SourceRef.ObservedAt)
	if err != nil {
		return ExecutionContextIntakeResult{}, executioncontext.ErrSourceContextInvalid
	}
	createdAt := time.Now().UTC()
	record := db.ExecutionContextSnapshot{
		ID: uuid.NewString(), Schema: executioncontext.SnapshotSchema,
		SourceInstanceID: pulled.SourceRef.SourceInstanceID, SourceRefJSON: string(sourceRefJSON),
		ContextSchema: pulled.Context.Schema, ContextDigest: pulled.ContextDigest, ContextJSON: string(pulled.ContextJSON),
		ExternalWorkspaceID: pulled.SourceRef.WorkspaceID, ExternalAgentID: pulled.SourceRef.AgentID,
		ExternalTaskID: pulled.SourceRef.TaskID, ExternalRunID: pulled.SourceRef.RunID,
		ExternalIssueID: pulled.SourceRef.IssueID, ExternalRuntimeID: pulled.SourceRef.RuntimeID,
		SourceObservedAt: observedAt.UTC(), CreatedAt: createdAt,
	}
	if err := s.DBForCtx(ctx).Create(&record).Error; err != nil {
		return ExecutionContextIntakeResult{}, ErrExecutionContextIntakeUnavailable
	}
	return ExecutionContextIntakeResult{
		Schema: ExecutionContextIntakeSchema, SnapshotID: record.ID,
		SourceRef: pulled.SourceRef, ContextDigest: pulled.ContextDigest,
		ContextSnapshot: pulled.Context, CreatedAt: createdAt,
	}, nil
}

func isExecutionContextAvailabilityError(err error) bool {
	return errors.Is(err, ErrExecutionContextIntakeUnavailable) ||
		errors.Is(err, executioncontext.ErrDisabled) ||
		errors.Is(err, executioncontext.ErrSourceUnavailable)
}

// intakeForAccessGrant prefers a live Multica CEC observation. Availability
// failures become a locator-only provisional intake so ordinary Git/PR can
// continue; credential/locator/connector denials stay hard errors.
func (s *Service) intakeForAccessGrant(ctx context.Context, input ExecutionContextIntakeInput) (ExecutionContextIntakeResult, error) {
	intake, err := s.IntakeExecutionContext(ctx, input)
	if err == nil {
		return intake, nil
	}
	if !isExecutionContextAvailabilityError(err) {
		return ExecutionContextIntakeResult{}, err
	}
	return s.provisionalIntakeFromLocator(ctx, input)
}

func (s *Service) provisionalIntakeFromLocator(ctx context.Context, input ExecutionContextIntakeInput) (ExecutionContextIntakeResult, error) {
	now := time.Now().UTC()
	observed := now.Format(time.RFC3339Nano)
	workspaceID := strings.TrimSpace(input.Locator.WorkspaceID)
	agentID := strings.TrimSpace(input.Locator.AgentID)
	taskID := strings.TrimSpace(input.Locator.TaskID)
	sourceRef := executioncontext.SourceRef{
		Schema:           executioncontext.SourceRefSchema,
		SourceInstanceID: strings.TrimSpace(input.SourceInstanceID),
		Adapter:          executioncontext.AdapterMulticaCurrentExecutionContextV1,
		WorkspaceID:      workspaceID,
		AgentID:          agentID,
		TaskID:           taskID,
		RunID:            taskID,
		ObservedAt:       observed,
	}
	current := executioncontext.CurrentContext{
		Schema:     executioncontext.MulticaCurrentExecutionContextSchemaV2,
		ObservedAt: observed,
		Workspace:  executioncontext.Workspace{ID: workspaceID},
		Agent:      executioncontext.Agent{ID: agentID},
		Task:       executioncontext.Task{ID: taskID, Status: "running"},
		Run:        executioncontext.Run{ID: taskID, TaskID: taskID},
	}
	contextJSON, err := json.Marshal(current)
	if err != nil {
		return ExecutionContextIntakeResult{}, ErrExecutionContextIntakeUnavailable
	}
	sourceRefJSON, err := json.Marshal(sourceRef)
	if err != nil {
		return ExecutionContextIntakeResult{}, ErrExecutionContextIntakeUnavailable
	}
	digest := sha256.Sum256(contextJSON)
	record := db.ExecutionContextSnapshot{
		ID: uuid.NewString(), Schema: executioncontext.SnapshotSchema,
		SourceInstanceID: sourceRef.SourceInstanceID, SourceRefJSON: string(sourceRefJSON),
		ContextSchema: current.Schema, ContextDigest: hex.EncodeToString(digest[:]), ContextJSON: string(contextJSON),
		ExternalWorkspaceID: sourceRef.WorkspaceID, ExternalAgentID: sourceRef.AgentID,
		ExternalTaskID: sourceRef.TaskID, ExternalRunID: sourceRef.RunID,
		SourceObservedAt: now, CreatedAt: now,
	}
	if err := s.DBForCtx(ctx).Create(&record).Error; err != nil {
		return ExecutionContextIntakeResult{}, ErrExecutionContextIntakeUnavailable
	}
	return ExecutionContextIntakeResult{
		Schema: ExecutionContextIntakeSchema, SnapshotID: record.ID,
		SourceRef: sourceRef, ContextDigest: record.ContextDigest,
		ContextSnapshot: current, CreatedAt: now, Provisional: true,
	}, nil
}

// GetExecutionContextSnapshot loads one immutable snapshot for later AGS-owned
// actor/grant evaluation. It is an internal service contract, not a public
// context-source read or authority decision.
func (s *Service) GetExecutionContextSnapshot(ctx context.Context, id string) (db.ExecutionContextSnapshot, executioncontext.SourceRef, executioncontext.CurrentContext, error) {
	var record db.ExecutionContextSnapshot
	if err := s.DBForCtx(ctx).First(&record, "id = ?", id).Error; err != nil {
		return db.ExecutionContextSnapshot{}, executioncontext.SourceRef{}, executioncontext.CurrentContext{}, wrapErr(err)
	}
	var sourceRef executioncontext.SourceRef
	var current executioncontext.CurrentContext
	if err := json.Unmarshal([]byte(record.SourceRefJSON), &sourceRef); err != nil {
		return db.ExecutionContextSnapshot{}, executioncontext.SourceRef{}, executioncontext.CurrentContext{}, ErrExecutionContextIntakeUnavailable
	}
	if err := json.Unmarshal([]byte(record.ContextJSON), &current); err != nil {
		return db.ExecutionContextSnapshot{}, executioncontext.SourceRef{}, executioncontext.CurrentContext{}, ErrExecutionContextIntakeUnavailable
	}
	return record, sourceRef, current, nil
}
