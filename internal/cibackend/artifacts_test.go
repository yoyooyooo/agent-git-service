package cibackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForgejoArtifactBridgeChecksBindingAndNeverHidesFailure(t *testing.T) {
	mode := "valid"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bridge-only" || r.URL.Query().Get("head_sha") != strings.Repeat("a", 40) {
			t.Error("wrong credential or commit binding")
		}
		if mode == "outage" {
			w.WriteHeader(503)
			return
		}
		if mode == "missing" {
			w.WriteHeader(404)
			return
		}
		run := "17"
		if mode == "foreign" {
			run = "18"
		}
		items := []any{map[string]any{"key": "51", "run": "17", "name": "test-results", "size_in_bytes": 6}}
		if mode == "empty" {
			items = []any{}
		}
		if mode == "duplicate" {
			items = append(items, items[0])
		}
		json.NewEncoder(w).Encode(map[string]any{"schema": "ags.internal-provider-artifacts.v1", "repo": "ci/project", "run": run, "head_sha": strings.Repeat("a", 40), "artifacts": items})
	}))
	defer server.Close()
	backend, err := NewHTTP(BackendConfig{Kind: "forgejo", URL: server.URL, TokenFile: "fixture", AllowHTTP: true, LogBridge: &LogBridgeConfig{URL: server.URL, TokenFile: "fixture", AllowHTTP: true}}, "provider-only", "bridge-only")
	if err != nil {
		t.Fatal(err)
	}
	bridge := backend.(ArtifactBackend)
	run := Run{Key: "17", HeadSHA: strings.Repeat("a", 40)}
	for _, test := range []struct {
		name  string
		want  error
		count int
	}{{"valid", nil, 1}, {"empty", nil, 0}, {"foreign", ErrInvalid, 0}, {"duplicate", ErrInvalid, 0}, {"missing", ErrNotFound, 0}, {"outage", ErrUnavailable, 0}} {
		t.Run(test.name, func(t *testing.T) {
			mode = test.name
			items, err := bridge.Artifacts(context.Background(), "ci/project", run)
			if !errors.Is(err, test.want) || len(items) != test.count {
				t.Fatalf("items=%v err=%v", items, err)
			}
		})
	}
}
