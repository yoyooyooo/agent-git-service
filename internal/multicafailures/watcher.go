package multicafailures

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"github.com/ngaut/agent-git-service/internal/service"
)

// Config controls polling Multica task failures into AGS incidents.
type Config struct {
	Enabled           bool
	Command           string
	Profile           string
	Workspace         string
	WorkspaceID       string
	AppURL            string
	PollInterval      time.Duration
	IssueLimit        int
	RollingWindowDays int
}

// Recorder is the AGS incident ingestion boundary used by the watcher.
type Recorder interface {
	RecordMulticaTaskFailure(context.Context, service.MulticaTaskFailedInput) (service.MulticaIncidentResult, error)
}

// Runner executes the Multica CLI. Tests replace it with a fake runner.
type Runner interface {
	Run(context.Context, ...string) ([]byte, error)
}

// Option customizes a watcher.
type Option func(*Watcher)

// WithRunner injects a command runner for tests.
func WithRunner(r Runner) Option {
	return func(w *Watcher) {
		if r != nil {
			w.runner = r
		}
	}
}

// WithLogger injects a logger for tests or embedders.
func WithLogger(logger *slog.Logger) Option {
	return func(w *Watcher) {
		if logger != nil {
			w.logger = logger
		}
	}
}

// Watcher polls Multica issue run history and records failed task facts in AGS.
type Watcher struct {
	cfg      Config
	recorder Recorder
	runner   Runner
	logger   *slog.Logger
}

type execRunner struct {
	command string
}

func (r execRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.command, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s failed: %w: %s", r.command, strings.Join(args, " "), err, truncate(string(out), 600))
	}
	return out, nil
}

// PollResult summarizes one watcher pass.
type PollResult struct {
	IssuesScanned    int
	IssuesSkipped    int
	RunsScanned      int
	FailedRuns       int
	Accepted         int
	Duplicates       int
	RecordErrors     int
	StaleRunsSkipped int
}

// New constructs a watcher. Disabled config returns nil.
func New(cfg Config, recorder Recorder, opts ...Option) *Watcher {
	if !cfg.Enabled {
		return nil
	}
	if strings.TrimSpace(cfg.Command) == "" {
		cfg.Command = "multica"
	}
	if strings.TrimSpace(cfg.Profile) == "" {
		cfg.Profile = "ags-multica-projection"
	}
	if strings.TrimSpace(cfg.AppURL) == "" {
		cfg.AppURL = "https://multica.ai"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Minute
	}
	if cfg.IssueLimit <= 0 {
		cfg.IssueLimit = 50
	}
	if cfg.RollingWindowDays <= 0 {
		cfg.RollingWindowDays = 30
	}
	w := &Watcher{
		cfg:      cfg,
		recorder: recorder,
		runner:   execRunner{command: cfg.Command},
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(w)
		}
	}
	return w
}

// Run polls immediately and then on the configured interval until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	if w == nil {
		return
	}
	w.pollAndLog(ctx)
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollAndLog(ctx)
		}
	}
}

func (w *Watcher) pollAndLog(ctx context.Context) {
	result, err := w.PollOnce(ctx)
	if err != nil {
		w.logger.Warn("multica failure watcher poll failed", "error", err, "issues", result.IssuesScanned, "runs", result.RunsScanned, "accepted", result.Accepted, "duplicates", result.Duplicates, "record_errors", result.RecordErrors)
		return
	}
	w.logger.Info("multica failure watcher poll completed", "issues", result.IssuesScanned, "runs", result.RunsScanned, "failed_runs", result.FailedRuns, "accepted", result.Accepted, "duplicates", result.Duplicates)
}

// PollOnce reads recent Multica issues and their run history once.
func (w *Watcher) PollOnce(ctx context.Context) (PollResult, error) {
	var result PollResult
	if w == nil || w.recorder == nil {
		return result, nil
	}
	if strings.TrimSpace(w.cfg.Workspace) != "" && strings.TrimSpace(w.cfg.WorkspaceID) == "" {
		if _, err := w.runCLI(ctx, "", "workspace", "switch", w.cfg.Workspace); err != nil {
			return result, fmt.Errorf("switch multica workspace: %w", err)
		}
	}
	issues, err := w.listIssues(ctx)
	if err != nil {
		return result, err
	}
	var recordErrs []error
	cutoff := time.Now().UTC().AddDate(0, 0, -w.cfg.RollingWindowDays)
	for _, issue := range issues {
		result.IssuesScanned++
		repoFullName := metadataString(issue.Metadata, "ags_repo")
		if repoFullName == "" {
			result.IssuesSkipped++
			continue
		}
		runs, err := w.listRuns(ctx, issue.key())
		if err != nil {
			recordErrs = append(recordErrs, fmt.Errorf("list runs for %s: %w", issue.key(), err))
			continue
		}
		for _, run := range runs {
			result.RunsScanned++
			if !strings.EqualFold(strings.TrimSpace(run.Status), "failed") {
				continue
			}
			input := w.toTaskFailureInput(issue, run, repoFullName)
			if !input.OccurredAt.IsZero() && input.OccurredAt.Before(cutoff) {
				result.StaleRunsSkipped++
				continue
			}
			result.FailedRuns++
			recorded, err := w.recorder.RecordMulticaTaskFailure(ctx, input)
			if err != nil {
				result.RecordErrors++
				recordErrs = append(recordErrs, fmt.Errorf("record failed task %s: %w", run.ID, err))
				continue
			}
			if recorded.EventAccepted {
				result.Accepted++
			} else {
				result.Duplicates++
			}
		}
	}
	return result, errors.Join(recordErrs...)
}

func (w *Watcher) listIssues(ctx context.Context) ([]multicaIssue, error) {
	out, err := w.runCLI(ctx, w.cfg.WorkspaceID, "issue", "list", "--limit", strconv.Itoa(w.cfg.IssueLimit), "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("list multica issues: %w", err)
	}
	var wrapped issueListResponse
	if err := json.Unmarshal(out, &wrapped); err == nil && wrapped.Issues != nil {
		return wrapped.Issues, nil
	}
	var direct []multicaIssue
	if err := json.Unmarshal(out, &direct); err != nil {
		return nil, fmt.Errorf("decode multica issue list: %w", err)
	}
	return direct, nil
}

func (w *Watcher) listRuns(ctx context.Context, issueKey string) ([]multicaRun, error) {
	out, err := w.runCLI(ctx, w.cfg.WorkspaceID, "issue", "runs", issueKey, "--output", "json")
	if err != nil {
		return nil, err
	}
	var direct []multicaRun
	if err := json.Unmarshal(out, &direct); err == nil {
		return direct, nil
	}
	var wrapped runListResponse
	if err := json.Unmarshal(out, &wrapped); err != nil {
		return nil, fmt.Errorf("decode multica runs: %w", err)
	}
	return wrapped.Runs, nil
}

func (w *Watcher) runCLI(ctx context.Context, workspaceID string, args ...string) ([]byte, error) {
	cmdArgs := make([]string, 0, len(args)+4)
	if profile := strings.TrimSpace(w.cfg.Profile); profile != "" && profile != "-" {
		cmdArgs = append(cmdArgs, "--profile", profile)
	}
	if strings.TrimSpace(workspaceID) != "" {
		cmdArgs = append(cmdArgs, "--workspace-id", strings.TrimSpace(workspaceID))
	}
	cmdArgs = append(cmdArgs, args...)
	return w.runner.Run(ctx, cmdArgs...)
}

func (w *Watcher) toTaskFailureInput(issue multicaIssue, run multicaRun, repoFullName string) service.MulticaTaskFailedInput {
	issueKey := firstNonEmpty(issue.Identifier, issue.ID)
	agentName := firstNonEmpty(run.AgentName, run.AgentID)
	return service.MulticaTaskFailedInput{
		RepoFullName:      repoFullName,
		WorkspaceID:       firstNonEmpty(issue.WorkspaceID, run.WorkspaceID),
		MulticaIssueKey:   issueKey,
		MulticaIssueTitle: issue.Title,
		TaskID:            run.ID,
		AgentID:           run.AgentID,
		AgentName:         agentName,
		FailureReason:     firstNonEmpty(run.FailureReason, "agent_error.unknown"),
		Error:             rawString(run.Error),
		RuntimeProvider:   firstNonEmpty(run.RuntimeProvider, metadataString(issue.Metadata, "runtime_provider"), "unknown"),
		RuntimeID:         run.RuntimeID,
		Attempt:           run.Attempt,
		MaxAttempts:       run.MaxAttempts,
		OccurredAt:        run.occurredAt(),
		MulticaIssueURL:   firstNonEmpty(metadataString(issue.Metadata, "multica_issue_url"), multicaprojection.IssueURL(w.cfg.AppURL, w.cfg.Workspace, issueKey)),
		AGSPrURL:          metadataString(issue.Metadata, "ags_pr_url"),
	}
}

type issueListResponse struct {
	Issues []multicaIssue `json:"issues"`
}

type runListResponse struct {
	Runs []multicaRun `json:"runs"`
}

type multicaIssue struct {
	ID          string         `json:"id"`
	Identifier  string         `json:"identifier"`
	Title       string         `json:"title"`
	WorkspaceID string         `json:"workspace_id"`
	Metadata    map[string]any `json:"metadata"`
}

func (i multicaIssue) key() string {
	return firstNonEmpty(i.Identifier, i.ID)
}

type multicaRun struct {
	ID              string          `json:"id"`
	Status          string          `json:"status"`
	AgentID         string          `json:"agent_id"`
	AgentName       string          `json:"agent_name"`
	FailureReason   string          `json:"failure_reason"`
	Error           json.RawMessage `json:"error"`
	RuntimeProvider string          `json:"runtime_provider"`
	RuntimeID       string          `json:"runtime_id"`
	WorkspaceID     string          `json:"workspace_id"`
	Attempt         int             `json:"attempt"`
	MaxAttempts     int             `json:"max_attempts"`
	CompletedAt     *time.Time      `json:"completed_at"`
	StartedAt       *time.Time      `json:"started_at"`
	DispatchedAt    *time.Time      `json:"dispatched_at"`
	CreatedAt       *time.Time      `json:"created_at"`
}

func (r multicaRun) occurredAt() time.Time {
	for _, candidate := range []*time.Time{r.CompletedAt, r.StartedAt, r.DispatchedAt, r.CreatedAt} {
		if candidate != nil && !candidate.IsZero() {
			return candidate.UTC()
		}
	}
	return time.Time{}
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', -1, 64), "0"), ".")
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func rawString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err == nil {
		return compact.String()
	}
	return string(trimmed)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func truncate(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	if max <= 1 {
		return value[:max]
	}
	return value[:max-1] + "…"
}
