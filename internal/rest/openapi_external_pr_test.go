package rest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/rest"
)

func TestMulticaExternalPRLinkRequestOpenAPIDeclaresTerminalInvariants(t *testing.T) {
	var spec map[string]any
	if err := json.Unmarshal(rest.OpenAPISpecBytes(), &spec); err != nil {
		t.Fatal(err)
	}
	components := spec["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	schema := schemas["MulticaExternalPRLinkRequestV1"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Fatalf("external PR request schema is not closed: %#v", schema)
	}
	if !strings.Contains(schema["description"].(string), "state=merged requires completion_intent=true") ||
		!strings.Contains(schema["description"].(string), "one complete group or omitted") {
		t.Fatalf("terminal/binding runtime invariants missing from description: %v", schema["description"])
	}
	properties := schema["properties"].(map[string]any)
	assertEnum := func(name, want string) {
		t.Helper()
		property := properties[name].(map[string]any)
		enum := property["enum"].([]any)
		if len(enum) != 1 || enum[0] != want {
			t.Fatalf("%s enum=%v, want [%s]", name, enum, want)
		}
	}
	assertEnum("merge_provider", "forgejo")
	assertEnum("link_confidence", "authoritative")
	if properties["merged_sha"].(map[string]any)["pattern"] != `^(|[0-9a-f]{40})$` {
		t.Fatalf("merged_sha pattern is not the canonical lower-40-hex/closed-empty shape: %#v", properties["merged_sha"])
	}
}
