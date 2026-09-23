package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/integrations"
)

func main() {
	configPath := flag.String("config", "", "path to AGS integrations YAML")
	mode := flag.String("mode", "plan", "authority workflow mode: plan, apply, or verify")
	operatorTokenFile := flag.String("operator-token-file", "", "optional Forgejo operator token file used only by this command")
	timeout := flag.Duration("timeout", 30*time.Second, "Forgejo API operation timeout")
	flag.Parse()

	if strings.TrimSpace(*configPath) == "" {
		fail("-config is required")
	}
	fileCfg, err := integrations.LoadFile(*configPath)
	if err != nil {
		fail(err.Error())
	}
	if err := validateAuthorityConfig(fileCfg); err != nil {
		fail(err.Error())
	}
	cfg := fileCfg.Forgejo.ToForgejoIntegrationConfig()
	if path := strings.TrimSpace(*operatorTokenFile); path != "" {
		cfg.AuthorityPolicyToken = ""
		cfg.AuthorityPolicyTokenFile = path
	}
	cfg, err = forgejointegration.LoadTokenFile(cfg)
	if err != nil {
		fail(err.Error())
	}
	cfg, err = forgejointegration.LoadAuthorityPolicyTokenFile(cfg)
	if err != nil {
		fail(err.Error())
	}
	cfg, err = forgejointegration.LoadWebhookSecretFile(cfg)
	if err != nil {
		fail(err.Error())
	}
	integration := forgejointegration.New(cfg, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var plans []forgejointegration.RepositoryAuthorityPlan
	switch strings.ToLower(strings.TrimSpace(*mode)) {
	case "plan":
		plans, err = integration.PlanRepositoryAuthority(ctx)
	case "apply":
		plans, err = integration.ApplyRepositoryAuthority(ctx)
	case "verify":
		plans, err = integration.VerifyRepositoryAuthority(ctx)
	default:
		fail("-mode must be plan, apply, or verify")
	}
	result := map[string]any{"mode": *mode, "repositories": plans, "status": "ok"}
	if err != nil {
		result["status"] = "failed"
		result["error"] = err.Error()
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
		fail(fmt.Sprintf("encode result: %v", encodeErr))
	}
	if err != nil {
		fail(err.Error())
	}
}

func validateAuthorityConfig(fileCfg integrations.Config) error {
	_, alertTargets, alertingOK, err := fileCfg.ResolveOutboundEventTargets(integrations.OutboundEventProjectionDrift)
	if err != nil {
		return fmt.Errorf("projection_drift alert target: %w", err)
	}
	if !alertingOK || len(alertTargets) == 0 {
		return fmt.Errorf("projection_drift alert target is required before Forgejo authority onboarding")
	}
	return nil
}

func fail(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
