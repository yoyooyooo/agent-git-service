// Package executioncontext resolves a runtime locator through an operator-owned
// connector registry and pulls one provider-neutral execution context without
// persisting or returning the source bearer credential.
package executioncontext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	AdapterMulticaCurrentExecutionContextV1 = "multica_current_execution_context_v1"
	MulticaCurrentExecutionContextSchema    = "multica.current-execution-context.v1"
	MulticaCurrentExecutionContextSchemaV2  = "multica.current-execution-context.v2"
	SourceRefSchema                         = "ags.execution-context-source-ref.v1"
	SnapshotSchema                          = "ags.execution-context-snapshot.v1"

	currentExecutionContextPath       = "/api/integrations/current-execution-context"
	defaultPullTimeout                = 5 * time.Second
	maxPullTimeout                    = 30 * time.Second
	maxContextBodyBytes         int64 = 1 << 20
)

var (
	ErrDisabled                 = errors.New("execution context intake is disabled")
	ErrInvalidLocator           = errors.New("execution context locator is invalid")
	ErrConnectorNotFound        = errors.New("execution context connector not found")
	ErrConnectorAmbiguous       = errors.New("execution context connector is ambiguous")
	ErrSourceCredentialRejected = errors.New("execution context source credential was rejected")
	ErrSourceUnavailable        = errors.New("execution context source is unavailable")
	ErrSourceRedirectRejected   = errors.New("execution context source redirect was rejected")
	ErrSourceContextInvalid     = errors.New("execution context source response is invalid")
)

var (
	sourceInstancePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	canonicalUUIDPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Config is the operator-owned connector registry configuration. It contains
// routing and adaptation metadata only; source credentials are never configured
// here.
type Config struct {
	Enabled    bool              `yaml:"enabled"`
	Connectors []ConnectorConfig `yaml:"connectors"`
}

// ConnectorConfig binds one stable source identity to accepted runtime-facing
// endpoint aliases and one fixed server egress endpoint.
type ConnectorConfig struct {
	SourceInstanceID         string            `yaml:"source_instance_id"`
	Adapter                  string            `yaml:"adapter"`
	AcceptedRuntimeEndpoints []string          `yaml:"accepted_runtime_endpoints"`
	EgressEndpoint           string            `yaml:"egress_endpoint"`
	WorkspaceMappings        map[string]string `yaml:"workspace_mappings"`
	Timeout                  string            `yaml:"timeout"`
}

// Locator is the runtime's non-secret claim about the currently executing task.
// Every field is checked against the task-token-bound source response.
type Locator struct {
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id"`
	TaskID      string `json:"task_id"`
}

// PullRequest supplies connector selectors, a runtime locator, and the current
// source token. SourceToken is request-scoped and must never be persisted,
// logged, returned, hashed for authentication, or included in an error.
type PullRequest struct {
	SourceInstanceID    string
	RuntimeEndpointHint string
	Locator             Locator
	SourceToken         string
}

// PullResult is a normalized, credential-free source observation.
type PullResult struct {
	SourceRef     SourceRef
	Context       CurrentContext
	ContextJSON   []byte
	ContextDigest string
}

// SourceRef is the stable source-local locator stored beside an immutable
// context snapshot. Runtime aliases and egress topology are intentionally
// omitted.
type SourceRef struct {
	Schema           string `json:"schema"`
	SourceInstanceID string `json:"source_instance_id"`
	Adapter          string `json:"adapter"`
	WorkspaceID      string `json:"workspace_id"`
	WorkspaceRef     string `json:"workspace_ref"`
	AgentID          string `json:"agent_id"`
	TaskID           string `json:"task_id"`
	RunID            string `json:"run_id"`
	IssueID          string `json:"issue_id,omitempty"`
	RuntimeID        string `json:"runtime_id,omitempty"`
	DaemonID         string `json:"daemon_id,omitempty"`
	ObservedAt       string `json:"observed_at"`
}

// CurrentContext mirrors the closed provider-neutral Multica context response.
// v1 carries display enrichment; v2 is minimal and dual-reads claim.generation
// with run.id until Run product language is retired.
type CurrentContext struct {
	Schema      string       `json:"schema"`
	ObservedAt  string       `json:"observed_at"`
	Workspace   Workspace    `json:"workspace"`
	Agent       Agent        `json:"agent"`
	Task        Task         `json:"task"`
	Claim       *Claim       `json:"claim,omitempty"`
	Run         Run          `json:"run"`
	Issue       *Issue       `json:"issue,omitempty"`
	Squad       *Squad       `json:"squad,omitempty"`
	Runtime     *Runtime     `json:"runtime,omitempty"`
	Trigger     *Trigger     `json:"trigger,omitempty"`
	Attribution *Attribution `json:"attribution"`
}

type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Slug string `json:"slug"`
}

type Agent struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
}

type Task struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Attempt      int32  `json:"attempt"`
	MaxAttempts  int32  `json:"max_attempts,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	DispatchedAt string `json:"dispatched_at,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
	ParentTaskID string `json:"parent_task_id,omitempty"`
}

// Claim is Multica's internal claim-generation coordinate (not a user-facing Run).
type Claim struct {
	Generation string `json:"generation"`
	TaskID     string `json:"task_id"`
}

type Run struct {
	ID           string `json:"id"`
	TaskID       string `json:"task_id"`
	Status       string `json:"status,omitempty"`
	Attempt      int32  `json:"attempt,omitempty"`
	MaxAttempts  int32  `json:"max_attempts,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	DispatchedAt string `json:"dispatched_at,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	CompletedAt  string `json:"completed_at,omitempty"`
}

type Issue struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type Squad struct {
	ID               string `json:"id"`
	Name             string `json:"name,omitempty"`
	DetailsAvailable bool   `json:"details_available,omitempty"`
}

type Runtime struct {
	ID               string `json:"id"`
	DaemonID         string `json:"daemon_id,omitempty"`
	Name             string `json:"name,omitempty"`
	CustomName       string `json:"custom_name,omitempty"`
	Provider         string `json:"provider,omitempty"`
	Status           string `json:"status,omitempty"`
	DetailsAvailable bool   `json:"details_available,omitempty"`
}

type Trigger struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	CommentID      string `json:"comment_id,omitempty"`
	AutopilotRunID string `json:"autopilot_run_id,omitempty"`
}

type Attribution struct {
	Source              string           `json:"source"`
	Precise             bool             `json:"precise"`
	Initiator           *AttributionUser `json:"initiator,omitempty"`
	Originator          *AttributionUser `json:"originator,omitempty"`
	Evidence            *TaskEvidence    `json:"evidence,omitempty"`
	RuleVersionID       string           `json:"rule_version_id,omitempty"`
	DelegatedFromTaskID string           `json:"delegated_from_task_id,omitempty"`
	RetryOfTaskID       string           `json:"retry_of_task_id,omitempty"`
	RerunOfTaskID       string           `json:"rerun_of_task_id,omitempty"`
}

type AttributionUser struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

type TaskEvidence struct {
	Kind  string `json:"kind"`
	RefID string `json:"ref_id"`
}

type registeredConnector struct {
	config            ConnectorConfig
	acceptedEndpoints map[string]struct{}
	egressEndpoint    string
	timeout           time.Duration
}

// Registry is immutable after construction and safe for concurrent pulls.
type Registry struct {
	enabled    bool
	connectors []registeredConnector
	client     *http.Client
}

// NormalizeConfig validates and canonicalizes registry configuration without
// performing network or persistence work.
func NormalizeConfig(cfg Config) (Config, error) {
	if !cfg.Enabled {
		return Config{}, nil
	}
	if len(cfg.Connectors) == 0 {
		return Config{}, fmt.Errorf("execution_context.connectors is required when enabled")
	}
	normalized := Config{Enabled: true, Connectors: make([]ConnectorConfig, 0, len(cfg.Connectors))}
	seenSourceIDs := make(map[string]struct{}, len(cfg.Connectors))
	for index, raw := range cfg.Connectors {
		connector, err := normalizeConnectorConfig(raw)
		if err != nil {
			return Config{}, fmt.Errorf("execution_context.connectors[%d]: %w", index, err)
		}
		if _, exists := seenSourceIDs[connector.SourceInstanceID]; exists {
			return Config{}, fmt.Errorf("execution_context.connectors[%d]: duplicate source_instance_id", index)
		}
		seenSourceIDs[connector.SourceInstanceID] = struct{}{}
		normalized.Connectors = append(normalized.Connectors, connector)
	}
	return normalized, nil
}

func normalizeConnectorConfig(raw ConnectorConfig) (ConnectorConfig, error) {
	id := strings.TrimSpace(raw.SourceInstanceID)
	if id != raw.SourceInstanceID || !sourceInstancePattern.MatchString(id) || secretShaped(id) {
		return ConnectorConfig{}, errors.New("source_instance_id is invalid")
	}
	adapter := strings.TrimSpace(raw.Adapter)
	if adapter != AdapterMulticaCurrentExecutionContextV1 {
		return ConnectorConfig{}, errors.New("adapter is unsupported")
	}
	if len(raw.AcceptedRuntimeEndpoints) == 0 {
		return ConnectorConfig{}, errors.New("accepted_runtime_endpoints is required")
	}
	accepted := make([]string, 0, len(raw.AcceptedRuntimeEndpoints))
	seenAccepted := make(map[string]struct{}, len(raw.AcceptedRuntimeEndpoints))
	for _, rawEndpoint := range raw.AcceptedRuntimeEndpoints {
		endpoint, err := normalizeRuntimeEndpoint(rawEndpoint)
		if err != nil {
			return ConnectorConfig{}, errors.New("accepted_runtime_endpoints contains an invalid endpoint")
		}
		if _, duplicate := seenAccepted[endpoint]; duplicate {
			continue
		}
		seenAccepted[endpoint] = struct{}{}
		accepted = append(accepted, endpoint)
	}
	egress, err := normalizeEgressEndpoint(raw.EgressEndpoint)
	if err != nil {
		return ConnectorConfig{}, errors.New("egress_endpoint is invalid")
	}
	timeout := strings.TrimSpace(raw.Timeout)
	if timeout == "" {
		timeout = defaultPullTimeout.String()
	}
	parsedTimeout, err := time.ParseDuration(timeout)
	if err != nil || parsedTimeout <= 0 || parsedTimeout > maxPullTimeout {
		return ConnectorConfig{}, errors.New("timeout must be greater than zero and at most 30s")
	}
	if len(raw.WorkspaceMappings) == 0 {
		return ConnectorConfig{}, errors.New("workspace_mappings is required")
	}
	workspaces := make(map[string]string, len(raw.WorkspaceMappings))
	for externalID, workspaceRef := range raw.WorkspaceMappings {
		if !canonicalUUIDPattern.MatchString(externalID) || !safeRef(workspaceRef) {
			return ConnectorConfig{}, errors.New("workspace_mappings contains an invalid mapping")
		}
		workspaces[externalID] = workspaceRef
	}
	return ConnectorConfig{
		SourceInstanceID:         id,
		Adapter:                  adapter,
		AcceptedRuntimeEndpoints: accepted,
		EgressEndpoint:           egress,
		WorkspaceMappings:        workspaces,
		Timeout:                  parsedTimeout.String(),
	}, nil
}

// New constructs a registry. The supplied client is cloned and its redirect
// policy is replaced so callers cannot accidentally enable credential-bearing
// redirects.
func New(cfg Config, client *http.Client) (*Registry, error) {
	normalized, err := NormalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		// A task-scoped source bearer must go directly to the fixed registered
		// egress rather than an ambient process proxy.
		transport.Proxy = nil
		client = &http.Client{Transport: transport}
	}
	clonedClient := *client
	clonedClient.Jar = nil
	clonedClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return ErrSourceRedirectRejected
	}
	registry := &Registry{enabled: normalized.Enabled, client: &clonedClient}
	for _, connector := range normalized.Connectors {
		timeout, _ := time.ParseDuration(connector.Timeout)
		accepted := make(map[string]struct{}, len(connector.AcceptedRuntimeEndpoints))
		for _, endpoint := range connector.AcceptedRuntimeEndpoints {
			accepted[endpoint] = struct{}{}
		}
		registry.connectors = append(registry.connectors, registeredConnector{
			config: connector, acceptedEndpoints: accepted,
			egressEndpoint: connector.EgressEndpoint, timeout: timeout,
		})
	}
	return registry, nil
}

func (r *Registry) Enabled() bool {
	return r != nil && r.enabled && len(r.connectors) > 0
}

// Pull resolves exactly one configured connector, calls only its fixed egress
// endpoint, validates the closed source response, and returns canonical JSON.
func (r *Registry) Pull(ctx context.Context, input PullRequest) (PullResult, error) {
	if !r.Enabled() {
		return PullResult{}, ErrDisabled
	}
	if err := validateLocator(input.Locator); err != nil {
		return PullResult{}, err
	}
	if !validSourceToken(input.SourceToken) {
		return PullResult{}, ErrSourceCredentialRejected
	}
	connector, err := r.resolve(input.SourceInstanceID, input.RuntimeEndpointHint)
	if err != nil {
		return PullResult{}, err
	}

	pullCtx, cancel := context.WithTimeout(ctx, connector.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pullCtx, http.MethodGet, connector.egressEndpoint, nil)
	if err != nil {
		return PullResult{}, ErrSourceUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+input.SourceToken)
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	req.Header.Del("Authorization")
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if errors.Is(err, ErrSourceRedirectRejected) {
			return PullResult{}, ErrSourceRedirectRejected
		}
		return PullResult{}, ErrSourceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return PullResult{}, ErrSourceCredentialRejected
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return PullResult{}, ErrSourceRedirectRejected
	}
	if resp.StatusCode != http.StatusOK {
		return PullResult{}, ErrSourceUnavailable
	}
	if !hasNoStore(resp.Header.Get("Cache-Control")) || !isJSONContentType(resp.Header.Get("Content-Type")) {
		return PullResult{}, ErrSourceContextInvalid
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxContextBodyBytes+1))
	if err != nil || int64(len(body)) > maxContextBodyBytes {
		return PullResult{}, ErrSourceContextInvalid
	}
	current, err := decodeCurrentContext(body)
	if err != nil || validateCurrentContext(current, input.Locator, connector.config.WorkspaceMappings) != nil {
		return PullResult{}, ErrSourceContextInvalid
	}
	canonical, err := json.Marshal(current)
	if err != nil || bytes.Contains(canonical, []byte(input.SourceToken)) {
		return PullResult{}, ErrSourceContextInvalid
	}
	digestBytes := sha256.Sum256(canonical)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	workspaceRef := current.Workspace.ID
	if mapped := connector.config.WorkspaceMappings[current.Workspace.ID]; mapped != "" {
		workspaceRef = mapped
	}
	ref := SourceRef{
		Schema: SourceRefSchema, SourceInstanceID: connector.config.SourceInstanceID,
		Adapter: connector.config.Adapter, WorkspaceID: current.Workspace.ID,
		WorkspaceRef: workspaceRef, AgentID: current.Agent.ID, TaskID: current.Task.ID,
		RunID: current.Run.ID, ObservedAt: current.ObservedAt,
	}
	if current.Issue != nil {
		ref.IssueID = current.Issue.ID
	}
	if current.Runtime != nil {
		ref.RuntimeID = current.Runtime.ID
		ref.DaemonID = current.Runtime.DaemonID
	}
	return PullResult{SourceRef: ref, Context: current, ContextJSON: canonical, ContextDigest: digest}, nil
}

func (r *Registry) resolve(sourceInstanceID, runtimeEndpointHint string) (registeredConnector, error) {
	if sourceInstanceID != strings.TrimSpace(sourceInstanceID) || runtimeEndpointHint != strings.TrimSpace(runtimeEndpointHint) {
		return registeredConnector{}, ErrInvalidLocator
	}
	if sourceInstanceID != "" && !sourceInstancePattern.MatchString(sourceInstanceID) {
		return registeredConnector{}, ErrInvalidLocator
	}
	var hint string
	var err error
	if runtimeEndpointHint != "" {
		hint, err = normalizeRuntimeEndpoint(runtimeEndpointHint)
		if err != nil {
			return registeredConnector{}, ErrConnectorNotFound
		}
	}
	if sourceInstanceID == "" && hint == "" {
		return registeredConnector{}, ErrInvalidLocator
	}
	matches := make([]registeredConnector, 0, 1)
	for _, connector := range r.connectors {
		if sourceInstanceID != "" && connector.config.SourceInstanceID != sourceInstanceID {
			continue
		}
		if hint != "" {
			if _, ok := connector.acceptedEndpoints[hint]; !ok {
				continue
			}
		}
		matches = append(matches, connector)
	}
	switch len(matches) {
	case 0:
		return registeredConnector{}, ErrConnectorNotFound
	case 1:
		return matches[0], nil
	default:
		return registeredConnector{}, ErrConnectorAmbiguous
	}
}

func decodeCurrentContext(body []byte) (CurrentContext, error) {
	var current CurrentContext
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&current); err != nil {
		return CurrentContext{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return CurrentContext{}, errors.New("trailing JSON")
	}
	normalizeCurrentContextDualRead(&current)
	return current, nil
}

// normalizeCurrentContextDualRead prefers claim.generation as the generation
// coordinate and mirrors it onto run.id during the dual-read window.
// v2 requires claim.generation on the wire. v1 may project claim from run.id as
// an additive dual-read field on the normalized snapshot (ABI additive; digests
// for pure-v1 wire will therefore include claim after upgrade).
func normalizeCurrentContextDualRead(current *CurrentContext) {
	if current == nil {
		return
	}
	if current.Claim != nil && current.Claim.Generation != "" {
		if current.Run.ID == "" {
			current.Run.ID = current.Claim.Generation
		}
		if current.Run.TaskID == "" {
			current.Run.TaskID = current.Claim.TaskID
		}
		if current.Claim.TaskID == "" {
			current.Claim.TaskID = current.Task.ID
		}
		return
	}
	if current.Schema == MulticaCurrentExecutionContextSchema && current.Run.ID != "" {
		current.Claim = &Claim{Generation: current.Run.ID, TaskID: current.Task.ID}
	}
}

func acceptedCurrentContextSchema(schema string) bool {
	return schema == MulticaCurrentExecutionContextSchema || schema == MulticaCurrentExecutionContextSchemaV2
}

func validateCurrentContext(current CurrentContext, locator Locator, workspaceMappings map[string]string) error {
	if !acceptedCurrentContextSchema(current.Schema) {
		return errors.New("schema mismatch")
	}
	if _, err := time.Parse(time.RFC3339Nano, current.ObservedAt); err != nil {
		return errors.New("observed_at is invalid")
	}
	if current.Workspace.ID != locator.WorkspaceID || current.Agent.ID != locator.AgentID || current.Task.ID != locator.TaskID {
		return errors.New("locator mismatch")
	}
	if current.Workspace.Slug == "" {
		return errors.New("workspace slug is missing")
	}
	if len(workspaceMappings) > 0 {
		if _, ok := workspaceMappings[current.Workspace.ID]; !ok {
			return errors.New("workspace is not mapped")
		}
	}
	if current.Task.Status != "running" || current.Task.Attempt < 1 {
		return errors.New("task is not running")
	}
	if current.Schema == MulticaCurrentExecutionContextSchemaV2 {
		if current.Claim == nil || current.Claim.Generation == "" {
			return errors.New("claim generation is missing")
		}
	}
	generation := current.Run.ID
	if current.Claim != nil && current.Claim.Generation != "" {
		generation = current.Claim.Generation
	}
	if !canonicalUUIDPattern.MatchString(generation) {
		return errors.New("claim generation is invalid")
	}
	if current.Runtime != nil {
		if current.Runtime.ID != "" && !canonicalUUIDPattern.MatchString(current.Runtime.ID) {
			return errors.New("runtime id is invalid")
		}
		if current.Runtime.DaemonID != "" && !canonicalUUIDPattern.MatchString(current.Runtime.DaemonID) {
			return errors.New("runtime daemon id is invalid")
		}
		if current.Runtime.DaemonID != "" && current.Runtime.ID == "" {
			return errors.New("runtime daemon id requires runtime id")
		}
	}
	if current.Run.TaskID != "" && current.Run.TaskID != current.Task.ID {
		return errors.New("run task mismatch")
	}
	if current.Claim != nil && current.Claim.TaskID != "" && current.Claim.TaskID != current.Task.ID {
		return errors.New("claim task mismatch")
	}
	if current.Claim != nil && current.Claim.Generation != "" && current.Run.ID != "" && current.Claim.Generation != current.Run.ID {
		return errors.New("claim/run dual-read mismatch")
	}
	// v1 keeps the richer run status/attempt contract; v2 only needs generation + running task.
	if current.Schema == MulticaCurrentExecutionContextSchema {
		if current.Run.Status != "running" || current.Run.TaskID != current.Task.ID {
			return errors.New("task is not running")
		}
		if current.Task.MaxAttempts < current.Task.Attempt || current.Run.Attempt != current.Task.Attempt || current.Run.MaxAttempts != current.Task.MaxAttempts {
			return errors.New("run facts are invalid")
		}
	}
	if current.Attribution == nil || current.Attribution.Source == "" {
		return errors.New("attribution is missing")
	}
	if current.Schema == MulticaCurrentExecutionContextSchemaV2 {
		if err := validateMinimalV2ClosedFields(current); err != nil {
			return err
		}
	}
	return nil
}

// validateMinimalV2ClosedFields rejects v1 display enrichment on v2 wire so the
// dual-read struct union cannot re-admit names/PII into AGS snapshots.
func validateMinimalV2ClosedFields(current CurrentContext) error {
	if current.Workspace.Name != "" {
		return errors.New("v2 workspace display fields are not allowed")
	}
	if current.Agent.Name != "" || current.Agent.Status != "" {
		return errors.New("v2 agent display fields are not allowed")
	}
	if current.Task.MaxAttempts != 0 || current.Task.CreatedAt != "" || current.Task.DispatchedAt != "" || current.Task.StartedAt != "" || current.Task.CompletedAt != "" || current.Task.ParentTaskID != "" {
		return errors.New("v2 task enrichment fields are not allowed")
	}
	if current.Run.Status != "" || current.Run.Attempt != 0 || current.Run.MaxAttempts != 0 || current.Run.CreatedAt != "" || current.Run.DispatchedAt != "" || current.Run.StartedAt != "" || current.Run.CompletedAt != "" {
		return errors.New("v2 run enrichment fields are not allowed")
	}
	if current.Issue != nil && (current.Issue.Title != "" || current.Issue.Status != "" || current.Issue.CreatedAt != "" || current.Issue.UpdatedAt != "") {
		return errors.New("v2 issue display fields are not allowed")
	}
	if current.Squad != nil && (current.Squad.Name != "" || current.Squad.DetailsAvailable) {
		return errors.New("v2 squad display fields are not allowed")
	}
	if current.Runtime != nil && (current.Runtime.Name != "" || current.Runtime.CustomName != "" || current.Runtime.Provider != "" || current.Runtime.Status != "" || current.Runtime.DetailsAvailable) {
		return errors.New("v2 runtime display fields are not allowed")
	}
	if current.Trigger != nil && (current.Trigger.CommentID != "" || current.Trigger.AutopilotRunID != "") {
		return errors.New("v2 trigger enrichment fields are not allowed")
	}
	if current.Attribution != nil {
		for _, user := range []*AttributionUser{current.Attribution.Initiator, current.Attribution.Originator} {
			if user == nil {
				continue
			}
			if user.Name != "" || user.Email != "" || user.AvatarURL != "" {
				return errors.New("v2 attribution display fields are not allowed")
			}
		}
	}
	return nil
}

func validateLocator(locator Locator) error {
	for _, value := range []string{locator.WorkspaceID, locator.AgentID, locator.TaskID} {
		if !canonicalUUIDPattern.MatchString(value) {
			return ErrInvalidLocator
		}
	}
	return nil
}

func validSourceToken(token string) bool {
	return len(token) >= 8 && len(token) <= 4096 && token == strings.TrimSpace(token) && strings.HasPrefix(token, "mat_")
}

func normalizeRuntimeEndpoint(raw string) (string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return "", errors.New("invalid runtime endpoint")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("invalid runtime endpoint")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("invalid runtime endpoint")
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

func normalizeEgressEndpoint(raw string) (string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) {
		return "", errors.New("invalid egress endpoint")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Scheme == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Path != currentExecutionContextPath {
		return "", errors.New("invalid egress endpoint")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("invalid egress endpoint")
	}
	return scheme + "://" + strings.ToLower(u.Host) + currentExecutionContextPath, nil
}

func isJSONContentType(raw string) bool {
	mediaType, _, err := mime.ParseMediaType(raw)
	return err == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"))
}

func hasNoStore(raw string) bool {
	for _, directive := range strings.Split(raw, ",") {
		if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
			return true
		}
	}
	return false
}

func safeRef(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 255 && !strings.ContainsAny(value, "\r\n\t") && !secretShaped(value)
}

func secretShaped(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "mat_") || strings.HasPrefix(lower, "ags_sess_") || strings.HasPrefix(value, "eyJ") || strings.Contains(value, "-----BEGIN")
}
