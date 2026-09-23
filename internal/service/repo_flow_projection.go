package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
)

// RepoFlowEnvProjectionRequest describes an AGS env/* ref update that should
// be projected to the configured deployment adapter.
type RepoFlowEnvProjectionRequest struct {
	RepoFullName string
	RepoPath     string
	Ref          string
	Before       string
	After        string
	Deleted      bool
}

// RepoFlowEnvProjectionResult records the deployment projection outcome.
type RepoFlowEnvProjectionResult struct {
	Handled      bool       `json:"handled"`
	RepoFullName string     `json:"repo,omitempty"`
	Env          string     `json:"env,omitempty"`
	SourceRef    string     `json:"source_ref,omitempty"`
	SourceSHA    string     `json:"source_sha,omitempty"`
	Provider     string     `json:"provider,omitempty"`
	ProjectPath  string     `json:"project_path,omitempty"`
	TargetBranch string     `json:"target_branch,omitempty"`
	PushedSHA    string     `json:"pushed_sha,omitempty"`
	State        string     `json:"state"`
	Error        string     `json:"error,omitempty"`
	TriggeredAt  *time.Time `json:"triggered_at,omitempty"`
	ProjectedAt  *time.Time `json:"projected_at,omitempty"`
}

// DispatchRepoFlowEnvProjection projects refs/heads/env/<env> updates from AGS
// to the configured deployment adapter. It is intentionally event-driven from
// the post-push ref changes; no polling is required.
func (s *Service) DispatchRepoFlowEnvProjection(ctx context.Context, req RepoFlowEnvProjectionRequest) (RepoFlowEnvProjectionResult, error) {
	if s == nil || s.GitLabIntegration == nil {
		return RepoFlowEnvProjectionResult{State: "skipped"}, nil
	}
	env, ok := repoFlowEnvFromRef(req.Ref)
	if !ok || req.Deleted {
		return RepoFlowEnvProjectionResult{State: "skipped"}, nil
	}
	triggeredAt := time.Now().UTC()
	pending := RepoFlowEnvProjectionResult{
		RepoFullName: req.RepoFullName,
		Env:          env,
		SourceRef:    req.Ref,
		SourceSHA:    req.After,
		Provider:     "gitlab",
		State:        "pending",
		TriggeredAt:  &triggeredAt,
	}
	s.recordRepoFlowEnvProjection(ctx, req.RepoFullName, env, req.Ref, req.After, pending)
	s.recordRepoFlowEnvEvidence(ctx, pending)
	res, handled, err := s.GitLabIntegration.ProjectEnv(ctx, gitlabintegration.EnvProjectionRequest{
		RepoFullName: req.RepoFullName,
		RepoPath:     req.RepoPath,
		Env:          env,
		SourceSHA:    req.After,
	})
	if err != nil {
		out := RepoFlowEnvProjectionResult{RepoFullName: req.RepoFullName, Env: env, SourceRef: req.Ref, SourceSHA: req.After, Provider: "gitlab", State: "failed", Error: err.Error(), TriggeredAt: &triggeredAt}
		s.recordRepoFlowEnvProjection(ctx, req.RepoFullName, env, req.Ref, req.After, out)
		s.recordRepoFlowEnvEvidence(ctx, out)
		return out, fmt.Errorf("repo-flow env projection: %w", err)
	}
	if !handled {
		out := RepoFlowEnvProjectionResult{RepoFullName: req.RepoFullName, Env: env, SourceRef: req.Ref, SourceSHA: req.After, Provider: "gitlab", State: "skipped", TriggeredAt: &triggeredAt}
		s.recordRepoFlowEnvProjection(ctx, req.RepoFullName, env, req.Ref, req.After, out)
		s.recordRepoFlowEnvEvidence(ctx, out)
		return out, nil
	}
	projectedAt := time.Now().UTC()
	out := RepoFlowEnvProjectionResult{
		Handled:      true,
		RepoFullName: req.RepoFullName,
		Env:          env,
		SourceRef:    req.Ref,
		SourceSHA:    req.After,
		Provider:     "gitlab",
		ProjectPath:  res.ProjectPath,
		TargetBranch: res.TargetBranch,
		PushedSHA:    res.PushedSHA,
		State:        "projected",
		TriggeredAt:  &triggeredAt,
		ProjectedAt:  &projectedAt,
	}
	s.recordRepoFlowEnvProjection(ctx, req.RepoFullName, env, req.Ref, req.After, out)
	s.recordRepoFlowEnvEvidence(ctx, out)
	slog.InfoContext(ctx, "repo-flow env projected", "repo", req.RepoFullName, "env", env, "target_branch", res.TargetBranch, "sha", res.PushedSHA)
	return out, nil
}

// GetRepoFlowEnvProjection returns the latest central status for one RepoFlow env.
func (s *Service) GetRepoFlowEnvProjection(ctx context.Context, repoFullName, env string) (RepoFlowEnvProjectionResult, error) {
	if s == nil || s.DB == nil {
		return RepoFlowEnvProjectionResult{}, gorm.ErrRecordNotFound
	}
	repo, err := s.LookupRepoIdentity(ctx, repoFullName)
	if err != nil {
		return RepoFlowEnvProjectionResult{}, err
	}
	var row db.RepoFlowEnvProjection
	if err := s.DBForCtx(ctx).Where("repository_id = ? AND env = ?", repo.ID, env).Take(&row).Error; err != nil {
		return RepoFlowEnvProjectionResult{}, err
	}
	return repoFlowProjectionRowResult(row), nil
}

// RetryRepoFlowEnvProjection replays projection for the current refs/heads/env/<env> SHA.
func (s *Service) RetryRepoFlowEnvProjection(ctx context.Context, repoFullName, env string) (RepoFlowEnvProjectionResult, error) {
	if s == nil || s.Git == nil {
		return RepoFlowEnvProjectionResult{}, fmt.Errorf("git store is not configured")
	}
	if !repoFlowValidEnv(env) {
		return RepoFlowEnvProjectionResult{}, fmt.Errorf("invalid env %q", env)
	}
	repoPath, err := s.Git.GetRepoPath(ctx, repoFullName)
	if err != nil {
		return RepoFlowEnvProjectionResult{}, err
	}
	ref := "refs/heads/env/" + env
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "rev-parse", ref).CombinedOutput()
	if err != nil {
		return RepoFlowEnvProjectionResult{}, fmt.Errorf("env ref %s not found: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	sha := strings.TrimSpace(string(out))
	return s.DispatchRepoFlowEnvProjection(ctx, RepoFlowEnvProjectionRequest{
		RepoFullName: repoFullName,
		RepoPath:     repoPath,
		Ref:          ref,
		After:        sha,
	})
}

// RecordRepoFlowEvidence appends a caller-provided record to the configured AGS evidence store.
func (s *Service) RecordRepoFlowEvidence(ctx context.Context, repoFullName string, record map[string]any) (map[string]any, error) {
	if s == nil || s.GitLabIntegration == nil {
		return nil, gorm.ErrRecordNotFound
	}
	out, handled, err := s.GitLabIntegration.RecordRepoFlowEvidence(ctx, repoFullName, record)
	if err != nil {
		return nil, err
	}
	if !handled {
		return nil, gorm.ErrRecordNotFound
	}
	return out, nil
}

// GetRepoFlowEvidenceStatus returns the configured AGS evidence store state.
func (s *Service) GetRepoFlowEvidenceStatus(ctx context.Context, repoFullName string) (gitlabintegration.RepoFlowEvidenceStatus, error) {
	if s == nil || s.GitLabIntegration == nil {
		return gitlabintegration.RepoFlowEvidenceStatus{}, gorm.ErrRecordNotFound
	}
	status, handled := s.GitLabIntegration.RepoFlowEvidenceStatus(repoFullName)
	if !handled {
		return gitlabintegration.RepoFlowEvidenceStatus{}, gorm.ErrRecordNotFound
	}
	return status, nil
}

// ListRepoFlowEvidenceHistory returns recent AGS-owned evidence records.
func (s *Service) ListRepoFlowEvidenceHistory(ctx context.Context, repoFullName, env string, limit int) (gitlabintegration.RepoFlowEvidenceHistory, error) {
	if s == nil || s.GitLabIntegration == nil {
		return gitlabintegration.RepoFlowEvidenceHistory{}, gorm.ErrRecordNotFound
	}
	history, handled, err := s.GitLabIntegration.ListRepoFlowEvidence(ctx, repoFullName, env, limit)
	if err != nil {
		return gitlabintegration.RepoFlowEvidenceHistory{}, err
	}
	if !handled {
		return gitlabintegration.RepoFlowEvidenceHistory{}, gorm.ErrRecordNotFound
	}
	return history, nil
}

func (s *Service) recordRepoFlowEnvProjection(ctx context.Context, repoFullName, env, sourceRef, sourceSHA string, res RepoFlowEnvProjectionResult) {
	if s == nil || s.DB == nil {
		return
	}
	repo, err := s.LookupRepoIdentity(ctx, repoFullName)
	if err != nil {
		slog.WarnContext(ctx, "repo-flow env projection status skipped", "repo", repoFullName, "env", env, "error", err)
		return
	}
	now := time.Now().UTC()
	triggeredAt := now
	if res.TriggeredAt != nil {
		triggeredAt = *res.TriggeredAt
	}
	row := db.RepoFlowEnvProjection{
		RepositoryID: repo.ID,
		RepoFullName: repo.FullName,
		Env:          env,
		SourceRef:    sourceRef,
		SourceSHA:    sourceSHA,
		Provider:     repoFlowFirstNonEmpty(res.Provider, "gitlab"),
		ProjectPath:  res.ProjectPath,
		TargetBranch: res.TargetBranch,
		State:        repoFlowFirstNonEmpty(res.State, "pending"),
		Error:        res.Error,
		TriggeredAt:  triggeredAt,
		ProjectedAt:  res.ProjectedAt,
	}
	err = s.DBForCtx(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "repository_id"}, {Name: "env"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"repo_full_name", "source_ref", "source_sha", "provider", "project_path", "target_branch", "state", "error", "triggered_at", "projected_at", "updated_at",
		}),
	}).Create(&row).Error
	if err != nil {
		slog.WarnContext(ctx, "repo-flow env projection status record failed", "repo", repoFullName, "env", env, "error", err)
	}
}

func (s *Service) recordRepoFlowEnvEvidence(ctx context.Context, res RepoFlowEnvProjectionResult) {
	if s == nil || s.GitLabIntegration == nil || strings.TrimSpace(res.RepoFullName) == "" || strings.TrimSpace(res.Env) == "" {
		return
	}
	record := map[string]any{
		"schema":        1,
		"time":          repoFlowEventTime(res),
		"action":        "env.projection",
		"repo":          res.RepoFullName,
		"env":           res.Env,
		"source_ref":    res.SourceRef,
		"source_sha":    res.SourceSHA,
		"provider":      res.Provider,
		"project_path":  res.ProjectPath,
		"target_branch": res.TargetBranch,
		"pushed_sha":    res.PushedSHA,
		"state":         res.State,
	}
	if strings.TrimSpace(res.Error) != "" {
		record["error"] = res.Error
	}
	if res.ProjectedAt != nil {
		record["projected_at"] = res.ProjectedAt.UTC().Format(time.RFC3339)
	}
	if _, handled, err := s.GitLabIntegration.RecordRepoFlowEvidence(ctx, res.RepoFullName, record); err != nil {
		slog.WarnContext(ctx, "repo-flow env evidence record failed", "repo", res.RepoFullName, "env", res.Env, "state", res.State, "error", err)
	} else if !handled {
		slog.DebugContext(ctx, "repo-flow env evidence skipped", "repo", res.RepoFullName, "env", res.Env)
	}
}

func repoFlowEventTime(res RepoFlowEnvProjectionResult) string {
	if res.ProjectedAt != nil {
		return res.ProjectedAt.UTC().Format(time.RFC3339)
	}
	if res.TriggeredAt != nil {
		return res.TriggeredAt.UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

func repoFlowProjectionRowResult(row db.RepoFlowEnvProjection) RepoFlowEnvProjectionResult {
	res := RepoFlowEnvProjectionResult{
		Handled:      row.State == "projected",
		RepoFullName: row.RepoFullName,
		Env:          row.Env,
		SourceRef:    row.SourceRef,
		SourceSHA:    row.SourceSHA,
		Provider:     row.Provider,
		ProjectPath:  row.ProjectPath,
		TargetBranch: row.TargetBranch,
		State:        row.State,
		Error:        row.Error,
		TriggeredAt:  &row.TriggeredAt,
		ProjectedAt:  row.ProjectedAt,
	}
	if row.State == "projected" {
		res.PushedSHA = row.SourceSHA
	}
	return res
}

func repoFlowEnvFromRef(ref string) (string, bool) {
	const prefix = "refs/heads/env/"
	if !strings.HasPrefix(ref, prefix) {
		return "", false
	}
	env := strings.TrimSpace(strings.TrimPrefix(ref, prefix))
	if !repoFlowValidEnv(env) {
		return "", false
	}
	return env, true
}

func repoFlowValidEnv(env string) bool {
	if env == "" || strings.Contains(env, "/") {
		return false
	}
	for _, r := range env {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func repoFlowFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isRepoFlowProjectionNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
