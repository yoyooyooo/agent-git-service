package rest

// The same standard paths serve native storage or the selected Forgejo binding.
// Their descriptions keep source and permission differences visible to clients.
func repositoryVariablesOpenAPIOperation(single bool) map[string]any {
	variable := map[string]any{
		"type":     "object",
		"required": []string{"name", "value"},
		"properties": map[string]any{
			"name":       stringSchema("Variable name. Forgejo names are returned in canonical uppercase."),
			"value":      stringSchema("Observed repository value. An empty string is a valid value."),
			"created_at": map[string]any{"type": "string", "format": "date-time", "description": "Native storage only. Omitted when Forgejo supplies no timestamp."},
			"updated_at": map[string]any{"type": "string", "format": "date-time", "description": "Native storage only. Omitted when Forgejo supplies no timestamp."},
		},
	}
	params := pathParams(param("owner", "string"), param("repo", "string"))
	id, summary := "listRepositoryActionsVariables", "List repository Actions variable definitions."
	schema := map[string]any{
		"type":     "object",
		"required": []string{"total_count", "variables"},
		"properties": map[string]any{
			"total_count": map[string]any{"type": "integer", "minimum": 0},
			"variables":   map[string]any{"type": "array", "items": variable},
		},
	}
	headers := map[string]any{
		"Cache-Control": map[string]any{"description": "Variable responses must not be stored.", "schema": map[string]any{"type": "string", "enum": []string{"no-store"}}},
	}
	if single {
		id, summary, schema = "getRepositoryActionsVariable", "Get one repository Actions variable definition.", variable
		params = append(params, pathParams(param("name", "string"))...)
	} else {
		params = append(params, queryParams(
			map[string]any{"name": "page", "description": "Forgejo-backed reads use this 1-based page.", "schema": map[string]any{"type": "integer", "minimum": 1, "default": 1}},
			map[string]any{"name": "per_page", "description": "Forgejo-backed reads return up to this many definitions, aggregating provider pages when needed.", "schema": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 30}},
		)...)
		headers["Link"] = map[string]any{"description": "Forgejo-backed pagination links in the AGS API namespace, when another page exists.", "schema": map[string]any{"type": "string"}}
	}
	op := operation(id, summary, auth(), nil, params, map[string]any{
		"200": map[string]any{"description": "Observed repository definitions", "headers": headers, "content": map[string]any{"application/json": map[string]any{"schema": schema}}},
		"401": map[string]any{"description": "AGS authentication required"},
		"403": map[string]any{"description": "Caller denied, including delegated review.read and ci.read sessions"},
		"404": map[string]any{"description": "Repository hidden or missing, or the requested repository variable is absent. This does not prove an effective workflow value is absent."},
		"422": map[string]any{"description": "Invalid variable name or pagination"},
		"501": map[string]any{"description": "Selected backend does not support repository variable reads"},
		"502": map[string]any{"description": "Provider credentials, transport, paging, scope, or response data could not be verified"},
	})
	op["description"] = "Native repositories retain their existing storage and permissions. Forgejo-backed reads require an authenticated AGS repository administrator or owner before using the configured provider credential. Public repository visibility and delegated review.read or ci.read do not grant access. Forgejo is the sole source: list values come from individual detail reads, never local rows or the provider's placeholder list data. Responses describe current repository definitions, not inherited/effective values or a running task snapshot. Forgejo repository writes and environment variables are unsupported. Organization variables require a purely native registry, with a native default and no nonnative repository override; unused provider declarations do not change that rule."
	return op
}
