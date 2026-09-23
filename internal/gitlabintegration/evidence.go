package gitlabintegration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RepoFlowEvidenceConfig controls AGS-owned RepoFlow evidence storage for a GitLab-backed repo.
type RepoFlowEvidenceConfig struct {
	Enabled bool
	Store   RepoFlowEvidenceStoreConfig
}

// RepoFlowEvidenceStoreConfig describes the physical evidence backend.
// The first supported backend is bounded JSONL on the AGS runtime host.
type RepoFlowEvidenceStoreConfig struct {
	Type       string
	Path       string
	MaxRecords int
}

// RepoFlowEvidenceStatus describes one repo's configured evidence store.
type RepoFlowEvidenceStatus struct {
	Enabled    bool   `json:"enabled"`
	State      string `json:"state"`
	Type       string `json:"type,omitempty"`
	Path       string `json:"path,omitempty"`
	MaxRecords int    `json:"max_records,omitempty"`
	Records    int    `json:"records,omitempty"`
	Error      string `json:"error,omitempty"`
}

// RepoFlowEvidenceHistory contains evidence records read from the AGS store.
type RepoFlowEvidenceHistory struct {
	Repo    string           `json:"repo"`
	Env     string           `json:"env,omitempty"`
	Source  string           `json:"source"`
	State   string           `json:"state"`
	Entries []map[string]any `json:"entries"`
}

// RecordRepoFlowEvidence appends one AGS-owned RepoFlow evidence record.
func (i *Integration) RecordRepoFlowEvidence(ctx context.Context, repoFullName string, record map[string]any) (map[string]any, bool, error) {
	cfg, handled, err := i.repoFlowEvidenceConfig(repoFullName)
	if !handled || err != nil {
		return nil, handled, err
	}
	if cfg.storeType() != "jsonl" {
		return nil, true, fmt.Errorf("repo-flow evidence: unsupported store type %q", cfg.storeType())
	}
	path := strings.TrimSpace(cfg.Store.Path)
	if path == "" {
		return nil, true, fmt.Errorf("repo-flow evidence: jsonl path is required")
	}
	normalized := normalizeRepoFlowEvidenceRecord(repoFullName, record)
	if err := appendJSONLEvidence(ctx, path, normalized, cfg.Store.MaxRecords); err != nil {
		return nil, true, err
	}
	return normalized, true, nil
}

// RepoFlowEvidenceStatus returns configuration and file state for a repo's evidence store.
func (i *Integration) RepoFlowEvidenceStatus(repoFullName string) (RepoFlowEvidenceStatus, bool) {
	cfg, handled, err := i.repoFlowEvidenceConfig(repoFullName)
	if !handled {
		return RepoFlowEvidenceStatus{}, false
	}
	status := RepoFlowEvidenceStatus{
		Enabled:    cfg.Enabled,
		State:      "available",
		Type:       cfg.storeType(),
		Path:       strings.TrimSpace(cfg.Store.Path),
		MaxRecords: cfg.Store.MaxRecords,
	}
	if err != nil {
		status.State = "misconfigured"
		status.Error = err.Error()
		return status, true
	}
	if status.Type != "jsonl" {
		status.State = "unsupported"
		status.Error = fmt.Sprintf("unsupported store type %q", status.Type)
		return status, true
	}
	if status.Path == "" {
		status.State = "misconfigured"
		status.Error = "jsonl path is required"
		return status, true
	}
	records, err := countJSONLLines(status.Path)
	if err != nil {
		if os.IsNotExist(err) {
			status.Records = 0
			return status, true
		}
		status.State = "error"
		status.Error = err.Error()
		return status, true
	}
	status.Records = records
	return status, true
}

// ListRepoFlowEvidence returns recent JSONL evidence records, newest first.
func (i *Integration) ListRepoFlowEvidence(ctx context.Context, repoFullName, env string, limit int) (RepoFlowEvidenceHistory, bool, error) {
	cfg, handled, err := i.repoFlowEvidenceConfig(repoFullName)
	if !handled || err != nil {
		return RepoFlowEvidenceHistory{}, handled, err
	}
	if cfg.storeType() != "jsonl" {
		return RepoFlowEvidenceHistory{}, true, fmt.Errorf("repo-flow evidence: unsupported store type %q", cfg.storeType())
	}
	path := strings.TrimSpace(cfg.Store.Path)
	if path == "" {
		return RepoFlowEvidenceHistory{}, true, fmt.Errorf("repo-flow evidence: jsonl path is required")
	}
	if limit <= 0 {
		limit = 100
	}
	if cfg.Store.MaxRecords > 0 && limit > cfg.Store.MaxRecords {
		limit = cfg.Store.MaxRecords
	}
	entries, err := readJSONLEvidence(ctx, path, strings.TrimSpace(env), limit)
	if err != nil {
		if os.IsNotExist(err) {
			entries = []map[string]any{}
		} else {
			return RepoFlowEvidenceHistory{}, true, err
		}
	}
	return RepoFlowEvidenceHistory{Repo: strings.TrimSpace(repoFullName), Env: strings.TrimSpace(env), Source: "ags-jsonl", State: "available", Entries: entries}, true, nil
}

func (i *Integration) repoFlowEvidenceConfig(repoFullName string) (RepoFlowEvidenceConfig, bool, error) {
	if i == nil {
		return RepoFlowEvidenceConfig{}, false, nil
	}
	mapping, ok := i.cfg.targetFor(repoFullName)
	if !ok || !mapping.RepoFlowEvidence.Enabled {
		return RepoFlowEvidenceConfig{}, false, nil
	}
	cfg := mapping.RepoFlowEvidence
	if cfg.storeType() == "" {
		cfg.Store.Type = "jsonl"
	}
	return cfg, true, nil
}

func (c RepoFlowEvidenceConfig) storeType() string {
	storeType := strings.ToLower(strings.TrimSpace(c.Store.Type))
	if storeType == "" {
		return "jsonl"
	}
	return storeType
}

func normalizeRepoFlowEvidenceRecord(repoFullName string, in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	if _, ok := out["schema"]; !ok {
		out["schema"] = 1
	}
	out["repo"] = strings.TrimSpace(repoFullName)
	if _, ok := out["time"]; !ok {
		out["time"] = time.Now().UTC().Format(time.RFC3339)
	}
	out["recorded_by"] = "ags"
	return out
}

func appendJSONLEvidence(ctx context.Context, path string, record map[string]any, maxRecords int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("repo-flow evidence: create jsonl dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("repo-flow evidence: open jsonl: %w", err)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(record); err != nil {
		_ = f.Close()
		return fmt.Errorf("repo-flow evidence: append jsonl: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("repo-flow evidence: close jsonl: %w", err)
	}
	if maxRecords > 0 {
		if err := trimJSONLEvidence(path, maxRecords); err != nil {
			return err
		}
	}
	return nil
}

func trimJSONLEvidence(path string, maxRecords int) error {
	lines, err := readAllLines(path)
	if err != nil {
		return err
	}
	if len(lines) <= maxRecords {
		return nil
	}
	lines = lines[len(lines)-maxRecords:]
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("repo-flow evidence: trim jsonl: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("repo-flow evidence: replace trimmed jsonl: %w", err)
	}
	return nil
}

func readJSONLEvidence(ctx context.Context, path, env string, limit int) ([]map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lines, err := readAllLines(path)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if env != "" && !evidenceRecordMatchesEnv(record, env) {
			continue
		}
		out = append(out, record)
	}
	return out, nil
}

func evidenceRecordMatchesEnv(record map[string]any, env string) bool {
	for _, key := range []string{"env", "environment"} {
		if value, ok := record[key]; ok && strings.TrimSpace(fmt.Sprint(value)) == env {
			return true
		}
	}
	return false
}

func countJSONLLines(path string) (int, error) {
	lines, err := readAllLines(path)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
}

func readAllLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		lines = append(lines, s.Text())
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}
