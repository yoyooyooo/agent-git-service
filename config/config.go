// Package config provides typed configuration loaded from environment variables.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all server configuration.
type Config struct {
	Port    string
	BaseURL string
	// APIBaseURL is an optional advertised API origin, independent of Git clone
	// URLs and callbacks. It is explicit operator configuration, never Host input.
	APIBaseURL string
	DBdsn      string
	GitRepoDir string
	// ControlPlaneDSN is retained only to reject retired deployment settings.
	// It must never silently fall back to the single application database.
	ControlPlaneDSN string
	// Fork deployments retain legacy extension URLs without redirects. Set
	// this after client migration to use upstream's canonical-only surface.
	DisableLegacyExtensionAliases bool

	// IntegrationsConfigFile points to optional YAML configuration for external integrations.
	IntegrationsConfigFile string

	// ReplicationConfigFile explicitly enables a separate mTLS peer listener
	// inside the owning primary process. Empty keeps existing deployments unchanged.
	ReplicationConfigFile string
	// Explicit operator identity enables native-admin registration before a
	// peer listener exists. Must match authority_id when both are configured.
	ReplicationAuthorityID string

	// ListenMode controls listener setup: "development" (default) starts
	// multiple listeners with TLS; "production" starts a single HTTP listener.
	ListenMode string

	// AllowAnyToken, when true, accepts any non-empty token when no
	// tokens exist in the database (dev-mode convenience).
	// Default is false (production-secure).
	AllowAnyToken bool

	// AdminLogin and AdminToken override the default seed credentials.
	// When both are empty the legacy testadmin / mytoken values are used.
	AdminLogin string
	AdminToken string

	// Environment controls operational behaviour that differs between
	// deployments. Allowed values: "production" (default, fail-closed) and
	// "development". When set to "development", test seed data is inserted
	// at startup. The default is "production" so that an unset variable
	// never silently seeds credentials.
	Environment string

	// Embedding provider configuration (all optional).
	// When EmbeddingAPIKey is empty, vector search is disabled and
	// search falls back to lexical-only matching.
	EmbeddingAPIKey  string
	EmbeddingBaseURL string
	EmbeddingModel   string
	// EmbeddingDimensions overrides the embedding vector size (0 = auto-detect).
	EmbeddingDimensions int

	OIDCProvider          string
	OIDCIssuer            string
	OIDCDiscoveryURL      string
	OIDCClientID          string
	OIDCClientSecret      string
	OIDCAudience          string
	OIDCScopes            string
	OIDCAllowInsecureHTTP bool

	// ConnectedLogin configures an OAuth-style browser login provider that does
	// not expose enough standard OIDC surface for the generic OIDC client.
	// Origin, APIOrigin, ClientID, and ClientSecret must be set together to
	// enable /auth/connected/login and /auth/connected/callback.
	ConnectedLoginProvider                  string
	ConnectedLoginOrigin                    string
	ConnectedLoginAPIOrigin                 string
	ConnectedLoginClientID                  string
	ConnectedLoginClientSecret              string
	ConnectedLoginLoginPath                 string
	ConnectedLoginTokenPath                 string
	ConnectedLoginUserinfoPath              string
	ConnectedLoginReturnToParam             string
	ConnectedLoginSubjectClaim              string
	ConnectedLoginSubjectNamespaceClaim     string
	ConnectedLoginSubjectNamespaceSlugClaim string
	ConnectedLoginActorTypeClaim            string
	ConnectedLoginHumanTypeValue            string
	ConnectedLoginAgentTypeValue            string
	ConnectedLoginClientIDClaim             string
	ConnectedLoginClientNameClaim           string
	ConnectedLoginPreferredUsernameClaim    string
	ConnectedLoginNameClaim                 string
	ConnectedLoginPictureClaim              string
	ConnectedLoginAvatarURLClaim            string
	ConnectedLoginDescriptionClaim          string
	ConnectedLoginScopeClaim                string

	// ConsoleBaseURL is the base URL of the console frontend used for browser redirects.
	ConsoleBaseURL string
	// OAuthDeviceVerificationURL is the optional external console URL shown to
	// device-flow users. When empty, the built-in /login/device fallback is used.
	OAuthDeviceVerificationURL string

	// Workflow execution sandbox configuration. Execution is fail-closed by
	// default and only enabled when ENABLE_WORKFLOW_EXEC is set.
	EnableWorkflowExec    bool
	WorkflowExecImage     string
	WorkflowExecTimeout   time.Duration
	WorkflowExecCPUs      string
	WorkflowExecMemory    string
	WorkflowExecPidsLimit int
	WorkflowExecNoFile    int
	WorkflowExecTmpfsSize string

	// ForgejoIntegration mirrors selected AGS post-push branch updates to a
	// Forgejo repository so Forgejo PRs and Actions can act as the CI control plane.
	ForgejoIntegrationEnabled             bool
	ForgejoIntegrationBaseURL             string
	ForgejoIntegrationToken               string
	ForgejoIntegrationTokenFile           string
	ForgejoIntegrationWebhookSecret       string
	ForgejoIntegrationWebhookSecretFile   string
	ForgejoIntegrationDefaultOwner        string
	ForgejoIntegrationRepoMapFile         string
	ForgejoIntegrationBranchInclude       []string // Legacy combined mirror/PR policy when split policies are unset.
	ForgejoIntegrationBranchExclude       []string // Legacy combined mirror/PR policy when split policies are unset.
	ForgejoIntegrationMirrorBranchInclude []string
	ForgejoIntegrationMirrorBranchExclude []string
	ForgejoIntegrationPRBranchInclude     []string
	ForgejoIntegrationPRBranchExclude     []string
	ForgejoIntegrationAutoCreateRepo      bool
	ForgejoIntegrationAutoPullRequest     bool
	ForgejoIntegrationDefaultBaseBranch   string
	ForgejoIntegrationPrivateRepos        bool
	ForgejoIntegrationPushGitConfig       []string
	ForgejoIntegrationPushTimeout         time.Duration
	ForgejoAuthorityPolicyEnabled         bool
	ForgejoAuthorityPolicyTokenFile       string
	ForgejoIntegrationBot                 string
	ForgejoProjectionWorkerTimeout        time.Duration
	ForgejoProjectionWorkerMaxAttempts    int
	ForgejoProjectionWorkerRetryDelay     time.Duration
	ForgejoActionsLogDir                  string
	ProviderLogBridgeEnabled              bool
	ProviderLogBridgeTokenFile            string
	ProviderLogBridgeForgejoDBDSNFile     string
	ProviderLogBridgeAllowedCIDRs         []string
	ProviderLogBridgeMaxBytes             int64
	// AllowMissingProjectionAlerting is an explicit development/test opt-out.
	// Production deployments using Forgejo workflow actions should leave it false.
	AllowMissingProjectionAlerting     bool
	AllowMissingForgejoAuthorityPolicy bool
}

// New reads environment variables and returns a fully-populated Config.
// It returns an error if any required variable (DB_DSN) is missing.
func New() (Config, error) {
	cfg := Config{
		Port:                                    os.Getenv("PORT"),
		BaseURL:                                 os.Getenv("BASE_URL"),
		APIBaseURL:                              os.Getenv("AGS_API_BASE_URL"),
		ConsoleBaseURL:                          os.Getenv("CONSOLE_BASE_URL"),
		OAuthDeviceVerificationURL:              os.Getenv("OAUTH_DEVICE_VERIFICATION_URL"),
		DBdsn:                                   os.Getenv("DB_DSN"),
		ControlPlaneDSN:                         os.Getenv("CONTROL_PLANE_DSN"),
		GitRepoDir:                              os.Getenv("GIT_REPO_DIR"),
		ReplicationConfigFile:                   os.Getenv("AGS_REPLICATION_CONFIG_FILE"),
		ReplicationAuthorityID:                  os.Getenv("AGS_REPLICATION_AUTHORITY_ID"),
		IntegrationsConfigFile:                  os.Getenv("AGS_INTEGRATIONS_CONFIG"),
		ListenMode:                              os.Getenv("LISTEN_MODE"),
		AllowAnyToken:                           os.Getenv("ALLOW_ANY_TOKEN") == "true" || os.Getenv("ALLOW_ANY_TOKEN") == "1",
		AdminLogin:                              os.Getenv("ADMIN_LOGIN"),
		AdminToken:                              os.Getenv("ADMIN_TOKEN"),
		Environment:                             os.Getenv("ENVIRONMENT"),
		EmbeddingAPIKey:                         os.Getenv("EMBEDDING_API_KEY"),
		EmbeddingBaseURL:                        os.Getenv("EMBEDDING_BASE_URL"),
		EmbeddingModel:                          os.Getenv("EMBEDDING_MODEL"),
		OIDCProvider:                            os.Getenv("OIDC_PROVIDER"),
		OIDCIssuer:                              os.Getenv("OIDC_ISSUER"),
		OIDCDiscoveryURL:                        os.Getenv("OIDC_DISCOVERY_URL"),
		OIDCClientID:                            os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:                        os.Getenv("OIDC_CLIENT_SECRET"),
		OIDCAudience:                            os.Getenv("OIDC_AUDIENCE"),
		OIDCScopes:                              os.Getenv("OIDC_SCOPES"),
		OIDCAllowInsecureHTTP:                   os.Getenv("OIDC_ALLOW_INSECURE_HTTP") == "true" || os.Getenv("OIDC_ALLOW_INSECURE_HTTP") == "1",
		ConnectedLoginProvider:                  os.Getenv("CONNECTED_LOGIN_PROVIDER"),
		ConnectedLoginOrigin:                    os.Getenv("CONNECTED_LOGIN_ORIGIN"),
		ConnectedLoginAPIOrigin:                 os.Getenv("CONNECTED_LOGIN_API_ORIGIN"),
		ConnectedLoginClientID:                  os.Getenv("CONNECTED_LOGIN_CLIENT_ID"),
		ConnectedLoginClientSecret:              os.Getenv("CONNECTED_LOGIN_CLIENT_SECRET"),
		ConnectedLoginLoginPath:                 os.Getenv("CONNECTED_LOGIN_LOGIN_PATH"),
		ConnectedLoginTokenPath:                 os.Getenv("CONNECTED_LOGIN_TOKEN_PATH"),
		ConnectedLoginUserinfoPath:              os.Getenv("CONNECTED_LOGIN_USERINFO_PATH"),
		ConnectedLoginReturnToParam:             os.Getenv("CONNECTED_LOGIN_RETURN_TO_PARAM"),
		ConnectedLoginSubjectClaim:              os.Getenv("CONNECTED_LOGIN_SUBJECT_CLAIM"),
		ConnectedLoginSubjectNamespaceClaim:     os.Getenv("CONNECTED_LOGIN_SUBJECT_NAMESPACE_CLAIM"),
		ConnectedLoginSubjectNamespaceSlugClaim: os.Getenv("CONNECTED_LOGIN_SUBJECT_NAMESPACE_SLUG_CLAIM"),
		ConnectedLoginActorTypeClaim:            os.Getenv("CONNECTED_LOGIN_ACTOR_TYPE_CLAIM"),
		ConnectedLoginHumanTypeValue:            os.Getenv("CONNECTED_LOGIN_HUMAN_TYPE_VALUE"),
		ConnectedLoginAgentTypeValue:            os.Getenv("CONNECTED_LOGIN_AGENT_TYPE_VALUE"),
		ConnectedLoginClientIDClaim:             os.Getenv("CONNECTED_LOGIN_CLIENT_ID_CLAIM"),
		ConnectedLoginClientNameClaim:           os.Getenv("CONNECTED_LOGIN_CLIENT_NAME_CLAIM"),
		ConnectedLoginPreferredUsernameClaim:    os.Getenv("CONNECTED_LOGIN_PREFERRED_USERNAME_CLAIM"),
		ConnectedLoginNameClaim:                 os.Getenv("CONNECTED_LOGIN_NAME_CLAIM"),
		ConnectedLoginPictureClaim:              os.Getenv("CONNECTED_LOGIN_PICTURE_CLAIM"),
		ConnectedLoginAvatarURLClaim:            os.Getenv("CONNECTED_LOGIN_AVATAR_URL_CLAIM"),
		ConnectedLoginDescriptionClaim:          os.Getenv("CONNECTED_LOGIN_DESCRIPTION_CLAIM"),
		ConnectedLoginScopeClaim:                os.Getenv("CONNECTED_LOGIN_SCOPE_CLAIM"),
		EnableWorkflowExec:                      os.Getenv("ENABLE_WORKFLOW_EXEC") == "true" || os.Getenv("ENABLE_WORKFLOW_EXEC") == "1",
		WorkflowExecImage:                       os.Getenv("WORKFLOW_EXEC_IMAGE"),
		WorkflowExecCPUs:                        os.Getenv("WORKFLOW_EXEC_CPUS"),
		WorkflowExecMemory:                      os.Getenv("WORKFLOW_EXEC_MEMORY"),
		WorkflowExecTmpfsSize:                   os.Getenv("WORKFLOW_EXEC_TMPFS_SIZE"),
		ForgejoIntegrationEnabled:               envBool("FORGEJO_INTEGRATION_ENABLED"),
		ForgejoIntegrationBaseURL:               os.Getenv("FORGEJO_INTEGRATION_BASE_URL"),
		ForgejoIntegrationToken:                 os.Getenv("FORGEJO_INTEGRATION_TOKEN"),
		ForgejoIntegrationTokenFile:             os.Getenv("FORGEJO_INTEGRATION_TOKEN_FILE"),
		ForgejoIntegrationWebhookSecret:         os.Getenv("FORGEJO_INTEGRATION_WEBHOOK_SECRET"),
		ForgejoIntegrationWebhookSecretFile:     os.Getenv("FORGEJO_INTEGRATION_WEBHOOK_SECRET_FILE"),
		ForgejoIntegrationDefaultOwner:          os.Getenv("FORGEJO_INTEGRATION_DEFAULT_OWNER"),
		ForgejoIntegrationRepoMapFile:           os.Getenv("FORGEJO_INTEGRATION_REPO_MAP"),
		ForgejoIntegrationBranchInclude:         splitCSV(os.Getenv("FORGEJO_INTEGRATION_BRANCH_INCLUDE")),
		ForgejoIntegrationBranchExclude:         splitCSV(os.Getenv("FORGEJO_INTEGRATION_BRANCH_EXCLUDE")),
		ForgejoIntegrationMirrorBranchInclude:   splitCSV(os.Getenv("FORGEJO_INTEGRATION_MIRROR_BRANCH_INCLUDE")),
		ForgejoIntegrationMirrorBranchExclude:   splitCSV(os.Getenv("FORGEJO_INTEGRATION_MIRROR_BRANCH_EXCLUDE")),
		ForgejoIntegrationPRBranchInclude:       splitCSV(os.Getenv("FORGEJO_INTEGRATION_PR_BRANCH_INCLUDE")),
		ForgejoIntegrationPRBranchExclude:       splitCSV(os.Getenv("FORGEJO_INTEGRATION_PR_BRANCH_EXCLUDE")),
		ForgejoIntegrationAutoCreateRepo:        envBool("FORGEJO_INTEGRATION_AUTO_CREATE_REPO"),
		ForgejoIntegrationAutoPullRequest:       envBool("FORGEJO_INTEGRATION_AUTO_PR"),
		ForgejoIntegrationDefaultBaseBranch:     os.Getenv("FORGEJO_INTEGRATION_PR_BASE"),
		ForgejoIntegrationPrivateRepos:          envBool("FORGEJO_INTEGRATION_PRIVATE_REPOS"),
		ForgejoIntegrationPushGitConfig:         splitCSV(os.Getenv("FORGEJO_INTEGRATION_PUSH_GIT_CONFIG")),
		ForgejoAuthorityPolicyEnabled:           envBool("FORGEJO_INTEGRATION_AUTHORITY_POLICY_ENABLED"),
		ForgejoAuthorityPolicyTokenFile:         os.Getenv("FORGEJO_INTEGRATION_AUTHORITY_POLICY_TOKEN_FILE"),
		ForgejoIntegrationBot:                   os.Getenv("FORGEJO_INTEGRATION_BOT"),
		ForgejoActionsLogDir:                    os.Getenv("FORGEJO_ACTIONS_LOG_DIR"),
		ProviderLogBridgeEnabled:                envBool("PROVIDER_LOG_BRIDGE_ENABLED"),
		ProviderLogBridgeTokenFile:              os.Getenv("PROVIDER_LOG_BRIDGE_TOKEN_FILE"),
		ProviderLogBridgeForgejoDBDSNFile:       os.Getenv("PROVIDER_LOG_BRIDGE_FORGEJO_DB_DSN_FILE"),
		ProviderLogBridgeAllowedCIDRs:           splitCSV(os.Getenv("PROVIDER_LOG_BRIDGE_ALLOWED_CIDRS")),
		ProviderLogBridgeMaxBytes:               envInt64("PROVIDER_LOG_BRIDGE_MAX_BYTES", 4<<20),
		AllowMissingProjectionAlerting:          envBool("AGS_ALLOW_MISSING_PROJECTION_ALERTING"),
		AllowMissingForgejoAuthorityPolicy:      envBool("AGS_ALLOW_MISSING_FORGEJO_AUTHORITY_POLICY"),
	}
	if v := os.Getenv("EMBEDDING_DIMENSIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("invalid EMBEDDING_DIMENSIONS %q: must be a non-negative integer", v)
		}
		cfg.EmbeddingDimensions = n
	}
	if v := os.Getenv("WORKFLOW_EXEC_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("invalid WORKFLOW_EXEC_TIMEOUT %q: must be a positive duration", v)
		}
		cfg.WorkflowExecTimeout = d
	}
	if v := os.Getenv("FORGEJO_INTEGRATION_PUSH_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("invalid FORGEJO_INTEGRATION_PUSH_TIMEOUT %q: must be a non-negative duration", v)
		}
		cfg.ForgejoIntegrationPushTimeout = d
	}
	if v := os.Getenv("FORGEJO_PROJECTION_WORKER_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("invalid FORGEJO_PROJECTION_WORKER_TIMEOUT %q: must be a non-negative duration", v)
		}
		cfg.ForgejoProjectionWorkerTimeout = d
	}
	if v := os.Getenv("FORGEJO_PROJECTION_WORKER_RETRY_DELAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("invalid FORGEJO_PROJECTION_WORKER_RETRY_DELAY %q: must be a non-negative duration", v)
		}
		cfg.ForgejoProjectionWorkerRetryDelay = d
	}
	if v := os.Getenv("WORKFLOW_EXEC_PIDS_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("invalid WORKFLOW_EXEC_PIDS_LIMIT %q: must be a positive integer", v)
		}
		cfg.WorkflowExecPidsLimit = n
	}
	if v := os.Getenv("FORGEJO_PROJECTION_WORKER_MAX_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("invalid FORGEJO_PROJECTION_WORKER_MAX_ATTEMPTS %q: must be a positive integer", v)
		}
		cfg.ForgejoProjectionWorkerMaxAttempts = n
	}
	if v := os.Getenv("WORKFLOW_EXEC_NOFILE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("invalid WORKFLOW_EXEC_NOFILE %q: must be a positive integer", v)
		}
		cfg.WorkflowExecNoFile = n
	}
	if value := os.Getenv("AGS_LEGACY_EXTENSION_ALIASES"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("invalid AGS_LEGACY_EXTENSION_ALIASES")
		}
		cfg.DisableLegacyExtensionAliases = !enabled
	}
	return Normalize(cfg)
}

// Normalize applies defaults and validates a programmatically supplied config.
func Normalize(cfg Config) (Config, error) {
	if strings.TrimSpace(cfg.ControlPlaneDSN) != "" {
		return Config{}, fmt.Errorf("CONTROL_PLANE_DSN is not supported by this single-database runtime; refusing root database fallback")
	}
	if err := validateReplicationAuthority(cfg); err != nil {
		return Config{}, err
	}
	cfg.Port = firstNonEmpty(cfg.Port, "8080")
	cfg.BaseURL = firstNonEmpty(cfg.BaseURL, "http://localhost:8080")
	cfg.APIBaseURL = strings.TrimSpace(cfg.APIBaseURL)
	if cfg.APIBaseURL != "" {
		value, err := url.Parse(cfg.APIBaseURL)
		if err != nil || value.Hostname() == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" || value.RawPath != "" || (value.Path != "" && value.Path != "/") || (value.Scheme != "https" && value.Scheme != "http") {
			return Config{}, fmt.Errorf("AGS_API_BASE_URL must be an absolute credential-free HTTP(S) origin")
		}
		cfg.APIBaseURL = strings.TrimRight(value.String(), "/")
	}
	cfg.ConsoleBaseURL = firstNonEmpty(cfg.ConsoleBaseURL, "http://localhost:5173")
	cfg.OAuthDeviceVerificationURL = strings.TrimSpace(cfg.OAuthDeviceVerificationURL)
	cfg.GitRepoDir = firstNonEmpty(cfg.GitRepoDir, "gitrepos")
	cfg.IntegrationsConfigFile = strings.TrimSpace(cfg.IntegrationsConfigFile)
	cfg.ListenMode = firstNonEmpty(cfg.ListenMode, "development")
	cfg.Environment = strings.ToLower(strings.TrimSpace(firstNonEmpty(cfg.Environment, "production")))
	cfg.EmbeddingBaseURL = firstNonEmpty(cfg.EmbeddingBaseURL, "https://api.openai.com")
	cfg.EmbeddingModel = firstNonEmpty(cfg.EmbeddingModel, "text-embedding-3-small")
	cfg.OIDCScopes = firstNonEmpty(strings.TrimSpace(cfg.OIDCScopes), "openid profile email")
	cfg.WorkflowExecImage = firstNonEmpty(cfg.WorkflowExecImage, "bash:5.2")
	if cfg.WorkflowExecTimeout == 0 {
		cfg.WorkflowExecTimeout = 2 * time.Minute
	}
	if cfg.WorkflowExecCPUs == "" {
		cfg.WorkflowExecCPUs = "1.0"
	}
	if cfg.WorkflowExecMemory == "" {
		cfg.WorkflowExecMemory = "256m"
	}
	if cfg.WorkflowExecPidsLimit == 0 {
		cfg.WorkflowExecPidsLimit = 128
	}
	if cfg.WorkflowExecNoFile == 0 {
		cfg.WorkflowExecNoFile = 1024
	}
	if cfg.WorkflowExecTmpfsSize == "" {
		cfg.WorkflowExecTmpfsSize = "64m"
	}
	cfg.ForgejoIntegrationBaseURL = strings.TrimSpace(cfg.ForgejoIntegrationBaseURL)
	cfg.ForgejoIntegrationToken = strings.TrimSpace(cfg.ForgejoIntegrationToken)
	cfg.ForgejoIntegrationTokenFile = strings.TrimSpace(cfg.ForgejoIntegrationTokenFile)
	cfg.ForgejoIntegrationWebhookSecret = strings.TrimSpace(cfg.ForgejoIntegrationWebhookSecret)
	cfg.ForgejoIntegrationWebhookSecretFile = strings.TrimSpace(cfg.ForgejoIntegrationWebhookSecretFile)
	cfg.ForgejoIntegrationDefaultOwner = strings.TrimSpace(cfg.ForgejoIntegrationDefaultOwner)
	cfg.ForgejoIntegrationRepoMapFile = strings.TrimSpace(cfg.ForgejoIntegrationRepoMapFile)
	cfg.ForgejoIntegrationDefaultBaseBranch = firstNonEmpty(cfg.ForgejoIntegrationDefaultBaseBranch, "main")
	if cfg.DBdsn == "" {
		return Config{}, fmt.Errorf("required environment variable not set: DB_DSN")
	}
	if cfg.ListenMode != "production" && cfg.ListenMode != "development" {
		return Config{}, fmt.Errorf("invalid LISTEN_MODE %q: must be \"production\" or \"development\"", cfg.ListenMode)
	}
	if cfg.Environment != "production" && cfg.Environment != "development" {
		return Config{}, fmt.Errorf("invalid ENVIRONMENT %q: must be \"production\" or \"development\"", cfg.Environment)
	}
	if cfg.EmbeddingDimensions < 0 {
		return Config{}, fmt.Errorf("invalid EMBEDDING_DIMENSIONS %d: must be a non-negative integer", cfg.EmbeddingDimensions)
	}
	if cfg.WorkflowExecTimeout <= 0 {
		return Config{}, fmt.Errorf("invalid WORKFLOW_EXEC_TIMEOUT %q: must be a positive duration", cfg.WorkflowExecTimeout)
	}
	if cfg.ForgejoIntegrationPushTimeout < 0 {
		return Config{}, fmt.Errorf("invalid FORGEJO_INTEGRATION_PUSH_TIMEOUT %q: must be a non-negative duration", cfg.ForgejoIntegrationPushTimeout)
	}
	if cfg.ForgejoProjectionWorkerTimeout < 0 {
		return Config{}, fmt.Errorf("invalid FORGEJO_PROJECTION_WORKER_TIMEOUT %q: must be a non-negative duration", cfg.ForgejoProjectionWorkerTimeout)
	}
	if cfg.ForgejoProjectionWorkerRetryDelay < 0 {
		return Config{}, fmt.Errorf("invalid FORGEJO_PROJECTION_WORKER_RETRY_DELAY %q: must be a non-negative duration", cfg.ForgejoProjectionWorkerRetryDelay)
	}
	if cfg.ForgejoProjectionWorkerMaxAttempts < 0 {
		return Config{}, fmt.Errorf("invalid FORGEJO_PROJECTION_WORKER_MAX_ATTEMPTS %d: must be a non-negative integer", cfg.ForgejoProjectionWorkerMaxAttempts)
	}
	if cfg.WorkflowExecPidsLimit <= 0 {
		return Config{}, fmt.Errorf("invalid WORKFLOW_EXEC_PIDS_LIMIT %d: must be a positive integer", cfg.WorkflowExecPidsLimit)
	}
	if cfg.WorkflowExecNoFile <= 0 {
		return Config{}, fmt.Errorf("invalid WORKFLOW_EXEC_NOFILE %d: must be a positive integer", cfg.WorkflowExecNoFile)
	}
	if cfg.OAuthDeviceVerificationURL != "" {
		parsed, err := url.Parse(cfg.OAuthDeviceVerificationURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return Config{}, fmt.Errorf("invalid OAUTH_DEVICE_VERIFICATION_URL %q: must be an absolute HTTP(S) URL", cfg.OAuthDeviceVerificationURL)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return Config{}, fmt.Errorf("invalid OAUTH_DEVICE_VERIFICATION_URL %q: must use http or https", cfg.OAuthDeviceVerificationURL)
		}
	}
	if cfg.ForgejoIntegrationEnabled {
		if cfg.ForgejoIntegrationBaseURL == "" {
			return Config{}, fmt.Errorf("FORGEJO_INTEGRATION_BASE_URL is required when FORGEJO_INTEGRATION_ENABLED=1")
		}
		if cfg.ForgejoIntegrationToken == "" && cfg.ForgejoIntegrationTokenFile == "" {
			return Config{}, fmt.Errorf("FORGEJO_INTEGRATION_TOKEN or FORGEJO_INTEGRATION_TOKEN_FILE is required when FORGEJO_INTEGRATION_ENABLED=1")
		}
	}
	if strings.TrimSpace(cfg.OIDCProvider) == "" && (cfg.OIDCIssuer != "" || cfg.OIDCDiscoveryURL != "" || cfg.OIDCClientID != "") {
		cfg.OIDCProvider = defaultOIDCProvider(cfg.OIDCIssuer, cfg.OIDCDiscoveryURL)
	}
	cfg.ConnectedLoginProvider = strings.TrimSpace(cfg.ConnectedLoginProvider)
	cfg.ConnectedLoginOrigin = strings.TrimSpace(cfg.ConnectedLoginOrigin)
	cfg.ConnectedLoginAPIOrigin = strings.TrimSpace(cfg.ConnectedLoginAPIOrigin)
	cfg.ConnectedLoginClientID = strings.TrimSpace(cfg.ConnectedLoginClientID)
	cfg.ConnectedLoginClientSecret = strings.TrimSpace(cfg.ConnectedLoginClientSecret)
	cfg.ConnectedLoginLoginPath = strings.TrimSpace(cfg.ConnectedLoginLoginPath)
	cfg.ConnectedLoginTokenPath = strings.TrimSpace(cfg.ConnectedLoginTokenPath)
	cfg.ConnectedLoginUserinfoPath = strings.TrimSpace(cfg.ConnectedLoginUserinfoPath)
	cfg.ConnectedLoginReturnToParam = strings.TrimSpace(cfg.ConnectedLoginReturnToParam)
	cfg.ConnectedLoginSubjectClaim = strings.TrimSpace(cfg.ConnectedLoginSubjectClaim)
	cfg.ConnectedLoginSubjectNamespaceClaim = strings.TrimSpace(cfg.ConnectedLoginSubjectNamespaceClaim)
	cfg.ConnectedLoginSubjectNamespaceSlugClaim = strings.TrimSpace(cfg.ConnectedLoginSubjectNamespaceSlugClaim)
	cfg.ConnectedLoginActorTypeClaim = strings.TrimSpace(cfg.ConnectedLoginActorTypeClaim)
	cfg.ConnectedLoginHumanTypeValue = strings.TrimSpace(cfg.ConnectedLoginHumanTypeValue)
	cfg.ConnectedLoginAgentTypeValue = strings.TrimSpace(cfg.ConnectedLoginAgentTypeValue)
	cfg.ConnectedLoginClientIDClaim = strings.TrimSpace(cfg.ConnectedLoginClientIDClaim)
	cfg.ConnectedLoginClientNameClaim = strings.TrimSpace(cfg.ConnectedLoginClientNameClaim)
	cfg.ConnectedLoginPreferredUsernameClaim = strings.TrimSpace(cfg.ConnectedLoginPreferredUsernameClaim)
	cfg.ConnectedLoginNameClaim = strings.TrimSpace(cfg.ConnectedLoginNameClaim)
	cfg.ConnectedLoginPictureClaim = strings.TrimSpace(cfg.ConnectedLoginPictureClaim)
	cfg.ConnectedLoginAvatarURLClaim = strings.TrimSpace(cfg.ConnectedLoginAvatarURLClaim)
	cfg.ConnectedLoginDescriptionClaim = strings.TrimSpace(cfg.ConnectedLoginDescriptionClaim)
	cfg.ConnectedLoginScopeClaim = strings.TrimSpace(cfg.ConnectedLoginScopeClaim)
	if err := validateConnectedLoginConfig(cfg); err != nil {
		return Config{}, err
	}
	if cfg.ConnectedLoginEnabled() {
		cfg.ConnectedLoginProvider = firstNonEmpty(cfg.ConnectedLoginProvider, "connected")
		cfg.ConnectedLoginLoginPath = firstNonEmpty(cfg.ConnectedLoginLoginPath, "/oauth/login")
		cfg.ConnectedLoginTokenPath = firstNonEmpty(cfg.ConnectedLoginTokenPath, "/api/oauth/token")
		cfg.ConnectedLoginUserinfoPath = firstNonEmpty(cfg.ConnectedLoginUserinfoPath, "/api/oauth/userinfo")
		cfg.ConnectedLoginReturnToParam = firstNonEmpty(cfg.ConnectedLoginReturnToParam, "return_to")
		cfg.ConnectedLoginSubjectClaim = firstNonEmpty(cfg.ConnectedLoginSubjectClaim, "sub")
		cfg.ConnectedLoginActorTypeClaim = firstNonEmpty(cfg.ConnectedLoginActorTypeClaim, "type")
		cfg.ConnectedLoginHumanTypeValue = firstNonEmpty(cfg.ConnectedLoginHumanTypeValue, "human")
		cfg.ConnectedLoginAgentTypeValue = firstNonEmpty(cfg.ConnectedLoginAgentTypeValue, "agent")
		cfg.ConnectedLoginClientIDClaim = firstNonEmpty(cfg.ConnectedLoginClientIDClaim, "client_id")
		cfg.ConnectedLoginClientNameClaim = firstNonEmpty(cfg.ConnectedLoginClientNameClaim, "client_name")
		cfg.ConnectedLoginPreferredUsernameClaim = firstNonEmpty(cfg.ConnectedLoginPreferredUsernameClaim, "preferred_username")
		cfg.ConnectedLoginNameClaim = firstNonEmpty(cfg.ConnectedLoginNameClaim, "name")
		cfg.ConnectedLoginPictureClaim = firstNonEmpty(cfg.ConnectedLoginPictureClaim, "picture")
		cfg.ConnectedLoginAvatarURLClaim = firstNonEmpty(cfg.ConnectedLoginAvatarURLClaim, "avatar_url")
		cfg.ConnectedLoginDescriptionClaim = firstNonEmpty(cfg.ConnectedLoginDescriptionClaim, "description")
		cfg.ConnectedLoginScopeClaim = firstNonEmpty(cfg.ConnectedLoginScopeClaim, "scope")
	}
	return cfg, nil
}

// ConnectedLoginEnabled reports whether connected login is configured.
func (c Config) ConnectedLoginEnabled() bool {
	return strings.TrimSpace(c.ConnectedLoginOrigin) != "" &&
		strings.TrimSpace(c.ConnectedLoginAPIOrigin) != "" &&
		strings.TrimSpace(c.ConnectedLoginClientID) != "" &&
		strings.TrimSpace(c.ConnectedLoginClientSecret) != ""
}

func envBool(key string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func firstNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func defaultOIDCProvider(issuer, discoveryURL string) string {
	if looksLikeAuth0Issuer(issuer) || looksLikeAuth0Issuer(discoveryURL) {
		return "auth0"
	}
	return "oidc"
}

func looksLikeAuth0Issuer(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	return host == "auth0.com" || strings.HasSuffix(host, ".auth0.com")
}

func validateConnectedLoginConfig(cfg Config) error {
	type envValue struct {
		name  string
		value string
	}
	required := []envValue{
		{name: "CONNECTED_LOGIN_ORIGIN", value: cfg.ConnectedLoginOrigin},
		{name: "CONNECTED_LOGIN_API_ORIGIN", value: cfg.ConnectedLoginAPIOrigin},
		{name: "CONNECTED_LOGIN_CLIENT_ID", value: cfg.ConnectedLoginClientID},
		{name: "CONNECTED_LOGIN_CLIENT_SECRET", value: cfg.ConnectedLoginClientSecret},
	}
	optional := []envValue{
		{name: "CONNECTED_LOGIN_PROVIDER", value: cfg.ConnectedLoginProvider},
		{name: "CONNECTED_LOGIN_LOGIN_PATH", value: cfg.ConnectedLoginLoginPath},
		{name: "CONNECTED_LOGIN_TOKEN_PATH", value: cfg.ConnectedLoginTokenPath},
		{name: "CONNECTED_LOGIN_USERINFO_PATH", value: cfg.ConnectedLoginUserinfoPath},
		{name: "CONNECTED_LOGIN_RETURN_TO_PARAM", value: cfg.ConnectedLoginReturnToParam},
		{name: "CONNECTED_LOGIN_SUBJECT_CLAIM", value: cfg.ConnectedLoginSubjectClaim},
		{name: "CONNECTED_LOGIN_SUBJECT_NAMESPACE_CLAIM", value: cfg.ConnectedLoginSubjectNamespaceClaim},
		{name: "CONNECTED_LOGIN_SUBJECT_NAMESPACE_SLUG_CLAIM", value: cfg.ConnectedLoginSubjectNamespaceSlugClaim},
		{name: "CONNECTED_LOGIN_ACTOR_TYPE_CLAIM", value: cfg.ConnectedLoginActorTypeClaim},
		{name: "CONNECTED_LOGIN_HUMAN_TYPE_VALUE", value: cfg.ConnectedLoginHumanTypeValue},
		{name: "CONNECTED_LOGIN_AGENT_TYPE_VALUE", value: cfg.ConnectedLoginAgentTypeValue},
		{name: "CONNECTED_LOGIN_CLIENT_ID_CLAIM", value: cfg.ConnectedLoginClientIDClaim},
		{name: "CONNECTED_LOGIN_CLIENT_NAME_CLAIM", value: cfg.ConnectedLoginClientNameClaim},
		{name: "CONNECTED_LOGIN_PREFERRED_USERNAME_CLAIM", value: cfg.ConnectedLoginPreferredUsernameClaim},
		{name: "CONNECTED_LOGIN_NAME_CLAIM", value: cfg.ConnectedLoginNameClaim},
		{name: "CONNECTED_LOGIN_PICTURE_CLAIM", value: cfg.ConnectedLoginPictureClaim},
		{name: "CONNECTED_LOGIN_AVATAR_URL_CLAIM", value: cfg.ConnectedLoginAvatarURLClaim},
		{name: "CONNECTED_LOGIN_DESCRIPTION_CLAIM", value: cfg.ConnectedLoginDescriptionClaim},
		{name: "CONNECTED_LOGIN_SCOPE_CLAIM", value: cfg.ConnectedLoginScopeClaim},
	}
	var set, missing []string
	for _, item := range required {
		if strings.TrimSpace(item.value) == "" {
			missing = append(missing, item.name)
			continue
		}
		set = append(set, item.name)
	}
	for _, item := range optional {
		if strings.TrimSpace(item.value) != "" {
			set = append(set, item.name)
		}
	}
	if len(set) > 0 && len(missing) > 0 {
		return fmt.Errorf("connected login: partial configuration; set %v, missing %v", set, missing)
	}
	return nil
}
