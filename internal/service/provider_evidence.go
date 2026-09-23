package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

const (
	ProviderProjectionEvidenceSchema = "ags.provider-projection-evidence.v1"
	ProviderCIEvidenceSchema         = "ags.provider-ci-evidence.v1"
	ProviderObservationReceiptSchema = "ags.provider-observation-receipt.v1"
	AuditActionProviderEvidenceRead  = "provider.evidence.read"
)

var ErrProviderEvidenceUnavailable = errors.New("provider evidence is unavailable")

type ProviderEvidenceBinding struct {
	Kind           string `json:"kind"`
	ExternalRepo   string `json:"external_repo"`
	ExternalNumber int    `json:"external_number"`
	DisplayURL     string `json:"display_url"`
	BindingSource  string `json:"binding_source"`
}

type ProviderObservationAuthentication struct {
	Mode           string `json:"mode"`
	PrincipalID    uint   `json:"principal_id"`
	PrincipalLogin string `json:"principal_login"`
	SessionID      string `json:"session_id,omitempty"`
}

type ProviderObservationWorkload struct {
	IssuerInstanceID string `json:"issuer_instance_id,omitempty"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	TaskID           string `json:"task_id,omitempty"`
	RunID            string `json:"run_id,omitempty"`
	IssueID          string `json:"issue_id,omitempty"`
	IssueKey         string `json:"issue_key,omitempty"`
	RuntimeID        string `json:"runtime_id,omitempty"`
	CorrelationID    string `json:"correlation_id,omitempty"`
}

type ProviderObservationReceipt struct {
	Schema          string                            `json:"schema"`
	ID              string                            `json:"id"`
	Repository      string                            `json:"repository"`
	AGSPR           int                               `json:"ags_pr"`
	Operation       string                            `json:"operation"`
	Authorization   string                            `json:"authorization"`
	ProviderAttempt string                            `json:"provider_attempt"`
	ProviderOutcome string                            `json:"provider_outcome"`
	RecoveryOwner   string                            `json:"recovery_owner"`
	Provider        ProviderEvidenceBinding           `json:"provider"`
	Authentication  ProviderObservationAuthentication `json:"authentication"`
	Workload        ProviderObservationWorkload       `json:"workload"`
	ObservedAt      time.Time                         `json:"observed_at"`
}

type ProviderEvidenceReadError struct {
	Receipt ProviderObservationReceipt
	Cause   error
}

func (e *ProviderEvidenceReadError) Error() string { return "provider evidence read failed" }
func (e *ProviderEvidenceReadError) Unwrap() error { return e.Cause }

type ProviderProjectionObservation struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	MergeBaseSHA   string `json:"merge_base_sha"`
	HeadRef        string `json:"head_ref"`
	HeadSHA        string `json:"head_sha"`
	BaseRef        string `json:"base_ref"`
	BaseSHA        string `json:"base_sha"`
}

type ProviderWorkflowRunObservation struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	WorkflowID        string `json:"workflow_id"`
	Status            string `json:"status"`
	Event             string `json:"event"`
	PullRequestNumber int    `json:"pull_request_number"`
	HeadBranch        string `json:"head_branch"`
	SourceHeadBranch  string `json:"source_head_branch"`
	HeadSHA           string `json:"head_sha"`
	HTMLURL           string `json:"html_url"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

type ProviderProjectionEvidence struct {
	Schema             string                        `json:"schema"`
	Repository         string                        `json:"repository"`
	AGSPR              int                           `json:"ags_pr"`
	Provider           ProviderEvidenceBinding       `json:"provider"`
	Observed           ProviderProjectionObservation `json:"observed"`
	ObservedAt         time.Time                     `json:"observed_at"`
	CorrelationReceipt ProviderObservationReceipt    `json:"correlation_receipt"`
}

type ProviderCIEvidence struct {
	Schema             string                           `json:"schema"`
	Repository         string                           `json:"repository"`
	AGSPR              int                              `json:"ags_pr"`
	HeadSHA            string                           `json:"head_sha"`
	Provider           ProviderEvidenceBinding          `json:"provider"`
	Runs               []ProviderWorkflowRunObservation `json:"runs"`
	ObservedAt         time.Time                        `json:"observed_at"`
	CorrelationReceipt ProviderObservationReceipt       `json:"correlation_receipt"`
}

func (s *Service) ReadProviderProjectionEvidence(ctx context.Context, pr db.PullRequest) (ProviderProjectionEvidence, error) {
	receipt := newProviderObservationReceipt(ctx, pr, "pr.read")
	binding, err := s.forgejoEvidenceBinding(ctx, pr)
	if err != nil {
		return ProviderProjectionEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, err)
	}
	receipt.Provider = binding
	if s.ForgejoIntegration == nil {
		return ProviderProjectionEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, ErrProviderEvidenceUnavailable)
	}
	if err := s.revalidateProviderEvidenceRead(ctx, pr, binding, "pr.read"); err != nil {
		return ProviderProjectionEvidence{}, err
	}
	receipt.ProviderAttempt = "attempted"
	observed, found, err := s.ForgejoIntegration.ExactPullRequestSnapshot(ctx, pr.Repository.FullName, binding.ExternalRepo, binding.ExternalNumber)
	if err != nil {
		receipt.ProviderOutcome = "outcome_unknown"
		return ProviderProjectionEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, fmt.Errorf("read provider pull request: %w", err))
	}
	if !found {
		receipt.ProviderOutcome = "not_found"
		return ProviderProjectionEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, ErrProviderEvidenceUnavailable)
	}
	observation, err := providerProjectionObservation(observed)
	if err != nil || observation.Number != binding.ExternalNumber {
		receipt.ProviderOutcome = "unsafe_observation"
		if err == nil {
			err = errors.New("provider pull request observation number does not match binding")
		}
		return ProviderProjectionEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, err)
	}
	binding.DisplayURL = observation.URL
	receipt.Provider = binding
	receipt.ProviderOutcome = "observed"
	receipt.RecoveryOwner = "none"
	receipt.ObservedAt = time.Now().UTC()
	if err := s.logProviderEvidenceReceipt(ctx, pr, receipt); err != nil {
		receipt.RecoveryOwner = "ags_operator"
		return ProviderProjectionEvidence{}, &ProviderEvidenceReadError{Receipt: receipt, Cause: fmt.Errorf("provider evidence audit: %w", err)}
	}
	return ProviderProjectionEvidence{
		Schema: ProviderProjectionEvidenceSchema, Repository: pr.Repository.FullName, AGSPR: pr.Number,
		Provider: binding, Observed: observation, ObservedAt: receipt.ObservedAt, CorrelationReceipt: receipt,
	}, nil
}

func (s *Service) ReadProviderCIEvidence(ctx context.Context, pr db.PullRequest) (ProviderCIEvidence, error) {
	receipt := newProviderObservationReceipt(ctx, pr, "ci.read")
	binding, err := s.forgejoEvidenceBinding(ctx, pr)
	if err != nil {
		return ProviderCIEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, err)
	}
	receipt.Provider = binding
	if s.ForgejoIntegration == nil {
		return ProviderCIEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, ErrProviderEvidenceUnavailable)
	}
	if err := s.revalidateProviderEvidenceRead(ctx, pr, binding, "ci.read"); err != nil {
		return ProviderCIEvidence{}, err
	}
	receipt.ProviderAttempt = "attempted"
	runs, supported, err := s.ForgejoIntegration.WorkflowRunsForPullRequest(ctx, pr.Repository.FullName, binding.ExternalRepo, binding.ExternalNumber, pr.HeadRef, pr.HeadSHA, 100)
	if err != nil {
		receipt.ProviderOutcome = "outcome_unknown"
		return ProviderCIEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, fmt.Errorf("read provider CI runs: %w", err))
	}
	if !supported {
		receipt.ProviderOutcome = "not_supported"
		return ProviderCIEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, ErrProviderEvidenceUnavailable)
	}
	if runs == nil {
		runs = []forgejointegration.WorkflowRun{}
	}
	observations, err := providerWorkflowRunObservations(runs, binding.ExternalNumber, pr.HeadRef)
	if err != nil {
		receipt.ProviderOutcome = "unsafe_observation"
		return ProviderCIEvidence{}, s.providerEvidenceFailure(ctx, pr, receipt, err)
	}
	receipt.ProviderOutcome = "observed"
	receipt.RecoveryOwner = "none"
	receipt.ObservedAt = time.Now().UTC()
	if err := s.logProviderEvidenceReceipt(ctx, pr, receipt); err != nil {
		receipt.RecoveryOwner = "ags_operator"
		return ProviderCIEvidence{}, &ProviderEvidenceReadError{Receipt: receipt, Cause: fmt.Errorf("provider evidence audit: %w", err)}
	}
	return ProviderCIEvidence{
		Schema: ProviderCIEvidenceSchema, Repository: pr.Repository.FullName, AGSPR: pr.Number, HeadSHA: pr.HeadSHA,
		Provider: binding, Runs: observations, ObservedAt: receipt.ObservedAt, CorrelationReceipt: receipt,
	}, nil
}

const ProviderCIRunLogsSchema = "ags.provider-ci-run-logs.v1"

type ProviderCIRunLogs struct {
	Schema             string                     `json:"schema"`
	Repository         string                     `json:"repository"`
	AGSPR              int                        `json:"ags_pr"`
	RunID              int64                      `json:"run_id"`
	Text               string                     `json:"text"`
	ObservedAt         time.Time                  `json:"observed_at"`
	CorrelationReceipt ProviderObservationReceipt `json:"correlation_receipt"`
}

func (s *Service) ReadProviderCIRunLogs(ctx context.Context, pr db.PullRequest, runID int64) (ProviderCIRunLogs, error) {
	receipt := newProviderObservationReceipt(ctx, pr, "ci.read")
	binding, err := s.forgejoEvidenceBinding(ctx, pr)
	if err != nil {
		return ProviderCIRunLogs{}, s.providerEvidenceFailure(ctx, pr, receipt, err)
	}
	receipt.Provider = binding
	if s.ForgejoIntegration == nil {
		return ProviderCIRunLogs{}, s.providerEvidenceFailure(ctx, pr, receipt, ErrProviderEvidenceUnavailable)
	}
	if err := s.revalidateProviderEvidenceRead(ctx, pr, binding, "ci.read"); err != nil {
		return ProviderCIRunLogs{}, err
	}
	receipt.ProviderAttempt = "attempted"
	logs, supported, err := s.ForgejoIntegration.WorkflowRunLogs(ctx, pr.Repository.FullName, binding.ExternalRepo, binding.ExternalNumber, pr.HeadRef, pr.HeadSHA, runID)
	if err != nil {
		receipt.ProviderOutcome = "outcome_unknown"
		return ProviderCIRunLogs{}, s.providerEvidenceFailure(ctx, pr, receipt, fmt.Errorf("read provider CI run logs: %w", err))
	}
	if !supported {
		receipt.ProviderOutcome = "not_supported"
		return ProviderCIRunLogs{}, s.providerEvidenceFailure(ctx, pr, receipt, ErrProviderEvidenceUnavailable)
	}
	if len(logs) == 0 {
		receipt.ProviderOutcome = "not_found"
		return ProviderCIRunLogs{}, s.providerEvidenceFailure(ctx, pr, receipt, ErrProviderEvidenceUnavailable)
	}
	if safeDelegatedAuthorityText(string(logs)) == "redacted" {
		receipt.ProviderOutcome = "unsafe_observation"
		return ProviderCIRunLogs{}, s.providerEvidenceFailure(ctx, pr, receipt, errors.New("provider CI log contains unsafe text"))
	}
	receipt.ProviderOutcome = "observed"
	receipt.RecoveryOwner = "none"
	receipt.ObservedAt = time.Now().UTC()
	if err := s.logProviderEvidenceReceipt(ctx, pr, receipt); err != nil {
		receipt.RecoveryOwner = "ags_operator"
		return ProviderCIRunLogs{}, &ProviderEvidenceReadError{Receipt: receipt, Cause: fmt.Errorf("provider evidence audit: %w", err)}
	}
	return ProviderCIRunLogs{
		Schema: ProviderCIRunLogsSchema, Repository: pr.Repository.FullName, AGSPR: pr.Number, RunID: runID,
		Text: string(logs), ObservedAt: receipt.ObservedAt, CorrelationReceipt: receipt,
	}, nil
}

func (s *Service) revalidateProviderEvidenceRead(ctx context.Context, pr db.PullRequest, binding ProviderEvidenceBinding, operation string) error {
	if user, ok := UserFromContext(ctx); !ok || user.ID == 0 {
		return ErrUnauthorized
	}
	if _, delegated := DelegatedSessionIDFromContext(ctx); !delegated {
		return nil
	}
	actual := map[string]string{"pull_request_number": fmt.Sprint(pr.Number)}
	if operation == "ci.read" {
		actual["forgejo_pull_request_number"] = fmt.Sprint(binding.ExternalNumber)
		actual["head_sha"] = pr.HeadSHA
	}
	_, err := s.RevalidateDelegatedSession(ctx, pr.RepositoryID, operation, "repo:read", actual)
	return err
}

func providerProjectionObservation(observed forgejointegration.PullRequestSnapshot) (ProviderProjectionObservation, error) {
	displayURL, err := providerDisplayURL(observed.URL)
	if err != nil {
		return ProviderProjectionObservation{}, err
	}
	values := []string{observed.State, observed.MergeCommitSHA, observed.MergeBaseSHA, observed.HeadRef, observed.HeadSHA, observed.BaseRef, observed.BaseSHA}
	for _, value := range values {
		if len(value) > 1024 || safeDelegatedAuthorityText(value) == "redacted" {
			return ProviderProjectionObservation{}, errors.New("provider pull request observation contains unsafe text")
		}
	}
	return ProviderProjectionObservation{
		Number: observed.Number, URL: displayURL, State: observed.State, Merged: observed.Merged,
		MergeCommitSHA: observed.MergeCommitSHA, MergeBaseSHA: observed.MergeBaseSHA,
		HeadRef: observed.HeadRef, HeadSHA: observed.HeadSHA, BaseRef: observed.BaseRef, BaseSHA: observed.BaseSHA,
	}, nil
}

func providerWorkflowRunObservations(runs []forgejointegration.WorkflowRun, forgejoPRNumber int, agsHeadRef string) ([]ProviderWorkflowRunObservation, error) {
	observations := make([]ProviderWorkflowRunObservation, 0, len(runs))
	bindingRef := fmt.Sprintf("#%d", forgejoPRNumber)
	sourceRef := strings.TrimSpace(agsHeadRef)
	for _, run := range runs {
		displayURL, err := providerDisplayURL(firstNonEmpty(run.HTMLURL, run.URL))
		if err != nil {
			return nil, err
		}
		values := []string{run.Name, run.WorkflowID, run.Status, run.Event, run.HeadBranch, run.HeadSHA, run.CreatedAt, run.UpdatedAt}
		for _, value := range values {
			if len(value) > 1024 || safeDelegatedAuthorityText(value) == "redacted" {
				return nil, errors.New("provider workflow observation contains unsafe text")
			}
		}
		headBranch := strings.TrimSpace(run.HeadBranch)
		if headBranch == sourceRef {
			headBranch = bindingRef
		}
		observations = append(observations, ProviderWorkflowRunObservation{
			ID: run.ID, Name: run.Name, WorkflowID: run.WorkflowID, Status: run.Status, Event: run.Event,
			PullRequestNumber: forgejoPRNumber, HeadBranch: headBranch, SourceHeadBranch: run.HeadBranch,
			HeadSHA: run.HeadSHA, HTMLURL: displayURL, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
		})
	}
	return observations, nil
}

func providerDisplayURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > 2048 || safeDelegatedAuthorityText(raw) == "redacted" {
		return "", errors.New("provider display URL is unsafe")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errors.New("provider display URL is unsafe")
	}
	return parsed.String(), nil
}

func newProviderObservationReceipt(ctx context.Context, pr db.PullRequest, operation string) ProviderObservationReceipt {
	receipt := ProviderObservationReceipt{
		Schema: ProviderObservationReceiptSchema, ID: uuid.NewString(), Repository: pr.Repository.FullName,
		AGSPR: pr.Number, Operation: operation, Authorization: "allowed", ProviderAttempt: "not_attempted",
		ProviderOutcome: "not_observed", RecoveryOwner: "ags_operator", ObservedAt: time.Now().UTC(),
		Authentication: ProviderObservationAuthentication{Mode: "durable"},
	}
	if user, ok := UserFromContext(ctx); ok {
		receipt.Authentication.PrincipalID = user.ID
		receipt.Authentication.PrincipalLogin = safeDelegatedAuditText(user.Login)
	}
	if session, ok := DelegatedSessionFromContext(ctx); ok {
		receipt.Authentication.Mode = accessGrantTransportCredentialMode
		receipt.Authentication.PrincipalID = session.PrincipalUserID
		receipt.Authentication.PrincipalLogin = safeDelegatedAuditText(session.PrincipalLogin)
		receipt.Authentication.SessionID = safeDelegatedAuditText(session.ID)
		receipt.Workload = ProviderObservationWorkload{
			IssuerInstanceID: safeDelegatedAuditText(session.IssuerInstanceID), WorkspaceID: safeDelegatedAuditText(session.IssuerWorkspaceID),
			AgentID: safeDelegatedAuditText(session.ExternalAgentID), TaskID: safeDelegatedAuditText(session.ExternalTaskID), RunID: safeDelegatedAuditText(session.ExternalRunID),
			IssueID: safeDelegatedAuditText(session.ExternalIssueID), IssueKey: safeDelegatedAuditText(session.ExternalIssueKey), RuntimeID: safeDelegatedAuditText(session.ExternalRuntimeID),
			CorrelationID: safeDelegatedAuditText(session.CorrelationID),
		}
	}
	return receipt
}

func (s *Service) providerEvidenceFailure(ctx context.Context, pr db.PullRequest, receipt ProviderObservationReceipt, cause error) error {
	receipt.RecoveryOwner = "ags_operator"
	receipt.ObservedAt = time.Now().UTC()
	_ = s.logProviderEvidenceReceipt(ctx, pr, receipt)
	return &ProviderEvidenceReadError{Receipt: receipt, Cause: cause}
}

func (s *Service) logProviderEvidenceReceipt(ctx context.Context, pr db.PullRequest, receipt ProviderObservationReceipt) error {
	details, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := validateSecretSafeReceiptJSON(details); err != nil {
		return err
	}
	return s.LogAudit(ctx, AuditEvent{
		Action: AuditActionProviderEvidenceRead, RepositoryFullName: pr.Repository.FullName,
		TargetLogin: receipt.ID, Details: string(details),
	})
}

func (s *Service) forgejoEvidenceBinding(ctx context.Context, pr db.PullRequest) (ProviderEvidenceBinding, error) {
	rows, err := s.ListPullRequestProjections(ctx, pr.ID)
	if err != nil {
		return ProviderEvidenceBinding{}, err
	}
	var selected *db.PullRequestProjection
	for index := range rows {
		row := &rows[index]
		if !strings.EqualFold(strings.TrimSpace(row.Provider), ProjectionProviderForgejo) {
			continue
		}
		if selected != nil {
			return ProviderEvidenceBinding{}, fmt.Errorf("%w: ambiguous Forgejo projection", ErrProviderEvidenceUnavailable)
		}
		selected = row
	}
	if selected == nil || selected.ExternalNumber <= 0 || strings.TrimSpace(selected.ExternalRepo) == "" {
		return ProviderEvidenceBinding{}, ErrProviderEvidenceUnavailable
	}
	displayURL, err := providerDisplayURL(selected.ExternalURL)
	if err != nil {
		return ProviderEvidenceBinding{}, err
	}
	return ProviderEvidenceBinding{
		Kind: ProjectionProviderForgejo, ExternalRepo: selected.ExternalRepo, ExternalNumber: selected.ExternalNumber,
		DisplayURL: displayURL, BindingSource: "legacy_repo_mapping",
	}, nil
}
