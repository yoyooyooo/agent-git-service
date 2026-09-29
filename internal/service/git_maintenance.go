package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
)

// GitMaintenanceRoots enumerates authoritative local Git identities, not
// external provider identities. Metadata is not a GC root unless represented
// explicitly in Git. Missing tables/query errors stop maintenance, never prune.
// Query results are bounded and credential-free; there is no second database.
func (s *Service) GitMaintenanceRoots(ctx context.Context, repo db.Repository) ([]string, error) {
	if s == nil || s.DBForCtx(ctx) == nil {
		return nil, fmt.Errorf("application database unavailable")
	}
	database := s.DBForCtx(ctx).WithContext(ctx)
	roots := map[string]bool{}
	addQuery := func(table, where string, args []any, columns ...string) error {
		for _, column := range columns {
			var values []string
			q := database.Table(table).Where(where, args...).Where(column+" IS NOT NULL AND "+column+" <> ?", "").Distinct(column).Limit(100001)
			if err := q.Pluck(column, &values).Error; err != nil {
				return fmt.Errorf("query application Git roots: %s.%s", table, column)
			}
			if len(values) > 100000 {
				return fmt.Errorf("application Git root inventory exceeds maintenance budget")
			}
			for _, oid := range values {
				if oid == strings.Repeat("0", 40) {
					continue
				}
				roots[oid] = true
				if len(roots) > 100000 {
					return fmt.Errorf("application Git root inventory exceeds maintenance budget")
				}
			}
		}
		return nil
	}
	specs := []struct {
		table, where string
		args         []any
		columns      []string
	}{
		{"pull_requests", "repository_id = ?", []any{repo.ID}, []string{"head_sha", "base_sha", "merge_commit_sha", "auto_merge_expected_head_sha"}},
		{"pull_requests", "head_repository_id = ? AND repository_id <> ?", []any{repo.ID, repo.ID}, []string{"head_sha"}},
		{"pull_request_reviews", "pull_request_id IN (SELECT id FROM pull_requests WHERE repository_id = ?)", []any{repo.ID}, []string{"commit_sha"}},
		{"pr_review_comments", "pull_request_id IN (SELECT id FROM pull_requests WHERE repository_id = ?)", []any{repo.ID}, []string{"commit_id"}},
		{"commit_statuses", "repository_id = ?", []any{repo.ID}, []string{"commit_sha"}},
		{"pages_builds", "repository_id = ?", []any{repo.ID}, []string{"commit_sha"}},
		{"repo_flow_env_projections", "repository_id = ?", []any{repo.ID}, []string{"source_sha"}},
		{"projection_events", "repository_id = ?", []any{repo.ID}, []string{"ags_sha"}},
		{"projection_ref_states", "repository_id = ?", []any{repo.ID}, []string{"ags_sha"}},
		{"pull_request_projections", "repository_id = ?", []any{repo.ID}, []string{"last_synced_sha"}},
		{"access_grant_invocations", "repository_id = ?", []any{repo.ID}, []string{"expected_head_sha", "expected_base_sha"}},
		{"delegated_agent_sessions", "repository_id = ?", []any{repo.ID}, []string{"merge_delegation_expected_head_sha", "merge_delegation_expected_base_sha"}},
		{"workflow_runs", "repository_id = ?", []any{repo.ID}, []string{"head_sha"}},
		{"pull_request_projection_jobs", "repository_id = ?", []any{repo.ID}, []string{"head_sha", "preflight_ags_head_sha", "preflight_base_sha", "desired_ags_head_sha"}},
		{"pull_request_action_intents", "repository_id = ?", []any{repo.ID}, []string{"expected_head_sha", "expected_base_sha", "result_sha"}},
	}
	for _, spec := range specs {
		if err := addQuery(spec.table, spec.where, spec.args, spec.columns...); err != nil {
			return nil, err
		}
	}
	// Exact identities in admitted operation constraints may precede PR facts.
	// Read only these two typed constraint columns, never bodies or credentials.
	for _, spec := range []struct{ table, column string }{{"access_grant_invocations", "constraints_json"}, {"delegated_agent_sessions", "operation_constraints"}} {
		rows, err := database.Table(spec.table).Select(spec.column).Where("repository_id = ? AND "+spec.column+" IS NOT NULL AND "+spec.column+" <> ?", repo.ID, "").Rows()
		if err != nil {
			return nil, fmt.Errorf("query application constraint roots")
		}
		var total, count int
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("read application constraint roots")
			}
			total += len(raw)
			count++
			if total > 16*1024*1024 || count > 100000 {
				rows.Close()
				return nil, fmt.Errorf("application constraints exceed maintenance budget")
			}
			var values map[string]json.RawMessage
			if err = json.Unmarshal([]byte(raw), &values); err != nil {
				rows.Close()
				return nil, fmt.Errorf("invalid application constraint roots")
			}
			for _, key := range []string{"head_sha", "base_sha", "expected_head_sha", "expected_base_sha", "exact_head", "commit_sha", "sha", "ref"} {
				var oid string
				if value, ok := values[key]; ok && json.Unmarshal(value, &oid) == nil && len(oid) == 40 && strings.Trim(oid, "0123456789abcdef") == "" && oid != strings.Repeat("0", 40) {
					roots[oid] = true
				}
			}
			if len(roots) > 100000 {
				rows.Close()
				return nil, fmt.Errorf("application Git root inventory exceeds maintenance budget")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("incomplete application constraint roots")
		}
	}
	// Deployments store a ref, not a sha column. Symbolic names are already
	// protected by Git; an explicit OID in Ref is an additional application root.
	var deploymentRefs []string
	if err := database.Model(&db.Deployment{}).Where("repository_id = ?", repo.ID).Limit(100001).Pluck("ref", &deploymentRefs).Error; err != nil {
		return nil, fmt.Errorf("query deployment refs")
	}
	if len(deploymentRefs) > 100000 {
		return nil, fmt.Errorf("deployment inventory exceeds maintenance budget")
	}
	for _, ref := range deploymentRefs {
		if len(ref) == 40 && strings.Trim(ref, "0123456789abcdef") == "" && ref != strings.Repeat("0", 40) {
			roots[ref] = true
		}
	}
	result := make([]string, 0, len(roots))
	for oid := range roots {
		result = append(result, oid)
	}
	sort.Strings(result)
	return result, nil
}
