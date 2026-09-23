package server

import (
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
