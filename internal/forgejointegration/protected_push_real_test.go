package forgejointegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run only against the disposable instance created by scripts/test-forgejo-projection.py.
// No deployment credentials or existing repository are accepted by that runner.
func TestRealForgejoProtectedPushIdentity(t *testing.T) {
	base := os.Getenv("AGS_TEST_FORGEJO_URL")
	if base == "" {
		t.Skip("run scripts/test-forgejo-projection.py for real Forgejo acceptance")
	}
	bot, operator := os.Getenv("AGS_TEST_FORGEJO_BOT"), os.Getenv("AGS_TEST_FORGEJO_OPERATOR")
	if bot == "" || operator == "" {
		t.Fatal("disposable Forgejo credentials missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	api := func(method, path, token string, body any) map[string]any {
		t.Helper()
		var data io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+"/api/v1"+path, data)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal("fixture API unavailable")
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			t.Fatalf("fixture %s %s HTTP %d", method, path, res.StatusCode)
		}
		var result map[string]any
		if res.StatusCode != http.StatusNoContent {
			if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	name := fmt.Sprintf("protected-%d", time.Now().UnixNano())
	api("POST", "/user/repos", bot, map[string]any{"name": name, "private": true, "default_branch": "main"})
	repo := "projection-bot/" + name
	source := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", source}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git %s: %v", args[0], err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-m", "initial")
	cfg := Config{Enabled: true, BaseURL: base, Token: bot, AuthorityPolicyEnabled: true, AuthorityPolicyToken: operator, IntegrationBot: "projection-bot", RepoMap: map[string]RepoMapping{repo: {Owner: "projection-bot", Repo: name, BaseBranch: "main"}}}
	initial := git("rev-parse", "HEAD")
	// Seed before protecting; then use exactly the same native Git transport for both identities.
	seed := New(Config{Enabled: true, BaseURL: base, Token: bot, RepoMap: cfg.RepoMap}, nil, nil)
	event := PushEvent{RepoFullName: repo, RepoPath: source, Changes: []RefChange{{Ref: "refs/heads/main", After: initial, Created: true}}}
	if err := seed.HandlePush(ctx, event); err != nil {
		t.Fatal("seed push:", err)
	}
	api("POST", "/repos/"+repo+"/branch_protections", bot, map[string]any{"branch_name": "main", "enable_push": true, "enable_push_whitelist": true, "push_whitelist_usernames": []string{"projection-bot"}, "apply_to_admins": true})
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-m", "fast-forward")
	head := git("rev-parse", "HEAD")
	event.Changes = []RefChange{{Ref: "refs/heads/main", Before: initial, After: head}}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	// Reproduce the original wrong-identity refusal against a real protected Forgejo branch.
	denied := New(Config{Enabled: true, BaseURL: base, Token: operator, TokenOwner: "fixture-operator", RepoMap: cfg.RepoMap}, nil, nil)
	err := denied.HandlePush(ctx, event)
	if err == nil {
		t.Fatal("operator unexpectedly updated protected main")
	}
	var failure *ProjectionError
	if !errors.As(err, &failure) || failure.Type != ProjectionFailureProtectedBranch {
		t.Fatalf("protected branch rejection lost its category: %v", err)
	}
	branch := api("GET", "/repos/"+repo+"/branches/main", bot, nil)
	if branch["commit"].(map[string]any)["id"] != initial {
		t.Fatal("ref moved after refused push")
	}
	t.Log("real Forgejo denied non-whitelisted operator; protected_branch_blocked; ref unchanged")
	// Both config/constructor override sites must preserve bot even without attribution.
	tokenFile := filepath.Join(t.TempDir(), "projection.token")
	if err := os.WriteFile(tokenFile, []byte(bot), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Token = ""
	cfg.TokenFile = tokenFile
	cfg, err = LoadTokenFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = ValidateAttribution(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fixed := New(cfg, nil, nil)
	if err := fixed.HandlePush(ctx, event); err != nil {
		t.Fatal("bot projection rejected with distinct operator configured:", err)
	}
	branch = api("GET", "/repos/"+repo+"/branches/main", bot, nil)
	if branch["commit"].(map[string]any)["id"] != head {
		t.Fatal("bot projection did not reach exact AGS head")
	}
	for _, want := range []string{`"authenticated_login":"fixture-operator"`, `"authenticated_login":"projection-bot"`, `"status":"protected_branch_blocked"`, `"status":"success"`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("push identity/outcome missing from logs: %s", want)
		}
	}
	for _, secret := range []string{bot, operator} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("credential leaked in push logs")
		}
	}
	t.Log("real Forgejo accepted bot fast-forward with distinct operator configured; exact head verified; safe identity logs verified")
}
