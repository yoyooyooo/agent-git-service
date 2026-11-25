package rest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGitLabMirrorFactsOpenAPIIsReadOnlyExtension(t *testing.T) {
	paths := buildRESTOpenAPIPaths()
	path := "/api/ext/v1/repos/{owner}/{repo}/projection/gitlab/facts"
	entry, ok := paths[path].(map[string]any)
	if !ok || len(entry) != 1 || entry["get"] == nil {
		t.Fatal("missing read-only extension operation")
	}
	if paths[strings.Replace(path, "/api/ext/v1/", "/api/v3/", 1)] != nil {
		t.Fatal("new extension leaked into v3")
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"name":"ref"`, `"required":true`, "not_claimed", "No provider request", "no-store"} {
		if !strings.Contains(string(b), required) {
			t.Fatalf("missing API boundary %s", required)
		}
	}
}
