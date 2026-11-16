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
	operatorTokenFile := flag.String("operator-token-file", "", "optional shared Forgejo admin token file for this command")
	timeout := flag.Duration("timeout", 30*time.Second, "Forgejo API operation timeout")
	var repos repoFlag
	flag.Var(&repos, "repo", "restrict plan/apply/verify to this repository (AGS or Forgejo owner/name); repeatable; default is every enabled mapping")
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
	cfg, err = forgejointegration.LoadAuthorityPolicyTokenFile(cfg)
	if err != nil {
		fail(err.Error())
	}
	if cfg.AuthorityPolicyTokenFile != "" && cfg.AuthorityPolicyToken == "" {
		fail("Forgejo authority operator credential is empty")
	}
	if cfg.AuthorityPolicyToken != "" {
		cfg.Token = cfg.AuthorityPolicyToken
	}
	cfg, err = forgejointegration.LoadTokenFile(cfg)
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
		plans, err = integration.PlanSelectedRepositoryAuthority(ctx, repos)
	case "apply":
		plans, err = integration.ApplySelectedRepositoryAuthority(ctx, repos)
	case "verify":
		plans, err = integration.VerifySelectedRepositoryAuthority(ctx, repos)
	default:
		fail("-mode must be plan, apply, or verify")
	}
	result := map[string]any{"mode": *mode, "repositories": plans, "status": "ok"}
	if len(repos) > 0 {
		result["selected_repositories"] = []string(repos)
	}
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

// repoFlag collects repeatable -repo selectors.
type repoFlag []string

func (f *repoFlag) String() string { return strings.Join(*f, ",") }

func (f *repoFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("-repo must not be empty")
	}
	*f = append(*f, value)
	return nil
}

func fail(message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
