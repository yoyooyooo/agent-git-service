package rest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderInventoryOpenAPIOnlyPublishesReadExtensions(t *testing.T) {
	paths := buildRESTOpenAPIPaths()
	for _, suffix := range []string{"pulls", "pulls/{provider_number}", "git/ref"} {
		path := "/api/ext/v1/repos/{owner}/{repo}/provider/" + suffix
		entry, ok := paths[path].(map[string]any)
		if !ok || len(entry) != 1 || entry["get"] == nil {
			t.Fatalf("missing GET-only provider API: %s", path)
		}
		if paths[strings.Replace(path, "/api/ext/v1/", "/api/v3/", 1)] != nil {
			t.Fatal("provider extension has a v3 alias")
		}
		body, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"native repository read", "delegated sessions", "no-store", "No repair", `"502"`, `"503"`} {
			if !strings.Contains(string(body), text) {
				t.Fatalf("missing %q contract at %s", text, path)
			}
		}
	}
}
