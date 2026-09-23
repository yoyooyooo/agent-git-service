package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	agsauth "github.com/ngaut/agent-git-service/auth"
	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/connectedlogin"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/delegationpolicy"
	"github.com/ngaut/agent-git-service/internal/embedding"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githttp"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	"github.com/ngaut/agent-git-service/internal/gitstore"
	"github.com/ngaut/agent-git-service/internal/graphql"
	"github.com/ngaut/agent-git-service/internal/integrations"
	"github.com/ngaut/agent-git-service/internal/metrics"
	srvmiddleware "github.com/ngaut/agent-git-service/internal/middleware"
	"github.com/ngaut/agent-git-service/internal/multicafailures"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"github.com/ngaut/agent-git-service/internal/notifications"
	"github.com/ngaut/agent-git-service/internal/oauth"
	"github.com/ngaut/agent-git-service/internal/oidc"
	"github.com/ngaut/agent-git-service/internal/projectionwatch"
	"github.com/ngaut/agent-git-service/internal/providerlogbridge"
	"github.com/ngaut/agent-git-service/internal/rest"
	"github.com/ngaut/agent-git-service/internal/rest/transform"
	"github.com/ngaut/agent-git-service/internal/router"
	"github.com/ngaut/agent-git-service/internal/service"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"github.com/ngaut/agent-git-service/internal/wikicatalog"
)

// gitSHA is set at build time via -ldflags.
var gitSHA = "unknown"

// Server exposes a programmatic server instance for embedders.
type Server struct {
	cfg         config.Config
	deps        *bootstrapDeps
	handler     http.Handler
	listeners   []net.Listener
	started     bool
	serveErrors chan error
	serveWG     sync.WaitGroup
}

// Authenticator authenticates a request using host-provided identity. ok=false
// means no embedded identity was present and AGS should continue with its
// built-in token flow when applicable.
type Authenticator interface {
	Authenticate(*http.Request) (agsauth.Identity, bool, error)
}

type options struct {
	authenticator Authenticator
}

// Option configures the embeddable server surface.
type Option func(*options)

// WithAuthenticator installs a host-provided request authenticator.
func WithAuthenticator(authenticator Authenticator) Option {
	return func(opts *options) {
		opts.authenticator = authenticator
	}
}

// bootstrapDeps holds all initialized dependencies for the application.
type bootstrapDeps struct {
	Cfg          config.Config
	Options      options
	DB           *gorm.DB
	Embedder     embedding.Embedder
	Store        *gitstore.Store
	SrvCtx       context.Context
	SrvCancel    context.CancelFunc
	SvcDeps      *service.Service
	GqlSrv       *graphql.Server
	GitHandler   *githttp.Handler
	OauthHandler *oauth.Handler
	Handlers     *rest.Deps
	Mux          http.Handler
	Servers      []*http.Server
	Labels       []string
	Replication  *managedReplication
}

// bootstrapResult is returned by bootstrap and contains all initialized components.
type bootstrapResult struct {
	Deps    *bootstrapDeps
	Err     error
	Partial *bootstrapDeps // Contains successfully initialized deps if bootstrap failed midway
}

type coreDeps struct {
	cfg       config.Config
	cfgLoaded bool
	db        *gorm.DB
	embedder  embedding.Embedder
	store     *gitstore.Store
}

type serviceDeps struct {
	svc          *service.Service
	gqlSrv       *graphql.Server
	gitHandler   *githttp.Handler
	oauthHandler *oauth.Handler
}

type muxDeps struct {
	handlers *rest.Deps
	router   *chi.Mux
	mux      http.Handler
}

type serverDeps struct {
	servers []*http.Server
	labels  []string
}

type embeddedAuthenticatorAdapter struct {
	authenticator Authenticator
}

func (a embeddedAuthenticatorAdapter) Authenticate(r *http.Request) (srvmiddleware.EmbeddedIdentity, bool, error) {
	identity, ok, err := a.authenticator.Authenticate(r)
	if err != nil || !ok {
		return srvmiddleware.EmbeddedIdentity{}, ok, err
	}
	return srvmiddleware.EmbeddedIdentity{
		Provider:  identity.Provider,
		Subject:   identity.Subject,
		Login:     identity.Login,
		Name:      identity.Name,
		Email:     identity.Email,
		Groups:    append([]string(nil), identity.Groups...),
		SiteAdmin: identity.SiteAdmin,
	}, true, nil
}

func embeddedAuthConfig(opts options) srvmiddleware.EmbeddedAuthConfig {
	if opts.authenticator == nil {
		return srvmiddleware.EmbeddedAuthConfig{}
	}
	return srvmiddleware.EmbeddedAuthConfig{
		Authenticator: embeddedAuthenticatorAdapter{authenticator: opts.authenticator},
	}
}

func initCoreDepsFromConfig(cfg config.Config) (coreDeps, error) {
	var deps coreDeps
	deps.cfg = cfg
	deps.cfgLoaded = true

	// Refuse insecure token bypass mode in production.
	if cfg.AllowAnyToken && cfg.Environment == "production" {
		return deps, fmt.Errorf("ALLOW_ANY_TOKEN=true is not allowed when ENVIRONMENT=production")
	}

	database, err := db.Init(cfg.DBdsn)
	if err != nil {
		return deps, fmt.Errorf("db: %w", err)
	}
	deps.db = database

	if cfg.Environment == "development" {
		if err := db.Seed(database, cfg.AdminLogin, cfg.AdminToken); err != nil {
			return deps, fmt.Errorf("seed: %w", err)
		}
		slog.Info("seed applied", "environment", cfg.Environment)
	} else {
		slog.Info("seed skipped", "environment", cfg.Environment)
	}

	var embedder embedding.Embedder
	if cfg.EmbeddingAPIKey != "" {
		opts := []embedding.OpenAIOption{
			embedding.WithBaseURL(cfg.EmbeddingBaseURL),
			embedding.WithModel(cfg.EmbeddingModel),
		}
		if cfg.EmbeddingDimensions > 0 {
			opts = append(opts, embedding.WithDimensions(cfg.EmbeddingDimensions))
		}
		embedder = embedding.NewOpenAI(cfg.EmbeddingAPIKey, opts...)
		// Attempt to add VECTOR columns (best-effort, idempotent).
		if dims := embedder.Dimensions(); dims > 0 {
			db.InitVector(database, dims)
			slog.Info("embedding enabled", "model", cfg.EmbeddingModel, "dimensions", dims)
		} else {
			slog.Info("embedding enabled", "model", cfg.EmbeddingModel, "dimensions", "auto")
		}
	} else {
		embedder = embedding.NopEmbedder{}
		slog.Info("embedding disabled", "reason", "EMBEDDING_API_KEY not set")
	}
	deps.embedder = embedder

	store, err := gitstore.New(cfg.GitRepoDir)
	if err != nil {
		return deps, fmt.Errorf("gitstore: %w", err)
	}
	deps.store = store

	return deps, nil
}

func initServiceDeps(cfg config.Config, database *gorm.DB, store *gitstore.Store, embedder embedding.Embedder, srvCtx context.Context) (serviceDeps, error) {
	var deps serviceDeps
	dataRoot := cfg.GitRepoDir
	if strings.TrimSpace(dataRoot) == "" {
		dataRoot = "."
	}

	// Wiki catalog: a content-addressed blob store on the filesystem
	// plus the catalog primitive backed by the same database.
	wikiBlob := wikicatalog.NewBlobStore(dataRoot)
	wikiCat := wikicatalog.New(database, wikiBlob)

	forgejoIntegration, err := initForgejoIntegration(cfg)
	if err != nil {
		return deps, fmt.Errorf("forgejo integration: %w", err)
	}
	gitLabIntegration, err := initGitLabIntegration(cfg)
	if err != nil {
		return deps, fmt.Errorf("gitlab integration: %w", err)
	}
	gitHubIntegration, err := initGitHubIntegration(cfg)
	if err != nil {
		return deps, fmt.Errorf("github integration: %w", err)
	}
	multicaProjection, err := initMulticaProjection(cfg)
	if err != nil {
		return deps, fmt.Errorf("multica projection: %w", err)
	}
	executionContextRegistry, err := initExecutionContextRegistry(cfg)
	if err != nil {
		return deps, fmt.Errorf("execution context registry: %w", err)
	}
	delegationPolicies, err := initDelegationPolicies(cfg)
	if err != nil {
		return deps, fmt.Errorf("delegation policies: %w", err)
	}
	principalSessions, err := initPrincipalSessions(cfg)
	if err != nil {
		return deps, fmt.Errorf("principal sessions: %w", err)
	}
	multicaIncidentNotifier, multicaIncidentNotifyThrottle, err := initMulticaIncidentNotifier(cfg)
	if err != nil {
		return deps, fmt.Errorf("multica incident notifier: %w", err)
	}
	multicaFailureWatcherCfg, err := initMulticaFailureWatcherConfig(cfg)
	if err != nil {
		return deps, fmt.Errorf("multica failure watcher: %w", err)
	}
	projectionWatcherCfg, projectionDriftNotifier, err := initProjectionWatcher(cfg)
	if err != nil {
		return deps, fmt.Errorf("projection watcher: %w", err)
	}
	pullRequestMergeNotifier, err := initPullRequestMergeNotifier(cfg)
	if err != nil {
		return deps, fmt.Errorf("pull request merge notifier: %w", err)
	}
	outboundDispatcher, outboundEventTargets, err := initOutboundDelivery(cfg)
	if err != nil {
		return deps, fmt.Errorf("outbound delivery: %w", err)
	}
	accessGrantRequireCI, err := initAccessGrantRequireCI(cfg)
	if err != nil {
		return deps, fmt.Errorf("access grant require ci: %w", err)
	}

	svcDeps := &service.Service{
		Ctx:                                srvCtx,
		DB:                                 database,
		Git:                                store,
		WikiCatalog:                        wikiCat,
		WikiBlob:                           wikiBlob,
		BaseURL:                            cfg.BaseURL,
		SourceRevision:                     strings.ToLower(strings.TrimSpace(gitSHA)),
		Embedder:                           embedder,
		AllowAnyToken:                      cfg.AllowAnyToken,
		ForgejoIntegration:                 forgejoIntegration,
		ForgejoProjectionWorkerTimeout:     cfg.ForgejoProjectionWorkerTimeout,
		ForgejoProjectionWorkerMaxAttempts: cfg.ForgejoProjectionWorkerMaxAttempts,
		ForgejoProjectionWorkerRetryDelay:  cfg.ForgejoProjectionWorkerRetryDelay,
		GitLabIntegration:                  gitLabIntegration,
		GitHubIntegration:                  gitHubIntegration,
		MulticaProjection:                  multicaProjection,
		ExecutionContextPuller:             executionContextRegistry,
		DelegationPolicies:                 delegationPolicies,
		PrincipalSessions:                  principalSessions,
		MulticaIncidentNotifier:            multicaIncidentNotifier,
		MulticaIncidentNotifyThrottle:      multicaIncidentNotifyThrottle,
		PullRequestMergeNotifier:           pullRequestMergeNotifier,
		OutboundDispatcher:                 outboundDispatcher,
		OutboundEventTargets:               outboundEventTargets,
		AccessGrantRequireCI:               accessGrantRequireCI,
		WorkflowExecEnabled:                cfg.EnableWorkflowExec,
		WorkflowExecImage:                  cfg.WorkflowExecImage,
		WorkflowExecTimeout:                cfg.WorkflowExecTimeout,
		WorkflowExecCPUs:                   cfg.WorkflowExecCPUs,
		WorkflowExecMemory:                 cfg.WorkflowExecMemory,
		WorkflowExecPids:                   cfg.WorkflowExecPidsLimit,
		WorkflowExecNoFile:                 cfg.WorkflowExecNoFile,
		WorkflowExecTmpfs:                  cfg.WorkflowExecTmpfsSize,
	}
	if err := svcDeps.ReconcilePrincipalBindingRevocations(srvCtx); err != nil {
		return deps, fmt.Errorf("reconcile principal binding revocations: %w", err)
	}
	if err := svcDeps.ReconcileTeamAuthorityEpochFloors(srvCtx); err != nil {
		return deps, fmt.Errorf("reconcile team authority epoch floors: %w", err)
	}
	// Post-commit hook: drive the wiki search index from catalog
	// writes so Step 4 cutover does not leave wiki_search_documents
	// stale. The hook is best-effort — failures log and do not roll
	// back the catalog commit.
	wikiCat.OnChangeSetCommitted = svcDeps.WikiCatalogPostCommit
	// Route catalog writes through the same context-aware DB accessor the
	// service layer uses so transactions and request cancellation propagate.
	wikiCat.DBFor = svcDeps.DBForCtx
	deps.svc = svcDeps

	if cfg.AllowAnyToken {
		slog.Warn("ALLOW_ANY_TOKEN is enabled; any non-empty token is accepted when no tokens exist in DB")
	}
	if cfg.EnableWorkflowExec {
		slog.Info("workflow execution sandbox enabled",
			"image", cfg.WorkflowExecImage,
			"timeout", cfg.WorkflowExecTimeout,
			"cpus", cfg.WorkflowExecCPUs,
			"memory", cfg.WorkflowExecMemory,
			"pids_limit", cfg.WorkflowExecPidsLimit,
			"nofile_limit", cfg.WorkflowExecNoFile,
			"tmpfs_size", cfg.WorkflowExecTmpfsSize,
		)
	} else {
		slog.Warn("workflow execution disabled; set ENABLE_WORKFLOW_EXEC=1 to allow sandboxed workflow steps")
	}
	if cfg.ConnectedLoginEnabled() {
		c, err := connectedlogin.New(connectedlogin.Config{
			Provider:                  cfg.ConnectedLoginProvider,
			Origin:                    cfg.ConnectedLoginOrigin,
			APIOrigin:                 cfg.ConnectedLoginAPIOrigin,
			ClientID:                  cfg.ConnectedLoginClientID,
			ClientSecret:              cfg.ConnectedLoginClientSecret,
			CallbackBaseURL:           cfg.BaseURL,
			LoginPath:                 cfg.ConnectedLoginLoginPath,
			TokenPath:                 cfg.ConnectedLoginTokenPath,
			UserinfoPath:              cfg.ConnectedLoginUserinfoPath,
			ReturnToParam:             cfg.ConnectedLoginReturnToParam,
			SubjectClaim:              cfg.ConnectedLoginSubjectClaim,
			SubjectNamespaceClaim:     cfg.ConnectedLoginSubjectNamespaceClaim,
			SubjectNamespaceSlugClaim: cfg.ConnectedLoginSubjectNamespaceSlugClaim,
			ActorTypeClaim:            cfg.ConnectedLoginActorTypeClaim,
			HumanTypeValue:            cfg.ConnectedLoginHumanTypeValue,
			AgentTypeValue:            cfg.ConnectedLoginAgentTypeValue,
			ClientIDClaim:             cfg.ConnectedLoginClientIDClaim,
			ClientNameClaim:           cfg.ConnectedLoginClientNameClaim,
			PreferredUsernameClaim:    cfg.ConnectedLoginPreferredUsernameClaim,
			NameClaim:                 cfg.ConnectedLoginNameClaim,
			PictureClaim:              cfg.ConnectedLoginPictureClaim,
			AvatarURLClaim:            cfg.ConnectedLoginAvatarURLClaim,
			DescriptionClaim:          cfg.ConnectedLoginDescriptionClaim,
			ScopeClaim:                cfg.ConnectedLoginScopeClaim,
		})
		if err != nil {
			return deps, fmt.Errorf("connectedlogin: %w", err)
		}
		svcDeps.ConnectedLogin = c
		slog.Info("connected login enabled",
			"provider", cfg.ConnectedLoginProvider,
			"client_id", cfg.ConnectedLoginClientID,
			"origin", cfg.ConnectedLoginOrigin,
			"callback", c.CallbackURL(),
		)
	} else {
		slog.Info("connected login disabled", "reason", "CONNECTED_LOGIN_* configuration not set")
	}
	if cfg.OIDCProvider != "" && cfg.OIDCClientID != "" && (cfg.OIDCIssuer != "" || cfg.OIDCDiscoveryURL != "") {
		c, err := oidc.New(oidc.Config{
			Provider:          cfg.OIDCProvider,
			Issuer:            cfg.OIDCIssuer,
			DiscoveryURL:      cfg.OIDCDiscoveryURL,
			ClientID:          cfg.OIDCClientID,
			ClientSecret:      cfg.OIDCClientSecret,
			Audience:          cfg.OIDCAudience,
			Scopes:            cfg.OIDCScopes,
			AllowInsecureHTTP: cfg.OIDCAllowInsecureHTTP,
		})
		if err != nil {
			return deps, fmt.Errorf("oidc: %w", err)
		}
		svcDeps.OIDC = c
		slog.Info("oidc enabled", "provider", cfg.OIDCProvider, "issuer", cfg.OIDCIssuer)
	} else {
		slog.Info("oidc disabled", "reason", "OIDC_ISSUER/OIDC_CLIENT_ID not set")
	}
	deps.gqlSrv = graphql.NewServer(svcDeps)
	deps.gitHandler = githttp.New(store, svcDeps)
	deps.oauthHandler = oauth.New(svcDeps, oauth.WithDeviceVerificationURL(cfg.OAuthDeviceVerificationURL))
	if len(svcDeps.OutboundEventTargets[service.OutboundEventMulticaIncident]) > 0 && svcDeps.OutboundDispatcher != nil {
		svcDeps.MulticaIncidentNotifier = svcDeps
	}
	startMulticaFailureWatcher(srvCtx, svcDeps, multicaFailureWatcherCfg)
	if len(svcDeps.OutboundEventTargets[service.OutboundEventProjectionDrift]) > 0 && svcDeps.OutboundDispatcher != nil {
		projectionDriftNotifier = svcDeps
	}
	startProjectionWatcher(srvCtx, svcDeps, projectionWatcherCfg, projectionDriftNotifier)
	startOutboundDeliveryWorker(srvCtx, svcDeps)
	startDelegatedSessionExpiryAuditor(srvCtx, svcDeps)

	return deps, nil
}

func forgejoIntegrationConfig(cfg config.Config, fileCfg integrations.Config) forgejointegration.Config {
	integrationCfg := forgejointegration.Config{
		Enabled:                  cfg.ForgejoIntegrationEnabled,
		BaseURL:                  cfg.ForgejoIntegrationBaseURL,
		Token:                    cfg.ForgejoIntegrationToken,
		TokenFile:                cfg.ForgejoIntegrationTokenFile,
		WebhookSecret:            cfg.ForgejoIntegrationWebhookSecret,
		WebhookSecretFile:        cfg.ForgejoIntegrationWebhookSecretFile,
		DefaultOwner:             cfg.ForgejoIntegrationDefaultOwner,
		RepoMapFile:              cfg.ForgejoIntegrationRepoMapFile,
		BranchIncludes:           cfg.ForgejoIntegrationBranchInclude,
		BranchExcludes:           cfg.ForgejoIntegrationBranchExclude,
		MirrorBranchIncludes:     cfg.ForgejoIntegrationMirrorBranchInclude,
		MirrorBranchExcludes:     cfg.ForgejoIntegrationMirrorBranchExclude,
		PRBranchIncludes:         cfg.ForgejoIntegrationPRBranchInclude,
		PRBranchExcludes:         cfg.ForgejoIntegrationPRBranchExclude,
		AutoCreateRepo:           cfg.ForgejoIntegrationAutoCreateRepo,
		AutoPullRequest:          cfg.ForgejoIntegrationAutoPullRequest,
		DefaultBaseBranch:        cfg.ForgejoIntegrationDefaultBaseBranch,
		PrivateRepos:             cfg.ForgejoIntegrationPrivateRepos,
		PushGitConfig:            cfg.ForgejoIntegrationPushGitConfig,
		PushTimeout:              cfg.ForgejoIntegrationPushTimeout,
		AuthorityPolicyEnabled:   cfg.ForgejoAuthorityPolicyEnabled,
		AuthorityPolicyTokenFile: cfg.ForgejoAuthorityPolicyTokenFile,
		WebhookURL:               strings.TrimRight(cfg.BaseURL, "/") + "/api/v3/integrations/forgejo/webhook",
		IntegrationBot:           cfg.ForgejoIntegrationBot,
		ActionsLogDir:            cfg.ForgejoActionsLogDir,
	}
	if cfg.IntegrationsConfigFile == "" {
		return integrationCfg
	}
	integrationCfg = fileCfg.Forgejo.ToForgejoIntegrationConfig()
	if strings.TrimSpace(integrationCfg.WebhookURL) == "" {
		integrationCfg.WebhookURL = strings.TrimRight(cfg.BaseURL, "/") + "/api/v3/integrations/forgejo/webhook"
	}
	if strings.TrimSpace(integrationCfg.ActionsLogDir) == "" {
		integrationCfg.ActionsLogDir = strings.TrimSpace(cfg.ForgejoActionsLogDir)
	}
	return integrationCfg
}

func initForgejoIntegration(cfg config.Config) (*forgejointegration.Integration, error) {
	var fileCfg integrations.Config
	if cfg.IntegrationsConfigFile != "" {
		loaded, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
		if err != nil {
			return nil, err
		}
		fileCfg = loaded
	}

	integrationCfg := forgejoIntegrationConfig(cfg, fileCfg)
	if !integrationCfg.Enabled {
		slog.Info("forgejo integration disabled", "reason", "not configured")
		return nil, nil
	}
	var err error
	integrationCfg, err = forgejointegration.LoadTokenFile(integrationCfg)
	if err != nil {
		return nil, err
	}
	integrationCfg, err = forgejointegration.LoadWebhookSecretFile(integrationCfg)
	if err != nil {
		return nil, err
	}
	integrationCfg, err = forgejointegration.LoadAuthorityPolicyTokenFile(integrationCfg)
	if err != nil {
		return nil, err
	}
	integrationCfg, err = forgejointegration.LoadRepoMapFile(integrationCfg)
	if err != nil {
		return nil, err
	}
	slog.Info("forgejo integration enabled",
		"base_url", integrationCfg.BaseURL,
		"default_owner", integrationCfg.DefaultOwner,
		"repo_map_file", integrationCfg.RepoMapFile,
		"branch_include", integrationCfg.BranchIncludes,
		"branch_exclude", integrationCfg.BranchExcludes,
		"mirror_branch_include", integrationCfg.MirrorBranchIncludes,
		"mirror_branch_exclude", integrationCfg.MirrorBranchExcludes,
		"pr_branch_include", integrationCfg.PRBranchIncludes,
		"pr_branch_exclude", integrationCfg.PRBranchExcludes,
		"auto_create_repo", integrationCfg.AutoCreateRepo,
		"auto_pr", integrationCfg.AutoPullRequest,
		"private_repos", integrationCfg.PrivateRepos,
		"push_git_config", integrationCfg.PushGitConfig,
		"push_timeout", integrationCfg.PushTimeout,
		"authority_policy_enabled", integrationCfg.AuthorityPolicyEnabled,
		"integration_bot", integrationCfg.IntegrationBot,
	)
	return forgejointegration.New(integrationCfg, nil, nil), nil
}

func initAccessGrantRequireCI(cfg config.Config) (func(string) bool, error) {
	if strings.TrimSpace(cfg.IntegrationsConfigFile) == "" {
		return nil, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	return fileCfg.AccessGrantRequiresCI, nil
}

func initGitLabIntegration(cfg config.Config) (*gitlabintegration.Integration, error) {
	if cfg.IntegrationsConfigFile == "" {
		slog.Info("gitlab integration disabled", "reason", "integrations config not configured")
		return nil, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	integrationCfg := fileCfg.GitLab.ToGitLabIntegrationConfig()
	if !integrationCfg.Enabled {
		slog.Info("gitlab integration disabled", "reason", "not configured")
		return nil, nil
	}
	integrationCfg, err = gitlabintegration.LoadTokenFile(integrationCfg)
	if err != nil {
		return nil, err
	}
	slog.Info("gitlab integration enabled",
		"base_url", integrationCfg.BaseURL,
		"merge_authority", integrationCfg.MergeAuthority,
		"repo_count", len(integrationCfg.Repos),
	)
	return gitlabintegration.New(integrationCfg, nil), nil
}

func initGitHubIntegration(cfg config.Config) (*githubintegration.Integration, error) {
	if cfg.IntegrationsConfigFile == "" {
		slog.Info("github integration disabled", "reason", "integrations config not configured")
		return nil, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	integrationCfg := fileCfg.GitHub.ToGitHubIntegrationConfig()
	if !integrationCfg.Enabled {
		slog.Info("github integration disabled", "reason", "not configured")
		return nil, nil
	}
	integrationCfg, err = githubintegration.LoadTokenFile(integrationCfg)
	if err != nil {
		return nil, err
	}
	slog.Info("github integration enabled",
		"base_url", integrationCfg.BaseURL,
		"remote_url", integrationCfg.RemoteURL,
		"merge_authority", integrationCfg.MergeAuthority,
		"repo_count", len(integrationCfg.Repos),
		"token_configured", strings.TrimSpace(integrationCfg.Token) != "",
	)
	return githubintegration.New(integrationCfg, nil), nil
}

func initMulticaProjection(cfg config.Config) (*multicaprojection.Projection, error) {
	if cfg.IntegrationsConfigFile == "" {
		slog.Info("multica projection disabled", "reason", "integrations config not configured")
		return nil, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	projectionCfg := fileCfg.Multica.ToMulticaProjectionConfig()
	if !projectionCfg.Enabled {
		slog.Info("multica projection disabled", "reason", "not configured")
		return nil, nil
	}
	slog.Info("multica projection enabled", "command", projectionCfg.Command, "profile", projectionCfg.Profile, "workspace", projectionCfg.Workspace, "comment", projectionCfg.Comment, "set_status_on_merge", projectionCfg.SetStatusOnMerge)
	return multicaprojection.New(projectionCfg), nil
}

func initExecutionContextRegistry(cfg config.Config) (*executioncontext.Registry, error) {
	if cfg.IntegrationsConfigFile == "" {
		slog.Info("execution context registry disabled", "reason", "integrations config not configured")
		return nil, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	if !fileCfg.ExecutionContext.Enabled {
		slog.Info("execution context registry disabled", "reason", "not configured")
		return nil, nil
	}
	registry, err := executioncontext.New(fileCfg.ExecutionContext, nil)
	if err != nil {
		return nil, err
	}
	slog.Info("execution context registry enabled", "connector_count", len(fileCfg.ExecutionContext.Connectors))
	return registry, nil
}

func initDelegationPolicies(cfg config.Config) (*delegationpolicy.Set, error) {
	if cfg.IntegrationsConfigFile == "" {
		return delegationpolicy.New(delegationpolicy.Config{})
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	policies, err := delegationpolicy.New(fileCfg.Delegation)
	if err != nil {
		return nil, err
	}
	slog.Info("delegation policies loaded", "policy_count", len(policies.Policies()))
	return policies, nil
}

func initPrincipalSessions(cfg config.Config) (*sessionauthority.Set, error) {
	if cfg.IntegrationsConfigFile == "" {
		return sessionauthority.New(sessionauthority.Config{})
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	set, err := sessionauthority.New(fileCfg.PrincipalSessions)
	if err != nil {
		return nil, err
	}
	normalized := set.Config()
	slog.Info("principal session authority loaded", "issuer_count", len(normalized.TrustedIssuers), "binding_count", len(normalized.Bindings), "team_binding_count", len(normalized.TeamBindings), "resource_default_count", len(normalized.ResourceDefaults), "resource_count", len(normalized.Resources))
	return set, nil
}

func initMulticaIncidentNotifier(cfg config.Config) (service.MulticaIncidentNotifier, time.Duration, error) {
	if cfg.IntegrationsConfigFile == "" {
		slog.Info("multica incident notifier disabled", "reason", "integrations config not configured")
		return nil, 0, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, 0, err
	}
	if !fileCfg.Multica.Enabled || !fileCfg.Multica.FailureWatch.Enabled {
		slog.Info("multica incident notifier disabled", "reason", "multica failure watch not enabled")
		return nil, 0, nil
	}
	eventCfg, feishuTargets, ok, err := feishuWebhookConfigsForEvent(fileCfg, integrations.NotificationEventMulticaIncident)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		slog.Info("multica incident notifier disabled", "reason", "notification event route not configured", "event", integrations.NotificationEventMulticaIncident)
		return nil, 0, nil
	}
	throttle, err := parseDurationSetting(eventCfg.ThrottleWindow, service.MulticaIncidentDefaultNotifyThrottle)
	if err != nil {
		return nil, 0, fmt.Errorf("notifications.events.%s.throttle_window: %w", integrations.NotificationEventMulticaIncident, err)
	}
	notifier, err := notifications.NewFeishuMulticaIncidentNotifierForTargets(feishuTargets)
	if err != nil {
		return nil, 0, err
	}
	slog.Info("multica incident feishu notifier enabled", "throttle_window", throttle.String(), "target_count", len(feishuTargets))
	return notifier, throttle, nil
}

func initMulticaFailureWatcherConfig(cfg config.Config) (multicafailures.Config, error) {
	if cfg.IntegrationsConfigFile == "" {
		slog.Info("multica failure watcher disabled", "reason", "integrations config not configured")
		return multicafailures.Config{}, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return multicafailures.Config{}, err
	}
	failureWatch := fileCfg.Multica.FailureWatch
	if !fileCfg.Multica.Enabled || !failureWatch.Enabled {
		slog.Info("multica failure watcher disabled", "reason", "not configured")
		return multicafailures.Config{}, nil
	}
	pollInterval, err := parseDurationSetting(failureWatch.PollInterval, time.Minute)
	if err != nil {
		return multicafailures.Config{}, fmt.Errorf("poll_interval: %w", err)
	}
	rollingWindowDays := failureWatch.RollingWindowDays
	if rollingWindowDays == 0 {
		rollingWindowDays = service.MulticaIncidentWindowDays
	}
	if rollingWindowDays != service.MulticaIncidentWindowDays {
		slog.Warn("multica failure watcher rolling_window_days overridden to match service incident window", "configured", rollingWindowDays, "effective", service.MulticaIncidentWindowDays)
		rollingWindowDays = service.MulticaIncidentWindowDays
	}
	workspace := strings.TrimSpace(fileCfg.Multica.DefaultWorkspace)
	if workspace == "" {
		workspace = strings.TrimSpace(fileCfg.Multica.Workspace)
	}
	workspaceID := strings.TrimSpace(fileCfg.Multica.DefaultWorkspaceID)
	if workspaceID == "" {
		workspaceID = strings.TrimSpace(fileCfg.Multica.WorkspaceID)
	}
	return multicafailures.Config{
		Enabled:           true,
		Command:           fileCfg.Multica.Command,
		Profile:           fileCfg.Multica.Profile,
		Workspace:         workspace,
		WorkspaceID:       workspaceID,
		AppURL:            fileCfg.Multica.AppURL,
		PollInterval:      pollInterval,
		IssueLimit:        failureWatch.IssueLimit,
		RollingWindowDays: rollingWindowDays,
	}, nil
}

func parseDurationSetting(value string, defaultValue time.Duration) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	return d, nil
}

func feishuWebhookConfigsForEvent(fileCfg integrations.Config, eventName string) (integrations.NotificationEventConfig, []notifications.FeishuWebhookConfig, bool, error) {
	eventCfg, targets, ok, err := fileCfg.ResolveOutboundEventTargets(eventName)
	if err != nil {
		return integrations.NotificationEventConfig{}, nil, false, fmt.Errorf("outbound.events.%s: %w", eventName, err)
	}
	legacyEvent := integrations.NotificationEventConfig{Enabled: eventCfg.Enabled, Targets: eventCfg.Targets, ThrottleWindow: eventCfg.ThrottleWindow}
	if !ok {
		return legacyEvent, nil, false, nil
	}
	configs := make([]notifications.FeishuWebhookConfig, 0, len(targets))
	for _, resolved := range targets {
		target := resolved.Target
		timeout, err := parseDurationSetting(target.Timeout, 0)
		if err != nil {
			return integrations.NotificationEventConfig{}, nil, false, fmt.Errorf("outbound.targets.%s.timeout: %w", resolved.ID, err)
		}
		configs = append(configs, notifications.FeishuWebhookConfig{
			ID:             resolved.ID,
			Name:           target.Name,
			Description:    target.Description,
			Enabled:        true,
			WebhookURL:     target.WebhookURL,
			WebhookURLFile: target.WebhookURLFile,
			Timeout:        timeout,
		})
	}
	return legacyEvent, configs, true, nil
}

func outboundWebhookConfigsForEvent(fileCfg integrations.Config, eventName string) (integrations.OutboundEventConfig, []notifications.FeishuWebhookConfig, []service.OutboundTarget, bool, error) {
	eventCfg, targets, ok, err := fileCfg.ResolveOutboundEventTargets(eventName)
	if err != nil {
		return integrations.OutboundEventConfig{}, nil, nil, false, fmt.Errorf("outbound.events.%s: %w", eventName, err)
	}
	if !ok {
		return eventCfg, nil, nil, false, nil
	}
	feishuConfigs := make([]notifications.FeishuWebhookConfig, 0, len(targets))
	outboundTargets := make([]service.OutboundTarget, 0, len(targets))
	for _, resolved := range targets {
		target := resolved.Target
		timeout, err := parseDurationSetting(target.Timeout, 0)
		if err != nil {
			return integrations.OutboundEventConfig{}, nil, nil, false, fmt.Errorf("outbound.targets.%s.timeout: %w", resolved.ID, err)
		}
		if target.Type != integrations.OutboundTargetTypeFeishuWebhook {
			return integrations.OutboundEventConfig{}, nil, nil, false, fmt.Errorf("outbound target %s unsupported type %q", resolved.ID, target.Type)
		}
		feishuConfigs = append(feishuConfigs, notifications.FeishuWebhookConfig{
			ID:             resolved.ID,
			Name:           target.Name,
			Description:    target.Description,
			Enabled:        true,
			WebhookURL:     target.WebhookURL,
			WebhookURLFile: target.WebhookURLFile,
			Timeout:        timeout,
		})
		outboundTargets = append(outboundTargets, service.OutboundTarget{Name: resolved.ID, Type: target.Type, DisplayName: target.Name})
	}
	return eventCfg, feishuConfigs, outboundTargets, true, nil
}

func initOutboundDelivery(cfg config.Config) (service.OutboundDispatcher, map[string][]service.OutboundTarget, error) {
	if cfg.IntegrationsConfigFile == "" {
		slog.Info("outbound delivery disabled", "reason", "integrations config not configured")
		return nil, nil, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, nil, err
	}
	events := map[string]string{
		integrations.OutboundEventPullRequestMerged: service.OutboundEventPullRequestMerged,
		integrations.OutboundEventProjectionDrift:   service.OutboundEventProjectionDrift,
		integrations.OutboundEventMulticaIncident:   service.OutboundEventMulticaIncident,
	}
	allFeishuTargets := map[string]notifications.FeishuWebhookConfig{}
	outboundEventTargets := map[string][]service.OutboundTarget{}
	for integrationEvent, serviceEvent := range events {
		_, feishuTargets, outboundTargets, ok, err := outboundWebhookConfigsForEvent(fileCfg, integrationEvent)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		outboundEventTargets[serviceEvent] = outboundTargets
		for _, target := range feishuTargets {
			allFeishuTargets[target.ID] = target
		}
	}
	if len(outboundEventTargets) == 0 && !fileCfg.Multica.ExternalPRDelivery.Enabled {
		slog.Info("outbound delivery disabled", "reason", "outbound event routes not configured")
		return nil, nil, nil
	}
	dispatchers := map[string]service.OutboundDispatcher{}
	feishuTargets := make([]notifications.FeishuWebhookConfig, 0, len(allFeishuTargets))
	for _, target := range allFeishuTargets {
		feishuTargets = append(feishuTargets, target)
	}
	if len(feishuTargets) > 0 {
		feishuDispatcher, err := notifications.NewFeishuOutboundDispatcher(feishuTargets)
		if err != nil {
			return nil, nil, err
		}
		dispatchers[service.OutboundTargetTypeFeishuWebhook] = feishuDispatcher
	}
	if fileCfg.Multica.ExternalPRDelivery.Enabled {
		timeout, err := parseDurationSetting(fileCfg.Multica.ExternalPRDelivery.Timeout, 15*time.Second)
		if err != nil {
			return nil, nil, fmt.Errorf("multica.external_pr_delivery.timeout: %w", err)
		}
		multicaDispatcher, err := notifications.NewMulticaExternalPRDispatcher(notifications.MulticaExternalPRDispatcherConfig{
			TargetName:     service.OutboundTargetNameMulticaExternalPR,
			TargetInstance: fileCfg.Multica.TargetInstance,
			ServerURL:      fileCfg.Multica.ServerURL,
			ServiceToken:   fileCfg.Multica.ServiceToken,
			Timeout:        timeout,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("multica external PR dispatcher: %w", err)
		}
		dispatchers[service.OutboundTargetTypeMulticaExternalPR] = multicaDispatcher
		outboundEventTargets[service.OutboundEventMulticaExternalPRTerminal] = []service.OutboundTarget{{Name: service.OutboundTargetNameMulticaExternalPR, Type: service.OutboundTargetTypeMulticaExternalPR}}
	}
	if len(dispatchers) == 0 {
		slog.Info("outbound delivery disabled", "reason", "no typed outbound dispatcher configured")
		return nil, nil, nil
	}
	dispatcher := service.NewOutboundDispatcherMux(dispatchers)
	slog.Info("outbound delivery enabled", "event_count", len(outboundEventTargets), "feishu_target_count", len(feishuTargets), "typed_dispatcher_count", len(dispatchers))
	return dispatcher, outboundEventTargets, nil
}

func initPullRequestMergeNotifier(cfg config.Config) (service.PullRequestMergeNotifier, error) {
	if cfg.IntegrationsConfigFile == "" {
		slog.Info("pull request merge notifier disabled", "reason", "integrations config not configured")
		return nil, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return nil, err
	}
	_, feishuTargets, ok, err := feishuWebhookConfigsForEvent(fileCfg, integrations.NotificationEventPullRequestMerged)
	if err != nil {
		return nil, err
	}
	if !ok {
		slog.Info("pull request merge notifier disabled", "reason", "notification event route not configured", "event", integrations.NotificationEventPullRequestMerged)
		return nil, nil
	}
	notifier, err := notifications.NewFeishuPullRequestMergeNotifier(feishuTargets)
	if err != nil {
		return nil, err
	}
	slog.Info("pull request merge feishu notifier enabled", "target_count", len(feishuTargets))
	return notifier, nil
}

func startMulticaFailureWatcher(ctx context.Context, svc *service.Service, cfg multicafailures.Config) {
	if !cfg.Enabled || svc == nil {
		return
	}
	watcher := multicafailures.New(cfg, svc)
	if watcher == nil {
		return
	}
	svc.Wg.Add(1)
	go func() {
		defer svc.Wg.Done()
		watcher.Run(ctx)
	}()
	slog.Info("multica failure watcher started", "command", cfg.Command, "profile", cfg.Profile, "workspace", cfg.Workspace, "poll_interval", cfg.PollInterval.String(), "issue_limit", cfg.IssueLimit)
}

func initProjectionWatcher(cfg config.Config) (projectionwatch.Config, projectionwatch.Notifier, error) {
	if cfg.IntegrationsConfigFile == "" {
		slog.Info("projection watcher disabled", "reason", "integrations config not configured")
		return projectionwatch.Config{}, nil, nil
	}
	fileCfg, err := integrations.LoadFile(cfg.IntegrationsConfigFile)
	if err != nil {
		return projectionwatch.Config{}, nil, err
	}
	watchCfg := fileCfg.ProjectionWatch
	if !watchCfg.Enabled {
		slog.Info("projection watcher disabled", "reason", "not configured")
		return projectionwatch.Config{}, nil, nil
	}
	pollInterval, err := parseDurationSetting(watchCfg.PollInterval, time.Minute)
	if err != nil {
		return projectionwatch.Config{}, nil, fmt.Errorf("poll_interval: %w", err)
	}
	scanInterval, err := parseDurationSetting(watchCfg.ScanInterval, 15*time.Minute)
	if err != nil {
		return projectionwatch.Config{}, nil, fmt.Errorf("scan_interval: %w", err)
	}
	fullAuditInterval, err := parseDurationSetting(watchCfg.FullAuditInterval, 24*time.Hour)
	if err != nil {
		return projectionwatch.Config{}, nil, fmt.Errorf("full_audit_interval: %w", err)
	}
	startupAuditDelay, err := parseDurationSetting(watchCfg.StartupAuditDelay, 5*time.Minute)
	if err != nil {
		return projectionwatch.Config{}, nil, fmt.Errorf("startup_audit_delay: %w", err)
	}
	throttleWindow, err := parseDurationSetting(watchCfg.Notify.ThrottleWindow, 30*time.Minute)
	if err != nil {
		return projectionwatch.Config{}, nil, fmt.Errorf("notify.throttle_window: %w", err)
	}
	gracePeriod, err := parseDurationSetting(watchCfg.Notify.GracePeriod, 2*time.Minute)
	if err != nil {
		return projectionwatch.Config{}, nil, fmt.Errorf("notify.grace_period: %w", err)
	}
	_, feishuTargets, ok, err := feishuWebhookConfigsForEvent(fileCfg, integrations.NotificationEventProjectionDrift)
	if err != nil {
		return projectionwatch.Config{}, nil, err
	}
	if !ok {
		slog.Info("projection watcher disabled", "reason", "notification event route not configured", "event", integrations.NotificationEventProjectionDrift)
		return projectionwatch.Config{}, nil, nil
	}
	notifier, err := notifications.NewFeishuProjectionNotifierForTargets(feishuTargets)
	if err != nil {
		return projectionwatch.Config{}, nil, err
	}
	return projectionwatch.Config{
		Enabled: true, PollInterval: pollInterval, ScanInterval: scanInterval,
		FullAuditInterval: fullAuditInterval, StartupAuditDelay: startupAuditDelay,
		ThrottleWindow: throttleWindow, GracePeriod: gracePeriod, Limit: 50,
	}, notifier, nil
}

func startOutboundDeliveryWorker(ctx context.Context, svc *service.Service) {
	if svc == nil || svc.OutboundDispatcher == nil {
		return
	}
	svc.Wg.Add(1)
	go func() {
		defer svc.Wg.Done()
		svc.RunOutboundDeliveryWorker(ctx, 30*time.Second, 20)
	}()
	slog.Info("outbound delivery worker started", "interval", (30 * time.Second).String(), "limit", 20)
}

func startDelegatedSessionExpiryAuditor(ctx context.Context, svc *service.Service) {
	if svc == nil {
		return
	}
	svc.Wg.Add(1)
	go func() {
		defer svc.Wg.Done()
		svc.RunDelegatedSessionExpiryAuditor(ctx, time.Minute, 100)
	}()
	slog.Info("delegated session expiry auditor started", "interval", time.Minute.String(), "limit", 100)
}

func startProjectionWatcher(ctx context.Context, svc *service.Service, cfg projectionwatch.Config, notifier projectionwatch.Notifier) {
	if !cfg.Enabled || svc == nil || notifier == nil {
		return
	}
	watcher := projectionwatch.New(cfg, svc, notifier)
	if watcher == nil {
		return
	}
	svc.Wg.Add(1)
	go func() {
		defer svc.Wg.Done()
		watcher.Run(ctx)
	}()
	slog.Info("projection watcher started",
		"poll_interval", cfg.PollInterval.String(),
		"scan_interval", cfg.ScanInterval.String(),
		"full_audit_interval", cfg.FullAuditInterval.String(),
		"startup_audit_delay", cfg.StartupAuditDelay.String(),
		"throttle_window", cfg.ThrottleWindow.String(),
		"grace_period", cfg.GracePeriod.String(),
	)
}

// httpMuxConfig holds all dependencies required to build the HTTP multiplexer.
// This struct reduces parameter count in buildHTTPMux and related functions.
type httpMuxConfig struct {
	Cfg          config.Config
	Database     *gorm.DB
	ServiceDeps  *service.Service
	GQLServer    *graphql.Server
	GitHandler   *githttp.Handler
	OAuthHandler *oauth.Handler
	Version      string
	EmbeddedAuth srvmiddleware.EmbeddedAuthConfig
}

func initProviderLogBridge(cfg config.Config) (http.Handler, error) {
	if !cfg.ProviderLogBridgeEnabled {
		return nil, nil
	}
	token, err := readProviderBridgeSecret(cfg.ProviderLogBridgeTokenFile)
	if err != nil {
		return nil, fmt.Errorf("read provider log bridge token: %w", err)
	}
	dsn, err := readProviderBridgeSecret(cfg.ProviderLogBridgeForgejoDBDSNFile)
	if err != nil {
		return nil, fmt.Errorf("read provider log bridge database DSN: %w", err)
	}
	forgejoDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("open provider log bridge database: %w", err)
	}
	return providerlogbridge.New(providerlogbridge.Config{
		DB: forgejoDB, ActionsLogDir: cfg.ForgejoActionsLogDir, ServiceToken: token,
		MaxBytes: cfg.ProviderLogBridgeMaxBytes, AllowedCIDRs: cfg.ProviderLogBridgeAllowedCIDRs,
	})
}

func readProviderBridgeSecret(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("owner-only secret file is required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("secret file must be owner-only regular non-symlink")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(value))
	if secret == "" {
		return "", errors.New("secret file is empty")
	}
	return secret, nil
}

func buildHTTPMux(cfg httpMuxConfig) (muxDeps, error) {
	providerLogBridge, err := initProviderLogBridge(cfg.Cfg)
	if err != nil {
		return muxDeps{}, err
	}
	handlers := &rest.Deps{
		Svc:                    cfg.ServiceDeps,
		ReplicationAuthorityID: cfg.Cfg.ReplicationAuthorityID,
		LegacyExtensionAliases: !cfg.Cfg.DisableLegacyExtensionAliases,
		ConsoleBaseURL:         cfg.Cfg.ConsoleBaseURL,
		ProviderLogBridge:      providerLogBridge,
	}

	r := chi.NewRouter()
	r.Use(providerlogbridge.CaptureSocketPeer)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.RequestID)
	r.Use(srvmiddleware.RequestIDResponseHeader())
	r.Use(srvmiddleware.RequestLogging())
	r.Use(srvmiddleware.Recoverer())
	metricsHandler := metrics.Init()
	r.Use(srvmiddleware.MetricsInstrumentation())

	hostMux := router.RegisterRoutes(r, handlers, cfg.GitHandler, cfg.GQLServer, cfg.OAuthHandler, cfg.Cfg.ConsoleBaseURL, cfg.EmbeddedAuth)
	mux := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		transform.Wrap(cfg.Cfg.BaseURL, func() {
			hostMux.ServeHTTP(w, req)
		})
	})
	r.Get("/metrics", metricsHandler.ServeHTTP)

	// Register readiness probe.
	projectionAlertingConfigured := cfg.ServiceDeps.OutboundDispatcher != nil && len(cfg.ServiceDeps.OutboundEventTargets[service.OutboundEventProjectionDrift]) > 0
	typedMulticaConfigured := len(cfg.ServiceDeps.OutboundEventTargets[service.OutboundEventMulticaExternalPRTerminal]) > 0
	typedMulticaWorkerAvailable := typedMulticaConfigured && cfg.ServiceDeps.OutboundDispatcher != nil
	var outboundWorkerHealth func() service.OutboundWorkerHealth
	if cfg.ServiceDeps.OutboundDispatcher != nil {
		outboundWorkerHealth = cfg.ServiceDeps.OutboundWorkerHealth
	}
	var authorityPolicyCheck func(context.Context) error
	if cfg.ServiceDeps.ForgejoIntegration != nil {
		authorityPolicyCheck = func(ctx context.Context) error {
			_, err := cfg.ServiceDeps.ForgejoIntegration.VerifyRepositoryAuthority(ctx)
			return err
		}
	}
	r.Get("/readyz", readyzHandler(readyzConfig{
		MainDB:                       cfg.Database,
		Version:                      cfg.Version,
		ProjectionAlertingRequired:   cfg.ServiceDeps.ForgejoIntegration != nil && cfg.ServiceDeps.ForgejoIntegration.WorkflowActionsEnabled(),
		ProjectionAlertingConfigured: projectionAlertingConfigured,
		TypedMulticaConfigured:       typedMulticaConfigured,
		TypedMulticaWorkerAvailable:  typedMulticaWorkerAvailable,
		OutboundWorkerHealth:         outboundWorkerHealth,
		ProjectionAlertingOptOut:     cfg.Cfg.AllowMissingProjectionAlerting,
		AuthorityPolicyCheck:         authorityPolicyCheck,
		AuthorityPolicyOptOut:        cfg.Cfg.AllowMissingForgejoAuthorityPolicy,
	}))

	return muxDeps{handlers: handlers, router: r, mux: mux}, nil
}

func buildServers(cfg config.Config, mux http.Handler) (serverDeps, error) {
	addr := ":" + cfg.Port

	if cfg.ListenMode == "production" {
		return serverDeps{
			servers: []*http.Server{
				{Addr: addr, Handler: mux},
			},
			labels: []string{
				fmt.Sprintf("http://0.0.0.0:%s", cfg.Port),
			},
		}, nil
	}

	tlsCert, err := tls.LoadX509KeyPair("cert.pem", "key.pem")
	if err != nil {
		return serverDeps{}, fmt.Errorf("TLS: %w", err)
	}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{tlsCert}}

	return serverDeps{
		servers: []*http.Server{
			{Addr: ":443", Handler: mux, TLSConfig: tlsCfg},
			{Addr: addr, Handler: mux, TLSConfig: tlsCfg},
			{Addr: ":8081", Handler: mux, TLSConfig: tlsCfg},
			{Addr: ":80", Handler: mux},
			{Addr: ":4003", Handler: mux},
		},
		labels: []string{
			"https://github.localhost:443",
			fmt.Sprintf("https://localhost:%s", cfg.Port),
			"https://github.localhost:8081",
			"http://github.localhost:80",
			"http://github.localhost:4003",
		},
	}, nil
}

// bootstrap initializes all application dependencies in order.
// It returns a bootstrapResult with either fully initialized deps or partial deps on failure.
func bootstrap() bootstrapResult {
	cfg, err := config.New()
	if err != nil {
		return bootstrapResult{Deps: &bootstrapDeps{}, Err: fmt.Errorf("config: %w", err)}
	}
	return bootstrapWithConfig(cfg, options{})
}

func bootstrapWithConfig(cfg config.Config, opts options) bootstrapResult {
	result := bootstrapResult{
		Deps: &bootstrapDeps{},
	}
	deps := result.Deps
	deps.Options = opts

	// Operator registration is also restricted when enabled before the peer
	// listener has been configured. Embedded/tenant identity is not admitted.
	if cfg.ReplicationAuthorityID != "" && (cfg.ControlPlaneDSN != "" || opts.authenticator != nil || cfg.AllowAnyToken) {
		result.Err = fmt.Errorf("replication registration requires strict single-DB native authentication")
		return result
	}
	// Validate explicit peer configuration before DB migration/worker startup.
	var peerConfig *loadedReplication
	if cfg.ReplicationConfigFile != "" {
		if cfg.ControlPlaneDSN != "" || opts.authenticator != nil || cfg.AllowAnyToken {
			result.Err = fmt.Errorf("replication requires single-DB strict native authentication")
			return result
		}
		var err error
		peerConfig, err = loadReplicationConfig(cfg.ReplicationConfigFile)
		if err != nil {
			result.Err = err
			return result
		}
		if cfg.ReplicationAuthorityID != "" && cfg.ReplicationAuthorityID != peerConfig.file.AuthorityID {
			result.Err = fmt.Errorf("replication authority configuration mismatch")
			return result
		}
		cfg.ReplicationAuthorityID = peerConfig.file.AuthorityID
	}

	// 1. Core dependencies (config, database, embedding, gitstore).
	core, err := initCoreDepsFromConfig(cfg)
	if err != nil {
		result.Err = err
		if core.cfgLoaded {
			partial := &bootstrapDeps{Cfg: core.cfg}
			if core.db != nil {
				partial.DB = core.db
			}
			if core.embedder != nil {
				partial.Embedder = core.embedder
			}
			if core.store != nil {
				partial.Store = core.store
			}
			result.Partial = partial
		}
		return result
	}
	deps.Cfg = core.cfg
	deps.DB = core.db
	deps.Embedder = core.embedder
	deps.Store = core.store

	// 2. Server-level context.
	srvCtx, srvCancel := context.WithCancel(context.Background())
	deps.SrvCtx = srvCtx
	deps.SrvCancel = srvCancel

	// 3. Service dependencies, OIDC, and handlers.
	svc, err := initServiceDeps(core.cfg, core.db, core.store, core.embedder, srvCtx)
	if err != nil {
		result.Err = err
		result.Partial = &bootstrapDeps{Cfg: core.cfg, DB: core.db, Embedder: core.embedder, Store: core.store, SrvCtx: srvCtx, SrvCancel: srvCancel, SvcDeps: svc.svc}
		return result
	}
	deps.SvcDeps = svc.svc
	deps.GqlSrv = svc.gqlSrv
	deps.GitHandler = svc.gitHandler
	deps.OauthHandler = svc.oauthHandler

	// 4. Build router and host-aware mux.
	mux, err := buildHTTPMux(httpMuxConfig{
		Cfg:          core.cfg,
		Database:     core.db,
		ServiceDeps:  svc.svc,
		GQLServer:    svc.gqlSrv,
		GitHandler:   svc.gitHandler,
		OAuthHandler: svc.oauthHandler,
		Version:      gitSHA,
		EmbeddedAuth: embeddedAuthConfig(opts),
	})
	if err != nil {
		result.Err = err
		result.Partial = &bootstrapDeps{
			Cfg:          core.cfg,
			DB:           core.db,
			Embedder:     core.embedder,
			Store:        core.store,
			SrvCtx:       srvCtx,
			SrvCancel:    srvCancel,
			SvcDeps:      svc.svc,
			GqlSrv:       svc.gqlSrv,
			GitHandler:   svc.gitHandler,
			OauthHandler: svc.oauthHandler,
		}
		return result
	}
	deps.Handlers = mux.handlers
	deps.Mux = mux.mux

	// 6. Set up HTTP servers.
	srvs, err := buildServers(core.cfg, mux.mux)
	if err != nil {
		result.Err = err
		result.Partial = &bootstrapDeps{
			Cfg:          core.cfg,
			DB:           core.db,
			Embedder:     core.embedder,
			Store:        core.store,
			SrvCtx:       srvCtx,
			SrvCancel:    srvCancel,
			SvcDeps:      svc.svc,
			GqlSrv:       svc.gqlSrv,
			GitHandler:   svc.gitHandler,
			OauthHandler: svc.oauthHandler,
		}
		return result
	}
	deps.Servers = srvs.servers
	deps.Labels = srvs.labels

	if peerConfig != nil {
		owner := &Server{cfg: cfg, deps: deps, handler: deps.Mux}
		managed, err := owner.configureReplication(peerConfig)
		if err != nil {
			result.Err = fmt.Errorf("replication setup: %w", err)
			result.Partial = deps
			return result
		}
		deps.Replication = managed
		deps.Servers = append(deps.Servers, managed.server)
		deps.Labels = append(deps.Labels, "replication "+managed.server.Addr)
	}

	return result
}

// shutdownConfig holds configuration for shutdown behavior.
type shutdownConfig struct {
	GracePeriod time.Duration
}

// shutdownResult captures the results of shutdown operations.
type shutdownResult struct {
	HTTPShutdownErrors []error
	BgDrained          bool
	BgDrainTimedOut    bool
	ContextCanceled    bool
}

// waitForWaitGroup waits for a wait group to complete or context to timeout.
func waitForWaitGroup(ctx context.Context, wg *sync.WaitGroup, name string, drainedFlag *bool, timedOutFlag *bool) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		*drainedFlag = true
		slog.Info("wait group drained", "name", name)
	case <-ctx.Done():
		*timedOutFlag = true
		slog.Warn("wait group drain timed out", "name", name, "error", ctx.Err())
	}
}

// shutdown gracefully stops all servers and waits for background workers.
func shutdown(deps *bootstrapDeps, cfg shutdownConfig) shutdownResult {
	result := shutdownResult{}

	// Shutdown HTTP servers.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.GracePeriod)
	defer cancel()

	for _, srv := range deps.Servers {
		if err := srv.Shutdown(ctx); err != nil {
			result.HTTPShutdownErrors = append(result.HTTPShutdownErrors, err)
		}
	}
	// Cancel the server context so background goroutines observe Done and begin draining.
	if deps.SrvCancel != nil {
		deps.SrvCancel()
		result.ContextCanceled = true
	}

	slog.Info("all listeners stopped; waiting for background goroutines")

	// Wait for background goroutines (svcDeps.Wg).
	waitForWaitGroup(ctx, &deps.SvcDeps.Wg, "background goroutines (svcDeps)", &result.BgDrained, &result.BgDrainTimedOut)

	return result
}

func run(sigCh <-chan struct{}, shutdownCfg shutdownConfig) (runErr error) {
	result := bootstrap()
	if result.Err != nil {
		cleanupFailedBootstrap(result.Partial)
		return result.Err
	}
	deps := result.Deps
	startRuntimeWorkers(deps)
	instance := &Server{cfg: deps.Cfg, deps: deps, handler: deps.Mux}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownCfg.GracePeriod)
		defer cancel()
		runErr = errors.Join(runErr, instance.Shutdown(ctx))
	}()

	// Recover durable action intents interrupted by restart.
	// Must run once per process start, after DB/service composition,
	// before serving HTTP. Bounded and safe on repeated restarts.
	recoverCtx, recoverCancel := context.WithTimeout(deps.SrvCtx, 30*time.Second)
	if err := deps.SvcDeps.RecoverDurableActionIntents(recoverCtx); err != nil {
		recoverCancel()
		slog.Error("durable action intent recovery failed", "error", err)
		return fmt.Errorf("startup recovery: %w", err)
	}
	recoverCancel()
	slog.Info("durable action intent recovery completed")
	// Expired intents must be denied before any durable job can reach a
	// provider seam during startup.
	if err := deps.SvcDeps.ResumePendingForgejoProjectionJobs(deps.SrvCtx); err != nil {
		return fmt.Errorf("resume pending Forgejo projection jobs: %w", err)
	}

	// Bind every listener before serving any request, including the optional
	// peer listener. A partial bind or later listener failure is a process error.
	if err := instance.Start(); err != nil {
		return err
	}
	select {
	case <-sigCh:
		slog.Info("shutdown initiated", "grace_period", shutdownCfg.GracePeriod.String())
		return nil
	case err := <-instance.serveErrors:
		return err
	}
}

// RunWikiReindex reindexes wiki search data for one repo or all repos.
func RunWikiReindex(args []string) error {
	cfg, err := config.New()
	if err != nil {
		return err
	}
	// This one-shot maintenance command never owns a peer listener or a
	// second instance of the live primary's retained snapshot store.
	cfg.ReplicationConfigFile = ""
	result := bootstrapWithConfig(cfg, options{})
	if result.Err != nil {
		cleanupFailedBootstrap(result.Partial)
		return result.Err
	}
	deps := result.Deps
	defer cleanupFailedBootstrap(deps)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		count, err := deps.SvcDeps.ReindexWikiSearch(ctx, strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}
		fmt.Printf("wiki-reindex repo=%s indexed=%d\n", strings.TrimSpace(args[0]), count)
		return nil
	}

	count, err := deps.SvcDeps.ReindexAllWikiSearch(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("wiki-reindex indexed=%d\n", count)
	return nil
}

// Run starts the gh-server listeners and blocks until shutdown is requested.
func Run(sigCh <-chan struct{}) error {
	return run(sigCh, shutdownConfig{GracePeriod: 10 * time.Second})
}

// New constructs a server from a caller-supplied config.
func New(cfg config.Config, opts ...Option) (*Server, error) {
	normalized, err := config.Normalize(cfg)
	if err != nil {
		return nil, err
	}
	parsed := options{}
	for _, opt := range opts {
		if opt != nil {
			opt(&parsed)
		}
	}
	result := bootstrapWithConfig(normalized, parsed)
	if result.Err != nil {
		cleanupFailedBootstrap(result.Partial)
		return nil, result.Err
	}
	startRuntimeWorkers(result.Deps)
	return &Server{
		cfg:     result.Deps.Cfg,
		deps:    result.Deps,
		handler: result.Deps.Mux,
	}, nil
}

// Handler returns the fully wired application handler tree.
func (s *Server) Handler() http.Handler { return s.handler }

// Start binds listeners and serves traffic in background goroutines.
func (s *Server) Start() error {
	if s.started {
		return nil
	}
	listeners := make([]net.Listener, 0, len(s.deps.Servers))
	for _, srv := range s.deps.Servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return err
		}
		listeners = append(listeners, ln)
	}
	s.listeners = listeners
	s.serveErrors = make(chan error, len(listeners))
	for i, srv := range s.deps.Servers {
		lbl := s.deps.Labels[i]
		ln := s.listeners[i]
		s.serveWG.Add(1)
		go func(srv *http.Server, ln net.Listener, label string) {
			defer s.serveWG.Done()
			fmt.Printf("gh-server listening on %s\n", label)
			var err error
			if srv.TLSConfig != nil {
				err = srv.Serve(tls.NewListener(ln, srv.TLSConfig))
			} else {
				err = srv.Serve(ln)
			}
			if err != nil && err != http.ErrServerClosed {
				slog.Error("listener exited unexpectedly", "listener", label, "error", err)
				s.serveErrors <- fmt.Errorf("listener %s failed: %w", label, err)
			}
		}(srv, ln, lbl)
	}
	s.started = true
	return nil
}

// Shutdown gracefully stops listeners and background work.
func (s *Server) Shutdown(ctx context.Context) error {
	var errs []string
	if s.deps.Replication != nil {
		s.deps.Replication.beginDrain()
	}
	for _, srv := range s.deps.Servers {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, err.Error())
			_ = srv.Close()
		}
	}
	if s.deps.Replication != nil {
		if err := s.deps.Replication.close(ctx); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if s.deps.SrvCancel != nil {
		s.deps.SrvCancel()
	}
	done := make(chan struct{})
	go func() {
		if s.deps.SvcDeps != nil {
			s.deps.SvcDeps.Wg.Wait()
		}
		s.serveWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, ctx.Err().Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("shutdown: %s", strings.Join(errs, "; "))
	}
	return nil
}

// readyzHandler returns an http.HandlerFunc that pings the main DB. Returns
// 200 when backing stores are reachable, 503 otherwise.
// readyzConfig holds dependencies for the readiness probe handler.
// This struct reduces parameter count and improves clarity.
type readyzConfig struct {
	MainDB                       *gorm.DB
	Version                      string
	ProjectionAlertingRequired   bool
	ProjectionAlertingConfigured bool
	TypedMulticaConfigured       bool
	TypedMulticaWorkerAvailable  bool
	OutboundWorkerHealth         func() service.OutboundWorkerHealth
	ProjectionAlertingOptOut     bool
	AuthorityPolicyCheck         func(context.Context) error
	AuthorityPolicyOptOut        bool
}

func readyzHandler(cfg readyzConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		w.Header().Set("Content-Type", "application/json")

		type checkResult struct {
			Status string `json:"status"`
			Error  string `json:"error,omitempty"`
		}
		checks := map[string]checkResult{}
		healthy := true

		// Check main DB.
		if sqlDB, err := cfg.MainDB.DB(); err != nil {
			checks["main_db"] = checkResult{Status: "unavailable", Error: err.Error()}
			healthy = false
		} else if err := sqlDB.PingContext(ctx); err != nil {
			checks["main_db"] = checkResult{Status: "unavailable", Error: err.Error()}
			healthy = false
		} else {
			checks["main_db"] = checkResult{Status: "ok"}
		}

		if cfg.TypedMulticaConfigured {
			checks["multica_typed_delivery"] = checkResult{Status: "configured"}
			if cfg.TypedMulticaWorkerAvailable {
				checks["multica_typed_worker"] = checkResult{Status: "ok"}
			} else {
				checks["multica_typed_worker"] = checkResult{Status: "unavailable", Error: "typed Multica target is configured but the outbound worker is unavailable"}
				healthy = false
			}
		} else {
			checks["multica_typed_delivery"] = checkResult{Status: "disabled"}
		}

		if cfg.OutboundWorkerHealth != nil {
			workerHealth := cfg.OutboundWorkerHealth()
			switch {
			case workerHealth.LastError != "":
				checks["outbound_worker"] = checkResult{Status: "unavailable", Error: workerHealth.LastError}
				healthy = false
			case !workerHealth.LastPollAt.IsZero() && time.Since(workerHealth.LastPollAt) > 2*time.Minute:
				checks["outbound_worker"] = checkResult{Status: "unavailable", Error: "outbound delivery worker heartbeat is stale"}
				healthy = false
			default:
				checks["outbound_worker"] = checkResult{Status: "ok"}
			}
		}

		// Required Forgejo workflow actions must have a durable outbound projection-drift route.
		if cfg.ProjectionAlertingRequired {
			switch {
			case cfg.ProjectionAlertingConfigured:
				checks["projection_alerting"] = checkResult{Status: "ok"}
			case cfg.ProjectionAlertingOptOut:
				checks["projection_alerting"] = checkResult{Status: "degraded", Error: "explicit_opt_out"}
			default:
				checks["projection_alerting"] = checkResult{Status: "unavailable", Error: "projection_drift outbound target and dispatcher are required for Forgejo workflow actions"}
				healthy = false
			}

			if cfg.AuthorityPolicyOptOut {
				checks["forgejo_authority_policy"] = checkResult{Status: "degraded", Error: "explicit_opt_out"}
			} else if cfg.AuthorityPolicyCheck == nil {
				checks["forgejo_authority_policy"] = checkResult{Status: "unavailable", Error: "authority policy verifier is not configured"}
				healthy = false
			} else if err := cfg.AuthorityPolicyCheck(ctx); err != nil {
				checks["forgejo_authority_policy"] = checkResult{Status: "unavailable", Error: err.Error()}
				healthy = false
			} else {
				checks["forgejo_authority_policy"] = checkResult{Status: "ok"}
			}
		}

		status := "ready"
		code := http.StatusOK
		if !healthy {
			status = "not_ready"
			code = http.StatusServiceUnavailable
		}
		metrics.ObserveReadyz(status)
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  status,
			"version": cfg.Version,
			"checks":  checks,
		})
	}
}
