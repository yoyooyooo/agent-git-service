package rest_test

import (
	"encoding/json"
	"testing"

	"github.com/ngaut/agent-git-service/internal/rest"
)

func TestReplicationRegistrationOpenAPIIsClosedAndExplicit(t *testing.T) {
	var spec map[string]any
	if err := json.Unmarshal(rest.OpenAPISpecBytes(), &spec); err != nil {
		t.Fatal(err)
	}
	paths := spec["paths"].(map[string]any)
	path, ok := paths["/api/v3/repos/{owner}/{repo}/replication/identity"].(map[string]any)
	if !ok {
		t.Fatal("registration path not documented")
	}
	post := path["post"].(map[string]any)
	body := post["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props := body["properties"].(map[string]any)
	if body["additionalProperties"] != false || len(props) != 4 || len(body["required"].([]any)) != 4 {
		t.Fatal("registration envelope not exact")
	}
	for _, key := range []string{"version", "expected_authority_id", "expected_repository_id", "expected_created_at"} {
		if props[key] == nil {
			t.Fatal("missing registration condition", key)
		}
	}
	for _, method := range []string{"get", "post"} {
		op := path[method].(map[string]any)
		if op["security"] == nil {
			t.Fatal("missing native auth contract")
		}
		responses := op["responses"].(map[string]any)
		if responses["403"] == nil || responses["404"] == nil || responses["409"] == nil {
			t.Fatal("missing disabled/admin/conflict outcomes")
		}
		receipt := responses["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
		if receipt["additionalProperties"] != false {
			t.Fatal("open response schema")
		}
		if receipt["properties"].(map[string]any)["identity"].(map[string]any)["additionalProperties"] != false {
			t.Fatal("open identity schema")
		}
	}
}
