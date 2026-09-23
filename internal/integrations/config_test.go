package integrations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFileRejectsIncompleteForgejoAuthorityPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	if err := os.WriteFile(path, []byte(`
forgejo:
  enabled: true
  authority_policy:
    enabled: true
    webhook_url: https://ags.example/hook
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "integration_bot") {
		t.Fatalf("expected missing integration bot error, got %v", err)
	}
}

func TestLoadFileParsesForgejoGitLabMulticaAndNotificationsConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	if err := os.WriteFile(path, []byte(`
notifications:
  targets:
    frontend_ci_debug:
      type: feishu_webhook
      enabled: true
      name: "前端CI测试"
      description: "前端 CI / debug 相关飞书机器人"
      webhook_url_file: /run/secrets/feishu-webhook
      timeout: 5s
  events:
    pull_request_merged:
      enabled: true
      targets: [frontend_ci_debug]
    projection_drift:
      enabled: true
      targets: [frontend_ci_debug]
    multica_incident:
      enabled: true
      targets: [frontend_ci_debug]
      throttle_window: 15m
projection_watch:
  enabled: true
  poll_interval: 2m
  scan_interval: 15m
  full_audit_interval: 24h
  startup_audit_delay: 5m
  repos: "*"
  ref_policy: mirror
  notify:
    on: [new, worsened, resolved]
    throttle_window: 30m
    grace_period: 90s
  repair:
    auto: false
    safe_repairs: [delete_smoke_refs, recreate_missing_webhook]
forgejo:
  enabled: true
  base_url: http://forgejo.local:5555
  token_file: /run/secrets/forgejo-token
  webhook_secret_file: /run/secrets/forgejo-webhook
  default_owner: example-owner
  private_repos: true
  auto_create_repo: true
  auto_pr: true
  default_base_branch: main
  push_git_config: [core.compression=0, pack.window=0]
  push_timeout: 8m
  actions_log_dir: /opt/forgejo/data/gitea/actions_log
  authority_policy:
    enabled: true
    webhook_url: https://ags.example/api/v3/integrations/forgejo/webhook
    integration_bot: ags-bot
    operator_token_file: /run/secrets/forgejo-operator-token
    action_principal_bindings:
      Example-Human: 7
  mirror:
    include: [main, agent/*, feature/*]
    exclude: [ci/*]
  pull_request:
    include: [agent/*, feature/*]
    exclude: [main, ci/*]
  repos:
    example-owner/demo:
      owner: example-owner
      repo: demo-ci
      base_branch: develop
      auto_pr: false
      delegated_merge_method: rebase
gitlab:
  enabled: true
  base_url: https://gitlab.example.local
  token_file: /run/secrets/gitlab-token
  merge_authority: forgejo
  mirror:
    include: [main, agent/*]
    exclude: [ci/*]
  repos:
    example-owner/demo:
      project_id: "123"
      project_path: backup/demo
      target_branch: main
      env_branches:
        uat: uat
github:
  enabled: true
  base_url: https://api.github.com
  remote_url: git@github.com:example-org/project-kit.git
  token_file: /run/secrets/github-token
  merge_authority: forgejo
  mirror:
    include: [main, agent/*]
    exclude: [ci/*]
  repos:
    operator/project-kit:
      owner: example-org
      repo: project-kit
      target_branch: main
execution_context:
  enabled: true
  connectors:
    - source_instance_id: multica-mini
      adapter: multica_current_execution_context_v1
      accepted_runtime_endpoints: [http://primary.example.test:37134]
      egress_endpoint: http://multica-backend:8080/api/integrations/current-execution-context
      timeout: 4s
      workspace_mappings:
        11111111-1111-4111-8111-111111111111: primary-a
multica:
  enabled: true
  target_instance: primary-b
  command: /usr/local/bin/multica
  profile: ags-multica-projection
  workspace: workspace-alpha
  workspace_id: 22222222-2222-4222-8222-222222222222
  default_workspace: workspace-alpha
  default_workspace_id: 22222222-2222-4222-8222-222222222222
  app_url: https://multica.ai
  server_url: http://localhost:3000
  external_pr_provider: ags
  link_token_audience: external-pr-link
  link_token_secret: link-secret
  service_token: service-secret
  completion_on_merge:
    enabled: true
    mode: leaf_child_only
  repos:
    example-owner/personal:
      workspace: workspace-beta
      workspace_id: 55555555-5555-4555-8555-555555555555
  workspaces:
    workspace-alpha:
      workspace_id: 22222222-2222-4222-8222-222222222222
    workspace-beta:
      workspace_id: 55555555-5555-4555-8555-555555555555
  comment: false
  set_status_on_merge: false
  failure_watch:
    enabled: true
    poll_interval: 1m
    rolling_window_days: 30
    issue_limit: 75
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !cfg.Forgejo.Enabled || cfg.Forgejo.BaseURL != "http://forgejo.local:5555" {
		t.Fatalf("forgejo config=%#v", cfg.Forgejo)
	}
	if cfg.Forgejo.WebhookSecretFile != "/run/secrets/forgejo-webhook" {
		t.Fatalf("webhook secret file=%q", cfg.Forgejo.WebhookSecretFile)
	}
	if got := cfg.Forgejo.Mirror.Include; len(got) != 3 || got[0] != "main" || got[1] != "agent/*" {
		t.Fatalf("mirror include=%v", got)
	}
	if cfg.Forgejo.Repos["example-owner/demo"].Repo != "demo-ci" || cfg.Forgejo.Repos["example-owner/demo"].DelegatedMergeMethod != "rebase" {
		t.Fatalf("repo map=%#v", cfg.Forgejo.Repos["example-owner/demo"])
	}
	forgejoCfg := cfg.Forgejo.ToForgejoIntegrationConfig()
	if len(forgejoCfg.PushGitConfig) != 2 || forgejoCfg.PushGitConfig[0] != "core.compression=0" || forgejoCfg.PushGitConfig[1] != "pack.window=0" || forgejoCfg.PushTimeout.String() != "8m0s" {
		t.Fatalf("forgejo push config mismatch: %#v timeout=%s", forgejoCfg.PushGitConfig, forgejoCfg.PushTimeout)
	}
	if !forgejoCfg.AuthorityPolicyEnabled || forgejoCfg.WebhookURL != "https://ags.example/api/v3/integrations/forgejo/webhook" || forgejoCfg.IntegrationBot != "ags-bot" || forgejoCfg.AuthorityPolicyTokenFile != "/run/secrets/forgejo-operator-token" {
		t.Fatalf("forgejo authority policy mismatch: enabled=%v webhook=%q bot=%q token_file=%q", forgejoCfg.AuthorityPolicyEnabled, forgejoCfg.WebhookURL, forgejoCfg.IntegrationBot, forgejoCfg.AuthorityPolicyTokenFile)
	}
	if forgejoCfg.ActionPrincipalBindings["example-human"] != 7 {
		t.Fatalf("action principal bindings=%#v", forgejoCfg.ActionPrincipalBindings)
	}
	if forgejoCfg.ActionsLogDir != "/opt/forgejo/data/gitea/actions_log" {
		t.Fatalf("actions log dir=%q", forgejoCfg.ActionsLogDir)
	}
	if !cfg.GitLab.Enabled || cfg.GitLab.BaseURL != "https://gitlab.example.local" {
		t.Fatalf("gitlab config=%#v", cfg.GitLab)
	}
	if cfg.GitLab.Repos["example-owner/demo"].ProjectID != "123" || cfg.GitLab.Repos["example-owner/demo"].ProjectPath != "backup/demo" || cfg.GitLab.Repos["example-owner/demo"].EnvBranches["uat"] != "uat" {
		t.Fatalf("gitlab repo map=%#v", cfg.GitLab.Repos["example-owner/demo"])
	}
	if got := cfg.GitLab.Mirror.Include; len(got) != 2 || got[0] != "main" || got[1] != "agent/*" {
		t.Fatalf("gitlab mirror include=%v", got)
	}
	gitLabCfg := cfg.GitLab.ToGitLabIntegrationConfig()
	if gitLabCfg.Repos["example-owner/demo"].ProjectPath != "backup/demo" || gitLabCfg.Repos["example-owner/demo"].EnvBranches["uat"] != "uat" {
		t.Fatalf("gitlab integration map=%#v", gitLabCfg.Repos["example-owner/demo"])
	}
	if len(gitLabCfg.MirrorBranchIncludes) != 2 || gitLabCfg.MirrorBranchIncludes[0] != "main" || gitLabCfg.MirrorBranchExcludes[0] != "ci/*" {
		t.Fatalf("gitlab integration mirror=%#v excludes=%#v", gitLabCfg.MirrorBranchIncludes, gitLabCfg.MirrorBranchExcludes)
	}
	if !cfg.GitHub.Enabled || cfg.GitHub.BaseURL != "https://api.github.com" || cfg.GitHub.RemoteURL != "git@github.com:example-org/project-kit.git" || cfg.GitHub.TokenFile != "/run/secrets/github-token" || cfg.GitHub.MergeAuthority != "forgejo" {
		t.Fatalf("github config=%#v", cfg.GitHub)
	}
	if cfg.GitHub.Repos["operator/project-kit"].Owner != "example-org" || cfg.GitHub.Repos["operator/project-kit"].Repo != "project-kit" || cfg.GitHub.Repos["operator/project-kit"].TargetBranch != "main" {
		t.Fatalf("github repo map=%#v", cfg.GitHub.Repos["operator/project-kit"])
	}
	gitHubCfg := cfg.GitHub.ToGitHubIntegrationConfig()
	if gitHubCfg.RemoteURL != "git@github.com:example-org/project-kit.git" || gitHubCfg.MergeAuthority != "forgejo" || gitHubCfg.BaseURL != "https://api.github.com" || gitHubCfg.Repos["operator/project-kit"].Owner != "example-org" || gitHubCfg.Repos["operator/project-kit"].Repo != "project-kit" {
		t.Fatalf("github integration map=%#v", gitHubCfg)
	}
	if len(gitHubCfg.MirrorBranchIncludes) != 2 || gitHubCfg.MirrorBranchIncludes[0] != "main" || gitHubCfg.MirrorBranchExcludes[0] != "ci/*" {
		t.Fatalf("github integration mirror=%#v excludes=%#v", gitHubCfg.MirrorBranchIncludes, gitHubCfg.MirrorBranchExcludes)
	}
	if !cfg.ExecutionContext.Enabled || len(cfg.ExecutionContext.Connectors) != 1 {
		t.Fatalf("execution context config=%#v", cfg.ExecutionContext)
	}
	contextConnector := cfg.ExecutionContext.Connectors[0]
	if contextConnector.SourceInstanceID != "multica-mini" || contextConnector.Adapter != "multica_current_execution_context_v1" || contextConnector.EgressEndpoint != "http://multica-backend:8080/api/integrations/current-execution-context" || contextConnector.Timeout != "4s" || contextConnector.WorkspaceMappings["11111111-1111-4111-8111-111111111111"] != "primary-a" {
		t.Fatalf("execution context connector=%#v", contextConnector)
	}
	target := cfg.Notifications.Targets["frontend_ci_debug"]
	if !target.Enabled || target.Type != "feishu_webhook" || target.Name != "前端CI测试" || target.WebhookURLFile != "/run/secrets/feishu-webhook" || target.Timeout != "5s" {
		t.Fatalf("notification target mismatch: %#v", target)
	}
	mergedEvent := cfg.Notifications.Events["pull_request_merged"]
	if !mergedEvent.Enabled || len(mergedEvent.Targets) != 1 || mergedEvent.Targets[0] != "frontend_ci_debug" {
		t.Fatalf("pull_request_merged event mismatch: %#v", mergedEvent)
	}
	incidentEvent := cfg.Notifications.Events["multica_incident"]
	if !incidentEvent.Enabled || incidentEvent.ThrottleWindow != "15m" || len(incidentEvent.Targets) != 1 || incidentEvent.Targets[0] != "frontend_ci_debug" {
		t.Fatalf("multica_incident event mismatch: %#v", incidentEvent)
	}
	if !cfg.ProjectionWatch.Enabled || cfg.ProjectionWatch.PollInterval != "2m" || cfg.ProjectionWatch.ScanInterval != "15m" || cfg.ProjectionWatch.FullAuditInterval != "24h" || cfg.ProjectionWatch.StartupAuditDelay != "5m" || cfg.ProjectionWatch.Repos != "*" || cfg.ProjectionWatch.RefPolicy != "mirror" {
		t.Fatalf("projection watch config mismatch: %#v", cfg.ProjectionWatch)
	}
	if len(cfg.ProjectionWatch.Notify.On) != 3 || cfg.ProjectionWatch.Notify.ThrottleWindow != "30m" || cfg.ProjectionWatch.Notify.GracePeriod != "90s" {
		t.Fatalf("projection watch notify mismatch: %#v", cfg.ProjectionWatch.Notify)
	}
	if cfg.ProjectionWatch.Repair.Auto || len(cfg.ProjectionWatch.Repair.SafeRepairs) != 2 {
		t.Fatalf("projection watch repair mismatch: %#v", cfg.ProjectionWatch.Repair)
	}
	if !cfg.Multica.Enabled || cfg.Multica.TargetInstance != "primary-b" || cfg.Multica.Command != "/usr/local/bin/multica" || cfg.Multica.Workspace != "workspace-alpha" || cfg.Multica.WorkspaceID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("multica config mismatch: %#v", cfg.Multica)
	}
	if cfg.Multica.DefaultWorkspace != "workspace-alpha" || cfg.Multica.DefaultWorkspaceID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("multica default workspace mismatch: %#v", cfg.Multica)
	}
	if cfg.Multica.ServerURL != "http://localhost:3000" || cfg.Multica.ExternalPRProvider != "ags" || cfg.Multica.LinkTokenAudience != "external-pr-link" || cfg.Multica.LinkTokenSecret != "link-secret" || cfg.Multica.ServiceToken != "service-secret" {
		t.Fatalf("multica external PR config mismatch: %#v", cfg.Multica)
	}
	if !cfg.Multica.CompletionOnMerge.Enabled || cfg.Multica.CompletionOnMerge.Mode != "leaf_child_only" {
		t.Fatalf("multica completion config mismatch: %#v", cfg.Multica.CompletionOnMerge)
	}
	if cfg.Multica.Repos["example-owner/personal"].Workspace != "workspace-beta" || cfg.Multica.Repos["example-owner/personal"].WorkspaceID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("multica repo workspace mismatch: %#v", cfg.Multica.Repos["example-owner/personal"])
	}
	if cfg.Multica.Workspaces["workspace-beta"].WorkspaceID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("multica workspace map mismatch: %#v", cfg.Multica.Workspaces["workspace-beta"])
	}
	projectionCfg := cfg.Multica.ToMulticaProjectionConfig()
	if projectionCfg.Repos["example-owner/personal"].WorkspaceID != "55555555-5555-4555-8555-555555555555" || projectionCfg.Workspaces["workspace-beta"].WorkspaceID != "55555555-5555-4555-8555-555555555555" {
		t.Fatalf("multica projection config mismatch: %#v", projectionCfg)
	}
	if projectionCfg.ServerURL != "http://localhost:3000" || projectionCfg.TargetInstance != "primary-b" || projectionCfg.ExternalPRProvider != "ags" || projectionCfg.LinkTokenAudience != "external-pr-link" || projectionCfg.LinkTokenSecret != "link-secret" || projectionCfg.ServiceToken != "service-secret" || !projectionCfg.CompletionOnMerge.Enabled {
		t.Fatalf("multica projection external PR config mismatch: %#v", projectionCfg)
	}
	if !cfg.Multica.FailureWatch.Enabled || cfg.Multica.FailureWatch.PollInterval != "1m" || cfg.Multica.FailureWatch.RollingWindowDays != 30 || cfg.Multica.FailureWatch.IssueLimit != 75 {
		t.Fatalf("multica failure watch mismatch: %#v", cfg.Multica.FailureWatch)
	}
}

func TestLoadFileRejectsInvalidForgejoActionPrincipalBindings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding string
	}{
		{name: "zero principal", binding: "example-human: 0"},
		{name: "path-like actor", binding: "'bad/actor': 7"},
		{name: "secret-shaped actor", binding: "mat_secretvalue: 7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "integrations.yaml")
			body := "forgejo:\n  authority_policy:\n    action_principal_bindings:\n      " + tc.binding + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "action_principal_bindings") {
				t.Fatalf("expected action principal binding rejection, got %v", err)
			}
		})
	}
}

func TestLoadFileRejectsRetiredMulticaAuthorityConfiguration(t *testing.T) {
	for _, key := range []string{"delegated_pr_merge_enabled: false", "workload_assertion: {}"} {
		path := filepath.Join(t.TempDir(), "integrations.yaml")
		if err := os.WriteFile(path, []byte("multica:\n  "+key+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "is retired") {
			t.Fatalf("retired key %q err=%v", key, err)
		}
	}
}

func TestLoadFileReadsMulticaExternalPRSecretFiles(t *testing.T) {
	dir := t.TempDir()
	linkSecretPath := filepath.Join(dir, "link-secret")
	serviceTokenPath := filepath.Join(dir, "service-token")
	if err := os.WriteFile(linkSecretPath, []byte("link-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serviceTokenPath, []byte("service-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "integrations.yaml")
	if err := os.WriteFile(path, []byte(`
multica:
  enabled: true
  link_token_secret_file: `+linkSecretPath+`
  service_token_file: `+serviceTokenPath+`
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Multica.LinkTokenSecret != "link-from-file" || cfg.Multica.ServiceToken != "service-from-file" {
		t.Fatalf("secrets not loaded from files: %#v", cfg.Multica)
	}
	projectionCfg := cfg.Multica.ToMulticaProjectionConfig()
	if projectionCfg.LinkTokenSecret != "link-from-file" || projectionCfg.ServiceToken != "service-from-file" {
		t.Fatalf("projection secrets not loaded from files: %#v", projectionCfg)
	}
}

func TestAccessGrantRequiresCIDefaultsTrueAndHonorsRepoFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	if err := os.WriteFile(path, []byte(`
repositories:
  example-team/shipping-fixture:
    merge_policy:
      min_approvals: 2
      require_ci: false
  operator/other:
    merge_policy:
      min_approvals: 1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.AccessGrantRequiresCI("example-team/shipping-fixture") {
		t.Fatal("explicit repository override should not require CI")
	}
	if !cfg.AccessGrantRequiresCI("operator/other") {
		t.Fatal("missing require_ci must default to require CI")
	}
	if !cfg.AccessGrantRequiresCI("operator/unknown") {
		t.Fatal("unknown repo must require CI")
	}
	var empty Config
	if !empty.AccessGrantRequiresCI("example-team/shipping-fixture") {
		t.Fatal("empty config must require CI")
	}
}
