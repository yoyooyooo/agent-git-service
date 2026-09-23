package multicaprojection

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Config controls optional projection of accepted AGS/Forgejo facts back into Multica issues.
type Config struct {
	Enabled            bool
	TargetInstance     string
	Command            string
	Profile            string
	Workspace          string
	WorkspaceID        string
	DefaultWorkspace   string
	DefaultWorkspaceID string
	AppURL             string
	Comment            bool
	SetStatusOnMerge   bool
	ServerURL          string
	ExternalPRProvider string
	LinkTokenAudience  string
	LinkTokenSecret    string
	ServiceToken       string
	CompletionOnMerge  CompletionOnMergeConfig
	Repos              map[string]WorkspaceConfig
	Workspaces         map[string]WorkspaceConfig
}

// CompletionOnMergeConfig scopes merge-time issue completion to verified,
// leaf-child-only Multica links.
type CompletionOnMergeConfig struct {
	Enabled bool
	Mode    string
}

// WorkspaceConfig identifies one Multica workspace. Workspace is the human slug;
// WorkspaceID is the CLI-safe UUID used for writes.
type WorkspaceConfig struct {
	Workspace   string
	WorkspaceID string
	Profile     string
}

// IssueRef is the workspace-scoped Multica issue identity used by AGS.
type IssueRef struct {
	Workspace   string
	WorkspaceID string
	IssueID     string
	IssueKey    string
	URL         string
}

// PRLinkClaims is the provider-neutral external-PR token minted by Multica from
// a task-scoped mat_ token. AGS verifies it at PR creation time and persists
// the resulting authoritative PR ↔ issue binding.
type PRLinkClaims struct {
	Workspace        string
	WorkspaceID      string
	IssueID          string
	IssueKey         string
	IssueURL         string
	TaskID           string
	AgentID          string
	AgentName        string
	RunID            string
	TargetProvider   string
	TargetInstance   string
	TargetRepository string
}

// ExternalPRLinkRequest is sent by AGS after an AGS PR state change or an
// external merge resolves to an authoritative PR ↔ Multica issue link.
type ExternalPRLinkRequest struct {
	Provider                string `json:"provider"`
	IssueID                 string `json:"issue_id"`
	WorkspaceID             string `json:"workspace_id"`
	Workspace               string `json:"workspace"`
	IssueKey                string `json:"issue_key"`
	ExternalRepo            string `json:"external_repo"`
	ExternalNumber          int    `json:"external_number"`
	ExternalURL             string `json:"external_url"`
	MergeProvider           string `json:"merge_provider,omitempty"`
	MergeRepo               string `json:"merge_repo,omitempty"`
	MergeNumber             int    `json:"merge_number,omitempty"`
	MergeURL                string `json:"merge_url,omitempty"`
	MergedSHA               string `json:"merged_sha"`
	TargetInstance          string `json:"target_instance,omitempty"`
	CanonicalRepositoryID   string `json:"canonical_repository_id,omitempty"`
	CanonicalRepository     string `json:"canonical_repository,omitempty"`
	ProviderBindingID       string `json:"provider_binding_id,omitempty"`
	ProviderBindingRevision string `json:"provider_binding_revision,omitempty"`
	ProviderRepository      string `json:"provider_repository,omitempty"`
	ExpectedHeadSHA         string `json:"expected_head_sha,omitempty"`
	ExpectedBaseSHA         string `json:"expected_base_sha,omitempty"`
	BaseRef                 string `json:"base_ref,omitempty"`
	DelegatedMergeMethod    string `json:"delegated_merge_method,omitempty"`
	ProjectionFactsRevision string `json:"projection_facts_revision,omitempty"`
	CompletionIntent        bool   `json:"completion_intent"`
	LinkConfidence          string `json:"link_confidence"`
	State                   string `json:"state,omitempty"`
	IdempotencyKey          string `json:"idempotency_key"`
}

// CompleteFromPRResponse describes Multica's atomic completion decision.
type CompleteFromPRResponse struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	IssueID string `json:"issue_id,omitempty"`
}

func (r IssueRef) issueRef() string {
	if r.Workspace != "" && r.IssueKey != "" {
		return r.Workspace + "/" + r.IssueKey
	}
	return r.IssueKey
}

// Projection writes AGS/Forgejo state into Multica through the Multica CLI.
type Projection struct {
	cfg Config
}

// Request is the minimal fact set needed to project a PR state into Multica.
type Request struct {
	IssueKey                 string
	Workspace                string
	WorkspaceID              string
	RepoFullName             string
	AGSPRNumber              int
	AGSPRURL                 string
	HeadBranch               string
	HeadSHA                  string
	ForgejoNumber            int
	ForgejoURL               string
	GitLabProject            string
	GitLabMRNumber           int
	GitLabMRURL              string
	GitHubRepo               string
	GitHubPRNumber           int
	GitHubPRURL              string
	CIState                  string
	CIPolicy                 string
	CIRunURL                 string
	PipelineStatus           string
	MergeState               string
	MergedSHA                string
	MergedAt                 string
	ExternalPRProvider       string
	ExternalPRLinkConfidence string
	ExternalPRLinkSource     string
	BackupState              string
	BackupRemote             string
	BackupSHA                string
	SourceBranchState        string
	SourceBranchDeletedAt    string
	GitLabSourceBranchState  string
	GitHubSourceBranchState  string
	MulticaURL               string
	AllowStatusSet           bool
}

var (
	multicaURLPattern      = regexp.MustCompile(`(?i)https?://[^\s/]+/([a-z0-9][a-z0-9._-]*)/issues/([A-Z][A-Z0-9]{1,9}-\d+)`)
	workspaceMarkerPattern = regexp.MustCompile(`(?i)Multica:\s*([a-z0-9][a-z0-9._-]*)/([A-Z][A-Z0-9]{1,9}-\d+)`)
	legacyMarkerPattern    = regexp.MustCompile(`(?i)Multica:\s*([A-Z][A-Z0-9]{1,9}-\d+)`)
)

// New constructs a Multica projection integration. Disabled config returns nil.
func New(cfg Config) *Projection {
	if !cfg.Enabled {
		return nil
	}
	if strings.TrimSpace(cfg.Command) == "" {
		cfg.Command = "multica"
	}
	if strings.TrimSpace(cfg.Profile) == "" {
		cfg.Profile = "ags-multica-projection"
	}
	if strings.TrimSpace(cfg.AppURL) == "" {
		cfg.AppURL = "https://multica.ai"
	}
	cfg.ServerURL = strings.TrimRight(strings.TrimSpace(cfg.ServerURL), "/")
	cfg.TargetInstance = strings.TrimSpace(cfg.TargetInstance)
	if strings.TrimSpace(cfg.ExternalPRProvider) == "" {
		cfg.ExternalPRProvider = "ags"
	}
	if strings.TrimSpace(cfg.LinkTokenAudience) == "" {
		cfg.LinkTokenAudience = "external-pr-link"
	}
	if strings.TrimSpace(cfg.DefaultWorkspace) == "" {
		cfg.DefaultWorkspace = cfg.Workspace
	}
	if strings.TrimSpace(cfg.DefaultWorkspaceID) == "" {
		cfg.DefaultWorkspaceID = cfg.WorkspaceID
	}
	return &Projection{cfg: cfg}
}

// VerifyPRLinkToken accepts only the purpose-bound external PR link token.
// Workload assertions, assertion key IDs, and assertion-shaped claims are
// retired and fail closed.
func (p *Projection) VerifyPRLinkToken(tokenString string) (PRLinkClaims, error) {
	if p == nil {
		return PRLinkClaims{}, fmt.Errorf("multica projection is not configured")
	}
	tokenString = strings.TrimSpace(tokenString)
	if tokenString == "" {
		return PRLinkClaims{}, fmt.Errorf("multica link token is empty")
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, p.linkTokenVerificationKey, jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil {
		return PRLinkClaims{}, err
	}
	if !token.Valid {
		return PRLinkClaims{}, fmt.Errorf("invalid multica link token")
	}
	if _, exists := token.Header["kid"]; exists {
		return PRLinkClaims{}, fmt.Errorf("multica workload assertion transport is retired")
	}
	if typ, ok := jwtHeaderString(token, "typ"); ok && typ != "JWT" {
		return PRLinkClaims{}, fmt.Errorf("invalid multica link token type")
	}
	if claimString(claims, "purpose") != "" {
		return PRLinkClaims{}, fmt.Errorf("multica workload assertion transport is retired")
	}
	return p.verifyExternalPRLinkClaims(claims)
}

func (p *Projection) linkTokenVerificationKey(token *jwt.Token) (any, error) {
	if token.Method != jwt.SigningMethodHS256 {
		return nil, fmt.Errorf("unexpected signing method %s", token.Method.Alg())
	}
	secret := strings.TrimSpace(p.cfg.LinkTokenSecret)
	if secret == "" {
		return nil, fmt.Errorf("multica link token secret is not configured")
	}
	return []byte(secret), nil
}

func (p *Projection) verifyExternalPRLinkClaims(claims jwt.MapClaims) (PRLinkClaims, error) {
	if err := requireExactAudience(claims, p.cfg.LinkTokenAudience); err != nil {
		return PRLinkClaims{}, err
	}
	if claimString(claims, "source") != "task_token" {
		return PRLinkClaims{}, fmt.Errorf("invalid multica link token source")
	}
	issuedAt, issuedAtErr := claims.GetIssuedAt()
	expiresAt, expiresAtErr := claims.GetExpirationTime()
	if issuedAtErr != nil || issuedAt == nil || expiresAtErr != nil || expiresAt == nil {
		return PRLinkClaims{}, fmt.Errorf("multica link token missing required temporal claims")
	}
	lifetime := expiresAt.Time.Sub(issuedAt.Time)
	if lifetime <= 0 || lifetime > 5*time.Minute {
		return PRLinkClaims{}, fmt.Errorf("multica link token lifetime exceeds protocol limit")
	}
	out := PRLinkClaims{
		Workspace:   claimString(claims, "workspace"),
		WorkspaceID: claimString(claims, "workspace_id"),
		IssueID:     claimString(claims, "issue_id"),
		IssueKey:    strings.ToUpper(claimString(claims, "issue_key")),
		IssueURL:    claimString(claims, "issue_url"),
		TaskID:      claimString(claims, "task_id"),
		AgentID:     claimString(claims, "agent_id"),
		RunID:       claimString(claims, "run_id"),
	}
	if out.Workspace == "" || out.WorkspaceID == "" || out.IssueID == "" || out.IssueKey == "" || out.TaskID == "" || out.AgentID == "" {
		return PRLinkClaims{}, fmt.Errorf("multica link token missing required claims")
	}
	if out.IssueURL == "" {
		out.IssueURL = IssueURL(p.cfg.AppURL, out.Workspace, out.IssueKey)
	}
	return out, nil
}

func requireExactAudience(claims jwt.MapClaims, expected string) error {
	audiences, err := claims.GetAudience()
	if err != nil || len(audiences) != 1 || strings.TrimSpace(audiences[0]) != strings.TrimSpace(expected) {
		return fmt.Errorf("invalid multica token audience")
	}
	return nil
}

func jwtHeaderString(token *jwt.Token, key string) (string, bool) {
	value, ok := token.Header[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(text), true
}

func claimString(claims jwt.MapClaims, key string) string {
	if v, ok := claims[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func (p *Projection) TargetInstance() string {
	if p == nil {
		return ""
	}
	return p.cfg.TargetInstance
}

// CompletionOnMergeEnabled reports whether merge-time completion should call
// Multica's atomic leaf-child completion endpoint.
func (p *Projection) CompletionOnMergeEnabled() bool {
	if p == nil {
		return false
	}
	mode := strings.TrimSpace(strings.ToLower(p.cfg.CompletionOnMerge.Mode))
	return p.cfg.CompletionOnMerge.Enabled && (mode == "" || mode == "leaf_child_only")
}

func (p *Projection) CompletionOnMergeMode() string {
	if p == nil || !p.CompletionOnMergeEnabled() {
		return ""
	}
	mode := strings.TrimSpace(strings.ToLower(p.cfg.CompletionOnMerge.Mode))
	if mode == "" {
		return "leaf_child_only"
	}
	return mode
}

// ExternalPRProvider returns the provider name AGS uses when registering its
// PRs with Multica's provider-neutral external PR integration.
func (p *Projection) ExternalPRProvider() string {
	if p == nil {
		return ""
	}
	provider := strings.Trim(strings.ToLower(strings.TrimSpace(p.cfg.ExternalPRProvider)), "/")
	if provider == "" {
		return "ags"
	}
	return provider
}

// RegisterPullRequestLink sends Multica the current AGS PR state for an
// authoritative external-PR link. Multica uses these rows to block
// auto-completion while another completion-intent PR for the same issue remains
// open.
func (p *Projection) RegisterPullRequestLink(ctx context.Context, req ExternalPRLinkRequest) error {
	if p == nil || strings.TrimSpace(req.IssueID) == "" {
		return nil
	}
	_, err := p.postExternalPRIntegration(ctx, "/api/integrations/external-pr/links", req)
	return err
}

// CompleteFromMerge asks Multica to atomically complete the linked issue if it
// is still an eligible leaf child and no open completion-intent PR remains.
func (p *Projection) CompleteFromMerge(ctx context.Context, req ExternalPRLinkRequest) (CompleteFromPRResponse, error) {
	if p == nil || !p.CompletionOnMergeEnabled() {
		return CompleteFromPRResponse{}, nil
	}
	body, err := p.postExternalPRIntegration(ctx, "/api/integrations/external-pr/complete-from-merge", req)
	if err != nil {
		return CompleteFromPRResponse{}, err
	}
	var out CompleteFromPRResponse
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			return CompleteFromPRResponse{}, err
		}
	}
	return out, nil
}

func (p *Projection) postExternalPRIntegration(ctx context.Context, path string, req ExternalPRLinkRequest) ([]byte, error) {
	if strings.TrimSpace(req.Provider) == "" {
		req.Provider = p.ExternalPRProvider()
	}
	serverURL := strings.TrimRight(strings.TrimSpace(p.cfg.ServerURL), "/")
	if serverURL == "" {
		serverURL = strings.TrimRight(strings.TrimSpace(p.cfg.AppURL), "/")
	}
	if serverURL == "" {
		return nil, fmt.Errorf("multica server URL is not configured")
	}
	serviceToken := strings.TrimSpace(p.cfg.ServiceToken)
	if serviceToken == "" {
		return nil, fmt.Errorf("multica service token is not configured")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+serviceToken)
	if req.IdempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("multica %s status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// ExtractIssueKey finds a Multica issue key from an explicit Multica marker or issue URL.
func ExtractIssueKey(values ...string) string {
	return extractIssueRef(values...).IssueKey
}

// ResolveIssueRef finds a workspace-scoped Multica issue reference from explicit PR markers or issue URLs.
func (p *Projection) ResolveIssueRef(repoFullName string, values ...string) IssueRef {
	if p == nil {
		return IssueRef{}
	}
	ref := extractIssueRef(values...)
	if ref.IssueKey == "" {
		return IssueRef{}
	}
	workspace := p.resolveWorkspace(repoFullName, ref.Workspace)
	if ref.Workspace == "" {
		ref.Workspace = workspace.Workspace
	}
	if ref.WorkspaceID == "" {
		ref.WorkspaceID = workspace.WorkspaceID
	}
	if ref.URL == "" {
		ref.URL = IssueURL(p.cfg.AppURL, ref.Workspace, ref.IssueKey)
	}
	return ref
}

func extractIssueRef(values ...string) IssueRef {
	for _, value := range values {
		text := strings.TrimSpace(value)
		if text == "" {
			continue
		}
		if match := multicaURLPattern.FindStringSubmatch(text); len(match) == 3 {
			workspace := strings.ToLower(match[1])
			issueKey := strings.ToUpper(match[2])
			return IssueRef{Workspace: workspace, IssueKey: issueKey, URL: IssueURL("https://multica.ai", workspace, issueKey)}
		}
	}
	for _, value := range values {
		text := strings.TrimSpace(value)
		if text == "" {
			continue
		}
		if match := workspaceMarkerPattern.FindStringSubmatch(text); len(match) == 3 {
			return IssueRef{Workspace: strings.ToLower(match[1]), IssueKey: strings.ToUpper(match[2])}
		}
		if match := legacyMarkerPattern.FindStringSubmatch(text); len(match) == 2 {
			return IssueRef{IssueKey: strings.ToUpper(match[1])}
		}
	}
	return IssueRef{}
}

func (p *Projection) resolveWorkspace(repoFullName, explicitWorkspace string) WorkspaceConfig {
	explicitWorkspace = strings.Trim(strings.ToLower(strings.TrimSpace(explicitWorkspace)), "/")
	if explicitWorkspace != "" {
		if cfg, ok := p.cfg.Workspaces[explicitWorkspace]; ok {
			if strings.TrimSpace(cfg.Workspace) == "" {
				cfg.Workspace = explicitWorkspace
			}
			return normalizeWorkspaceConfig(cfg)
		}
		if strings.EqualFold(explicitWorkspace, p.cfg.DefaultWorkspace) || strings.EqualFold(explicitWorkspace, p.cfg.Workspace) {
			return normalizeWorkspaceConfig(WorkspaceConfig{Workspace: explicitWorkspace, WorkspaceID: firstNonEmpty(p.cfg.DefaultWorkspaceID, p.cfg.WorkspaceID)})
		}
		return WorkspaceConfig{Workspace: explicitWorkspace}
	}
	if cfg, ok := p.cfg.Repos[strings.TrimSpace(repoFullName)]; ok {
		return normalizeWorkspaceConfig(cfg)
	}
	return normalizeWorkspaceConfig(WorkspaceConfig{Workspace: firstNonEmpty(p.cfg.DefaultWorkspace, p.cfg.Workspace), WorkspaceID: firstNonEmpty(p.cfg.DefaultWorkspaceID, p.cfg.WorkspaceID)})
}

func normalizeWorkspaceConfig(cfg WorkspaceConfig) WorkspaceConfig {
	cfg.Workspace = strings.Trim(strings.ToLower(strings.TrimSpace(cfg.Workspace)), "/")
	cfg.WorkspaceID = strings.TrimSpace(cfg.WorkspaceID)
	cfg.Profile = strings.TrimSpace(cfg.Profile)
	return cfg
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// IssueURL returns the canonical Multica web URL for an issue key.
func (p *Projection) IssueURL(issueKey string) string {
	if p == nil {
		return ""
	}
	return IssueURL(p.cfg.AppURL, firstNonEmpty(p.cfg.DefaultWorkspace, p.cfg.Workspace), issueKey)
}

// AppURL returns the configured Multica web base URL.
func (p *Projection) AppURL() string {
	if p == nil {
		return ""
	}
	return p.cfg.AppURL
}

// IssueURL builds the canonical Multica issue URL.
func IssueURL(appURL, workspace, issueKey string) string {
	appURL = strings.TrimRight(strings.TrimSpace(appURL), "/")
	workspace = strings.Trim(strings.TrimSpace(workspace), "/")
	issueKey = strings.ToUpper(strings.TrimSpace(issueKey))
	if appURL == "" || workspace == "" || issueKey == "" {
		return ""
	}
	return appURL + "/" + url.PathEscape(workspace) + "/issues/" + url.PathEscape(issueKey)
}

// EnrichPullRequestBody appends a durable Multica issue link when the PR contains an explicit Multica marker or issue URL.
func (p *Projection) EnrichPullRequestBody(body string, values ...string) string {
	return p.EnrichPullRequestBodyForRepo("", body, values...)
}

// EnrichPullRequestBodyForRepo appends a durable workspace-scoped Multica issue link.
func (p *Projection) EnrichPullRequestBodyForRepo(repoFullName, body string, values ...string) string {
	if p == nil {
		return body
	}
	issueRef := p.ResolveIssueRef(repoFullName, append([]string{body}, values...)...)
	if issueRef.IssueKey == "" || issueRef.URL == "" || strings.Contains(body, issueRef.URL) {
		return body
	}
	trimmed := strings.TrimSpace(body)
	lines := make([]string, 0, 3)
	if trimmed != "" {
		lines = append(lines, trimmed)
	}
	if !strings.Contains(strings.ToUpper(trimmed), issueRef.IssueKey) {
		marker := issueRef.IssueKey
		if issueRef.Workspace != "" {
			marker = issueRef.Workspace + "/" + issueRef.IssueKey
		}
		lines = append(lines, "Multica: "+marker)
	}
	lines = append(lines, "Multica issue: "+issueRef.URL)
	return strings.Join(lines, "\n\n")
}

// Project writes the supplied facts into Multica metadata only.
//
// It intentionally does not add Multica issue comments: ordinary comments can
// wake issue assignees/agents, while AGS PR/CI/merge projection is bookkeeping.
// Merge-time issue completion is handled through the provider-neutral External
// PR completion endpoint where Multica owns status and parent-stage effects.
func (p *Projection) Project(ctx context.Context, req Request) error {
	if p == nil {
		return nil
	}
	issueKey := strings.ToUpper(strings.TrimSpace(req.IssueKey))
	if issueKey == "" {
		return nil
	}
	req.IssueKey = issueKey
	workspace := p.resolveWorkspace(req.RepoFullName, req.Workspace)
	if req.Workspace == "" {
		req.Workspace = workspace.Workspace
	}
	if req.WorkspaceID == "" {
		req.WorkspaceID = workspace.WorkspaceID
	}
	if req.MulticaURL == "" {
		req.MulticaURL = IssueURL(p.cfg.AppURL, req.Workspace, issueKey)
	}
	if req.WorkspaceID == "" && req.Workspace != "" {
		if err := p.run(ctx, "", "workspace", "switch", req.Workspace); err != nil {
			return fmt.Errorf("switch Multica workspace: %w", err)
		}
	}
	for key, value := range req.metadata() {
		if value == "" {
			continue
		}
		if err := p.run(ctx, req.WorkspaceID, "issue", "metadata", "set", issueKey, "--key", key, "--value", value, "--type", "string", "--output", "json"); err != nil {
			return fmt.Errorf("set Multica metadata %s: %w", key, err)
		}
	}
	// Do not add comments or set issue status through the metadata projection path.
	// Comments can wake agents; merge-time completion is handled by CompleteFromPR,
	// where Multica can atomically enforce authoritative-link and leaf-child-only
	// guards.
	return nil
}

func (p *Projection) run(ctx context.Context, workspaceID string, args ...string) error {
	cmdArgs := make([]string, 0, len(args)+4)
	if profile := strings.TrimSpace(p.cfg.Profile); profile != "" && profile != "-" {
		cmdArgs = append(cmdArgs, "--profile", profile)
	}
	if strings.TrimSpace(workspaceID) != "" {
		cmdArgs = append(cmdArgs, "--workspace-id", strings.TrimSpace(workspaceID))
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, p.cfg.Command, cmdArgs...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w: %s", p.cfg.Command, strings.Join(cmdArgs, " "), err, truncate(string(out), 400))
	}
	return nil
}

func (r Request) metadata() map[string]string {
	metadata := map[string]string{
		"ags_repo":                   r.RepoFullName,
		"ags_pr_number":              numberString(r.AGSPRNumber),
		"multica_workspace":          r.Workspace,
		"multica_workspace_id":       r.WorkspaceID,
		"multica_issue_key":          r.IssueKey,
		"multica_issue_ref":          IssueRef{Workspace: r.Workspace, IssueKey: r.IssueKey}.issueRef(),
		"ags_pr_url":                 r.AGSPRURL,
		"ags_head_branch":            r.HeadBranch,
		"ags_head_sha":               r.HeadSHA,
		"forgejo_pr_number":          numberString(r.ForgejoNumber),
		"forgejo_pr_url":             r.ForgejoURL,
		"gitlab_project":             r.GitLabProject,
		"gitlab_mr_number":           numberString(r.GitLabMRNumber),
		"gitlab_mr_url":              r.GitLabMRURL,
		"github_repo":                r.GitHubRepo,
		"github_pr_number":           numberString(r.GitHubPRNumber),
		"github_pr_url":              r.GitHubPRURL,
		"ci_state":                   r.CIState,
		"ci_policy":                  r.effectiveCIPolicy(),
		"ci_run_url":                 r.CIRunURL,
		"pipeline_status":            r.effectivePipelineStatus(),
		"merge_state":                r.MergeState,
		"merged_sha":                 r.MergedSHA,
		"merged_at":                  r.MergedAt,
		"external_pr_link":           r.effectiveExternalPRLink(),
		"backup_state":               r.BackupState,
		"backup_remote":              r.BackupRemote,
		"backup_sha":                 r.BackupSHA,
		"source_branch_state":        r.SourceBranchState,
		"source_branch_deleted_at":   r.SourceBranchDeletedAt,
		"gitlab_source_branch_state": r.GitLabSourceBranchState,
		"github_source_branch_state": r.GitHubSourceBranchState,
		"multica_issue_url":          r.MulticaURL,
	}
	return metadata
}

func (r Request) effectiveExternalPRLink() string {
	confidence := strings.TrimSpace(r.ExternalPRLinkConfidence)
	if confidence == "" {
		return ""
	}
	provider := strings.TrimSpace(r.ExternalPRProvider)
	if provider == "" {
		provider = "ags"
	}
	parts := []string{provider, confidence}
	if source := strings.TrimSpace(r.ExternalPRLinkSource); source != "" {
		parts = append(parts, source)
	}
	return strings.Join(parts, ":")
}

func (r Request) effectiveCIPolicy() string {
	policy := strings.ToLower(strings.TrimSpace(r.CIPolicy))
	switch policy {
	case "required", "optional", "absent_allowed", "unknown":
		return policy
	}
	switch normalizedCIState(r.CIState) {
	case "not_configured":
		return "absent_allowed"
	case "no_run":
		return "unknown"
	case "":
		return ""
	default:
		return "optional"
	}
}

func (r Request) effectivePipelineStatus() string {
	if status := strings.ToLower(strings.TrimSpace(r.PipelineStatus)); status != "" {
		return status
	}
	mergeState := strings.ToLower(strings.TrimSpace(r.MergeState))
	if mergeState == "merged" {
		return "merged"
	}
	if mergeState == "closed" {
		return "closed"
	}
	ciState := normalizedCIState(r.CIState)
	ciPolicy := r.effectiveCIPolicy()
	switch ciState {
	case "passed":
		return "ci_passed"
	case "failed":
		return "ci_blocked"
	case "pending", "running":
		return "ci_pending"
	case "not_configured", "no_run":
		if ciPolicy == "required" {
			return "ci_blocked"
		}
		return "projection_verified"
	case "canceled", "cancelled":
		return "closed"
	case "":
		if r.ForgejoNumber != 0 || strings.TrimSpace(r.ForgejoURL) != "" || strings.TrimSpace(r.HeadSHA) != "" {
			return "projection_verified"
		}
	}
	return "projection_verified"
}

func normalizedCIState(value string) string {
	state := strings.ToLower(strings.TrimSpace(value))
	switch state {
	case "cancelled":
		return "canceled"
	default:
		return state
	}
}

func (r Request) comment() string {
	lines := []string{fmt.Sprintf("AGS projection updated for %s#%d.", r.RepoFullName, r.AGSPRNumber)}
	if r.AGSPRURL != "" {
		lines = append(lines, "AGS PR: "+r.AGSPRURL)
	}
	if r.ForgejoURL != "" {
		lines = append(lines, "Forgejo PR: "+r.ForgejoURL)
	}
	if r.GitLabMRURL != "" {
		lines = append(lines, "GitLab MR: "+r.GitLabMRURL)
	}
	if r.GitHubPRURL != "" {
		lines = append(lines, "GitHub PR: "+r.GitHubPRURL)
	}
	if r.CIState != "" {
		lines = append(lines, "CI: "+r.CIState)
	}
	if r.MergeState != "" {
		lines = append(lines, "Merge: "+r.MergeState)
	}
	if r.BackupState != "" {
		lines = append(lines, "Backup: "+r.BackupState)
	}
	if r.SourceBranchState != "" {
		lines = append(lines, "Source branch: "+r.SourceBranchState)
	}
	if r.MulticaURL != "" {
		lines = append(lines, "Multica issue: "+r.MulticaURL)
	}
	return strings.Join(lines, "\n")
}

func numberString(value int) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprint(value)
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
