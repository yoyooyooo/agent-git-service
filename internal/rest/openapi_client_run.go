package rest

func clientRunContextSchema() map[string]any {
	return closedObjectSchema(map[string]any{
		"source": map[string]any{"type": "string", "maxLength": 256}, "agent": map[string]any{"type": "string", "maxLength": 256},
		"task": map[string]any{"type": "string", "maxLength": 256}, "run": map[string]any{"type": "string", "maxLength": 256},
	}, nil)
}
func clientRunOpenAPIBody() map[string]any {
	return jsonBody(true, map[string]any{
		"id":          map[string]any{"type": "string", "format": "uuid", "description": "Optional caller UUID for readback/revocation after a lost response. Reusing it never reissues the bearer."},
		"ttl_seconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 28800, "default": 3600},
		"context":     clientRunContextSchema(),
		"source": closedObjectSchema(map[string]any{
			"instance":      stringSchema("Registered fixed-egress source ID."),
			"endpoint_hint": stringSchema("Registered connector hint, never arbitrary egress."),
			"locator":       closedObjectSchema(map[string]any{"workspace_id": stringSchema("Source workspace"), "agent_id": stringSchema("Source Agent"), "task_id": stringSchema("Source Task")}, nil),
			"token":         map[string]any{"type": "string", "writeOnly": true, "description": "Optional source verification credential; request-memory only."},
		}, nil),
	}, nil)
}
func clientRunOpenAPIResponses(issue bool) map[string]any {
	properties := map[string]any{
		"schema": map[string]any{"type": "string", "enum": []string{"ags.client-run.v1"}}, "id": map[string]any{"type": "string", "format": "uuid"},
		"actor_id": map[string]any{"type": "integer"}, "actor_login": stringSchema("Authenticated native actor; context cannot select another user."),
		"context": clientRunContextSchema(), "association_status": map[string]any{"type": "string", "enum": []string{"linked", "provisional", "unlinked", "conflict"}},
		"context_trust": map[string]any{"type": "string", "enum": []string{"caller_claimed_not_authority", "verified_source_bound_to_native_actor"}},
		"expires_at":    map[string]any{"type": "string", "format": "date-time"}, "revoked": map[string]any{"type": "boolean"},
		"permissions": map[string]any{"type": "string", "enum": []string{"current_native_actor_permissions"}},
	}
	code := "200"
	if issue {
		code = "201"
		properties["token"] = map[string]any{"type": "string", "description": "Sensitive opaque run credential, returned once with Cache-Control: no-store. Never logged or returned by read endpoints."}
	}
	return map[string]any{code: map[string]any{"description": "Native actor run receipt", "headers": map[string]any{"Cache-Control": map[string]any{"schema": map[string]any{"type": "string", "enum": []string{"no-store"}}}}, "content": map[string]any{"application/json": map[string]any{"schema": closedObjectSchema(properties, nil)}}},
		"401": map[string]any{"description": "Native credential, parent or user no longer valid"}, "403": map[string]any{"description": "Cannot mint/revoke another actor's identity or manage durable credentials with a run credential"}, "404": map[string]any{"description": "Run not found under the selected native identity"}, "409": map[string]any{"description": "Existing request UUID or conflicting state; observe/revoke, do not replay"}, "422": map[string]any{"description": "Invalid input or TTL"},
	}
}
