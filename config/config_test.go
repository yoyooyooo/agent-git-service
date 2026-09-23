package config

import (
	"strings"
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	// Set optional vars to empty to verify defaults (t.Setenv restores on cleanup)
	t.Setenv("PORT", "")
	t.Setenv("BASE_URL", "")
	t.Setenv("CONSOLE_BASE_URL", "")
	t.Setenv("GIT_REPO_DIR", "")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("expected default Port=8080, got %q", cfg.Port)
	}
	if cfg.BaseURL != "http://localhost:8080" {
		t.Errorf("expected default BaseURL, got %q", cfg.BaseURL)
	}
	if cfg.ConsoleBaseURL != "http://localhost:5173" {
		t.Errorf("expected default ConsoleBaseURL, got %q", cfg.ConsoleBaseURL)
	}
	if cfg.OAuthDeviceVerificationURL != "" {
		t.Errorf("expected default OAuthDeviceVerificationURL empty, got %q", cfg.OAuthDeviceVerificationURL)
	}
	if cfg.GitRepoDir != "gitrepos" {
		t.Errorf("expected default GitRepoDir=gitrepos, got %q", cfg.GitRepoDir)
	}
	if cfg.DBdsn != "user:pass@tcp(localhost)/testdb" {
		t.Errorf("expected DBdsn from env, got %q", cfg.DBdsn)
	}
	if cfg.EnableWorkflowExec {
		t.Error("expected workflow execution to be disabled by default")
	}
	if cfg.WorkflowExecImage != "bash:5.2" {
		t.Errorf("expected default WorkflowExecImage=bash:5.2, got %q", cfg.WorkflowExecImage)
	}
	if cfg.WorkflowExecTimeout != 2*time.Minute {
		t.Errorf("expected default WorkflowExecTimeout=2m, got %s", cfg.WorkflowExecTimeout)
	}
	if cfg.WorkflowExecCPUs != "1.0" {
		t.Errorf("expected default WorkflowExecCPUs=1.0, got %q", cfg.WorkflowExecCPUs)
	}
	if cfg.WorkflowExecMemory != "256m" {
		t.Errorf("expected default WorkflowExecMemory=256m, got %q", cfg.WorkflowExecMemory)
	}
	if cfg.WorkflowExecPidsLimit != 128 {
		t.Errorf("expected default WorkflowExecPidsLimit=128, got %d", cfg.WorkflowExecPidsLimit)
	}
	if cfg.WorkflowExecNoFile != 1024 {
		t.Errorf("expected default WorkflowExecNoFile=1024, got %d", cfg.WorkflowExecNoFile)
	}
	if cfg.WorkflowExecTmpfsSize != "64m" {
		t.Errorf("expected default WorkflowExecTmpfsSize=64m, got %q", cfg.WorkflowExecTmpfsSize)
	}
}

func TestNewOverrides(t *testing.T) {
	t.Setenv("DB_DSN", "custom-dsn")
	t.Setenv("PORT", "9090")
	t.Setenv("BASE_URL", "https://example.com")
	t.Setenv("CONSOLE_BASE_URL", "https://console.example.com")
	t.Setenv("OAUTH_DEVICE_VERIFICATION_URL", "https://console.example.com/device-login")
	t.Setenv("GIT_REPO_DIR", "/tmp/repos")
	t.Setenv("ENABLE_WORKFLOW_EXEC", "1")
	t.Setenv("WORKFLOW_EXEC_IMAGE", "custom/bash:latest")
	t.Setenv("WORKFLOW_EXEC_TIMEOUT", "45s")
	t.Setenv("WORKFLOW_EXEC_CPUS", "0.5")
	t.Setenv("WORKFLOW_EXEC_MEMORY", "128m")
	t.Setenv("WORKFLOW_EXEC_PIDS_LIMIT", "64")
	t.Setenv("WORKFLOW_EXEC_NOFILE", "256")
	t.Setenv("WORKFLOW_EXEC_TMPFS_SIZE", "16m")
	t.Setenv("OIDC_PROVIDER", "casdoor")
	t.Setenv("OIDC_ISSUER", "https://door.example.com")
	t.Setenv("OIDC_CLIENT_ID", "oidc-client")
	t.Setenv("OIDC_SCOPES", "openid profile email groups")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != "9090" {
		t.Errorf("expected Port=9090, got %q", cfg.Port)
	}
	if cfg.BaseURL != "https://example.com" {
		t.Errorf("expected BaseURL=https://example.com, got %q", cfg.BaseURL)
	}
	if cfg.ConsoleBaseURL != "https://console.example.com" {
		t.Errorf("expected ConsoleBaseURL=https://console.example.com, got %q", cfg.ConsoleBaseURL)
	}
	if cfg.OAuthDeviceVerificationURL != "https://console.example.com/device-login" {
		t.Errorf("expected OAuthDeviceVerificationURL override, got %q", cfg.OAuthDeviceVerificationURL)
	}
	if cfg.GitRepoDir != "/tmp/repos" {
		t.Errorf("expected GitRepoDir=/tmp/repos, got %q", cfg.GitRepoDir)
	}
	if cfg.DBdsn != "custom-dsn" {
		t.Errorf("expected DBdsn=custom-dsn, got %q", cfg.DBdsn)
	}
	if !cfg.EnableWorkflowExec {
		t.Error("expected workflow execution to be enabled")
	}
	if cfg.WorkflowExecImage != "custom/bash:latest" {
		t.Errorf("expected WorkflowExecImage override, got %q", cfg.WorkflowExecImage)
	}
	if cfg.WorkflowExecTimeout != 45*time.Second {
		t.Errorf("expected WorkflowExecTimeout=45s, got %s", cfg.WorkflowExecTimeout)
	}
	if cfg.WorkflowExecCPUs != "0.5" {
		t.Errorf("expected WorkflowExecCPUs=0.5, got %q", cfg.WorkflowExecCPUs)
	}
	if cfg.WorkflowExecMemory != "128m" {
		t.Errorf("expected WorkflowExecMemory=128m, got %q", cfg.WorkflowExecMemory)
	}
	if cfg.WorkflowExecPidsLimit != 64 {
		t.Errorf("expected WorkflowExecPidsLimit=64, got %d", cfg.WorkflowExecPidsLimit)
	}
	if cfg.WorkflowExecNoFile != 256 {
		t.Errorf("expected WorkflowExecNoFile=256, got %d", cfg.WorkflowExecNoFile)
	}
	if cfg.WorkflowExecTmpfsSize != "16m" {
		t.Errorf("expected WorkflowExecTmpfsSize=16m, got %q", cfg.WorkflowExecTmpfsSize)
	}
	if cfg.OIDCProvider != "casdoor" || cfg.OIDCIssuer != "https://door.example.com" || cfg.OIDCClientID != "oidc-client" {
		t.Fatalf("expected explicit oidc config to be loaded, got %+v", cfg)
	}
	if cfg.OIDCScopes != "openid profile email groups" {
		t.Fatalf("expected explicit oidc scopes, got %q", cfg.OIDCScopes)
	}
}

func TestNewLoadsConnectedLoginConfig(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("CONNECTED_LOGIN_PROVIDER", " provider ")
	t.Setenv("CONNECTED_LOGIN_ORIGIN", " https://app.provider.example ")
	t.Setenv("CONNECTED_LOGIN_API_ORIGIN", " https://api.provider.example ")
	t.Setenv("CONNECTED_LOGIN_CLIENT_ID", "connected-client")
	t.Setenv("CONNECTED_LOGIN_CLIENT_SECRET", "connected-secret")
	t.Setenv("CONNECTED_LOGIN_LOGIN_PATH", "/custom/login")
	t.Setenv("CONNECTED_LOGIN_SUBJECT_NAMESPACE_CLAIM", "workspace_id")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !cfg.ConnectedLoginEnabled() {
		t.Fatal("expected connected login to be enabled")
	}
	if cfg.ConnectedLoginProvider != "provider" {
		t.Fatalf("ConnectedLoginProvider: got %q", cfg.ConnectedLoginProvider)
	}
	if cfg.ConnectedLoginOrigin != "https://app.provider.example" {
		t.Fatalf("ConnectedLoginOrigin: got %q", cfg.ConnectedLoginOrigin)
	}
	if cfg.ConnectedLoginAPIOrigin != "https://api.provider.example" {
		t.Fatalf("ConnectedLoginAPIOrigin: got %q", cfg.ConnectedLoginAPIOrigin)
	}
	if cfg.ConnectedLoginClientID != "connected-client" {
		t.Fatalf("ConnectedLoginClientID: got %q", cfg.ConnectedLoginClientID)
	}
	if cfg.ConnectedLoginClientSecret != "connected-secret" {
		t.Fatalf("ConnectedLoginClientSecret: got %q", cfg.ConnectedLoginClientSecret)
	}
	if cfg.ConnectedLoginLoginPath != "/custom/login" {
		t.Fatalf("ConnectedLoginLoginPath: got %q", cfg.ConnectedLoginLoginPath)
	}
	if cfg.ConnectedLoginSubjectNamespaceClaim != "workspace_id" {
		t.Fatalf("ConnectedLoginSubjectNamespaceClaim: got %q", cfg.ConnectedLoginSubjectNamespaceClaim)
	}
}

func TestOAuthDeviceVerificationURLValidation(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")

	t.Run("trims valid url", func(t *testing.T) {
		t.Setenv("OAUTH_DEVICE_VERIFICATION_URL", " https://console.example.com/device?tenant=one ")
		cfg, err := New()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.OAuthDeviceVerificationURL != "https://console.example.com/device?tenant=one" {
			t.Fatalf("unexpected OAuthDeviceVerificationURL: %q", cfg.OAuthDeviceVerificationURL)
		}
	})

	t.Run("rejects invalid urls", func(t *testing.T) {
		for _, raw := range []string{"console.example.com/device", "ftp://console.example.com/device", "https:///device"} {
			t.Run(raw, func(t *testing.T) {
				t.Setenv("OAUTH_DEVICE_VERIFICATION_URL", raw)
				_, err := New()
				if err == nil {
					t.Fatalf("expected OAUTH_DEVICE_VERIFICATION_URL=%q to fail", raw)
				}
				if !strings.Contains(err.Error(), "OAUTH_DEVICE_VERIFICATION_URL") {
					t.Fatalf("unexpected error: %v", err)
				}
			})
		}
	})
}

func TestNewRejectsPartialConnectedLoginConfig(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("CONNECTED_LOGIN_ORIGIN", "https://app.provider.example")
	t.Setenv("CONNECTED_LOGIN_API_ORIGIN", "")
	t.Setenv("CONNECTED_LOGIN_CLIENT_ID", "connected-client")
	t.Setenv("CONNECTED_LOGIN_CLIENT_SECRET", "")

	_, err := New()
	if err == nil {
		t.Fatal("expected partial connected login config to fail")
	}
	if !strings.Contains(err.Error(), "connected login: partial configuration") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOIDCProviderDefaultsToAuth0ForAuth0Issuer(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("OIDC_PROVIDER", "")
	t.Setenv("OIDC_ISSUER", "https://account.us.auth0.com/")
	t.Setenv("OIDC_CLIENT_ID", "oidc-client")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OIDCProvider != "auth0" {
		t.Fatalf("expected OIDCProvider=auth0 for Auth0 issuer, got %q", cfg.OIDCProvider)
	}
}

func TestOIDCProviderDefaultsToOIDCForNonAuth0Issuer(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("OIDC_PROVIDER", "")
	t.Setenv("OIDC_ISSUER", "https://issuer.example.com")
	t.Setenv("OIDC_CLIENT_ID", "oidc-client")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OIDCProvider != "oidc" {
		t.Fatalf("expected OIDCProvider=oidc for generic issuer, got %q", cfg.OIDCProvider)
	}
}

func TestNewErrorsWithoutDBDSN(t *testing.T) {
	t.Setenv("DB_DSN", "")

	_, err := New()
	if err == nil {
		t.Fatal("expected error when DB_DSN is not set")
	}
	expected := "required environment variable not set: DB_DSN"
	if err.Error() != expected {
		t.Errorf("unexpected error message: %q", err.Error())
	}
}

func TestEnvironmentDefaultsToProduction(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("ENVIRONMENT", "")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Environment != "production" {
		t.Errorf("expected default Environment=production, got %q", cfg.Environment)
	}
}

func TestEnvironmentExplicitDevelopment(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("ENVIRONMENT", "development")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Environment != "development" {
		t.Errorf("expected Environment=development, got %q", cfg.Environment)
	}
}

func TestListenModeDefault(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("LISTEN_MODE", "")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenMode != "development" {
		t.Errorf("expected default ListenMode=development, got %q", cfg.ListenMode)
	}
}

func TestListenModeProduction(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("LISTEN_MODE", "production")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenMode != "production" {
		t.Errorf("expected ListenMode=production, got %q", cfg.ListenMode)
	}
}

func TestListenModeInvalid(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")

	for _, mode := range []string{"Production", "PRODUCTION", "dev", "staging", "typo"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("LISTEN_MODE", mode)
			_, err := New()
			if err == nil {
				t.Fatalf("expected error for LISTEN_MODE=%q, got nil", mode)
			}
		})
	}
}

func TestEnvironmentDefaultProduction(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("ENVIRONMENT", "") // unset — must default to production

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Environment != "production" {
		t.Errorf("expected default Environment=production, got %q", cfg.Environment)
	}
}

func TestEnvironmentDevelopment(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("ENVIRONMENT", "development")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Environment != "development" {
		t.Errorf("expected Environment=development, got %q", cfg.Environment)
	}
}

func TestEnvironmentNormalization(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")

	for _, val := range []string{"PRODUCTION", "Production", " production ", "DEVELOPMENT", "Development", " development "} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("ENVIRONMENT", val)
			cfg, err := New()
			if err != nil {
				t.Fatalf("unexpected error for ENVIRONMENT=%q: %v", val, err)
			}
			norm := cfg.Environment
			if norm != "production" && norm != "development" {
				t.Errorf("expected normalized value, got %q for input %q", norm, val)
			}
		})
	}
}

func TestEnvironmentInvalid(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")

	for _, val := range []string{"staging", "dev", "prod", "test", "typo"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("ENVIRONMENT", val)
			_, err := New()
			if err == nil {
				t.Fatalf("expected error for ENVIRONMENT=%q, got nil", val)
			}
		})
	}
}

func TestWorkflowExecTimeoutInvalid(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("WORKFLOW_EXEC_TIMEOUT", "nope")

	if _, err := New(); err == nil {
		t.Fatal("expected error for invalid WORKFLOW_EXEC_TIMEOUT")
	}
}

func TestWorkflowExecPidsLimitInvalid(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("WORKFLOW_EXEC_PIDS_LIMIT", "0")

	if _, err := New(); err == nil {
		t.Fatal("expected error for invalid WORKFLOW_EXEC_PIDS_LIMIT")
	}
}

func TestWorkflowExecNoFileInvalid(t *testing.T) {
	t.Setenv("DB_DSN", "user:pass@tcp(localhost)/testdb")
	t.Setenv("WORKFLOW_EXEC_NOFILE", "-1")

	if _, err := New(); err == nil {
		t.Fatal("expected error for invalid WORKFLOW_EXEC_NOFILE")
	}
}

func TestForgejoIntegrationConfigFromEnv(t *testing.T) {
	t.Setenv("DB_DSN", "custom-dsn")
	t.Setenv("AGS_INTEGRATIONS_CONFIG", "/etc/ags/integrations.yaml")
	t.Setenv("FORGEJO_INTEGRATION_ENABLED", "1")
	t.Setenv("FORGEJO_INTEGRATION_BASE_URL", "http://forgejo.local:5555")
	t.Setenv("FORGEJO_INTEGRATION_TOKEN_FILE", "/run/secrets/forgejo-token")
	t.Setenv("FORGEJO_INTEGRATION_DEFAULT_OWNER", "ci")
	t.Setenv("FORGEJO_INTEGRATION_REPO_MAP", "/etc/ags/forgejo-map.json")
	t.Setenv("FORGEJO_INTEGRATION_BRANCH_INCLUDE", "legacy/*")
	t.Setenv("FORGEJO_INTEGRATION_BRANCH_EXCLUDE", "legacy-skip/*")
	t.Setenv("FORGEJO_INTEGRATION_MIRROR_BRANCH_INCLUDE", "main,agent/*,feature/*")
	t.Setenv("FORGEJO_INTEGRATION_MIRROR_BRANCH_EXCLUDE", "ci/*")
	t.Setenv("FORGEJO_INTEGRATION_PR_BRANCH_INCLUDE", "agent/*,feature/*")
	t.Setenv("FORGEJO_INTEGRATION_PR_BRANCH_EXCLUDE", "main,ci/*")
	t.Setenv("FORGEJO_INTEGRATION_AUTO_CREATE_REPO", "1")
	t.Setenv("FORGEJO_INTEGRATION_AUTO_PR", "true")
	t.Setenv("FORGEJO_INTEGRATION_PR_BASE", "develop")
	t.Setenv("FORGEJO_INTEGRATION_PRIVATE_REPOS", "true")
	t.Setenv("FORGEJO_INTEGRATION_PUSH_GIT_CONFIG", "core.compression=0, pack.window=0")
	t.Setenv("FORGEJO_INTEGRATION_PUSH_TIMEOUT", "8m")
	t.Setenv("FORGEJO_PROJECTION_WORKER_TIMEOUT", "10m")
	t.Setenv("FORGEJO_PROJECTION_WORKER_MAX_ATTEMPTS", "4")
	t.Setenv("FORGEJO_PROJECTION_WORKER_RETRY_DELAY", "2s")
	t.Setenv("AGS_ALLOW_MISSING_PROJECTION_ALERTING", "true")
	t.Setenv("FORGEJO_INTEGRATION_AUTHORITY_POLICY_ENABLED", "true")
	t.Setenv("FORGEJO_INTEGRATION_AUTHORITY_POLICY_TOKEN_FILE", "/run/secrets/forgejo-operator-token")
	t.Setenv("FORGEJO_INTEGRATION_BOT", "ags-bot")
	t.Setenv("FORGEJO_ACTIONS_LOG_DIR", "/opt/forgejo/data/gitea/actions_log")
	t.Setenv("AGS_ALLOW_MISSING_FORGEJO_AUTHORITY_POLICY", "true")

	cfg, err := New()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.IntegrationsConfigFile != "/etc/ags/integrations.yaml" {
		t.Fatalf("integrations config=%q", cfg.IntegrationsConfigFile)
	}
	if !cfg.ForgejoIntegrationEnabled {
		t.Fatal("expected ForgejoIntegrationEnabled=true")
	}
	if cfg.ForgejoIntegrationBaseURL != "http://forgejo.local:5555" {
		t.Fatalf("base URL=%q", cfg.ForgejoIntegrationBaseURL)
	}
	if cfg.ForgejoIntegrationTokenFile != "/run/secrets/forgejo-token" {
		t.Fatalf("token file=%q", cfg.ForgejoIntegrationTokenFile)
	}
	if cfg.ForgejoIntegrationDefaultOwner != "ci" {
		t.Fatalf("default owner=%q", cfg.ForgejoIntegrationDefaultOwner)
	}
	if cfg.ForgejoIntegrationRepoMapFile != "/etc/ags/forgejo-map.json" {
		t.Fatalf("repo map=%q", cfg.ForgejoIntegrationRepoMapFile)
	}
	if strings.Join(cfg.ForgejoIntegrationBranchInclude, ",") != "legacy/*" {
		t.Fatalf("legacy include=%v", cfg.ForgejoIntegrationBranchInclude)
	}
	if strings.Join(cfg.ForgejoIntegrationBranchExclude, ",") != "legacy-skip/*" {
		t.Fatalf("legacy exclude=%v", cfg.ForgejoIntegrationBranchExclude)
	}
	if strings.Join(cfg.ForgejoIntegrationMirrorBranchInclude, ",") != "main,agent/*,feature/*" {
		t.Fatalf("mirror include=%v", cfg.ForgejoIntegrationMirrorBranchInclude)
	}
	if strings.Join(cfg.ForgejoIntegrationMirrorBranchExclude, ",") != "ci/*" {
		t.Fatalf("mirror exclude=%v", cfg.ForgejoIntegrationMirrorBranchExclude)
	}
	if strings.Join(cfg.ForgejoIntegrationPRBranchInclude, ",") != "agent/*,feature/*" {
		t.Fatalf("pr include=%v", cfg.ForgejoIntegrationPRBranchInclude)
	}
	if strings.Join(cfg.ForgejoIntegrationPRBranchExclude, ",") != "main,ci/*" {
		t.Fatalf("pr exclude=%v", cfg.ForgejoIntegrationPRBranchExclude)
	}
	if !cfg.ForgejoIntegrationAutoCreateRepo || !cfg.ForgejoIntegrationAutoPullRequest || !cfg.ForgejoIntegrationPrivateRepos {
		t.Fatalf("expected auto-create, auto-pr, and private repos enabled")
	}
	if cfg.ForgejoIntegrationDefaultBaseBranch != "develop" {
		t.Fatalf("base branch=%q", cfg.ForgejoIntegrationDefaultBaseBranch)
	}
	if strings.Join(cfg.ForgejoIntegrationPushGitConfig, ",") != "core.compression=0,pack.window=0" {
		t.Fatalf("push git config=%v", cfg.ForgejoIntegrationPushGitConfig)
	}
	if cfg.ForgejoIntegrationPushTimeout.String() != "8m0s" {
		t.Fatalf("push timeout=%s", cfg.ForgejoIntegrationPushTimeout)
	}
	if cfg.ForgejoProjectionWorkerTimeout.String() != "10m0s" || cfg.ForgejoProjectionWorkerMaxAttempts != 4 || cfg.ForgejoProjectionWorkerRetryDelay.String() != "2s" {
		t.Fatalf("worker timeout/retry config=%s/%d/%s", cfg.ForgejoProjectionWorkerTimeout, cfg.ForgejoProjectionWorkerMaxAttempts, cfg.ForgejoProjectionWorkerRetryDelay)
	}
	if !cfg.AllowMissingProjectionAlerting {
		t.Fatal("expected explicit projection alerting opt-out")
	}
	if !cfg.ForgejoAuthorityPolicyEnabled || cfg.ForgejoIntegrationBot != "ags-bot" || cfg.ForgejoAuthorityPolicyTokenFile != "/run/secrets/forgejo-operator-token" {
		t.Fatalf("unexpected Forgejo authority policy config: enabled=%v bot=%q token_file=%q", cfg.ForgejoAuthorityPolicyEnabled, cfg.ForgejoIntegrationBot, cfg.ForgejoAuthorityPolicyTokenFile)
	}
	if !cfg.AllowMissingForgejoAuthorityPolicy {
		t.Fatal("expected explicit Forgejo authority policy opt-out")
	}
	if cfg.ForgejoActionsLogDir != "/opt/forgejo/data/gitea/actions_log" {
		t.Fatalf("actions log dir=%q", cfg.ForgejoActionsLogDir)
	}
}
