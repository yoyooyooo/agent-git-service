package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/integrations"
)

func TestForgejoIntegrationConfigKeepsHostActionsLogDirectory(t *testing.T) {
	cfg := config.Config{
		BaseURL:                "http://ags.local",
		IntegrationsConfigFile: "/runtime/integrations.yaml",
		ForgejoActionsLogDir:   "/opt/forgejo/data/gitea/actions_log",
	}
	fileCfg := integrations.Config{}
	fileCfg.Forgejo.Enabled = true
	fileCfg.Forgejo.BaseURL = "http://forgejo.local"

	got := forgejoIntegrationConfig(cfg, fileCfg)
	if got.ActionsLogDir != cfg.ForgejoActionsLogDir {
		t.Fatalf("actions log dir=%q, want host-local %q", got.ActionsLogDir, cfg.ForgejoActionsLogDir)
	}
}

func TestForgejoIntegrationConfigPrefersExplicitFileActionsLogDirectory(t *testing.T) {
	cfg := config.Config{
		BaseURL:                "http://ags.local",
		IntegrationsConfigFile: "/runtime/integrations.yaml",
		ForgejoActionsLogDir:   "/host/actions_log",
	}
	fileCfg := integrations.Config{}
	fileCfg.Forgejo.Enabled = true
	fileCfg.Forgejo.ActionsLogDir = "/file/actions_log"

	got := forgejoIntegrationConfig(cfg, fileCfg)
	if got.ActionsLogDir != fileCfg.Forgejo.ActionsLogDir {
		t.Fatalf("actions log dir=%q, want file-owned %q", got.ActionsLogDir, fileCfg.Forgejo.ActionsLogDir)
	}
}

// Verify the complete loader -> constructor -> HTTP/Git credential path with
// synthetic secrets. No provider permission query may gate the admin executor.
func TestForgejoAdminTokenWiring(t *testing.T) {
	for _, source := range []string{"environment_operator_file", "yaml_operator_file", "yaml_shared_admin_file", "yaml_operator_inline"} {
		t.Run(source, func(t *testing.T) {
			const adminToken = "fixture-admin-token"
			var writes, reads, permissionReads atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "token "+adminToken {
					t.Error("Forgejo request did not use the configured admin credential")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.Contains(r.URL.Path, "/collaborators/") {
					permissionReads.Add(1)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
					writes.Add(1)
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{}`))
					return
				}
				reads.Add(1)
				switch {
				case strings.Contains(r.URL.Path, "/branch_protections/"):
					_, _ = w.Write([]byte(`{"enable_force_push":false}`))
				case strings.HasSuffix(r.URL.Path, "/labels"), strings.HasSuffix(r.URL.Path, "/hooks"):
					_, _ = w.Write([]byte(`[]`))
				default:
					_, _ = w.Write([]byte(`{"allow_rebase_update":false}`))
				}
			}))
			defer provider.Close()
			root := t.TempDir()
			adminFile := filepath.Join(root, "admin.token")
			if err := os.WriteFile(adminFile, []byte(adminToken+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{
				ForgejoIntegrationEnabled: true, ForgejoIntegrationBaseURL: provider.URL,
				ForgejoIntegrationToken:       adminToken,
				ForgejoAuthorityPolicyEnabled: true, ForgejoAuthorityPolicyTokenFile: adminFile,
				ForgejoIntegrationBot: "fixture-admin",
			}
			if source != "environment_operator_file" {
				credential := "  token: " + adminToken + "\n"
				operator := fmt.Sprintf("    operator_token_file: %q\n", adminFile)
				if source == "yaml_shared_admin_file" {
					if err := os.WriteFile(adminFile, []byte(adminToken+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					credential = fmt.Sprintf("  token_file: %q\n", adminFile)
					operator = ""
				} else if source == "yaml_operator_inline" {
					operator = "    operator_token: " + adminToken + "\n"
				}
				body := fmt.Sprintf("forgejo:\n  enabled: true\n  base_url: %q\n%s  authority_policy:\n    enabled: true\n    webhook_url: https://primary.example.test/hook\n    integration_bot: fixture-admin\n%s  repos:\n    fixture/repo:\n      owner: fixture\n      repo: repo\n      base_branch: main\n", provider.URL, credential, operator)
				cfg.IntegrationsConfigFile = filepath.Join(root, "integrations.yaml")
				if err := os.WriteFile(cfg.IntegrationsConfigFile, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				cfg.ForgejoIntegrationRepoMapFile = filepath.Join(root, "repos.json")
				if err := os.WriteFile(cfg.ForgejoIntegrationRepoMapFile, []byte(`{"fixture/repo":{"owner":"fixture","repo":"repo","base_branch":"main"}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			integration, err := initForgejoIntegration(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := integration.CreatePullRequestComment(context.Background(), "fixture/repo", 1, "fixture comment"); err != nil {
				t.Fatal(err)
			}
			if _, err := integration.VerifyPullRequestMergeAuthority(context.Background(), "fixture/repo", "main"); err != nil {
				t.Fatal("admin executor was gated on repository permissions:", err)
			}
			remote, err := integration.RemoteURLForRepo("fixture/repo")
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(remote)
			if err != nil {
				t.Fatal("invalid Git remote")
			}
			password, ok := parsed.User.Password()
			if !ok || password != adminToken {
				t.Fatal("Git HTTP did not use the same admin credential")
			}
			if writes.Load() != 1 || reads.Load() != 4 || permissionReads.Load() != 0 {
				t.Fatalf("writes=%d reads=%d permission_reads=%d", writes.Load(), reads.Load(), permissionReads.Load())
			}
		})
	}
}
