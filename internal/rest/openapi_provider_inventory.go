package rest

func providerInventoryOpenAPIParameters() []map[string]any {
	return append(pathParams(param("owner", "string"), param("repo", "string")),
		map[string]any{"name": "state", "in": "query", "schema": map[string]any{"type": "string", "enum": []string{"open", "closed", "all"}, "default": "open"}},
		map[string]any{"name": "page", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000, "default": 1}},
		map[string]any{"name": "per_page", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 100}},
	)
}

func providerPullRequestOpenAPISchema() map[string]any {
	properties := map[string]any{
		"number":         map[string]any{"type": "integer", "minimum": 1, "description": "Provider PR number; never assumed to equal the AGS PR number."},
		"ags_pr_number":  map[string]any{"type": "integer", "nullable": true, "description": "Current AGS mapping only; null for an unmapped or superseded provider projection."},
		"mapping_status": map[string]any{"type": "string", "enum": []string{"current", "unmapped"}},
		"state":          map[string]any{"type": "string", "enum": []string{"open", "closed"}},
		"merged":         map[string]any{"type": "boolean"},
	}
	for _, name := range []string{"url", "title", "updated_at", "merge_commit_sha", "merge_base_sha", "head_ref", "head_sha", "base_ref", "base_sha"} {
		properties[name] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "required": []string{"number", "ags_pr_number", "mapping_status", "state", "merged"}, "properties": properties}
}

func providerObservationOpenAPIResponses(kind string) map[string]any {
	properties := map[string]any{
		"schema":        map[string]any{"type": "string"},
		"repository":    map[string]any{"type": "string"},
		"provider":      map[string]any{"type": "string", "enum": []string{"forgejo"}},
		"external_repo": map[string]any{"type": "string", "description": "Server-configured provider repository; callers cannot override it."},
		"observed_at":   map[string]any{"type": "string", "format": "date-time"},
	}
	switch kind {
	case "list":
		properties["schema"] = map[string]any{"type": "string", "enum": []string{"ags.provider-pull-request-inventory.v1"}}
		properties["state"] = map[string]any{"type": "string"}
		properties["page"] = map[string]any{"type": "integer"}
		properties["per_page"] = map[string]any{"type": "integer"}
		properties["next_page"] = map[string]any{"type": "integer", "nullable": true, "description": "Continue while non-null, including after short pages. Only an empty provider page terminates the traversal. A nonempty final page therefore has one more continuation request."}
		properties["pull_requests"] = map[string]any{"type": "array", "items": providerPullRequestOpenAPISchema()}
	case "pull":
		properties["schema"] = map[string]any{"type": "string", "enum": []string{"ags.provider-pull-request.v1"}}
		properties["pull_request"] = providerPullRequestOpenAPISchema()
	case "ref":
		properties["schema"] = map[string]any{"type": "string", "enum": []string{"ags.provider-git-ref.v1"}}
		properties["ref"] = map[string]any{"type": "string"}
		properties["sha"] = map[string]any{"type": "string", "pattern": "^([0-9a-f]{40}|[0-9a-f]{64})$", "description": "Live Forgejo branch commit; never a native AGS ref or a PR base SHA."}
	}
	return map[string]any{
		"200": map[string]any{
			"description": "Live provider observation and current AGS mapping. Cache-Control: no-store. No repair, upsert or provider write. Pages are separate observations, not an atomic cross-system snapshot.",
			"headers":     map[string]any{"Link": map[string]any{"description": "For nonempty list pages, an absolute next-page URL on the configured AGS API origin; compatible with gh api --paginate.", "schema": map[string]any{"type": "string"}}},
			"content":     map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object", "properties": properties}}},
		},
		"401": map[string]any{"description": "Native AGS authentication required, including for public repositories."},
		"403": map[string]any{"description": "Delegated sessions and PR-scoped grants cannot use repository-wide provider observations."},
		"404": map[string]any{"description": "Native repository missing or unreadable, or exact provider PR/ref not found."},
		"422": map[string]any{"description": "Invalid pagination, state, provider number or full branch ref."},
		"502": map[string]any{"description": "Provider read failed, returned invalid observations or exhausted the pagination bound. No partial successful inventory is claimed."},
		"503": map[string]any{"description": "No enabled provider target configured for this repository."},
	}
}
