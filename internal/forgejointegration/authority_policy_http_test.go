package forgejointegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestHTTPClientRepositoryAuthorityRetrofit(t *testing.T) {
	var mu sync.Mutex
	applied := false
	pushWhitelistOpen := false
	mergeWhitelistOpen := false
	hookHasRequiredEvents := false
	mutations := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet {
			mutations[r.Method+" "+r.URL.Path]++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if strings.HasSuffix(r.URL.Path, "/branch_protections") {
				pushWhitelistOpen = body["enable_push_whitelist"] == false
				mergeWhitelistOpen = body["enable_merge_whitelist"] == false && body["apply_to_admins"] == false
			}
			if strings.HasSuffix(r.URL.Path, "/hooks") {
				events, _ := body["events"].([]any)
				hookHasRequiredEvents = len(events) == 3
			}
			applied = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/collaborators/ags-bot/permission"):
			if !applied {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"permission": "write"})
		case r.URL.Path == "/api/v1/repos/ci/widget":
			_ = json.NewEncoder(w).Encode(map[string]any{"allow_rebase_update": !applied})
		case strings.Contains(r.URL.Path, "/branch_protections/main"):
			if !applied {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"enable_push": true, "enable_push_whitelist": false, "push_whitelist_usernames": []string{},
				"enable_merge_whitelist": false, "merge_whitelist_usernames": []string{}, "apply_to_admins": false, "enable_force_push": false,
			})
		case strings.HasSuffix(r.URL.Path, "/labels"):
			labels := []map[string]string{}
			if applied {
				for _, label := range authorityWorkflowLabels {
					labels = append(labels, map[string]string{"name": label})
				}
			}
			_ = json.NewEncoder(w).Encode(labels)
		case strings.HasSuffix(r.URL.Path, "/hooks"):
			hooks := []map[string]any{}
			if applied {
				hooks = append(hooks, map[string]any{"id": 9, "active": true, "events": []string{"pull_request", "issues", "delete"}, "config": map[string]string{"url": "https://ags.example/hook"}})
			}
			_ = json.NewEncoder(w).Encode(hooks)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "token")
	before, err := client.InspectRepositoryAuthority(context.Background(), "ci", "widget", "main", "https://ags.example/hook", "ags-bot")
	if err != nil {
		t.Fatalf("inspect before: %v", err)
	}
	if !before.AllowRebaseUpdate || before.BaseBranchProtected || before.WebhookActive {
		t.Fatalf("unexpected initial authority: %#v", before)
	}
	if err := client.ApplyRepositoryAuthority(context.Background(), "ci", "widget", "main", "https://ags.example/hook", "secret", "ags-bot", before); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after, err := client.InspectRepositoryAuthority(context.Background(), "ci", "widget", "main", "https://ags.example/hook", "ags-bot")
	if err != nil {
		t.Fatalf("inspect after: %v", err)
	}
	if changes := authorityPolicyChanges(after); len(changes) != 0 {
		t.Fatalf("retrofit did not converge: %v (%#v)", changes, after)
	}
	if !after.IntegrationBotMergeAuthorized {
		t.Fatalf("open merge whitelist should authorize the bot: %#v", after)
	}
	if mutations["PATCH /api/v1/repos/ci/widget"] != 1 || mutations["PUT /api/v1/repos/ci/widget/collaborators/ags-bot"] != 1 || mutations["POST /api/v1/repos/ci/widget/branch_protections"] != 1 || mutations["POST /api/v1/repos/ci/widget/hooks"] != 1 {
		t.Fatalf("missing authority mutations: %#v", mutations)
	}
	if mutations["POST /api/v1/repos/ci/widget/labels"] != len(authorityWorkflowLabels) {
		t.Fatalf("label mutation count=%d, want %d", mutations["POST /api/v1/repos/ci/widget/labels"], len(authorityWorkflowLabels))
	}
	if !pushWhitelistOpen || !mergeWhitelistOpen || !hookHasRequiredEvents {
		t.Fatalf("authority payload did not open whitelists: push_open=%v merge_open=%v hook_events=%v", pushWhitelistOpen, mergeWhitelistOpen, hookHasRequiredEvents)
	}
}

func TestMergeWhitelistWithIntegrationBotPreservesExistingHumans(t *testing.T) {
	got := mergeWhitelistWithIntegrationBot([]string{"human-maintainer", "AGS-BOT", "", "human-maintainer"}, "ags-bot")
	if strings.Join(got, ",") != "human-maintainer,AGS-BOT" {
		t.Fatalf("merge whitelist=%v", got)
	}
}
