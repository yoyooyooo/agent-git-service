package gitlabintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/gittransport"
)

// Config controls the optional Forgejo merge-authority -> GitLab backup/shadow sync.
type Config struct {
	Enabled              bool
	BaseURL              string
	Token                string
	TokenFile            string
	MergeAuthority       string
	MirrorBranchIncludes []string
	MirrorBranchExcludes []string
	Repos                map[string]RepoMapping
}

// RepoMapping maps an AGS repository full name to a GitLab backup project.
type RepoMapping struct {
	ProjectID        string
	ProjectPath      string
	TargetBranch     string
	EnvBranches      map[string]string
	Enabled          *bool
	RepoFlowEvidence RepoFlowEvidenceConfig
}

// EnvProjectionRequest asks the integration to project an AGS env ref to GitLab.
type EnvProjectionRequest struct {
	RepoFullName string
	RepoPath     string
	Env          string
	SourceSHA    string
}

// EnvProjectionResult describes a successful env projection push.
type EnvProjectionResult struct {
	ProjectPath  string
	Env          string
	TargetBranch string
	PushedSHA    string
}

// PushRequest asks the integration to push an exact object to a GitLab branch.
type PushRequest struct {
	RepoFullName string
	RepoPath     string
	SourceSHA    string
	BaseBranch   string
}

// PushResult describes a successful GitLab backup push.
type PushResult struct {
	ProjectPath  string
	TargetBranch string
	PushedSHA    string
}

// PushRefRequest asks the integration to mirror one AGS branch ref to GitLab.
type PushRefRequest struct {
	RepoFullName string
	RepoPath     string
	Ref          string
	SourceSHA    string
	Forced       bool
	Deleted      bool
	// BeforeWrite is a service-owned fresh authority check invoked immediately
	// before the provider push/delete mutation.
	BeforeWrite func(context.Context) error
}

// PushRefResult describes a successful GitLab branch mirror push.
type PushRefResult struct {
	ProjectPath string
	Branch      string
	PushedSHA   string
	Deleted     bool
}

// ShadowMergeRequestRequest asks the integration to mirror a PR branch and ensure a shadow MR.
type ShadowMergeRequestRequest struct {
	RepoFullName string
	RepoPath     string
	HeadBranch   string
	HeadSHA      string
	BaseBranch   string
	Title        string
	Body         string
	AGSNumber    int
	// BeforeWrite is a service-owned final authority check invoked immediately
	// before each GitLab mutation.
	BeforeWrite func(context.Context) error
}

// ShadowMergeRequestResult describes an ensured shadow MR.
type ShadowMergeRequestResult struct {
	ProjectPath  string
	SourceBranch string
	TargetBranch string
	WebURL       string
	IID          int
}

// DeleteSourceBranchRequest asks the integration to delete a mirrored PR source branch.
type DeleteSourceBranchRequest struct {
	RepoFullName string
	RepoPath     string
	HeadBranch   string
}

// DeleteSourceBranchResult describes a deleted GitLab source branch.
type DeleteSourceBranchResult struct {
	ProjectPath  string
	SourceBranch string
}

// CloseShadowMergeRequestRequest asks the integration to close/comment shadow MRs after Forgejo merge.
type CloseShadowMergeRequestRequest struct {
	RepoFullName string
	HeadBranch   string
	BaseBranch   string
	PRNumber     int
	MergedSHA    string
	CloseReason  string
}

// CloseShadowMergeRequestResult describes a closed/commented shadow MR.
type CloseShadowMergeRequestResult struct {
	ProjectPath  string
	SourceBranch string
	TargetBranch string
	WebURL       string
	IID          int
}

// GitPusher pushes a ref update to GitLab.
type GitPusher func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error

// APIClient is the small GitLab API surface needed by the integration.
type APIClient interface {
	EnsureMergeRequest(ctx context.Context, project string, in MergeRequestInput) (MergeRequest, error)
	CloseMergeRequestForBranch(ctx context.Context, project, sourceBranch, targetBranch, note string) (MergeRequest, bool, error)
}

// MergeRequestInput describes a GitLab MR ensure request.
type MergeRequestInput struct {
	SourceBranch string
	TargetBranch string
	Title        string
	Description  string
}

// MergeRequest is the minimal GitLab MR shape used by AGS.
type MergeRequest struct {
	IID         int    `json:"iid"`
	WebURL      string `json:"web_url"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// Integration mirrors Forgejo-authoritative merged branches to GitLab backup projects.
type Integration struct {
	cfg    Config
	push   GitPusher
	client APIClient
}

// New constructs a GitLab backup integration.
func New(cfg Config, pusher GitPusher) *Integration {
	return NewWithClient(cfg, pusher, nil)
}

// NewWithClient constructs a GitLab backup integration with an injectable API client.
func NewWithClient(cfg Config, pusher GitPusher, client APIClient) *Integration {
	cfg = cfg.withDefaults()
	if pusher == nil {
		pusher = GitPush
	}
	if client == nil {
		client = NewHTTPClient(cfg.BaseURL, cfg.Token)
	}
	return &Integration{cfg: cfg, push: pusher, client: client}
}

// MirrorBranchEnabled reports whether a branch should be mirrored to GitLab on AGS push.
func (c Config) MirrorBranchEnabled(branch string) bool {
	return branchAllowed(branch, c.MirrorBranchIncludes, c.MirrorBranchExcludes)
}

func (c Config) withDefaults() Config {
	if c.Repos == nil {
		c.Repos = map[string]RepoMapping{}
	}
	return c
}

// LoadTokenFile loads token_file into token when configured.
func LoadTokenFile(cfg Config) (Config, error) {
	if strings.TrimSpace(cfg.Token) != "" || strings.TrimSpace(cfg.TokenFile) == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return cfg, fmt.Errorf("read gitlab token file: %w", err)
	}
	cfg.Token = strings.TrimSpace(string(data))
	return cfg, nil
}

// ProjectEnv pushes an AGS env/* pointer to the configured GitLab deployment branch.
func (i *Integration) ProjectEnv(ctx context.Context, req EnvProjectionRequest) (EnvProjectionResult, bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return EnvProjectionResult{}, false, nil
		}
		return EnvProjectionResult{}, false, err
	}
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.SourceSHA) == "" || strings.TrimSpace(req.Env) == "" {
		return EnvProjectionResult{}, false, fmt.Errorf("gitlab integration: repo path, env, and source SHA are required")
	}
	mapping, ok := i.cfg.targetFor(req.RepoFullName)
	if !ok {
		return EnvProjectionResult{}, false, nil
	}
	targetBranch := mapping.envBranchOr(req.Env)
	remoteURL, err := i.cfg.authenticatedRemoteURL(mapping)
	if err != nil {
		return EnvProjectionResult{}, false, err
	}
	refspec := fmt.Sprintf("+%s:refs/heads/%s", req.SourceSHA, targetBranch)
	if err := i.push(ctx, req.RepoPath, remoteURL, refspec, i.cfg.Token); err != nil {
		return EnvProjectionResult{}, false, err
	}
	return EnvProjectionResult{ProjectPath: mapping.projectPathOrDefault(req.RepoFullName), Env: req.Env, TargetBranch: targetBranch, PushedSHA: req.SourceSHA}, true, nil
}

// PushBackup pushes the exact merged SHA from AGS to the configured GitLab backup branch.
func (i *Integration) PushBackup(ctx context.Context, req PushRequest) (PushResult, bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return PushResult{}, false, nil
		}
		return PushResult{}, false, err
	}
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.SourceSHA) == "" {
		return PushResult{}, false, fmt.Errorf("gitlab integration: repo path and source SHA are required")
	}
	mapping, ok := i.cfg.targetFor(req.RepoFullName)
	if !ok {
		return PushResult{}, false, nil
	}
	targetBranch := mapping.targetBranchOr(req.BaseBranch)
	remoteURL, err := i.cfg.authenticatedRemoteURL(mapping)
	if err != nil {
		return PushResult{}, false, err
	}
	refspec := fmt.Sprintf("%s:refs/heads/%s", req.SourceSHA, targetBranch)
	if err := i.push(ctx, req.RepoPath, remoteURL, refspec, i.cfg.Token); err != nil {
		return PushResult{}, false, err
	}
	return PushResult{ProjectPath: mapping.projectPathOrDefault(req.RepoFullName), TargetBranch: targetBranch, PushedSHA: req.SourceSHA}, true, nil
}

// PushRef mirrors one AGS branch ref to the same branch in the configured GitLab project.
func (i *Integration) PushRef(ctx context.Context, req PushRefRequest) (PushRefResult, bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return PushRefResult{}, false, nil
		}
		return PushRefResult{}, false, err
	}
	branch, ok := branchFromRef(req.Ref)
	if !ok || !i.cfg.MirrorBranchEnabled(branch) {
		return PushRefResult{}, false, nil
	}
	if strings.TrimSpace(req.RepoPath) == "" {
		return PushRefResult{}, false, fmt.Errorf("gitlab integration: repo path is required")
	}
	mapping, ok := i.cfg.targetFor(req.RepoFullName)
	if !ok {
		return PushRefResult{}, false, nil
	}
	remoteURL, err := i.cfg.authenticatedRemoteURL(mapping)
	if err != nil {
		return PushRefResult{}, false, err
	}
	refspec := ""
	deleted := req.Deleted || strings.TrimSpace(req.SourceSHA) == zeroSHA
	if deleted {
		refspec = fmt.Sprintf(":refs/heads/%s", branch)
	} else {
		if strings.TrimSpace(req.SourceSHA) == "" {
			return PushRefResult{}, false, fmt.Errorf("gitlab integration: source SHA is required")
		}
		force := ""
		if req.Forced {
			force = "+"
		}
		refspec = fmt.Sprintf("%s%s:refs/heads/%s", force, req.SourceSHA, branch)
	}
	if err := runBeforeShadowWrite(ctx, req.BeforeWrite); err != nil {
		return PushRefResult{}, false, err
	}
	if err := i.push(ctx, req.RepoPath, remoteURL, refspec, i.cfg.Token); err != nil {
		return PushRefResult{}, false, err
	}
	return PushRefResult{ProjectPath: mapping.projectPathOrDefault(req.RepoFullName), Branch: branch, PushedSHA: req.SourceSHA, Deleted: deleted}, true, nil
}

func runBeforeShadowWrite(ctx context.Context, check func(context.Context) error) error {
	if check == nil {
		return nil
	}
	if err := check(ctx); err != nil {
		return fmt.Errorf("gitlab integration: provider admission denied: %w", err)
	}
	return nil
}

// EnsureShadowMergeRequest mirrors a PR branch and creates a GitLab shadow MR if needed.
func (i *Integration) EnsureShadowMergeRequest(ctx context.Context, req ShadowMergeRequestRequest) (ShadowMergeRequestResult, bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return ShadowMergeRequestResult{}, false, nil
		}
		return ShadowMergeRequestResult{}, false, err
	}
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.HeadSHA) == "" || strings.TrimSpace(req.HeadBranch) == "" {
		return ShadowMergeRequestResult{}, false, fmt.Errorf("gitlab integration: repo path, head SHA, and head branch are required")
	}
	mapping, ok := i.cfg.targetFor(req.RepoFullName)
	if !ok {
		return ShadowMergeRequestResult{}, false, nil
	}
	targetBranch := mapping.targetBranchOr(req.BaseBranch)
	remoteURL, err := i.cfg.authenticatedRemoteURL(mapping)
	if err != nil {
		return ShadowMergeRequestResult{}, false, err
	}
	refspec := fmt.Sprintf("+%s:refs/heads/%s", req.HeadSHA, req.HeadBranch)
	if err := runBeforeShadowWrite(ctx, req.BeforeWrite); err != nil {
		return ShadowMergeRequestResult{}, false, err
	}
	if err := i.push(ctx, req.RepoPath, remoteURL, refspec, i.cfg.Token); err != nil {
		return ShadowMergeRequestResult{}, false, err
	}
	description := shadowMRDescription(req)
	if err := runBeforeShadowWrite(ctx, req.BeforeWrite); err != nil {
		return ShadowMergeRequestResult{}, false, err
	}
	mr, err := i.client.EnsureMergeRequest(ctx, mapping.apiProject(), MergeRequestInput{
		SourceBranch: req.HeadBranch,
		TargetBranch: targetBranch,
		Title:        firstNonEmpty(req.Title, fmt.Sprintf("%s -> %s", req.HeadBranch, targetBranch)),
		Description:  description,
	})
	if err != nil {
		return ShadowMergeRequestResult{}, false, err
	}
	return ShadowMergeRequestResult{ProjectPath: mapping.projectPathOrDefault(req.RepoFullName), SourceBranch: req.HeadBranch, TargetBranch: targetBranch, WebURL: mr.WebURL, IID: mr.IID}, true, nil
}

// DeleteSourceBranch deletes the mirrored PR source branch from GitLab when configured.
func (i *Integration) DeleteSourceBranch(ctx context.Context, req DeleteSourceBranchRequest) (DeleteSourceBranchResult, bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return DeleteSourceBranchResult{}, false, nil
		}
		return DeleteSourceBranchResult{}, false, err
	}
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.HeadBranch) == "" {
		return DeleteSourceBranchResult{}, false, fmt.Errorf("gitlab integration: repo path and head branch are required")
	}
	mapping, ok := i.cfg.targetFor(req.RepoFullName)
	if !ok {
		return DeleteSourceBranchResult{}, false, nil
	}
	remoteURL, err := i.cfg.authenticatedRemoteURL(mapping)
	if err != nil {
		return DeleteSourceBranchResult{}, false, err
	}
	refspec := fmt.Sprintf(":refs/heads/%s", req.HeadBranch)
	if err := i.push(ctx, req.RepoPath, remoteURL, refspec, i.cfg.Token); err != nil {
		return DeleteSourceBranchResult{}, false, err
	}
	return DeleteSourceBranchResult{ProjectPath: mapping.projectPathOrDefault(req.RepoFullName), SourceBranch: req.HeadBranch}, true, nil
}

// CloseShadowMergeRequest comments and closes the shadow MR for a merged Forgejo PR.
func (i *Integration) CloseShadowMergeRequest(ctx context.Context, req CloseShadowMergeRequestRequest) (CloseShadowMergeRequestResult, bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return CloseShadowMergeRequestResult{}, false, nil
		}
		return CloseShadowMergeRequestResult{}, false, err
	}
	mapping, ok := i.cfg.targetFor(req.RepoFullName)
	if !ok {
		return CloseShadowMergeRequestResult{}, false, nil
	}
	targetBranch := mapping.targetBranchOr(req.BaseBranch)
	reason := strings.TrimSpace(req.CloseReason)
	note := ""
	if reason != "" {
		note = reason
		if strings.TrimSpace(req.MergedSHA) != "" {
			note += fmt.Sprintf(" AGS SHA: %s.", req.MergedSHA)
		}
	} else {
		note = fmt.Sprintf("Closed by AGS after Forgejo PR #%d merged. Forgejo is merge authority; GitLab main is synchronized to `%s` only as backup.", req.PRNumber, req.MergedSHA)
	}
	mr, handled, err := i.client.CloseMergeRequestForBranch(ctx, mapping.apiProject(), req.HeadBranch, targetBranch, note)
	if err != nil || !handled {
		return CloseShadowMergeRequestResult{}, handled, err
	}
	return CloseShadowMergeRequestResult{ProjectPath: mapping.projectPathOrDefault(req.RepoFullName), SourceBranch: req.HeadBranch, TargetBranch: targetBranch, WebURL: mr.WebURL, IID: mr.IID}, true, nil
}

var errIntegrationDisabled = fmt.Errorf("gitlab integration disabled")

func (i *Integration) ready() error {
	if i == nil || !i.cfg.Enabled {
		return errIntegrationDisabled
	}
	if authority := strings.TrimSpace(i.cfg.MergeAuthority); authority != "" && !strings.EqualFold(authority, "forgejo") {
		return errIntegrationDisabled
	}
	if strings.TrimSpace(i.cfg.BaseURL) == "" {
		return fmt.Errorf("gitlab integration: base URL is required")
	}
	if strings.TrimSpace(i.cfg.Token) == "" {
		return fmt.Errorf("gitlab integration: token is required")
	}
	return nil
}

func shadowMRDescription(req ShadowMergeRequestRequest) string {
	parts := []string{
		"Shadow MR created by AGS.",
		"",
		"Merge authority: Forgejo.",
		"Do not merge this GitLab MR; it exists for backup and visibility.",
	}
	if req.AGSNumber > 0 {
		parts = append(parts, "", fmt.Sprintf("AGS PR: #%d", req.AGSNumber))
	}
	if body := strings.TrimSpace(req.Body); body != "" {
		parts = append(parts, "", "Original AGS PR body:", "", body)
	}
	return strings.Join(parts, "\n")
}

// ProjectPathForRepo returns the configured GitLab project path for an AGS repo.
func (i *Integration) ProjectPathForRepo(repoFullName string) (string, bool) {
	if i == nil {
		return "", false
	}
	mapping, ok := i.cfg.targetFor(repoFullName)
	if !ok {
		return "", false
	}
	return mapping.projectPathOrDefault(repoFullName), true
}

func (c Config) targetFor(repoFullName string) (RepoMapping, bool) {
	repoFullName = strings.TrimSpace(repoFullName)
	if repoFullName == "" {
		return RepoMapping{}, false
	}
	if mapping, ok := c.Repos[repoFullName]; ok {
		if mapping.Enabled != nil && !*mapping.Enabled {
			return RepoMapping{}, false
		}
		if strings.TrimSpace(mapping.ProjectPath) == "" && strings.TrimSpace(mapping.ProjectID) == "" {
			mapping.ProjectPath = repoFullName
		}
		return mapping, true
	}
	if len(c.Repos) == 0 {
		return RepoMapping{ProjectPath: repoFullName}, true
	}
	return RepoMapping{}, false
}

func branchAllowed(branch string, includes, excludes []string) bool {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false
	}
	for _, pattern := range excludes {
		if globMatch(pattern, branch) {
			return false
		}
	}
	if len(includes) == 0 {
		return true
	}
	for _, pattern := range includes {
		if globMatch(pattern, branch) {
			return true
		}
	}
	return false
}

func globMatch(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	// Trailing "/" is a prefix: "sync/upstream-resolve/" excludes that
	// branch and every nested ref under it. Go path.Match "*" does not cross "/".
	if strings.HasSuffix(pattern, "/") {
		prefix := strings.TrimSuffix(pattern, "/")
		return value == prefix || strings.HasPrefix(value, pattern)
	}
	matched, err := path.Match(pattern, value)
	return err == nil && matched
}

func branchFromRef(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", false
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	return branch, branch != ""
}

const zeroSHA = "0000000000000000000000000000000000000000"

func (m RepoMapping) envBranchOr(env string) string {
	if m.EnvBranches != nil {
		if branch := strings.TrimSpace(m.EnvBranches[strings.TrimSpace(env)]); branch != "" {
			return branch
		}
	}
	if branch := strings.TrimSpace(env); branch != "" {
		return branch
	}
	return "main"
}

func (m RepoMapping) targetBranchOr(base string) string {
	if branch := strings.TrimSpace(base); branch != "" {
		return branch
	}
	if branch := strings.TrimSpace(m.TargetBranch); branch != "" {
		return branch
	}
	return "main"
}

func (m RepoMapping) projectPathOrDefault(repoFullName string) string {
	if strings.TrimSpace(m.ProjectPath) != "" {
		return strings.TrimSpace(m.ProjectPath)
	}
	return strings.TrimSpace(repoFullName)
}

func (m RepoMapping) apiProject() string {
	if strings.TrimSpace(m.ProjectID) != "" {
		return strings.TrimSpace(m.ProjectID)
	}
	return url.PathEscape(m.projectPathOrDefault(""))
}

func (c Config) authenticatedRemoteURL(mapping RepoMapping) (string, error) {
	projectPath := mapping.projectPathOrDefault("")
	if projectPath == "" {
		return "", fmt.Errorf("gitlab integration: project_path is required for git remote push")
	}
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("gitlab integration: invalid base URL: %w", err)
	}
	base.Path = path.Join(base.Path, projectPath) + ".git"
	// HTTP tokens have no role in a filesystem or SSH transport.
	if base.Scheme == "http" || base.Scheme == "https" {
		base.User = url.UserPassword("oauth2", c.Token)
	}
	return base.String(), nil
}

// GitPush pushes one refspec to a GitLab remote.
func GitPush(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
	_, err := gittransport.Run(ctx, remoteURL, token, "-C", repoPath, "push", remoteURL, refspec)
	if err != nil {
		return fmt.Errorf("gitlab integration: push: %w", err)
	}
	return nil
}

// HTTPClient calls GitLab's REST API.
type HTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPClient constructs a GitLab REST client.
func NewHTTPClient(baseURL, token string) *HTTPClient {
	return &HTTPClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &http.Client{Timeout: 15 * time.Second}}
}

// EnsureMergeRequest creates a GitLab MR unless an open one already exists for source/target.
func (c *HTTPClient) EnsureMergeRequest(ctx context.Context, project string, in MergeRequestInput) (MergeRequest, error) {
	mrs, err := c.listOpenMRs(ctx, project, in.SourceBranch, in.TargetBranch)
	if err != nil {
		return MergeRequest{}, err
	}
	if len(mrs) > 0 {
		return c.reconcileMergeRequestMetadata(ctx, project, mrs[0], in)
	}
	body := map[string]any{
		"source_branch":        in.SourceBranch,
		"target_branch":        in.TargetBranch,
		"title":                in.Title,
		"description":          in.Description,
		"remove_source_branch": false,
	}
	status, response, err := c.request(ctx, http.MethodPost, fmt.Sprintf("/api/v4/projects/%s/merge_requests", project), body)
	if err != nil {
		return MergeRequest{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return MergeRequest{}, fmt.Errorf("create merge request returned status %d: %s", status, string(response))
	}
	var mr MergeRequest
	if err := json.Unmarshal(response, &mr); err != nil {
		return MergeRequest{}, fmt.Errorf("decode merge request: %w", err)
	}
	return mr, nil
}

func (c *HTTPClient) reconcileMergeRequestMetadata(ctx context.Context, project string, existing MergeRequest, in MergeRequestInput) (MergeRequest, error) {
	update := map[string]any{}
	if strings.TrimSpace(in.Title) != "" && in.Title != existing.Title {
		update["title"] = in.Title
	}
	if strings.TrimSpace(in.Description) != "" && in.Description != existing.Description {
		update["description"] = in.Description
	}
	if len(update) == 0 {
		return existing, nil
	}
	status, response, err := c.request(ctx, http.MethodPut, fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d", project, existing.IID), update)
	if err != nil {
		return MergeRequest{}, err
	}
	if status < 200 || status >= 300 {
		return MergeRequest{}, fmt.Errorf("update merge request returned status %d: %s", status, string(response))
	}
	var mr MergeRequest
	if err := json.Unmarshal(response, &mr); err != nil {
		return MergeRequest{}, fmt.Errorf("decode merge request: %w", err)
	}
	return mr, nil
}

// CloseMergeRequestForBranch comments and closes the first open MR for source/target.
func (c *HTTPClient) CloseMergeRequestForBranch(ctx context.Context, project, sourceBranch, targetBranch, note string) (MergeRequest, bool, error) {
	mrs, err := c.listOpenMRs(ctx, project, sourceBranch, targetBranch)
	if err != nil {
		return MergeRequest{}, false, err
	}
	if len(mrs) == 0 {
		return MergeRequest{}, false, nil
	}
	mr := mrs[0]
	if strings.TrimSpace(note) != "" {
		status, response, err := c.request(ctx, http.MethodPost, fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/notes", project, mr.IID), map[string]any{"body": note})
		if err != nil {
			return MergeRequest{}, false, err
		}
		if status < 200 || status >= 300 {
			return MergeRequest{}, false, fmt.Errorf("comment merge request returned status %d: %s", status, string(response))
		}
	}
	status, response, err := c.request(ctx, http.MethodPut, fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d", project, mr.IID), map[string]any{"state_event": "close"})
	if err != nil {
		return MergeRequest{}, false, err
	}
	if status < 200 || status >= 300 {
		return MergeRequest{}, false, fmt.Errorf("close merge request returned status %d: %s", status, string(response))
	}
	var closed MergeRequest
	if err := json.Unmarshal(response, &closed); err == nil && closed.IID != 0 {
		mr = closed
	}
	return mr, true, nil
}

func (c *HTTPClient) listOpenMRs(ctx context.Context, project, sourceBranch, targetBranch string) ([]MergeRequest, error) {
	q := url.Values{}
	q.Set("state", "opened")
	q.Set("source_branch", sourceBranch)
	q.Set("target_branch", targetBranch)
	status, response, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/api/v4/projects/%s/merge_requests?%s", project, q.Encode()), nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("list merge requests returned status %d: %s", status, string(response))
	}
	var mrs []MergeRequest
	if err := json.Unmarshal(response, &mrs); err != nil {
		return nil, fmt.Errorf("decode merge requests: %w", err)
	}
	return mrs, nil
}

func (c *HTTPClient) request(ctx context.Context, method, apiPath string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+apiPath, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("PRIVATE-TOKEN", c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if readErr != nil {
		return resp.StatusCode, nil, readErr
	}
	return resp.StatusCode, data, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func redactToken(s, token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}
