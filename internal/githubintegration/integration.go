package githubintegration

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

	"github.com/ngaut/agent-git-service/internal/attribution"
	"github.com/ngaut/agent-git-service/internal/gittransport"
)

const (
	zeroSHA           = "0000000000000000000000000000000000000000"
	defaultAPIBaseURL = "https://api.github.com"
)

// Config controls the optional Forgejo merge-authority -> GitHub backup/shadow sync.
//
// GitHub is GitLab-class: branch PushRef, merged-SHA PushBackup, and shadow
// pull requests. Forgejo remains merge authority; AGS never merges GitHub PRs.
type Config struct {
	Attribution          attribution.Config
	TokenOwner           string
	Enabled              bool
	BaseURL              string
	RemoteURL            string
	Token                string
	TokenFile            string
	MergeAuthority       string
	MirrorBranchIncludes []string
	MirrorBranchExcludes []string
	Repos                map[string]RepoMapping
}

// RepoMapping maps an AGS repository to a GitHub backup repository.
type RepoMapping struct {
	Owner        string
	Repo         string
	RemoteURL    string
	TargetBranch string
	Enabled      *bool
}

// PushRequest asks the integration to push an exact object to the backup branch.
type PushRequest struct {
	RepoFullName string
	RepoPath     string
	SourceSHA    string
	BaseBranch   string
}

// PushResult describes a successful GitHub backup push.
type PushResult struct {
	TargetRepo   string
	TargetBranch string
	PushedSHA    string
}

// PushRefRequest asks the integration to mirror one AGS branch ref to GitHub.
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

// PushRefResult describes a successful GitHub branch mirror push.
type PushRefResult struct {
	TargetRepo string
	Branch     string
	PushedSHA  string
	Deleted    bool
}

// ShadowPullRequestRequest asks the integration to mirror a PR branch and ensure a shadow PR.
type ShadowPullRequestRequest struct {
	RepoFullName   string
	RepoPath       string
	HeadBranch     string
	HeadSHA        string
	BaseBranch     string
	Title          string
	Body           string
	AGSNumber      int
	ExternalNumber int
	// BeforeWrite is a service-owned final authority check invoked immediately
	// before each GitHub mutation.
	BeforeWrite func(context.Context) error
}

// ShadowPullRequestResult describes an ensured shadow PR.
type ShadowPullRequestResult struct {
	State        string
	TargetRepo   string
	SourceBranch string
	TargetBranch string
	WebURL       string
	Number       int
}

// DeleteSourceBranchRequest asks the integration to delete a mirrored PR source branch.
type DeleteSourceBranchRequest struct {
	RepoFullName string
	RepoPath     string
	HeadBranch   string
}

// DeleteSourceBranchResult describes a deleted GitHub source branch.
type DeleteSourceBranchResult struct {
	TargetRepo   string
	SourceBranch string
}

// CloseShadowPullRequestRequest asks the integration to close/comment shadow PRs after Forgejo merge.
type CloseShadowPullRequestRequest struct {
	RepoFullName   string
	HeadBranch     string
	BaseBranch     string
	PRNumber       int
	MergedSHA      string
	CloseReason    string
	ExternalNumber int
	BeforeWrite    func(context.Context) error
}

// CloseShadowPullRequestResult describes a closed/commented shadow PR.
type CloseShadowPullRequestResult struct {
	TargetRepo   string
	SourceBranch string
	TargetBranch string
	WebURL       string
	Number       int
}

// GitPusher pushes a ref update to GitHub.
type GitPusher func(ctx context.Context, repoPath, remoteURL, refspec string, token string) error

// APIClient is the small GitHub API surface needed by the integration.
type APIClient interface {
	ObservePullRequest(context.Context, string, int, string, string) (PullRequest, bool, error)
	ClosePullRequest(context.Context, string, int) (PullRequest, error)
	EnsurePullRequest(ctx context.Context, repo string, in PullRequestInput) (PullRequest, error)
	ClosePullRequestForBranch(ctx context.Context, repo, sourceBranch, targetBranch, note string) (PullRequest, bool, error)
}

// PullRequestInput describes a GitHub PR ensure request.
type PullRequestInput struct {
	// ExternalNumber pins reconciliation to an already observed PR.
	ExternalNumber int
	SourceBranch   string
	TargetBranch   string
	Title          string
	Body           string
	// BeforeWrite revalidates authority after observation, immediately before
	// the HTTP client performs a create or metadata mutation.
	BeforeWrite func(context.Context) error
}

// PullRequest is the minimal GitHub PR shape used by AGS.
type PullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	Body    string `json:"body"`
}

// Integration mirrors AGS refs and optional shadow PRs to GitHub.
type Integration struct {
	cfg    Config
	push   GitPusher
	client APIClient
}

var errIntegrationDisabled = fmt.Errorf("github integration disabled")

// New constructs a GitHub backup integration.
func New(cfg Config, pusher GitPusher) *Integration {
	return NewWithClient(cfg, pusher, nil)
}

// NewWithClient constructs a GitHub backup integration with an injectable API client.
func NewWithClient(cfg Config, pusher GitPusher, client APIClient) *Integration {
	cfg = cfg.withDefaults()
	if pusher == nil {
		pusher = GitPush
	}
	if client == nil {
		client = NewHTTPClient(cfg.BaseURL, cfg.Token)
		client.(*HTTPClient).attribution = cfg.Attribution
		client.(*HTTPClient).tokenOwner = cfg.TokenOwner
	}
	rawPush := pusher
	pusher = func(ctx context.Context, repoPath, remoteURL, refspec, token string) error {
		err := rawPush(ctx, repoPath, remoteURL, refspec, token)
		status := 0
		if err != nil {
			status = 1
		}
		target, _ := ownerRepoFromRemote(remoteURL)
		attribution.Log(ctx, cfg.Attribution, "github", attribution.PushOp(ctx), target, cfg.TokenOwner, "", "text_only", 0, status, false, "")
		return err
	}
	return &Integration{cfg: cfg, push: pusher, client: client}
}

func (c Config) withDefaults() Config {
	if c.Repos == nil {
		c.Repos = map[string]RepoMapping{}
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		c.BaseURL = defaultAPIBaseURL
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
		return cfg, fmt.Errorf("read github token file: %w", err)
	}
	cfg.Token = strings.TrimSpace(string(data))
	return cfg, nil
}

// MirrorBranchEnabled reports whether a branch should be mirrored to GitHub on AGS push.
func (c Config) MirrorBranchEnabled(branch string) bool {
	return branchAllowed(branch, c.MirrorBranchIncludes, c.MirrorBranchExcludes)
}

// ShadowPullRequestEnabled applies the same mirror branch policy used for refs.
func (i *Integration) ShadowPullRequestEnabled(repo, branch string) bool {
	return i.ShadowPullRequestsConfigured(repo) && i.cfg.MirrorBranchEnabled(branch)
}

func (i *Integration) ShadowPullRequestsConfigured(repo string) bool {
	if i == nil || i.ready() != nil || strings.TrimSpace(i.cfg.Token) == "" {
		return false
	}
	_, ok := i.cfg.targetFor(repo)
	return ok
}

// ObserveShadowPullRequest reads the recorded number, or all states for head/base.
func (i *Integration) ObserveShadowPullRequest(ctx context.Context, repo string, number int, head, base string) (PullRequest, bool, error) {
	mapping, ok := i.cfg.targetFor(repo)
	if !ok || i.ready() != nil {
		return PullRequest{}, false, errIntegrationDisabled
	}
	return i.client.ObservePullRequest(ctx, mapping.targetRepoOrDefault(repo, i.cfg.RemoteURL), number, head, mapping.targetBranchOr(base))
}

// TargetRepoFor returns the GitHub owner/repo for an AGS repo.
func (i *Integration) TargetRepoFor(repoFullName string) (string, bool) {
	if i == nil {
		return "", false
	}
	mapping, ok := i.cfg.targetFor(repoFullName)
	if !ok {
		return "", false
	}
	return mapping.targetRepoOrDefault(repoFullName, i.cfg.RemoteURL), true
}

func (i *Integration) ready() error {
	if i == nil || !i.cfg.Enabled {
		return errIntegrationDisabled
	}
	if authority := strings.TrimSpace(i.cfg.MergeAuthority); authority != "" && !strings.EqualFold(authority, "forgejo") {
		return errIntegrationDisabled
	}
	return nil
}

// PushBackup pushes the exact merged SHA from AGS to the configured GitHub backup branch.
func (i *Integration) PushBackup(ctx context.Context, req PushRequest) (PushResult, bool, error) {
	ctx = attribution.WithPushOp(attribution.WithCoordinates(ctx, req.RepoFullName, attribution.FromContext(ctx).PR), "push_backup")
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return PushResult{}, false, nil
		}
		return PushResult{}, false, err
	}
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.SourceSHA) == "" {
		return PushResult{}, false, fmt.Errorf("github integration: repo path and source SHA are required")
	}
	mapping, ok := i.cfg.targetFor(req.RepoFullName)
	if !ok {
		return PushResult{}, false, nil
	}
	remoteURL, err := i.cfg.authenticatedRemoteURL(mapping)
	if err != nil {
		return PushResult{}, false, err
	}
	targetBranch := mapping.targetBranchOr(req.BaseBranch)
	refspec := fmt.Sprintf("%s:refs/heads/%s", req.SourceSHA, targetBranch)
	if err := i.push(ctx, req.RepoPath, remoteURL, refspec, i.cfg.Token); err != nil {
		return PushResult{}, false, err
	}
	return PushResult{TargetRepo: mapping.targetRepoOrDefault(req.RepoFullName, i.cfg.RemoteURL), TargetBranch: targetBranch, PushedSHA: req.SourceSHA}, true, nil
}

// PushRef mirrors one AGS branch ref to the same branch on GitHub.
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
		return PushRefResult{}, false, fmt.Errorf("github integration: repo path is required")
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
			return PushRefResult{}, false, fmt.Errorf("github integration: source SHA is required")
		}
		force := ""
		if req.Forced {
			force = "+"
		}
		refspec = fmt.Sprintf("%s%s:refs/heads/%s", force, req.SourceSHA, branch)
	}
	if err := runBeforeWrite(ctx, req.BeforeWrite); err != nil {
		return PushRefResult{}, false, err
	}
	if err := i.push(ctx, req.RepoPath, remoteURL, refspec, i.cfg.Token); err != nil {
		return PushRefResult{}, false, err
	}
	return PushRefResult{TargetRepo: mapping.targetRepoOrDefault(req.RepoFullName, i.cfg.RemoteURL), Branch: branch, PushedSHA: req.SourceSHA, Deleted: deleted}, true, nil
}

func runBeforeWrite(ctx context.Context, check func(context.Context) error) error {
	if check == nil {
		return nil
	}
	if err := check(ctx); err != nil {
		return fmt.Errorf("github integration: provider admission denied: %w", err)
	}
	return nil
}

// ArtifactRetirementConfigured reports whether this repository owns a mapped
// GitHub backup/shadow target that must follow an explicit history rewrite.
func (i *Integration) ArtifactRetirementConfigured(repoFullName string) bool {
	if err := i.ready(); err != nil {
		return false
	}
	_, ok := i.cfg.targetFor(repoFullName)
	return ok
}

// RewriteBranchWithLease rewrites one already-existing GitHub backup/shadow
// branch during an operator-approved history migration. It is idempotent and
// refuses any remote head other than the exact accepted old/new identities.
func (i *Integration) RewriteBranchWithLease(ctx context.Context, repoFullName, repoPath, branch, oldSHA, newSHA string) (bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return false, nil
		}
		return false, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, "\x00\r\n") || !fullSHA40(oldSHA) || !fullSHA40(newSHA) {
		return false, fmt.Errorf("github integration: invalid retirement branch rewrite")
	}
	mapping, ok := i.cfg.targetFor(repoFullName)
	if !ok {
		return false, nil
	}
	remoteURL, err := i.cfg.authenticatedRemoteURL(mapping)
	if err != nil {
		return false, err
	}
	ref := "refs/heads/" + branch
	out, err := gittransport.Run(ctx, remoteURL, i.cfg.Token, "-C", repoPath, "ls-remote", remoteURL, ref)
	if err != nil {
		return true, fmt.Errorf("github integration: read remote branch: %w", err)
	}
	actual := ""
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			actual = fields[0]
			break
		}
	}
	if actual == "" {
		return false, nil
	}
	if strings.EqualFold(actual, newSHA) {
		return true, nil
	}
	if !strings.EqualFold(actual, oldSHA) {
		return true, fmt.Errorf("github integration: retirement branch %s drifted from accepted old head", branch)
	}
	lease := "--force-with-lease=" + ref + ":" + oldSHA
	if _, err := gittransport.Run(ctx, remoteURL, i.cfg.Token, "-C", repoPath, "push", lease, remoteURL, ref+":"+ref); err != nil {
		return true, fmt.Errorf("github integration: retirement push failed: %w", err)
	}
	out, err = gittransport.Run(ctx, remoteURL, i.cfg.Token, "-C", repoPath, "ls-remote", remoteURL, ref)
	if err != nil {
		return true, fmt.Errorf("github integration: retirement readback failed: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || !strings.EqualFold(fields[0], newSHA) {
		return true, fmt.Errorf("github integration: retirement branch readback mismatch")
	}
	return true, nil
}

func fullSHA40(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// EnsureShadowPullRequest mirrors a PR branch and creates a GitHub shadow PR if needed.
func (i *Integration) EnsureShadowPullRequest(ctx context.Context, req ShadowPullRequestRequest) (ShadowPullRequestResult, bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return ShadowPullRequestResult{}, false, nil
		}
		return ShadowPullRequestResult{}, false, err
	}
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.HeadSHA) == "" || strings.TrimSpace(req.HeadBranch) == "" {
		return ShadowPullRequestResult{}, false, fmt.Errorf("github integration: repo path, head SHA, and head branch are required")
	}
	if strings.TrimSpace(i.cfg.Token) == "" {
		return ShadowPullRequestResult{}, false, nil
	}
	mapping, ok := i.cfg.targetFor(req.RepoFullName)
	if !ok {
		return ShadowPullRequestResult{}, false, nil
	}
	targetBranch := mapping.targetBranchOr(req.BaseBranch)
	targetRepo := mapping.targetRepoOrDefault(req.RepoFullName, i.cfg.RemoteURL)
	observed, found, err := i.client.ObservePullRequest(ctx, targetRepo, req.ExternalNumber, req.HeadBranch, targetBranch)
	if err != nil {
		return ShadowPullRequestResult{}, false, err
	}
	// A terminal shadow is a historical fact, never an instruction to reopen or
	// create a replacement. AGS remains authoritative and is not edited here.
	if found && (observed.State == "closed" || observed.Merged) {
		return ShadowPullRequestResult{TargetRepo: targetRepo, SourceBranch: req.HeadBranch, TargetBranch: targetBranch, Number: observed.Number, WebURL: observed.HTMLURL, State: "closed"}, true, nil
	}
	remoteURL, err := i.cfg.authenticatedRemoteURL(mapping)
	if err != nil {
		return ShadowPullRequestResult{}, false, err
	}
	refspec := fmt.Sprintf("+%s:refs/heads/%s", req.HeadSHA, req.HeadBranch)
	if err := runBeforeWrite(ctx, req.BeforeWrite); err != nil {
		return ShadowPullRequestResult{}, false, err
	}
	if err := i.push(ctx, req.RepoPath, remoteURL, refspec, i.cfg.Token); err != nil {
		return ShadowPullRequestResult{}, false, err
	}
	if err := runBeforeWrite(ctx, req.BeforeWrite); err != nil {
		return ShadowPullRequestResult{}, false, err
	}
	targetRepo = mapping.targetRepoOrDefault(req.RepoFullName, i.cfg.RemoteURL)
	if strings.TrimSpace(targetRepo) == "" || !strings.Contains(targetRepo, "/") {
		return ShadowPullRequestResult{}, false, fmt.Errorf("github integration: owner/repo is required for pull request API")
	}
	number := 0
	if found {
		number = observed.Number
	}
	pr, err := i.client.EnsurePullRequest(ctx, targetRepo, PullRequestInput{
		ExternalNumber: number,
		SourceBranch:   req.HeadBranch,
		TargetBranch:   targetBranch,
		Title:          firstNonEmpty(req.Title, fmt.Sprintf("%s -> %s", req.HeadBranch, targetBranch)),
		Body:           i.attributedText(ctx, shadowPRBody(req)),
		BeforeWrite:    req.BeforeWrite,
	})
	if err != nil {
		return ShadowPullRequestResult{}, false, err
	}
	return ShadowPullRequestResult{TargetRepo: targetRepo, SourceBranch: req.HeadBranch, TargetBranch: targetBranch, WebURL: pr.HTMLURL, Number: pr.Number, State: firstNonEmpty(pr.State, "open")}, true, nil
}

// DeleteSourceBranch deletes the mirrored PR source branch from GitHub when configured.
func (i *Integration) DeleteSourceBranch(ctx context.Context, req DeleteSourceBranchRequest) (DeleteSourceBranchResult, bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return DeleteSourceBranchResult{}, false, nil
		}
		return DeleteSourceBranchResult{}, false, err
	}
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.HeadBranch) == "" {
		return DeleteSourceBranchResult{}, false, fmt.Errorf("github integration: repo path and head branch are required")
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
	return DeleteSourceBranchResult{TargetRepo: mapping.targetRepoOrDefault(req.RepoFullName, i.cfg.RemoteURL), SourceBranch: req.HeadBranch}, true, nil
}

// CloseShadowPullRequest comments and closes the shadow PR for a merged Forgejo PR.
func (i *Integration) CloseShadowPullRequest(ctx context.Context, req CloseShadowPullRequestRequest) (CloseShadowPullRequestResult, bool, error) {
	if err := i.ready(); err != nil {
		if err == errIntegrationDisabled {
			return CloseShadowPullRequestResult{}, false, nil
		}
		return CloseShadowPullRequestResult{}, false, err
	}
	mapping, ok := i.cfg.targetFor(req.RepoFullName)
	if !ok {
		return CloseShadowPullRequestResult{}, false, nil
	}
	if strings.TrimSpace(i.cfg.Token) == "" {
		return CloseShadowPullRequestResult{}, false, nil
	}
	targetBranch := mapping.targetBranchOr(req.BaseBranch)
	targetRepo := mapping.targetRepoOrDefault(req.RepoFullName, i.cfg.RemoteURL)
	pr, found, err := i.client.ObservePullRequest(ctx, targetRepo, req.ExternalNumber, req.HeadBranch, targetBranch)
	result := CloseShadowPullRequestResult{TargetRepo: targetRepo, SourceBranch: req.HeadBranch, TargetBranch: targetBranch, Number: pr.Number, WebURL: pr.HTMLURL}
	if err != nil {
		return result, false, err
	}
	if !found {
		return result, false, nil
	}
	if pr.State != "closed" && !pr.Merged {
		if err := runBeforeWrite(ctx, req.BeforeWrite); err != nil {
			return result, false, err
		}
		pr, err = i.client.ClosePullRequest(ctx, targetRepo, pr.Number)
		if err != nil {
			return result, false, err
		}
		result.WebURL = firstNonEmpty(pr.HTMLURL, result.WebURL)
	}
	return result, true, nil
}

func shadowPRBody(req ShadowPullRequestRequest) string {
	parts := []string{
		"Shadow PR created by AGS.",
		"",
		"Merge authority: Forgejo.",
		"Do not merge this GitHub PR; it exists for backup and visibility.",
	}
	if req.AGSNumber > 0 {
		parts = append(parts, "", fmt.Sprintf("AGS PR: #%d", req.AGSNumber))
	}
	if body := strings.TrimSpace(req.Body); body != "" {
		parts = append(parts, "", "Original AGS PR body:", "", body)
	}
	return strings.Join(parts, "\n")
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
		return mapping, true
	}
	if len(c.Repos) == 0 {
		return RepoMapping{}, true
	}
	return RepoMapping{}, false
}

func (c Config) authenticatedRemoteURL(mapping RepoMapping) (string, error) {
	raw := strings.TrimSpace(mapping.RemoteURL)
	if raw == "" {
		raw = strings.TrimSpace(c.RemoteURL)
	}
	if raw == "" {
		owner := strings.TrimSpace(mapping.Owner)
		repo := strings.TrimSpace(mapping.Repo)
		if owner == "" || repo == "" {
			return "", fmt.Errorf("github integration: remote_url or owner/repo is required")
		}
		if strings.TrimSpace(c.Token) != "" {
			raw = fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
		} else {
			raw = fmt.Sprintf("git@github.com:%s/%s.git", owner, repo)
		}
	}
	return injectToken(raw, c.Token)
}

func injectToken(remoteURL, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" || !strings.Contains(remoteURL, "://") {
		return remoteURL, nil
	}
	parsed, err := url.Parse(remoteURL)
	if err != nil {
		return "", fmt.Errorf("github integration: invalid remote URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return remoteURL, nil
	}
	parsed.User = url.UserPassword("x-access-token", token)
	return parsed.String(), nil
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

func (m RepoMapping) targetRepoOrDefault(repoFullName, fallbackRemote string) string {
	owner := strings.TrimSpace(m.Owner)
	repo := strings.TrimSpace(m.Repo)
	if owner != "" && repo != "" {
		return owner + "/" + repo
	}
	if parsed, ok := ownerRepoFromRemote(m.RemoteURL); ok {
		return parsed
	}
	if parsed, ok := ownerRepoFromRemote(fallbackRemote); ok {
		return parsed
	}
	return strings.TrimSpace(repoFullName)
}

func ownerRepoFromRemote(remoteURL string) (string, bool) {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return "", false
	}
	raw = strings.TrimSuffix(raw, ".git")
	if strings.HasPrefix(raw, "git@") {
		_, rest, ok := strings.Cut(raw, ":")
		if !ok {
			return "", false
		}
		rest = strings.TrimPrefix(rest, "/")
		if strings.Count(rest, "/") != 1 {
			return "", false
		}
		return rest, true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	pathName := strings.Trim(parsed.Path, "/")
	if strings.Count(pathName, "/") != 1 {
		return "", false
	}
	return pathName, true
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

// GitPush pushes one refspec to a GitHub remote.
func GitPush(ctx context.Context, repoPath, remoteURL, refspec string, token string) error {
	_, err := gittransport.Run(ctx, remoteURL, token, "-C", repoPath, "push", remoteURL, refspec)
	if err != nil {
		return fmt.Errorf("github integration: push: %w", err)
	}
	return nil
}

// HTTPClient calls GitHub's REST API.
type HTTPClient struct {
	attribution attribution.Config
	tokenOwner  string
	baseURL     string
	token       string
	client      *http.Client
}

// NewHTTPClient constructs a GitHub REST client.
func NewHTTPClient(baseURL, token string) *HTTPClient {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = defaultAPIBaseURL
	}
	return &HTTPClient{baseURL: base, token: token, client: &http.Client{Timeout: 15 * time.Second}}
}

// EnsurePullRequest reconciles an exact observed PR, or observes all states
// before creating. An open-only absence never authorizes a replacement shadow.
func (c *HTTPClient) EnsurePullRequest(ctx context.Context, repo string, in PullRequestInput) (PullRequest, error) {
	existing, found, err := c.ObservePullRequest(ctx, repo, in.ExternalNumber, in.SourceBranch, in.TargetBranch)
	if err != nil {
		return PullRequest{}, err
	}
	if found {
		if existing.State == "closed" || existing.Merged {
			return existing, nil
		}
		return c.reconcilePullRequestMetadata(ctx, repo, existing, in)
	}
	if in.ExternalNumber > 0 {
		return PullRequest{}, fmt.Errorf("github integration: observed pull request %d disappeared", in.ExternalNumber)
	}
	body := map[string]any{
		"head":  in.SourceBranch,
		"base":  in.TargetBranch,
		"title": in.Title,
		"body":  in.Body,
	}
	if err := runBeforeWrite(ctx, in.BeforeWrite); err != nil {
		return PullRequest{}, err
	}
	status, response, err := c.request(ctx, http.MethodPost, "/repos/"+repo+"/pulls", body)
	if err != nil {
		return PullRequest{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return PullRequest{}, fmt.Errorf("create pull request returned status %d: %s", status, string(response))
	}
	var pr PullRequest
	if err := json.Unmarshal(response, &pr); err != nil {
		return PullRequest{}, fmt.Errorf("decode pull request: %w", err)
	}
	return pr, nil
}

func (c *HTTPClient) reconcilePullRequestMetadata(ctx context.Context, repo string, existing PullRequest, in PullRequestInput) (PullRequest, error) {
	update := map[string]any{}
	if strings.TrimSpace(in.Title) != "" && in.Title != existing.Title {
		update["title"] = in.Title
	}
	if strings.TrimSpace(in.Body) != "" && in.Body != existing.Body {
		update["body"] = in.Body
	}
	if len(update) == 0 {
		return existing, nil
	}
	if err := runBeforeWrite(ctx, in.BeforeWrite); err != nil {
		return PullRequest{}, err
	}
	status, response, err := c.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, existing.Number), update)
	if err != nil {
		return PullRequest{}, err
	}
	if status < 200 || status >= 300 {
		return PullRequest{}, fmt.Errorf("update pull request returned status %d: %s", status, string(response))
	}
	var pr PullRequest
	if err := json.Unmarshal(response, &pr); err != nil {
		return PullRequest{}, fmt.Errorf("decode pull request: %w", err)
	}
	return pr, nil
}

// ClosePullRequestForBranch comments and closes the first open PR for source/target.
func (c *HTTPClient) ClosePullRequestForBranch(ctx context.Context, repo, sourceBranch, targetBranch, note string) (PullRequest, bool, error) {
	pr, found, err := c.ObservePullRequest(ctx, repo, 0, sourceBranch, targetBranch)
	if err != nil || !found {
		return pr, found, err
	}
	if pr.State == "closed" || pr.Merged {
		return pr, true, nil
	}
	pr, err = c.ClosePullRequest(ctx, repo, pr.Number)
	return pr, err == nil, err
}

// ClosePullRequest only closes; comments are deliberately omitted so an unknown
// PATCH outcome can be retried by readback without duplicate comment noise.
func (c *HTTPClient) ClosePullRequest(ctx context.Context, repo string, number int) (PullRequest, error) {
	status, response, err := c.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), map[string]any{"state": "closed"})
	if err != nil {
		return PullRequest{}, err
	}
	if status < 200 || status >= 300 {
		return PullRequest{}, fmt.Errorf("close pull request returned status %d", status)
	}
	var pr PullRequest
	if err := json.Unmarshal(response, &pr); err != nil {
		return pr, err
	}
	return pr, nil
}

func (c *HTTPClient) ObservePullRequest(ctx context.Context, repo string, number int, head, base string) (PullRequest, bool, error) {
	if number > 0 {
		status, response, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil)
		if err != nil {
			return PullRequest{}, false, err
		}
		if status == http.StatusNotFound {
			return PullRequest{}, false, nil
		}
		if status != http.StatusOK {
			return PullRequest{}, false, fmt.Errorf("get pull request returned status %d", status)
		}
		var pr PullRequest
		if err := json.Unmarshal(response, &pr); err != nil {
			return pr, false, err
		}
		if pr.Number != number || (pr.State != "open" && pr.State != "closed") {
			return PullRequest{}, false, fmt.Errorf("invalid pull request observation")
		}
		return pr, true, nil
	}
	prs, err := c.listPRs(ctx, repo, head, base, "all")
	if err != nil || len(prs) == 0 {
		return PullRequest{}, false, err
	}
	// Prefer an open PR when a branch has been reused; otherwise retain the
	// latest terminal fact (GitHub's default list order is newest first).
	for _, pr := range prs {
		if pr.State == "open" {
			return pr, true, nil
		}
	}
	return prs[0], true, nil
}

func (c *HTTPClient) listOpenPRs(ctx context.Context, repo, sourceBranch, targetBranch string) ([]PullRequest, error) {
	return c.listPRs(ctx, repo, sourceBranch, targetBranch, "open")
}

func (c *HTTPClient) listPRs(ctx context.Context, repo, sourceBranch, targetBranch, state string) ([]PullRequest, error) {
	owner, _, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("github integration: owner/repo is required")
	}
	q := url.Values{}
	q.Set("state", state)
	q.Set("head", owner+":"+sourceBranch)
	q.Set("base", targetBranch)
	status, response, err := c.request(ctx, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("list pull requests returned status %d: %s", status, string(response))
	}
	var prs []PullRequest
	if err := json.Unmarshal(response, &prs); err != nil {
		return nil, fmt.Errorf("decode pull requests: %w", err)
	}
	return prs, nil
}

func (c *HTTPClient) request(ctx context.Context, method, apiPath string, body any) (int, []byte, error) {
	if strings.TrimSpace(c.token) == "" {
		return 0, nil, fmt.Errorf("github integration: token is required for pull request API")
	}
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
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "agent-git-service")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if method != http.MethodGet {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		repo, op, number := githubWriteCoordinates(method, apiPath)
		attribution.Log(ctx, c.attribution, "github", op, repo, c.tokenOwner, "", "text_only", number, status, false, "")
	}
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

func (i *Integration) attributedText(ctx context.Context, text string) string {
	if len(i.cfg.Attribution.Principals) == 0 {
		return text
	}
	return attribution.Mark(text, attribution.FromContext(ctx), "github", i.cfg.Attribution.Principals[attribution.FromContext(ctx).Login].GitHub, "text-only")
}
