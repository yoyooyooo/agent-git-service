package integrations

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/cibackend"
	"github.com/ngaut/agent-git-service/internal/delegationpolicy"
	"github.com/ngaut/agent-git-service/internal/executioncontext"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"github.com/ngaut/agent-git-service/internal/githubintegration"
	"github.com/ngaut/agent-git-service/internal/gitlabintegration"
	"github.com/ngaut/agent-git-service/internal/multicaprojection"
	"github.com/ngaut/agent-git-service/internal/sessionauthority"
	"gopkg.in/yaml.v3"
)

var targetInstancePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

const maxTypedOutboundDispatchTimeout = 2 * time.Minute

// Config is the file-backed integration configuration.
type Config struct {
	CI         cibackend.Config        `yaml:"ci"`
	Delegation delegationpolicy.Config `yaml:"delegation"`
	// TeamAuthority is the current Multica workspace → AGS principal binding
	// surface (AGS-T022 Stage 5). It replaces the retired principal_sessions
	// permission projection key. Runtime code still consumes the normalized
	// value via PrincipalSessions after load.
	TeamAuthority sessionauthority.Config `yaml:"team_authority"`
	// PrincipalSessions is the normalized authority config used by services.
	// It is not a live YAML key; LoadFile copies team_authority into it and
	// rejects residual principal_sessions dual-path configs.
	PrincipalSessions sessionauthority.Config `yaml:"-"`
	// PrincipalSessionsLegacy is parsed only so LoadFile can fail closed when a
	// residual principal_sessions permission projection is still present.
	PrincipalSessionsLegacy sessionauthority.Config     `yaml:"principal_sessions"`
	Forgejo                 ForgejoConfig               `yaml:"forgejo"`
	GitLab                  GitLabConfig                `yaml:"gitlab"`
	GitHub                  GitHubConfig                `yaml:"github"`
	Multica                 MulticaConfig               `yaml:"multica"`
	ExecutionContext        executioncontext.Config     `yaml:"execution_context"`
	Outbound                OutboundConfig              `yaml:"outbound"`
	Notifications           NotificationsConfig         `yaml:"notifications"`
	ProjectionWatch         ProjectionWatchConfig       `yaml:"projection_watch"`
	Repositories            map[string]RepositoryConfig `yaml:"repositories"`
}

// RepositoryConfig is per-repo host policy that is not Forgejo/GitLab mapping.
type RepositoryConfig struct {
	MergePolicy RepositoryMergePolicy `yaml:"merge_policy"`
}

// RepositoryMergePolicy is consumed by Access Grant pr.merge preflight and ags-cli.
type RepositoryMergePolicy struct {
	MinApprovals                   int      `yaml:"min_approvals"`
	RequireForgejoBranchProtection bool     `yaml:"require_forgejo_branch_protection"`
	BlockOnRequestedChanges        *bool    `yaml:"block_on_requested_changes"`
	CountPRAuthorApproval          bool     `yaml:"count_pr_author_approval"`
	IgnoreReviewers                []string `yaml:"ignore_reviewers"`
	RequireCI                      *bool    `yaml:"require_ci"`
}

// AccessGrantRequiresCI reports whether workload pr.merge must see exact-head CI success.
// Missing repos and missing require_ci default to true so other repositories stay gated.
func (c Config) AccessGrantRequiresCI(repoFullName string) bool {
	repoFullName = strings.TrimSpace(repoFullName)
	if repoFullName == "" || c.Repositories == nil {
		return true
	}
	repo, ok := c.Repositories[repoFullName]
	if !ok || repo.MergePolicy.RequireCI == nil {
		return true
	}
	return *repo.MergePolicy.RequireCI
}

type BranchPolicy struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

type ForgejoConfig struct {
	Enabled                   bool                         `yaml:"enabled"`
	BaseURL                   string                       `yaml:"base_url"`
	Token                     string                       `yaml:"token"`
	TokenFile                 string                       `yaml:"token_file"`
	WebhookSecret             string                       `yaml:"webhook_secret"`
	WebhookSecretFile         string                       `yaml:"webhook_secret_file"`
	DefaultOwner              string                       `yaml:"default_owner"`
	PrivateRepos              bool                         `yaml:"private_repos"`
	AutoCreateRepo            bool                         `yaml:"auto_create_repo"`
	AutoPR                    bool                         `yaml:"auto_pr"`
	DefaultBaseBranch         string                       `yaml:"default_base_branch"`
	PushGitConfig             []string                     `yaml:"push_git_config"`
	PushTimeout               string                       `yaml:"push_timeout"`
	Mirror                    BranchPolicy                 `yaml:"mirror"`
	PullRequest               BranchPolicy                 `yaml:"pull_request"`
	Repos                     map[string]ForgejoRepoConfig `yaml:"repos"`
	AuthorityPolicy           ForgejoAuthorityPolicyConfig `yaml:"authority_policy"`
	ActionsLogDir             string                       `yaml:"actions_log_dir"`
	ActionsLogBridgeURL       string                       `yaml:"actions_log_bridge_url"`
	ActionsLogBridgeTokenFile string                       `yaml:"actions_log_bridge_token_file"`
	ActionsLogBridgeToken     string                       `yaml:"-"`
}

type ForgejoAuthorityPolicyConfig struct {
	Enabled                 bool            `yaml:"enabled"`
	WebhookURL              string          `yaml:"webhook_url"`
	IntegrationBot          string          `yaml:"integration_bot"`
	OperatorToken           string          `yaml:"operator_token"`
	OperatorTokenFile       string          `yaml:"operator_token_file"`
	ActionPrincipalBindings map[string]uint `yaml:"action_principal_bindings"`
}

type ForgejoRepoConfig struct {
	Owner                string `yaml:"owner"`
	Repo                 string `yaml:"repo"`
	Enabled              *bool  `yaml:"enabled"`
	BaseBranch           string `yaml:"base_branch"`
	AutoPR               *bool  `yaml:"auto_pr"`
	DelegatedMergeMethod string `yaml:"delegated_merge_method"`
	FastForwardAck       *bool  `yaml:"fast_forward_ack"`
}

type GitLabConfig struct {
	Enabled        bool                        `yaml:"enabled"`
	BaseURL        string                      `yaml:"base_url"`
	Token          string                      `yaml:"token"`
	TokenFile      string                      `yaml:"token_file"`
	MergeAuthority string                      `yaml:"merge_authority"`
	Mirror         BranchPolicy                `yaml:"mirror"`
	Repos          map[string]GitLabRepoConfig `yaml:"repos"`
}

type GitLabRepoConfig struct {
	ProjectID        string                       `yaml:"project_id"`
	ProjectPath      string                       `yaml:"project_path"`
	TargetBranch     string                       `yaml:"target_branch"`
	EnvBranches      map[string]string            `yaml:"env_branches"`
	Enabled          *bool                        `yaml:"enabled"`
	RepoFlowEvidence GitLabRepoFlowEvidenceConfig `yaml:"repo_flow_evidence"`
}

type GitLabRepoFlowEvidenceConfig struct {
	Enabled bool                              `yaml:"enabled"`
	Store   GitLabRepoFlowEvidenceStoreConfig `yaml:"store"`
}

type GitLabRepoFlowEvidenceStoreConfig struct {
	Type       string `yaml:"type"`
	Path       string `yaml:"path"`
	MaxRecords int    `yaml:"max_records"`
}

// GitHubConfig is the optional AGS -> GitHub backup/shadow sync (PushRef/PushBackup/shadow PRs).
// Forgejo remains merge authority; GitHub PRs are visibility-only.
type GitHubConfig struct {
	Enabled        bool                        `yaml:"enabled"`
	BaseURL        string                      `yaml:"base_url"`
	RemoteURL      string                      `yaml:"remote_url"`
	Token          string                      `yaml:"token"`
	TokenFile      string                      `yaml:"token_file"`
	MergeAuthority string                      `yaml:"merge_authority"`
	Mirror         BranchPolicy                `yaml:"mirror"`
	Repos          map[string]GitHubRepoConfig `yaml:"repos"`
}

type GitHubRepoConfig struct {
	Owner        string `yaml:"owner"`
	Repo         string `yaml:"repo"`
	RemoteURL    string `yaml:"remote_url"`
	TargetBranch string `yaml:"target_branch"`
	Enabled      *bool  `yaml:"enabled"`
}

type MulticaConfig struct {
	Enabled             bool                            `yaml:"enabled"`
	TargetInstance      string                          `yaml:"target_instance"`
	Command             string                          `yaml:"command"`
	Profile             string                          `yaml:"profile"`
	Workspace           string                          `yaml:"workspace"`
	WorkspaceID         string                          `yaml:"workspace_id"`
	DefaultWorkspace    string                          `yaml:"default_workspace"`
	DefaultWorkspaceID  string                          `yaml:"default_workspace_id"`
	AppURL              string                          `yaml:"app_url"`
	ServerURL           string                          `yaml:"server_url"`
	ExternalPRProvider  string                          `yaml:"external_pr_provider"`
	LinkTokenAudience   string                          `yaml:"link_token_audience"`
	LinkTokenSecret     string                          `yaml:"link_token_secret"`
	LinkTokenSecretFile string                          `yaml:"link_token_secret_file"`
	ServiceToken        string                          `yaml:"service_token"`
	ServiceTokenFile    string                          `yaml:"service_token_file"`
	Comment             bool                            `yaml:"comment"`
	SetStatusOnMerge    bool                            `yaml:"set_status_on_merge"`
	CompletionOnMerge   MulticaCompletionOnMergeConfig  `yaml:"completion_on_merge"`
	ExternalPRDelivery  MulticaExternalPRDeliveryConfig `yaml:"external_pr_delivery"`
	Repos               map[string]MulticaWorkspaceRef  `yaml:"repos"`
	Workspaces          map[string]MulticaWorkspaceRef  `yaml:"workspaces"`
	FailureWatch        MulticaFailureWatchConfig       `yaml:"failure_watch"`
}

// MulticaCompletionOnMergeConfig enables Multica's provider-neutral atomic
// external-PR completion endpoint. The only supported v1 mode is
// leaf_child_only.
type MulticaCompletionOnMergeConfig struct {
	Enabled bool   `yaml:"enabled"`
	Mode    string `yaml:"mode"`
}

// MulticaExternalPRDeliveryConfig enables the typed, durable AGS-to-Multica
// terminal handoff. The endpoint path and target type are fixed in code.
type MulticaExternalPRDeliveryConfig struct {
	Enabled bool   `yaml:"enabled"`
	Timeout string `yaml:"timeout"`
}

type MulticaWorkspaceRef struct {
	Workspace   string `yaml:"workspace"`
	WorkspaceID string `yaml:"workspace_id"`
	Profile     string `yaml:"profile"`
}

type MulticaFailureWatchConfig struct {
	Enabled           bool   `yaml:"enabled"`
	PollInterval      string `yaml:"poll_interval"`
	RollingWindowDays int    `yaml:"rolling_window_days"`
	IssueLimit        int    `yaml:"issue_limit"`
}

type ProjectionWatchConfig struct {
	Enabled           bool                        `yaml:"enabled"`
	PollInterval      string                      `yaml:"poll_interval"`
	ScanInterval      string                      `yaml:"scan_interval"`
	FullAuditInterval string                      `yaml:"full_audit_interval"`
	StartupAuditDelay string                      `yaml:"startup_audit_delay"`
	Repos             string                      `yaml:"repos"`
	RefPolicy         string                      `yaml:"ref_policy"`
	Notify            ProjectionWatchNotifyConfig `yaml:"notify"`
	Repair            ProjectionWatchRepairConfig `yaml:"repair"`
}

type ProjectionWatchNotifyConfig struct {
	On             []string `yaml:"on"`
	ThrottleWindow string   `yaml:"throttle_window"`
	GracePeriod    string   `yaml:"grace_period"`
}

type ProjectionWatchRepairConfig struct {
	Auto        bool     `yaml:"auto"`
	SafeRepairs []string `yaml:"safe_repairs"`
}

const (
	NotificationTargetTypeFeishuWebhook = "feishu_webhook"

	NotificationEventPullRequestMerged = "pull_request_merged"
	NotificationEventProjectionDrift   = "projection_drift"
	NotificationEventMulticaIncident   = "multica_incident"
)

type NotificationsConfig struct {
	Targets map[string]NotificationTargetConfig `yaml:"targets"`
	Events  map[string]NotificationEventConfig  `yaml:"events"`
}

type NotificationTargetConfig struct {
	Type           string `yaml:"type"`
	Enabled        bool   `yaml:"enabled"`
	Name           string `yaml:"name"`
	Description    string `yaml:"description"`
	WebhookURL     string `yaml:"webhook_url"`
	WebhookURLFile string `yaml:"webhook_url_file"`
	Timeout        string `yaml:"timeout"`
}

type NotificationEventConfig struct {
	Enabled        bool     `yaml:"enabled"`
	Targets        []string `yaml:"targets"`
	ThrottleWindow string   `yaml:"throttle_window"`
}

type ResolvedNotificationTarget struct {
	ID     string
	Target NotificationTargetConfig
}

const (
	OutboundTargetTypeFeishuWebhook = NotificationTargetTypeFeishuWebhook

	OutboundEventPullRequestMerged = NotificationEventPullRequestMerged
	OutboundEventProjectionDrift   = NotificationEventProjectionDrift
	OutboundEventMulticaIncident   = NotificationEventMulticaIncident
)

type OutboundConfig struct {
	Targets map[string]OutboundTargetConfig `yaml:"targets"`
	Events  map[string]OutboundEventConfig  `yaml:"events"`
}

type OutboundTargetConfig struct {
	Type           string `yaml:"type"`
	Enabled        bool   `yaml:"enabled"`
	Name           string `yaml:"name"`
	Description    string `yaml:"description"`
	WebhookURL     string `yaml:"webhook_url"`
	WebhookURLFile string `yaml:"webhook_url_file"`
	Timeout        string `yaml:"timeout"`
}

type OutboundEventConfig struct {
	Enabled        bool     `yaml:"enabled"`
	Targets        []string `yaml:"targets"`
	ThrottleWindow string   `yaml:"throttle_window"`
}

type ResolvedOutboundTarget struct {
	ID     string
	Target OutboundTargetConfig
}

func (c Config) ResolveOutboundEventTargets(eventName string) (OutboundEventConfig, []ResolvedOutboundTarget, bool, error) {
	event, targets, ok, err := c.Outbound.ResolveEventTargets(eventName)
	if err != nil || ok || hasOutboundEventConfig(c.Outbound) {
		return event, targets, ok, err
	}
	legacyEvent, legacyTargets, legacyOK, legacyErr := c.Notifications.ResolveEventTargets(eventName)
	if legacyErr != nil || !legacyOK {
		return OutboundEventConfig{Enabled: legacyEvent.Enabled, Targets: legacyEvent.Targets, ThrottleWindow: legacyEvent.ThrottleWindow}, nil, legacyOK, legacyErr
	}
	converted := make([]ResolvedOutboundTarget, 0, len(legacyTargets))
	for _, target := range legacyTargets {
		converted = append(converted, ResolvedOutboundTarget{ID: target.ID, Target: outboundTargetFromNotification(target.Target)})
	}
	return OutboundEventConfig{Enabled: legacyEvent.Enabled, Targets: legacyEvent.Targets, ThrottleWindow: legacyEvent.ThrottleWindow}, converted, true, nil
}

func hasOutboundEventConfig(c OutboundConfig) bool {
	return len(c.Targets) > 0 || len(c.Events) > 0
}

func outboundTargetFromNotification(target NotificationTargetConfig) OutboundTargetConfig {
	return OutboundTargetConfig{
		Type:           target.Type,
		Enabled:        target.Enabled,
		Name:           target.Name,
		Description:    target.Description,
		WebhookURL:     target.WebhookURL,
		WebhookURLFile: target.WebhookURLFile,
		Timeout:        target.Timeout,
	}
}

func (c OutboundConfig) ResolveEventTargets(eventName string) (OutboundEventConfig, []ResolvedOutboundTarget, bool, error) {
	eventName = strings.TrimSpace(eventName)
	if eventName == "" {
		return OutboundEventConfig{}, nil, false, fmt.Errorf("event name is empty")
	}
	event, ok := c.Events[eventName]
	if !ok || !event.Enabled {
		return event, nil, false, nil
	}
	if len(event.Targets) == 0 {
		return event, nil, false, fmt.Errorf("event %q has no targets", eventName)
	}
	resolved := make([]ResolvedOutboundTarget, 0, len(event.Targets))
	for _, rawID := range event.Targets {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return event, nil, false, fmt.Errorf("event %q has empty target id", eventName)
		}
		target, ok := c.Targets[id]
		if !ok {
			return event, nil, false, fmt.Errorf("event %q references missing target %q", eventName, id)
		}
		if !target.Enabled {
			return event, nil, false, fmt.Errorf("event %q references disabled target %q", eventName, id)
		}
		target.Type = strings.TrimSpace(strings.ToLower(target.Type))
		if target.Type == "" {
			return event, nil, false, fmt.Errorf("target %q type is required", id)
		}
		if target.Type != OutboundTargetTypeFeishuWebhook {
			return event, nil, false, fmt.Errorf("target %q has unsupported type %q", id, target.Type)
		}
		resolved = append(resolved, ResolvedOutboundTarget{ID: id, Target: target})
	}
	return event, resolved, true, nil
}

func (c NotificationsConfig) ResolveEventTargets(eventName string) (NotificationEventConfig, []ResolvedNotificationTarget, bool, error) {
	eventName = strings.TrimSpace(eventName)
	if eventName == "" {
		return NotificationEventConfig{}, nil, false, fmt.Errorf("event name is empty")
	}
	event, ok := c.Events[eventName]
	if !ok || !event.Enabled {
		return event, nil, false, nil
	}
	if len(event.Targets) == 0 {
		return event, nil, false, fmt.Errorf("event %q has no targets", eventName)
	}
	resolved := make([]ResolvedNotificationTarget, 0, len(event.Targets))
	for _, rawID := range event.Targets {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return event, nil, false, fmt.Errorf("event %q has empty target id", eventName)
		}
		target, ok := c.Targets[id]
		if !ok {
			return event, nil, false, fmt.Errorf("event %q references missing target %q", eventName, id)
		}
		if !target.Enabled {
			return event, nil, false, fmt.Errorf("event %q references disabled target %q", eventName, id)
		}
		target.Type = strings.TrimSpace(strings.ToLower(target.Type))
		if target.Type == "" {
			return event, nil, false, fmt.Errorf("target %q type is required", id)
		}
		if target.Type != NotificationTargetTypeFeishuWebhook {
			return event, nil, false, fmt.Errorf("target %q has unsupported type %q", id, target.Type)
		}
		resolved = append(resolved, ResolvedNotificationTarget{ID: id, Target: target})
	}
	return event, resolved, true, nil
}

func (c MulticaConfig) ToMulticaProjectionConfig() multicaprojection.Config {
	repos := make(map[string]multicaprojection.WorkspaceConfig, len(c.Repos))
	for repo, workspace := range c.Repos {
		repos[repo] = workspace.toProjectionWorkspace(repo)
	}
	workspaces := make(map[string]multicaprojection.WorkspaceConfig, len(c.Workspaces))
	for slug, workspace := range c.Workspaces {
		workspaces[slug] = workspace.toProjectionWorkspace(slug)
	}
	return multicaprojection.Config{
		Enabled:            c.Enabled,
		TargetInstance:     c.TargetInstance,
		Command:            c.Command,
		Profile:            c.Profile,
		Workspace:          c.Workspace,
		WorkspaceID:        c.WorkspaceID,
		DefaultWorkspace:   c.DefaultWorkspace,
		DefaultWorkspaceID: c.DefaultWorkspaceID,
		AppURL:             c.AppURL,
		ServerURL:          c.ServerURL,
		ExternalPRProvider: firstNonEmptyString(c.ExternalPRProvider, "ags"),
		LinkTokenAudience:  c.LinkTokenAudience,
		LinkTokenSecret:    c.LinkTokenSecret,
		ServiceToken:       c.ServiceToken,
		Comment:            c.Comment,
		SetStatusOnMerge:   c.SetStatusOnMerge,
		CompletionOnMerge: multicaprojection.CompletionOnMergeConfig{
			Enabled: c.CompletionOnMerge.Enabled,
			Mode:    c.CompletionOnMerge.Mode,
		},
		Repos:      repos,
		Workspaces: workspaces,
	}
}

func (w MulticaWorkspaceRef) toProjectionWorkspace(defaultWorkspace string) multicaprojection.WorkspaceConfig {
	workspace := w.Workspace
	if strings.TrimSpace(workspace) == "" {
		workspace = defaultWorkspace
	}
	return multicaprojection.WorkspaceConfig{
		Workspace:   workspace,
		WorkspaceID: w.WorkspaceID,
		Profile:     w.Profile,
	}
}

func validDelegatedMergeMethod(method string) bool {
	switch method {
	case "merge", "rebase", "rebase-merge", "squash", "fast-forward-only":
		return true
	default:
		return false
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// LoadFile reads an integrations YAML file.
func LoadFile(path string) (Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Config{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read integrations config: %w", err)
	}
	var retiredSurface struct {
		Multica map[string]any `yaml:"multica"`
	}
	if err := yaml.Unmarshal(data, &retiredSurface); err != nil {
		return Config{}, fmt.Errorf("decode integrations config: %w", err)
	}
	for _, key := range []string{"delegated_pr_merge_enabled", "workload_assertion"} {
		if _, present := retiredSurface.Multica[key]; present {
			return Config{}, fmt.Errorf("multica.%s is retired; Access Grant authority is required", key)
		}
	}
	_, inlineMulticaServiceTokenConfigured := retiredSurface.Multica["service_token"]
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode integrations config: %w", err)
	}
	// CI is a new explicit contract: misspelled selectors must not quietly
	// revert a repository to native execution while appearing configured.
	var sections map[string]yaml.Node
	if err := yaml.Unmarshal(data, &sections); err != nil {
		return Config{}, fmt.Errorf("invalid integrations document")
	}
	if node, exists := sections["ci"]; exists {
		encoded, err := yaml.Marshal(&node)
		if err != nil {
			return Config{}, fmt.Errorf("invalid CI configuration")
		}
		decoder := yaml.NewDecoder(strings.NewReader(string(encoded)))
		decoder.KnownFields(true)
		if err := decoder.Decode(&cfg.CI); err != nil {
			return Config{}, fmt.Errorf("CI configuration has unknown or invalid fields")
		}
	}
	if err := cibackend.Validate(cfg.CI); err != nil {
		return Config{}, err
	}
	if _, err := parseOptionalDuration("forgejo.push_timeout", cfg.Forgejo.PushTimeout); err != nil {
		return Config{}, err
	}
	if cfg.Forgejo.AuthorityPolicy.Enabled {
		if strings.TrimSpace(cfg.Forgejo.AuthorityPolicy.WebhookURL) == "" {
			return Config{}, fmt.Errorf("forgejo.authority_policy.webhook_url is required when authority policy is enabled")
		}
		if strings.TrimSpace(cfg.Forgejo.AuthorityPolicy.IntegrationBot) == "" {
			return Config{}, fmt.Errorf("forgejo.authority_policy.integration_bot is required when authority policy is enabled")
		}
		if strings.TrimSpace(cfg.Forgejo.AuthorityPolicy.OperatorToken) == "" && strings.TrimSpace(cfg.Forgejo.AuthorityPolicy.OperatorTokenFile) == "" {
			return Config{}, fmt.Errorf("forgejo.authority_policy.operator_token_file is required when authority policy is enabled")
		}
	}
	if strings.TrimSpace(cfg.Forgejo.ActionsLogBridgeURL) != "" {
		if strings.TrimSpace(cfg.Forgejo.ActionsLogBridgeTokenFile) == "" {
			return Config{}, fmt.Errorf("forgejo.actions_log_bridge_token_file is required when actions_log_bridge_url is configured")
		}
		secret, err := readOwnerOnlySecretFile(cfg.Forgejo.ActionsLogBridgeTokenFile)
		if err != nil {
			return Config{}, fmt.Errorf("read Forgejo actions log bridge token file: %w", err)
		}
		cfg.Forgejo.ActionsLogBridgeToken = secret
	}
	seenActionActors := map[string]uint{}
	for actor, principalID := range cfg.Forgejo.AuthorityPolicy.ActionPrincipalBindings {
		cleanActor := strings.ToLower(strings.TrimSpace(actor))
		if cleanActor == "" || len(cleanActor) > 255 || strings.ContainsAny(cleanActor, "/\\\t\r\n ") ||
			sessionauthority.IsSecretShapedValue(cleanActor) || principalID == 0 {
			return Config{}, fmt.Errorf("forgejo.authority_policy.action_principal_bindings contains an invalid actor-to-principal binding")
		}
		if previous, ok := seenActionActors[cleanActor]; ok && previous != principalID {
			return Config{}, fmt.Errorf("forgejo.authority_policy.action_principal_bindings contains an ambiguous Forgejo actor")
		}
		seenActionActors[cleanActor] = principalID
	}
	cfg.Forgejo.AuthorityPolicy.ActionPrincipalBindings = seenActionActors
	if strings.TrimSpace(cfg.Multica.LinkTokenSecret) == "" && strings.TrimSpace(cfg.Multica.LinkTokenSecretFile) != "" {
		secret, err := readTrimmedSecretFile(cfg.Multica.LinkTokenSecretFile)
		if err != nil {
			return Config{}, fmt.Errorf("read multica link token secret file: %w", err)
		}
		cfg.Multica.LinkTokenSecret = secret
	}
	if cfg.Multica.ExternalPRDelivery.Enabled {
		if !inlineMulticaServiceTokenConfigured && strings.TrimSpace(cfg.Multica.ServiceTokenFile) != "" {
			secret, err := readOwnerOnlySecretFile(cfg.Multica.ServiceTokenFile)
			if err != nil {
				return Config{}, fmt.Errorf("read typed multica service token file: %w", err)
			}
			cfg.Multica.ServiceToken = secret
		}
	} else if strings.TrimSpace(cfg.Multica.ServiceToken) == "" && strings.TrimSpace(cfg.Multica.ServiceTokenFile) != "" {
		secret, err := readTrimmedSecretFile(cfg.Multica.ServiceTokenFile)
		if err != nil {
			return Config{}, fmt.Errorf("read multica service token file: %w", err)
		}
		cfg.Multica.ServiceToken = secret
	}
	if cfg.Multica.ExternalPRDelivery.Enabled {
		if !cfg.Multica.Enabled {
			return Config{}, fmt.Errorf("multica.external_pr_delivery requires multica.enabled")
		}
		provider := strings.TrimSpace(strings.ToLower(cfg.Multica.ExternalPRProvider))
		if provider != "" && provider != "ags" {
			return Config{}, fmt.Errorf("multica.external_pr_provider must be ags when external_pr_delivery is enabled")
		}
		cfg.Multica.ExternalPRProvider = provider
		if strings.TrimSpace(cfg.Multica.ServerURL) == "" {
			return Config{}, fmt.Errorf("multica.server_url is required for external_pr_delivery")
		}
		if inlineMulticaServiceTokenConfigured {
			return Config{}, fmt.Errorf("multica.external_pr_delivery accepts service_token_file only; inline service_token is rejected")
		}
		if strings.TrimSpace(cfg.Multica.ServiceTokenFile) == "" {
			return Config{}, fmt.Errorf("multica.service_token_file is required for external_pr_delivery")
		}
		if strings.TrimSpace(cfg.Multica.TargetInstance) == "" {
			return Config{}, fmt.Errorf("multica.target_instance is required for external_pr_delivery")
		}
		if strings.TrimSpace(cfg.Multica.ServiceToken) == "" {
			return Config{}, fmt.Errorf("multica.service_token_file must contain a non-empty token for external_pr_delivery")
		}
		timeout, err := parseOptionalDuration("multica.external_pr_delivery.timeout", cfg.Multica.ExternalPRDelivery.Timeout)
		if err != nil {
			return Config{}, err
		}
		if timeout >= maxTypedOutboundDispatchTimeout {
			return Config{}, fmt.Errorf("multica.external_pr_delivery.timeout must be less than %s", maxTypedOutboundDispatchTimeout)
		}
	}
	if target := strings.TrimSpace(cfg.Multica.TargetInstance); target != "" && !targetInstancePattern.MatchString(target) {
		return Config{}, fmt.Errorf("multica target_instance is invalid")
	}
	for repo, mapping := range cfg.Forgejo.Repos {
		if method := strings.TrimSpace(mapping.DelegatedMergeMethod); method != "" && !validDelegatedMergeMethod(method) {
			return Config{}, fmt.Errorf("forgejo repository %q delegated_merge_method is invalid", repo)
		}
	}
	executionContext, err := executioncontext.NormalizeConfig(cfg.ExecutionContext)
	if err != nil {
		return Config{}, fmt.Errorf("invalid execution context config: %w", err)
	}
	cfg.ExecutionContext = executionContext
	delegationPolicies, err := delegationpolicy.New(cfg.Delegation)
	if err != nil {
		return Config{}, fmt.Errorf("invalid delegation config: %w", err)
	}
	cfg.Delegation = delegationPolicies.Config()
	if !sessionAuthorityConfigEmpty(cfg.PrincipalSessionsLegacy) {
		return Config{}, fmt.Errorf("principal_sessions is retired; migrate bindings to team_authority (AGS-T022 Stage 5 dual-path refusal)")
	}
	principalSessions, err := sessionauthority.New(cfg.TeamAuthority)
	if err != nil {
		return Config{}, fmt.Errorf("invalid team_authority config: %w", err)
	}
	cfg.TeamAuthority = principalSessions.Config()
	cfg.PrincipalSessions = cfg.TeamAuthority
	cfg.PrincipalSessionsLegacy = sessionauthority.Config{}
	return cfg, nil
}

func sessionAuthorityConfigEmpty(input sessionauthority.Config) bool {
	return input.Version == 0 &&
		strings.TrimSpace(input.ContractRevision) == "" &&
		input.TrustedIssuers == nil &&
		input.Bindings == nil &&
		input.TeamBindings == nil &&
		input.PolicyClasses == nil &&
		input.ResourceDefaults == nil &&
		input.Resources == nil
}

func parseOptionalDuration(field, raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		if err == nil {
			err = fmt.Errorf("must be non-negative")
		}
		return 0, fmt.Errorf("invalid %s %q: %w", field, raw, err)
	}
	return d, nil
}

func readTrimmedSecretFile(path string) (string, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func readOwnerOnlySecretFile(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("secret file path is empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("secret file must be a regular file and symlinks are not allowed")
	}
	if perm := info.Mode().Perm(); perm != 0o400 && perm != 0o600 {
		return "", fmt.Errorf("secret file must have owner-only permissions 0400 or 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return "", fmt.Errorf("secret file changed or is not a regular file")
	}
	if perm := openedInfo.Mode().Perm(); perm != 0o400 && perm != 0o600 {
		return "", fmt.Errorf("secret file permissions changed; owner-only permissions 0400 or 0600 are required")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (c GitHubConfig) ToGitHubIntegrationConfig() githubintegration.Config {
	repos := make(map[string]githubintegration.RepoMapping, len(c.Repos))
	for name, repo := range c.Repos {
		repos[name] = githubintegration.RepoMapping{
			Owner:        repo.Owner,
			Repo:         repo.Repo,
			RemoteURL:    repo.RemoteURL,
			TargetBranch: repo.TargetBranch,
			Enabled:      repo.Enabled,
		}
	}
	return githubintegration.Config{
		Enabled:              c.Enabled,
		BaseURL:              c.BaseURL,
		RemoteURL:            c.RemoteURL,
		Token:                c.Token,
		TokenFile:            c.TokenFile,
		MergeAuthority:       c.MergeAuthority,
		MirrorBranchIncludes: c.Mirror.Include,
		MirrorBranchExcludes: c.Mirror.Exclude,
		Repos:                repos,
	}
}

func (c GitLabConfig) ToGitLabIntegrationConfig() gitlabintegration.Config {
	repos := make(map[string]gitlabintegration.RepoMapping, len(c.Repos))
	for name, repo := range c.Repos {
		repos[name] = gitlabintegration.RepoMapping{
			ProjectID:    repo.ProjectID,
			ProjectPath:  repo.ProjectPath,
			TargetBranch: repo.TargetBranch,
			EnvBranches:  repo.EnvBranches,
			Enabled:      repo.Enabled,
			RepoFlowEvidence: gitlabintegration.RepoFlowEvidenceConfig{
				Enabled: repo.RepoFlowEvidence.Enabled,
				Store: gitlabintegration.RepoFlowEvidenceStoreConfig{
					Type:       repo.RepoFlowEvidence.Store.Type,
					Path:       repo.RepoFlowEvidence.Store.Path,
					MaxRecords: repo.RepoFlowEvidence.Store.MaxRecords,
				},
			},
		}
	}
	return gitlabintegration.Config{
		Enabled:              c.Enabled,
		BaseURL:              c.BaseURL,
		Token:                c.Token,
		TokenFile:            c.TokenFile,
		MergeAuthority:       c.MergeAuthority,
		MirrorBranchIncludes: c.Mirror.Include,
		MirrorBranchExcludes: c.Mirror.Exclude,
		Repos:                repos,
	}
}

func (c ForgejoConfig) ToForgejoIntegrationConfig() forgejointegration.Config {
	repos := make(map[string]forgejointegration.RepoMapping, len(c.Repos))
	for name, repo := range c.Repos {
		repos[name] = forgejointegration.RepoMapping{
			Owner:                repo.Owner,
			Repo:                 repo.Repo,
			Enabled:              repo.Enabled,
			BaseBranch:           repo.BaseBranch,
			AutoPR:               repo.AutoPR,
			DelegatedMergeMethod: repo.DelegatedMergeMethod,
			FastForwardAck:       repo.FastForwardAck != nil && *repo.FastForwardAck,
		}
	}
	pushTimeout, _ := parseOptionalDuration("forgejo.push_timeout", c.PushTimeout)
	return forgejointegration.Config{
		Enabled:                  c.Enabled,
		BaseURL:                  c.BaseURL,
		Token:                    c.Token,
		TokenFile:                c.TokenFile,
		WebhookSecret:            c.WebhookSecret,
		WebhookSecretFile:        c.WebhookSecretFile,
		DefaultOwner:             c.DefaultOwner,
		RepoMap:                  repos,
		MirrorBranchIncludes:     c.Mirror.Include,
		MirrorBranchExcludes:     c.Mirror.Exclude,
		PRBranchIncludes:         c.PullRequest.Include,
		PRBranchExcludes:         c.PullRequest.Exclude,
		AutoCreateRepo:           c.AutoCreateRepo,
		AutoPullRequest:          c.AutoPR,
		DefaultBaseBranch:        c.DefaultBaseBranch,
		PrivateRepos:             c.PrivateRepos,
		PushGitConfig:            c.PushGitConfig,
		PushTimeout:              pushTimeout,
		AuthorityPolicyEnabled:   c.AuthorityPolicy.Enabled,
		WebhookURL:               c.AuthorityPolicy.WebhookURL,
		IntegrationBot:           c.AuthorityPolicy.IntegrationBot,
		AuthorityPolicyToken:     c.AuthorityPolicy.OperatorToken,
		AuthorityPolicyTokenFile: c.AuthorityPolicy.OperatorTokenFile,
		ActionPrincipalBindings:  c.AuthorityPolicy.ActionPrincipalBindings,
		ActionsLogDir:            strings.TrimSpace(c.ActionsLogDir),
		ActionsLogBridgeURL:      strings.TrimRight(strings.TrimSpace(c.ActionsLogBridgeURL), "/"),
		ActionsLogBridgeToken:    c.ActionsLogBridgeToken,
	}
}
