package rest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClientRunOpenAPISeparatesAssociationFromNativeAuthority(t *testing.T) {
	paths := buildRESTOpenAPIPaths()
	for _, path := range []string{"/api/ext/v1/client-runs", "/api/ext/v1/client-runs/{run_id}", "/api/ext/v1/repos/{owner}/{repo}/ci", "/api/ext/v1/repos/{owner}/{repo}/pulls/{number}/context"} {
		if paths[path] == nil {
			t.Errorf("missing route %s", path)
		}
	}
	body, _ := json.Marshal(clientRunOpenAPIBody())
	text := string(body)
	if !strings.Contains(text, `"additionalProperties":false`) || !strings.Contains(text, `"writeOnly":true`) || strings.Contains(text, `"operations"`) || strings.Contains(text, `"role"`) {
		t.Fatalf("association input became authority schema: %s", text)
	}
	issued, _ := json.Marshal(clientRunOpenAPIResponses(true))
	status, _ := json.Marshal(clientRunOpenAPIResponses(false))
	if !strings.Contains(string(issued), `"token":`) || strings.Contains(string(status), `"token":`) {
		t.Fatal("read path exposes issue-only bearer")
	}
	if !strings.Contains(string(status), "caller_claimed_not_authority") || !strings.Contains(string(status), "no-store") {
		t.Fatal("missing trust/cache semantics")
	}
}
