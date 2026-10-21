package forgejointegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/ngaut/agent-git-service/internal/attribution"
)

func TestAttributionWritesAndReadIsolation(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token fixture-admin" {
			t.Error("credential changed")
		}
		if r.Method == "GET" && r.Header.Get("Sudo") != "" {
			t.Error("read used Sudo")
		}
		if r.Method != "GET" {
			seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Sudo"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == "GET":
			w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == "GET":
			w.Write([]byte(`[{"id":1,"name":"ready"}]`))
		default:
			w.Write([]byte(`{"number":1}`))
		}
	}))
	defer server.Close()
	c := NewHTTPClient(server.URL, "fixture-admin")
	c.configureAttribution(Config{Attribution: attribution.Config{Principals: map[string]attribution.Principal{"alice": {Forgejo: "alice.remote"}}}})
	ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: 7, Login: "alice"})
	_, err := c.EnsurePullRequest(ctx, PullRequestRequest{Owner: "team", Repo: "demo", Head: "agent/topic", Base: "main", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.EditPullRequestBase(ctx, "team", "demo", 1, "release")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.UpdatePullRequestState(ctx, "team", "demo", 1, "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.MergePullRequest(ctx, "team", "demo", 1, PullRequestMergeRequest{Method: "merge"}); err != nil {
		t.Fatal(err)
	}
	if err = c.CreateIssueComment(ctx, "team", "demo", 1, "comment"); err != nil {
		t.Fatal(err)
	}
	if err = c.AddIssueLabels(ctx, "team", "demo", 1, []string{"ready"}); err != nil {
		t.Fatal(err)
	}
	if err = c.RemoveIssueLabel(ctx, "team", "demo", 1, "ready"); err != nil {
		t.Fatal(err)
	}
	// Even with an actor, administrative repository writes never acquire Sudo.
	_, _, err = c.request(ctx, "POST", "/api/v1/orgs/team/repos", map[string]any{"name": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /api/v1/repos/team/demo/pulls alice.remote", "PATCH /api/v1/repos/team/demo/pulls/1 alice.remote", "PATCH /api/v1/repos/team/demo/pulls/1 alice.remote", "POST /api/v1/repos/team/demo/pulls/1/merge alice.remote", "POST /api/v1/repos/team/demo/issues/1/comments alice.remote", "POST /api/v1/repos/team/demo/issues/1/labels alice.remote", "DELETE /api/v1/repos/team/demo/issues/1/labels/1 alice.remote", "POST /api/v1/orgs/team/repos "}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("requests=%v", seen)
	}
}

func TestAttributionBoundedPermissionRecovery(t *testing.T) {
	for _, tc := range []struct {
		name        string
		denial      int
		grantStatus int
		second      int
		merge       bool
		mapped      bool
		want        []string
	}{
		{"grant_write", 403, 204, 201, false, true, []string{"sudo", "grant", "sudo"}},
		{"private_404", 404, 204, 201, false, true, []string{"sudo", "grant", "sudo"}},
		{"second_denial", 403, 204, 403, false, true, []string{"sudo", "grant", "sudo", "bot"}},
		{"grant_failed", 403, 403, 201, false, true, []string{"sudo", "grant", "bot"}},
		{"merge_denied", 403, 204, 201, true, true, []string{"sudo", "bot", "note"}},
		{"merge_protected", 405, 204, 201, true, true, []string{"sudo", "bot", "note"}},
		{"unmapped_repo", 403, 204, 201, false, false, []string{"sudo", "bot"}},
		{"conflict_no_replay", 409, 204, 201, true, true, []string{"sudo"}},
		{"server_error_no_replay", 500, 204, 201, false, true, []string{"sudo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			sudoCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					if r.Header.Get("Sudo") != "" {
						t.Error("read was sudo")
					}
					if strings.HasPrefix(r.URL.Path, "/api/v1/users/") {
						w.Write([]byte(`{"login":"alice.remote"}`))
						return
					}
					w.WriteHeader(404)
					return
				}
				if r.Method == "PUT" {
					seen = append(seen, "grant")
					if r.Header.Get("Sudo") != "" {
						t.Error("grant was sudo")
					}
					var b map[string]any
					json.NewDecoder(r.Body).Decode(&b)
					if b["permission"] != "write" {
						t.Error("grant exceeded write")
					}
					w.WriteHeader(tc.grantStatus)
					return
				}
				if tc.merge && strings.HasSuffix(r.URL.Path, "/comments") {
					seen = append(seen, "note")
					w.WriteHeader(201)
					return
				}
				if r.Header.Get("Sudo") != "" {
					seen = append(seen, "sudo")
					sudoCount++
					if sudoCount == 1 {
						w.WriteHeader(tc.denial)
					} else {
						w.WriteHeader(tc.second)
					}
					return
				}
				seen = append(seen, "bot")
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				if !tc.merge && !strings.HasPrefix(b["body"].(string), "AGS-Actor: alice") {
					t.Error("missing fallback marker")
				}
				w.WriteHeader(201)
			}))
			defer server.Close()
			cfg := Config{Attribution: attribution.Config{Principals: map[string]attribution.Principal{"alice": {Forgejo: "alice.remote"}}}}
			if tc.mapped {
				cfg.RepoMap = map[string]RepoMapping{"ags/demo": {Owner: "team", Repo: "demo"}}
			}
			c := NewHTTPClient(server.URL, "fixture-admin")
			c.configureAttribution(cfg)
			ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: 7, Login: "alice"})
			path := "/api/v1/repos/team/demo/issues/1/comments"
			body := map[string]any{"body": "hello"}
			if tc.merge {
				path = "/api/v1/repos/team/demo/pulls/1/merge"
				body = map[string]any{"Do": "merge"}
			}
			_, _, err := c.request(ctx, "POST", path, body)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(seen, tc.want) {
				t.Fatalf("seen=%v want=%v", seen, tc.want)
			}
		})
	}
}

func TestAttributionMissingUserAndMissingMapping(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmapped", true: "sudo_user_disappeared"}[mapped], func(t *testing.T) {
			sudo, bot, grants := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(404)
					return
				}
				if r.Method == "PUT" {
					grants++
					t.Error("must not create/grant disappeared user")
				}
				if r.Header.Get("Sudo") != "" {
					sudo++
					w.WriteHeader(404)
					return
				}
				bot++
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				if !strings.HasPrefix(b["body"].(string), "AGS-Actor: alice") {
					t.Error("marker missing")
				}
				w.WriteHeader(201)
			}))
			defer server.Close()
			cfg := Config{RepoMap: map[string]RepoMapping{"ags/demo": {Owner: "team", Repo: "demo"}}}
			if mapped {
				cfg.Attribution.Principals = map[string]attribution.Principal{"alice": {Forgejo: "gone"}}
			}
			c := NewHTTPClient(server.URL, "fixture")
			c.configureAttribution(cfg)
			ctx := attribution.WithActor(context.Background(), attribution.Actor{UserID: 7, Login: "alice"})
			if err := c.CreateIssueComment(ctx, "team", "demo", 1, "body"); err != nil {
				t.Fatal(err)
			}
			wantSudo := 0
			if mapped {
				wantSudo = 1
			}
			if sudo != wantSudo || bot != 1 || grants != 0 {
				t.Fatalf("sudo=%d bot=%d grants=%d", sudo, bot, grants)
			}
		})
	}
}

func TestAttributionStartupValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		admin   bool
		exists  bool
		wantErr bool
	}{{"valid", true, true, false}, {"not_admin", false, true, true}, {"missing_user", true, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.Header.Get("Sudo") != "" {
					writes++
				}
				if r.URL.Path == "/api/v1/user" {
					json.NewEncoder(w).Encode(map[string]any{"login": "bot", "is_admin": tc.admin})
					return
				}
				if !tc.exists {
					w.WriteHeader(404)
					return
				}
				w.Write([]byte(`{"login":"alice.remote"}`))
			}))
			defer server.Close()
			cfg, err := ValidateAttribution(context.Background(), Config{BaseURL: server.URL, Token: "fixture", Attribution: attribution.Config{Principals: map[string]attribution.Principal{"alice": {Forgejo: "alice.remote"}}}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			if err == nil && cfg.TokenOwner != "bot" {
				t.Error("owner not resolved")
			}
			if writes != 0 {
				t.Fatal("startup made writes")
			}
		})
	}
}

func TestAttributionWebhookSenderRoundTrips(t *testing.T) {
	body := []byte(`{"action":"closed","sender":{"login":"alice.remote"},"repository":{"full_name":"team/demo","default_branch":"main"},"pull_request":{"number":1,"merged":true,"head":{"ref":"agent/topic"},"base":{"ref":"main"}}}`)
	event, ok, err := ParseMergedPullRequestWebhook(body)
	if err != nil || !ok || event.SenderLogin != "alice.remote" {
		t.Fatal("sender lost", err)
	}
	integration := New(Config{Attribution: attribution.Config{Principals: map[string]attribution.Principal{"alice": {Forgejo: "alice.remote"}}}}, nil, nil)
	if integration.AGSLoginForActor(event.SenderLogin) != "alice" || integration.AGSLoginForActor("unknown") != "forgejo" {
		t.Fatal("reverse mapping failed")
	}
}
