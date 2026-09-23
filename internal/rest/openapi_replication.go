package rest

import "github.com/ngaut/agent-git-service/internal/edgeprotocol"

func replicationRegistrationBody() map[string]any {
	schema := closedObjectSchema(map[string]any{
		"version":                map[string]any{"type": "string", "enum": []string{edgeprotocol.RegistrationRequestVersion}},
		"expected_authority_id":  map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"expected_repository_id": map[string]any{"type": "integer", "minimum": 1},
		"expected_created_at":    map[string]any{"type": "string", "format": "date-time"},
	}, []string{"version", "expected_authority_id", "expected_repository_id", "expected_created_at"})
	return map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schema}}}
}

func replicationRegistrationResponses() map[string]any {
	identity := closedObjectSchema(map[string]any{
		"authority_id": map[string]any{"type": "string"}, "store_id": map[string]any{"type": "string"},
		"repository_id": map[string]any{"type": "integer", "minimum": 1}, "kind": map[string]any{"type": "string", "enum": []string{"repo"}},
	}, []string{"authority_id", "store_id", "repository_id", "kind"})
	receipt := closedObjectSchema(map[string]any{
		"version":      map[string]any{"type": "string", "enum": []string{edgeprotocol.RegistrationVersion}},
		"authority_id": map[string]any{"type": "string"}, "repository": map[string]any{"type": "string"},
		"repository_id": map[string]any{"type": "integer", "minimum": 1}, "created_at": map[string]any{"type": "string", "format": "date-time"}, "identity": identity,
	}, []string{"version", "authority_id", "repository", "repository_id", "created_at"})
	return map[string]any{
		"200": map[string]any{"description": "Non-authorizing registration receipt; identity omitted before provisioning. Cache-Control: no-store.", "content": map[string]any{"application/json": map[string]any{"schema": receipt}}},
		"400": map[string]any{"description": "Invalid closed request or query"},
		"401": map[string]any{"description": "Native authentication required"},
		"403": map[string]any{"description": "Native repository administrator required; transport sessions denied"},
		"404": map[string]any{"description": "Registration not enabled or repository not visible"},
		"409": map[string]any{"description": "Expected authority/repository creation facts changed; inspect again"},
		"415": map[string]any{"description": "Requires application/json"},
	}
}
