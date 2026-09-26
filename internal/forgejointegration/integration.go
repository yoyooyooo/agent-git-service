package forgejointegration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ngaut/agent-git-service/internal/gittransport"
)

const zeroSHA = "0000000000000000000000000000000000000000"

var defaultPushGitConfig = []string{"core.compression=0", "pack.window=0"}

// Config controls the optional AGS -> Forgejo post-push integration.
type Config struct {
	Enabled              bool
	BaseURL              string
	Token                string
	TokenFile            string
	WebhookSecret        string
	WebhookSecretFile    string
	DefaultOwner         string
	RepoMapFile          string
	RepoMap              map[string]RepoMapping
	BranchIncludes       []string // Deprecated alias used for both mirror and PR policies when split policies are unset.
	BranchExcludes       []string // Deprecated alias used for both mirror and PR policies when split policies are unset.
	MirrorBranchIncludes []string
	MirrorBranchExcludes []string
	PRBranchIncludes     []string
	PRBranchExcludes     []string
	AutoCreateRepo       bool
	AutoPullRequest      bool
	DefaultBaseBranch    string
	PrivateRepos         bool
	// PushGitConfig is applied as repeated `git -c <key=value>` options for
	// Forgejo mirror pushes. Nil uses large-binary-friendly defaults; an empty
	// non-nil slice disables per-command git config overrides.
	PushGitConfig []string
	// PushTimeout optionally bounds only the git push subprocess. A zero value
	// inherits the caller/worker context timeout.
	PushTimeout time.Duration
	// AuthorityPolicyEnabled requires mapped Forgejo repositories to preserve
	// AGS merge authority before workflow actions are considered production-ready.
	AuthorityPolicyEnabled bool
	// WebhookURL is the AGS callback registered by authority-policy onboarding.
	WebhookURL string
	// IntegrationBot is the only Forgejo identity allowed to push protected base refs.
	IntegrationBot string
	// AuthorityPolicyToken/File is a separate operator credential used only for
	// live authority-policy reads. Projection pushes continue to use Token.
	AuthorityPolicyToken     string
	AuthorityPolicyTokenFile string
	// ActionPrincipalBindings maps an exact Forgejo actor login to the immutable
	// AGS principal that the shared evaluator must authorize for label-first
	// workflow actions. There is deliberately no same-login fallback.
	ActionPrincipalBindings map[string]uint
	// ActionsLogDir is the colocated Forgejo Actions log root
	// (`<owner>/<repo>/<shard>/<id>.log.zst`). Empty disables file fallback.
	ActionsLogDir string
	// ActionsLogBridgeURL and token bind a server-owned cross-host read adapter.
	ActionsLogBridgeURL   string
	ActionsLogBridgeToken string
}

// RepoMapping maps an AGS repository full name to a Forgejo repository.
type RepoMapping struct {
	Owner                string `json:"owner"`
	Repo                 string `json:"repo"`
	Enabled              *bool  `json:"enabled"`
	BaseBranch           string `json:"base_branch"`
	AutoPR               *bool  `json:"auto_pr"`
	DelegatedMergeMethod string `json:"delegated_merge_method"`
	// FastForwardAck enables the repo-local merge-channel acknowledgement
	// when the mapped Forgejo target ref already equals the expected head.
	// Default remains off; do not enable globally.
	FastForwardAck bool `json:"fast_forward_ack"`
}

// RefChange describes one pushed ref transition.
type RefChange struct {
	Ref     string
	Before  string
	After   string
	Created bool
	Deleted bool
	Forced  bool
}

// PushEvent is emitted after an AGS git push has updated repository refs.
type PushEvent struct {
	RepoFullName string
	RepoPath     string
	Changes      []RefChange
	// BeforeWrite is a service-owned fresh authority check invoked immediately
	// before every ensure/push/delete provider mutation.
	BeforeWrite func(context.Context) error
}

// PushRequest asks the integration to mirror one ref into Forgejo.
type PushRequest struct {
	RepoPath          string
	RemoteURL         string
	Refspec           string
	Repo              TargetRepo
	BranchName        string
	GitConfig         []string
	Timeout           time.Duration
	ForceWithLeaseRef string
	ForceWithLeaseSHA string
}

// TargetRepo is the resolved Forgejo repository target.
type TargetRepo struct {
	Owner      string
	Repo       string
	BaseBranch string
	AutoPR     bool
	Enabled    bool
}

// RepositorySnapshot is the secret-free provider identity for one mapped repo.
type RepositorySnapshot struct {
	ID               int64
	FullName         string
	HTMLURL          string
	CloneURL         string
	DefaultBranch    string
	DefaultBranchSHA string
}

// PullRequestRequest asks the Forgejo API client to ensure a PR exists.
type PullRequestRequest struct {
	Owner                 string
	Repo                  string
	Head                  string
	Base                  string
	Title                 string
	Body                  string
	SkipMetadataReconcile bool
}

// PullRequestRewriteLease authorizes one historical rewrite of an existing,
// mapped Forgejo PR work branch. ExpectedOldHeadSHA is the durable, full object
// ID accepted during preflight; it must never be inferred from a later read.
type PullRequestRewriteLease struct {
	ExternalRepo       string
	ExternalNumber     int
	ExpectedOldHeadSHA string
}

// PullRequestSyncRequest asks the integration to mirror an AGS PR head branch and ensure its Forgejo PR projection.
type PullRequestSyncRequest struct {
	RepoFullName string
	RepoPath     string
	Head         string
	Base         string
	Title        string
	Body         string
	HeadSHA      string
	RewriteLease *PullRequestRewriteLease
	// BeforeWrite is a service-owned final authority check. The integration
	// invokes it immediately before every provider mutation and never caches a
	// successful result across remote preflight/read phases.
	BeforeWrite func(context.Context) error
}

// PullRequestResult is the minimal Forgejo PR shape returned after ensure.
type PullRequestResult struct {
	Number       int
	URL          string
	ExternalRepo string
	HeadSHA      string
}

// PullRequestSnapshot is the live provider state used by integrity scans.
type PullRequestSnapshot struct {
	Number         int      `json:"number"`
	URL            string   `json:"url"`
	State          string   `json:"state"`
	Merged         bool     `json:"merged"`
	Mergeable      bool     `json:"mergeable"`
	MergeCommitSHA string   `json:"merge_commit_sha"`
	MergeBaseSHA   string   `json:"merge_base_sha"`
	HeadRef        string   `json:"head_ref"`
	HeadSHA        string   `json:"head_sha"`
	BaseRef        string   `json:"base_ref"`
	BaseSHA        string   `json:"base_sha"`
	Body           string   `json:"body"`
	Labels         []string `json:"labels"`
}

type PullRequestMergeRequest struct {
	Method       string
	ExpectedHead string
	Title        string
	Message      string
	DeleteBranch bool
}

type PullRequestMergeResult struct {
	ProviderOutcome string
	Snapshot        PullRequestSnapshot
}

// AutoPullRequestResult describes a Forgejo PR created from an AGS push event.
type AutoPullRequestResult struct {
	RepoFullName string
	ExternalRepo string
	Number       int
	URL          string
	Head         string
	Base         string
	HeadSHA      string
	Title        string
	Body         string
}

// PushResult captures side effects produced while handling an AGS push.
type PushResult struct {
	AutoPullRequests []AutoPullRequestResult
}

// WorkflowRun is the minimal Forgejo Actions task shape AGS needs for CI projection.
type WorkflowRun struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	WorkflowID   string `json:"workflow_id"`
	Status       string `json:"status"`
	Event        string `json:"event"`
	HeadBranch   string `json:"head_branch"`
	HeadSHA      string `json:"head_sha"`
	DisplayTitle string `json:"display_title"`
	HTMLURL      string `json:"html_url"`
	URL          string `json:"url"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type workflowRunLister interface {
	ListWorkflowRuns(ctx context.Context, owner, repo string, limit int) ([]WorkflowRun, error)
}

type workflowRunPageLister interface {
	ListWorkflowRunsPage(ctx context.Context, owner, repo string, page, limit int) ([]WorkflowRun, bool, error)
}

type workflowRunLogger interface {
	GetWorkflowRunLogs(ctx context.Context, owner, repo string, runID int64) ([]byte, error)
}

type pullRequestLister interface {
	ListPullRequests(ctx context.Context, owner, repo, state string) ([]PullRequestSnapshot, error)
}

type pullRequestGetter interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (PullRequestSnapshot, bool, error)
}

type pullRequestMerger interface {
	MergePullRequest(ctx context.Context, owner, repo string, number int, request PullRequestMergeRequest) error
}

type pullRequestLabelClient interface {
	AddIssueLabels(ctx context.Context, owner, repo string, issueNumber int, labels []string) error
	RemoveIssueLabel(ctx context.Context, owner, repo string, issueNumber int, label string) error
	CreateIssueComment(ctx context.Context, owner, repo string, issueNumber int, body string) error
}

type PullRequestComment struct {
	Body string `json:"body"`
}

type pullRequestCommentLister interface {
	ListIssueComments(ctx context.Context, owner, repo string, issueNumber, page, limit int) ([]PullRequestComment, error)
}

type pullRequestLabelLister interface {
	ListIssueLabels(ctx context.Context, owner, repo string, issueNumber int) ([]string, error)
}

type collaboratorPermissionClient interface {
	CollaboratorPermission(ctx context.Context, owner, repo, username string) (string, error)
}

type repositorySnapshotClient interface {
	GetRepository(ctx context.Context, owner, repo string) (RepositorySnapshot, bool, error)
}

// Client is the small Forgejo API surface needed by the integration.
type Client interface {
	EnsureRepository(ctx context.Context, owner, repo string, private bool) error
	EnsurePullRequest(ctx context.Context, in PullRequestRequest) (PullRequestResult, error)
	UpdatePullRequestState(ctx context.Context, owner, repo string, number int, state string) (PullRequestResult, error)
}

// GitPusher mirrors a local bare repo ref to a remote Forgejo ref.
type GitPusher func(ctx context.Context, req PushRequest) error

// GitRemoteRefReader reads a remote Git ref SHA.
type GitRemoteRefReader func(ctx context.Context, repoPath, remoteURL, ref string) (string, error)

// Integration mirrors selected AGS pushes to Forgejo.
type Integration struct {
	cfg             Config
	client          Client
	authorityClient repositoryAuthorityClient
	push            GitPusher
	remoteRef       GitRemoteRefReader
}

// New constructs a Forgejo integration. A nil client or pusher is replaced by the default implementation.
func New(cfg Config, client Client, pusher GitPusher) *Integration {
	return NewWithGitCapabilities(cfg, client, pusher, GitRemoteRefSHA)
}

// NewWithGitCapabilities constructs the adapter with explicit Git capability
// ports. Hermetic service tests use it to model fresh provider ref readback;
// production composition uses New and therefore the real Git implementations.
func NewWithGitCapabilities(cfg Config, client Client, pusher GitPusher, remoteRef GitRemoteRefReader) *Integration {
	cfg = cfg.withDefaults()
	if client == nil {
		client = NewHTTPClient(cfg.BaseURL, cfg.Token)
	}
	var authorityClient repositoryAuthorityClient
	if strings.TrimSpace(cfg.AuthorityPolicyToken) != "" {
		authorityClient = NewHTTPClient(cfg.BaseURL, cfg.AuthorityPolicyToken)
	} else if candidate, ok := client.(repositoryAuthorityClient); ok {
		authorityClient = candidate
	}
	if pusher == nil {
		pusher = GitPush
	}
	if remoteRef == nil {
		remoteRef = GitRemoteRefSHA
	}
	return &Integration{cfg: cfg, client: client, authorityClient: authorityClient, push: pusher, remoteRef: remoteRef}
}

// WorkflowActionsEnabled reports whether signed Forgejo webhook actions can
// mutate AGS/projected PR state. Readiness uses this to require drift alerting.
func (i *Integration) WorkflowActionsEnabled() bool {
	return i != nil && i.cfg.Enabled && strings.TrimSpace(i.cfg.WebhookSecret) != ""
}

func (c Config) withDefaults() Config {
	if c.RepoMap == nil {
		c.RepoMap = map[string]RepoMapping{}
	}
	bindings := make(map[string]uint, len(c.ActionPrincipalBindings))
	ambiguous := map[string]struct{}{}
	for actor, principalID := range c.ActionPrincipalBindings {
		actor = strings.ToLower(strings.TrimSpace(actor))
		if actor == "" || principalID == 0 {
			continue
		}
		if previous, exists := bindings[actor]; exists && previous != principalID {
			delete(bindings, actor)
			ambiguous[actor] = struct{}{}
			continue
		}
		if _, denied := ambiguous[actor]; !denied {
			bindings[actor] = principalID
		}
	}
	c.ActionPrincipalBindings = bindings
	if c.DefaultBaseBranch == "" {
		c.DefaultBaseBranch = "main"
	}
	if c.PushGitConfig == nil {
		c.PushGitConfig = append([]string(nil), defaultPushGitConfig...)
	} else {
		c.PushGitConfig = normalizeGitConfig(c.PushGitConfig)
	}
	return c
}

// ProviderMergeBinding is the immutable server-owned mapping used by exact
// Access Grant merge preflight and external-PR projection metadata.
type ProviderMergeBinding struct {
	TargetInstance          string
	CanonicalRepositoryID   string
	CanonicalRepository     string
	ProviderBindingID       string
	ProviderBindingRevision string
	ProviderRepository      string
	BaseRef                 string
	MergeMethod             string
}

// ProviderMergeBinding derives immutable AGS/provider coordinates from local
// server configuration. Callers cannot select these identities or the merge
// method through the operation request.
func (i *Integration) ProviderMergeBinding(repoFullName string, repositoryID uint, targetInstance string) (ProviderMergeBinding, error) {
	return i.providerMergeBinding(repoFullName, repositoryID, targetInstance, "", false)
}

// ProviderMergeBindingForBase derives the binding from an authoritative AGS
// pull request base. The base must already be admitted to the provider mirror;
// callers cannot use this method to select an unprojected provider branch.
func (i *Integration) ProviderMergeBindingForBase(repoFullName string, repositoryID uint, targetInstance, baseRef string) (ProviderMergeBinding, error) {
	return i.providerMergeBinding(repoFullName, repositoryID, targetInstance, baseRef, true)
}

// ConfiguredMergeMethod reports an explicit repository merge authority choice.
// CI backend selection never changes this policy. A broken configured binding
// must remain an error, not permission to merge locally instead.
func (i *Integration) ConfiguredMergeMethod(repository string) (string, bool) {
	if i == nil { return "", false }
	mapping, ok := i.cfg.RepoMap[strings.TrimSpace(repository)]
	if !ok || strings.TrimSpace(mapping.DelegatedMergeMethod) == "" { return "", false }
	return strings.TrimSpace(mapping.DelegatedMergeMethod), true
}

// FastForwardAckEnabled reports the repo-local acknowledgement path. Default is off.
func (i *Integration) FastForwardAckEnabled(repoFullName string) bool {
	if i == nil || i.cfg.RepoMap == nil {
		return false
	}
	mapping, ok := i.cfg.RepoMap[strings.TrimSpace(repoFullName)]
	return ok && mapping.FastForwardAck
}

func (i *Integration) providerMergeBinding(repoFullName string, repositoryID uint, targetInstance, baseRef string, requireAllowedBase bool) (ProviderMergeBinding, error) {
	if i == nil || repositoryID == 0 || strings.TrimSpace(targetInstance) == "" {
		return ProviderMergeBinding{}, fmt.Errorf("provider merge binding is unavailable")
	}
	repoFullName = strings.TrimSpace(repoFullName)
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil || !target.Enabled {
		return ProviderMergeBinding{}, fmt.Errorf("provider merge repository mapping is unavailable")
	}
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" {
		baseRef = strings.TrimSpace(target.BaseBranch)
	}
	if baseRef == "" || (requireAllowedBase && !i.cfg.MirrorBranchEnabled(baseRef)) {
		return ProviderMergeBinding{}, fmt.Errorf("provider merge base is unavailable")
	}
	mapping, ok := i.cfg.RepoMap[repoFullName]
	method := ""
	if ok {
		method = strings.TrimSpace(mapping.DelegatedMergeMethod)
	}
	switch method {
	case "merge", "rebase", "rebase-merge", "squash", "fast-forward-only":
	default:
		return ProviderMergeBinding{}, fmt.Errorf("provider merge method is not configured")
	}
	providerRepo := target.Owner + "/" + target.Repo
	canonicalID := sha256Identity(struct {
		Kind           string `json:"kind"`
		TargetInstance string `json:"target_instance"`
		RepositoryID   uint   `json:"repository_id"`
		Repository     string `json:"repository"`
	}{"ags.canonical-repository.v1", targetInstance, repositoryID, repoFullName})
	bindingID := sha256Identity(struct {
		Kind                  string `json:"kind"`
		TargetInstance        string `json:"target_instance"`
		CanonicalRepositoryID string `json:"canonical_repository_id"`
		ProviderRepository    string `json:"provider_repository"`
	}{"ags.forgejo-provider-binding.v1", targetInstance, canonicalID, providerRepo})
	revision := sha256Identity(struct {
		Kind                   string `json:"kind"`
		ProviderBindingID      string `json:"provider_binding_id"`
		BaseRef                string `json:"base_ref"`
		MergeMethod            string `json:"merge_method"`
		IntegrationBot         string `json:"integration_bot"`
		AuthorityPolicyEnabled bool   `json:"authority_policy_enabled"`
	}{"ags.forgejo-provider-binding-revision.v1", bindingID, baseRef, method, strings.TrimSpace(i.cfg.IntegrationBot), i.cfg.AuthorityPolicyEnabled})
	return ProviderMergeBinding{TargetInstance: targetInstance, CanonicalRepositoryID: canonicalID, CanonicalRepository: repoFullName,
		ProviderBindingID: bindingID, ProviderBindingRevision: revision, ProviderRepository: providerRepo,
		BaseRef: baseRef, MergeMethod: method}, nil
}

func sha256Identity(value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (i *Integration) ActionPrincipalBinding(actorLogin string) (principalID uint, revision string, ok bool) {
	if i == nil {
		return 0, "", false
	}
	actor := strings.ToLower(strings.TrimSpace(actorLogin))
	principalID, ok = i.cfg.ActionPrincipalBindings[actor]
	if !ok || principalID == 0 {
		return 0, "", false
	}
	encoded, _ := json.Marshal(struct {
		Kind        string `json:"kind"`
		Actor       string `json:"actor"`
		PrincipalID uint   `json:"principal_id"`
	}{Kind: "forgejo_action_principal_binding.v1", Actor: actor, PrincipalID: principalID})
	digest := sha256.Sum256(encoded)
	return principalID, "sha256:" + hex.EncodeToString(digest[:]), true
}

func (c Config) BranchEnabled(branch string) bool {
	return branchAllowed(branch, c.BranchIncludes, c.BranchExcludes)
}

// MirrorBranchEnabled reports whether a branch should be mirrored to Forgejo.
func (c Config) MirrorBranchEnabled(branch string) bool {
	includes := c.MirrorBranchIncludes
	excludes := c.MirrorBranchExcludes
	if len(includes) == 0 && len(excludes) == 0 {
		includes = c.BranchIncludes
		excludes = c.BranchExcludes
	}
	return branchAllowed(branch, includes, excludes)
}

// PullRequestBranchEnabled reports whether a mirrored branch should also get a Forgejo PR.
func (c Config) PullRequestBranchEnabled(branch string) bool {
	includes := c.PRBranchIncludes
	excludes := c.PRBranchExcludes
	if len(includes) == 0 && len(excludes) == 0 {
		includes = c.BranchIncludes
		excludes = c.BranchExcludes
	}
	return branchAllowed(branch, includes, excludes)
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
	matched, err := path.Match(pattern, value)
	return err == nil && matched
}

// MirrorBranchEnabled reports whether this integration mirrors a branch.
func (i *Integration) MirrorBranchEnabled(branch string) bool {
	if i == nil {
		return false
	}
	return i.cfg.MirrorBranchEnabled(branch)
}

func (i *Integration) pushRequest(repoPath, remoteURL, refspec string, target TargetRepo, branch string) PushRequest {
	return PushRequest{
		RepoPath:   repoPath,
		RemoteURL:  remoteURL,
		Refspec:    refspec,
		Repo:       target,
		BranchName: branch,
		GitConfig:  append([]string(nil), i.cfg.PushGitConfig...),
		Timeout:    i.cfg.PushTimeout,
	}
}

// RemoteURLForRepo returns the authenticated Forgejo Git remote URL for an AGS repo.
func (i *Integration) RemoteURLForRepo(repoFullName string) (string, error) {
	if i == nil {
		return "", fmt.Errorf("forgejo integration is nil")
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return "", err
	}
	return i.cfg.authenticatedRemoteURL(target)
}

// RepositorySnapshot reads the provider-owned identity for a mapped repository.
func (i *Integration) RepositorySnapshot(ctx context.Context, repoFullName string) (RepositorySnapshot, bool, error) {
	if i == nil {
		return RepositorySnapshot{}, false, nil
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return RepositorySnapshot{}, false, err
	}
	client, ok := i.client.(repositorySnapshotClient)
	if !ok {
		return RepositorySnapshot{}, false, nil
	}
	return client.GetRepository(ctx, target.Owner, target.Repo)
}

// RemoteBranchSHA reads a Forgejo branch SHA for a mapped AGS repository.
func (i *Integration) RemoteBranchSHA(ctx context.Context, repoFullName, repoPath, branch string) (string, bool, error) {
	if i == nil || !i.cfg.Enabled || i.remoteRef == nil || strings.TrimSpace(branch) == "" {
		return "", false, nil
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return "", false, err
	}
	if !target.Enabled {
		return "", false, nil
	}
	remoteURL, err := i.cfg.authenticatedRemoteURL(target)
	if err != nil {
		return "", false, err
	}
	sha, err := i.remoteRef(ctx, repoPath, remoteURL, "refs/heads/"+branch)
	if err != nil {
		return "", true, err
	}
	return strings.TrimSpace(sha), true, nil
}

// ValidatePullRequestProjection reports whether this repository requires a
// Forgejo PR projection and rejects heads that the configured mirror/PR policy
// would silently skip.
func (i *Integration) ValidatePullRequestProjection(repoFullName, head, base string) (bool, error) {
	if i == nil || !i.cfg.Enabled {
		return false, nil
	}
	target, ok, err := i.cfg.resolveTarget(strings.TrimSpace(repoFullName))
	if err != nil || !ok || !target.Enabled || !target.AutoPR {
		return false, nil
	}
	head = strings.TrimSpace(head)
	if !i.cfg.PullRequestBranchEnabled(head) {
		return true, fmt.Errorf("forgejo integration: PR branch %s is not pull-request-enabled", head)
	}
	if !i.cfg.MirrorBranchEnabled(head) {
		return true, fmt.Errorf("forgejo integration: PR branch %s is not mirror-enabled", head)
	}
	return true, nil
}

func runBeforeProviderWrite(ctx context.Context, check func(context.Context) error) error {
	if check == nil {
		return nil
	}
	if err := check(ctx); err != nil {
		return fmt.Errorf("forgejo integration: provider admission denied: %w", err)
	}
	return nil
}

// EnsurePullRequest mirrors an AGS PR head branch and ensures its Forgejo PR projection exists.
func (i *Integration) EnsurePullRequest(ctx context.Context, in PullRequestSyncRequest) (PullRequestResult, error) {
	if i == nil || !i.cfg.Enabled {
		return PullRequestResult{}, nil
	}
	if strings.TrimSpace(i.cfg.BaseURL) == "" {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: base URL is required")
	}
	if strings.TrimSpace(i.cfg.Token) == "" {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: token is required")
	}
	head := strings.TrimSpace(in.Head)
	required, err := i.ValidatePullRequestProjection(in.RepoFullName, head, in.Base)
	if err != nil {
		return PullRequestResult{}, err
	}
	if !required {
		return PullRequestResult{}, nil
	}
	target, err := i.cfg.requireTargetFor(in.RepoFullName)
	if err != nil {
		return PullRequestResult{}, err
	}
	base := strings.TrimSpace(in.Base)
	if base == "" {
		base = target.BaseBranch
	}
	if strings.TrimSpace(in.RepoPath) == "" {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: repo path is required to mirror PR branch %s", head)
	}
	if i.cfg.AutoCreateRepo {
		if err := runBeforeProviderWrite(ctx, in.BeforeWrite); err != nil {
			return PullRequestResult{}, err
		}
		if err := i.client.EnsureRepository(ctx, target.Owner, target.Repo, i.cfg.PrivateRepos); err != nil {
			return PullRequestResult{}, fmt.Errorf("forgejo integration: ensure repository %s/%s: %w", target.Owner, target.Repo, err)
		}
	}
	remoteURL, err := i.cfg.authenticatedRemoteURL(target)
	if err != nil {
		return PullRequestResult{}, err
	}
	ref := "refs/heads/" + head
	if in.RewriteLease != nil {
		result, err := i.rewritePullRequestHeadWithLease(ctx, in, target, remoteURL, ref, base)
		if result.ExternalRepo == "" {
			result.ExternalRepo = target.Owner + "/" + target.Repo
		}
		return result, err
	}
	if err := runBeforeProviderWrite(ctx, in.BeforeWrite); err != nil {
		return PullRequestResult{}, err
	}
	if err := i.pushAndVerify(ctx, i.pushRequest(in.RepoPath, remoteURL, mirrorRefspec(ref, false), target, head), ref, in.HeadSHA); err != nil {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: push %s to %s/%s: %w", head, target.Owner, target.Repo, err)
	}
	if err := runBeforeProviderWrite(ctx, in.BeforeWrite); err != nil {
		return PullRequestResult{}, err
	}
	result, err := i.client.EnsurePullRequest(ctx, PullRequestRequest{
		Owner: target.Owner,
		Repo:  target.Repo,
		Head:  head,
		Base:  base,
		Title: in.Title,
		Body:  in.Body,
	})
	if result.ExternalRepo == "" {
		result.ExternalRepo = target.Owner + "/" + target.Repo
	}
	if err != nil {
		return result, err
	}
	if err := verifyProjectedHeadSHA(in.HeadSHA, result.HeadSHA, head); err != nil {
		return result, err
	}
	return result, nil
}

// rewritePullRequestHeadWithLease updates one existing mapped PR work branch
// using the exact old head accepted by service preflight.
func (i *Integration) rewritePullRequestHeadWithLease(ctx context.Context, in PullRequestSyncRequest, target TargetRepo, remoteURL, ref, base string) (PullRequestResult, error) {
	lease := in.RewriteLease
	if lease == nil {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: rewrite lease is required")
	}
	targetRepo := target.Owner + "/" + target.Repo
	if strings.TrimSpace(lease.ExternalRepo) != targetRepo || lease.ExternalNumber <= 0 {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: rewrite lease does not match configured pull request target %s", targetRepo)
	}
	if in.Head == base || in.Head == target.BaseBranch || in.Head == i.cfg.DefaultBaseBranch {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: refusing lease rewrite of base branch %s", in.Head)
	}
	if strings.HasPrefix(in.Head, "refs/") || strings.Contains(in.Head, ":") {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: refusing invalid PR work branch %q", in.Head)
	}
	oldSHA := strings.ToLower(strings.TrimSpace(lease.ExpectedOldHeadSHA))
	newSHA := strings.ToLower(strings.TrimSpace(in.HeadSHA))
	if !isFullGitObjectID(oldSHA) || !isFullGitObjectID(newSHA) {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: lease rewrite requires full old and new object IDs")
	}
	before, ok, err := i.pullRequestSnapshot(ctx, target, lease.ExternalNumber)
	if err != nil {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: inspect mapped pull request #%d before rewrite: %w", lease.ExternalNumber, err)
	}
	if !ok || before.HeadRef != in.Head || before.BaseRef != base || !exactGitSHA(before.HeadSHA, oldSHA) {
		actual := ""
		if ok {
			actual = before.HeadSHA
		}
		return PullRequestResult{}, leaseDriftError(ref, oldSHA, actual, "mapped Forgejo pull request is not at the accepted preflight head")
	}
	actual, err := i.remoteRef(ctx, in.RepoPath, remoteURL, ref)
	if err != nil {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: inspect remote ref before lease rewrite: %w", err)
	}
	if !exactGitSHA(actual, oldSHA) {
		return PullRequestResult{}, leaseDriftError(ref, oldSHA, actual, "Forgejo work branch changed before lease push")
	}
	req := i.pushRequest(in.RepoPath, remoteURL, mirrorRefspec(ref, false), target, in.Head)
	req.ForceWithLeaseRef = ref
	req.ForceWithLeaseSHA = oldSHA
	if err := runBeforeProviderWrite(ctx, in.BeforeWrite); err != nil {
		return PullRequestResult{}, err
	}
	pushErr := i.push(ctx, req)
	if err := i.waitForRemoteRef(ctx, in.RepoPath, remoteURL, ref, newSHA, pushErr); err != nil {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: lease push %s to %s: %w", in.Head, targetRepo, err)
	}
	after, err := i.waitForPullRequestHead(ctx, target, lease.ExternalNumber, in.Head, base, newSHA)
	result := PullRequestResult{
		Number: lease.ExternalNumber, URL: after.URL, ExternalRepo: targetRepo, HeadSHA: after.HeadSHA,
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

func (i *Integration) pullRequestSnapshot(ctx context.Context, target TargetRepo, number int) (PullRequestSnapshot, bool, error) {
	if getter, ok := i.client.(pullRequestGetter); ok {
		return getter.GetPullRequest(ctx, target.Owner, target.Repo, number)
	}
	lister, ok := i.client.(pullRequestLister)
	if !ok {
		return PullRequestSnapshot{}, false, fmt.Errorf("Forgejo client cannot inspect live pull request state")
	}
	rows, err := lister.ListPullRequests(ctx, target.Owner, target.Repo, "open")
	if err != nil {
		return PullRequestSnapshot{}, false, err
	}
	for _, row := range rows {
		if row.Number == number {
			return row, true, nil
		}
	}
	return PullRequestSnapshot{}, false, nil
}

// ConfiguredTargetFullName returns the mapped Forgejo owner/repo for an AGS repo.
// Integrity scans use this read-only identity to fail-closed when a projection
// ExternalRepo does not match the configured target.
func (i *Integration) ConfiguredTargetFullName(repoFullName string) (string, error) {
	if i == nil || !i.cfg.Enabled {
		return "", fmt.Errorf("Forgejo integration is unavailable")
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return "", err
	}
	if !target.Enabled {
		return "", fmt.Errorf("forgejo integration: repository %s target is disabled", repoFullName)
	}
	owner := strings.TrimSpace(target.Owner)
	repo := strings.TrimSpace(target.Repo)
	if owner == "" || repo == "" {
		return "", fmt.Errorf("forgejo integration: repository %s has incomplete target", repoFullName)
	}
	return owner + "/" + repo, nil
}

// ExactPullRequestSnapshot reads one mapped Forgejo PR through the provider's
// exact-number endpoint. Integrity scans use this fail-closed path instead of
// treating another mutable paginated list as confirmation.
func (i *Integration) ExactPullRequestSnapshot(ctx context.Context, repoFullName, externalRepo string, number int) (PullRequestSnapshot, bool, error) {
	if i == nil || !i.cfg.Enabled || number <= 0 {
		return PullRequestSnapshot{}, false, nil
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return PullRequestSnapshot{}, false, err
	}
	if strings.TrimSpace(externalRepo) != target.Owner+"/"+target.Repo {
		return PullRequestSnapshot{}, false, fmt.Errorf("Forgejo projection target %s does not match configured repository %s/%s", strings.TrimSpace(externalRepo), target.Owner, target.Repo)
	}
	getter, ok := i.client.(pullRequestGetter)
	if !ok {
		return PullRequestSnapshot{}, false, fmt.Errorf("Forgejo client cannot inspect an exact pull request")
	}
	return getter.GetPullRequest(ctx, target.Owner, target.Repo, number)
}

func (i *Integration) MergePullRequest(ctx context.Context, repoFullName, externalRepo string, number int, request PullRequestMergeRequest) (PullRequestMergeResult, error) {
	if i == nil || !i.cfg.Enabled || number <= 0 {
		return PullRequestMergeResult{}, fmt.Errorf("Forgejo integration is unavailable")
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return PullRequestMergeResult{}, err
	}
	if strings.TrimSpace(externalRepo) != target.Owner+"/"+target.Repo {
		return PullRequestMergeResult{}, fmt.Errorf("Forgejo projection target %s does not match configured repository %s/%s", strings.TrimSpace(externalRepo), target.Owner, target.Repo)
	}
	merger, ok := i.client.(pullRequestMerger)
	if !ok {
		return PullRequestMergeResult{}, fmt.Errorf("Forgejo client cannot execute a pull request merge")
	}
	mergeErr := merger.MergePullRequest(ctx, target.Owner, target.Repo, number, request)
	delays := []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second}
	var last PullRequestSnapshot
	for _, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return PullRequestMergeResult{}, ctx.Err()
			case <-time.After(delay):
			}
		}
		snapshot, found, err := i.ExactPullRequestSnapshot(ctx, repoFullName, externalRepo, number)
		if err != nil {
			continue
		}
		if found {
			last = snapshot
			if snapshot.Merged && exactGitSHA(snapshot.HeadSHA, request.ExpectedHead) {
				outcome := "confirmed"
				if mergeErr != nil {
					outcome = "reconciled_after_error"
				}
				return PullRequestMergeResult{ProviderOutcome: outcome, Snapshot: snapshot}, nil
			}
		}
	}
	if mergeErr != nil {
		return PullRequestMergeResult{ProviderOutcome: "outcome_unknown", Snapshot: last}, fmt.Errorf("Forgejo merge outcome unknown: %w", mergeErr)
	}
	return PullRequestMergeResult{ProviderOutcome: "outcome_unknown", Snapshot: last}, fmt.Errorf("Forgejo merge outcome is not confirmed at the expected head")
}

// PullRequestSnapshot reads one exactly mapped Forgejo PR without creating or
// updating it. externalRepo must match the configured target, preventing a
// stale or forged durable mapping from authorizing a rewrite in another repo.
func (i *Integration) PullRequestSnapshot(ctx context.Context, repoFullName, externalRepo string, number int) (PullRequestSnapshot, bool, error) {
	if i == nil || !i.cfg.Enabled || number <= 0 {
		return PullRequestSnapshot{}, false, nil
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return PullRequestSnapshot{}, false, err
	}
	if strings.TrimSpace(externalRepo) != target.Owner+"/"+target.Repo {
		return PullRequestSnapshot{}, false, fmt.Errorf("Forgejo projection target %s does not match configured repository %s/%s", strings.TrimSpace(externalRepo), target.Owner, target.Repo)
	}
	return i.pullRequestSnapshot(ctx, target, number)
}

func (i *Integration) waitForPullRequestHead(ctx context.Context, target TargetRepo, number int, head, base, expectedSHA string) (PullRequestSnapshot, error) {
	delays := []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}
	var last PullRequestSnapshot
	for _, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return last, ctx.Err()
			case <-time.After(delay):
			}
		}
		row, ok, err := i.pullRequestSnapshot(ctx, target, number)
		if err != nil {
			return PullRequestSnapshot{}, err
		}
		if !ok {
			continue
		}
		last = row
		if row.HeadRef == head && row.BaseRef == base && exactGitSHA(row.HeadSHA, expectedSHA) {
			return row, nil
		}
	}
	return last, fmt.Errorf("forgejo integration: mapped pull request #%d did not converge to %s (observed head %s)", number, expectedSHA, strings.TrimSpace(last.HeadSHA))
}

// ListPullRequests returns all provider PR pages for one mapped repository.
// The boolean is false when the configured client cannot inspect live PR state.
func (i *Integration) ListPullRequests(ctx context.Context, repoFullName, state string) ([]PullRequestSnapshot, bool, error) {
	if i == nil || !i.cfg.Enabled {
		return nil, false, nil
	}
	lister, ok := i.client.(pullRequestLister)
	if !ok {
		return nil, false, nil
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return nil, true, err
	}
	if !target.Enabled {
		return nil, true, nil
	}
	rows, err := lister.ListPullRequests(ctx, target.Owner, target.Repo, state)
	return rows, true, err
}

// UpdatePullRequestState updates the projected Forgejo PR lifecycle state.
func (i *Integration) UpdatePullRequestState(ctx context.Context, repoFullName string, number int, state string) (PullRequestResult, error) {
	if i == nil || !i.cfg.Enabled || number == 0 {
		return PullRequestResult{}, nil
	}
	if strings.TrimSpace(i.cfg.BaseURL) == "" {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: base URL is required")
	}
	if strings.TrimSpace(i.cfg.Token) == "" {
		return PullRequestResult{}, fmt.Errorf("forgejo integration: token is required")
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return PullRequestResult{}, err
	}
	if !target.Enabled {
		return PullRequestResult{}, nil
	}
	res, err := i.client.UpdatePullRequestState(ctx, target.Owner, target.Repo, number, state)
	if res.ExternalRepo == "" {
		res.ExternalRepo = target.Owner + "/" + target.Repo
	}
	return res, err
}

// WorkflowRunsForPullRequest returns provider CI runs bound to one exact mapped
// pull request, its AGS head ref, and full AGS head SHA. Pull-request runs use
// Forgejo's synthetic #<number> ref; workflow_dispatch reruns use the source
// branch ref. Provider credentials remain inside AGS.
func (i *Integration) WorkflowRunsForPullRequest(ctx context.Context, repoFullName, externalRepo string, prNumber int, headRef, headSHA string, limit int) ([]WorkflowRun, bool, error) {
	if i == nil || !i.cfg.Enabled || prNumber <= 0 {
		return nil, false, nil
	}
	if len(headSHA) != 40 || strings.ToLower(headSHA) != headSHA || !isFullGitObjectID(headSHA) {
		return nil, true, fmt.Errorf("Forgejo workflow run evidence requires a canonical lowercase 40-character head SHA")
	}
	lister, ok := i.client.(workflowRunLister)
	if !ok {
		return nil, false, nil
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return nil, true, err
	}
	if strings.TrimSpace(externalRepo) != target.Owner+"/"+target.Repo {
		return nil, true, fmt.Errorf("Forgejo projection target %s does not match configured repository %s/%s", strings.TrimSpace(externalRepo), target.Owner, target.Repo)
	}
	if !target.Enabled {
		return []WorkflowRun{}, true, nil
	}
	if limit <= 0 {
		limit = 100
	}
	var runs []WorkflowRun
	if pager, ok := i.client.(workflowRunPageLister); ok {
		const maxWorkflowRunPages = 100
		for page := 1; page <= maxWorkflowRunPages; page++ {
			pageRuns, hasMore, err := pager.ListWorkflowRunsPage(ctx, target.Owner, target.Repo, page, limit)
			if err != nil {
				return nil, true, err
			}
			runs = append(runs, pageRuns...)
			if !hasMore {
				break
			}
			if page == maxWorkflowRunPages {
				return nil, true, fmt.Errorf("Forgejo workflow run evidence exceeded the complete pagination bound")
			}
		}
	} else {
		pageRuns, err := lister.ListWorkflowRuns(ctx, target.Owner, target.Repo, limit)
		if err != nil {
			return nil, true, err
		}
		if len(pageRuns) >= limit {
			return nil, true, fmt.Errorf("Forgejo workflow run evidence completeness is unavailable")
		}
		runs = pageRuns
	}
	wantPullRequestRef := fmt.Sprintf("#%d", prNumber)
	wantHeadRef := strings.TrimSpace(headRef)
	if wantHeadRef == "" {
		return nil, true, fmt.Errorf("Forgejo workflow run evidence requires the AGS pull request head ref")
	}
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	matched := make([]WorkflowRun, 0, len(runs))
	for _, run := range runs {
		if strings.ToLower(strings.TrimSpace(run.HeadSHA)) != headSHA {
			continue
		}
		runRef := strings.TrimSpace(run.HeadBranch)
		if runRef != wantPullRequestRef && runRef != wantHeadRef {
			continue
		}
		matched = append(matched, run)
	}
	return matched, true, nil
}

// WorkflowRunLogs returns the provider log text for one exact mapped run.
func (i *Integration) WorkflowRunLogs(ctx context.Context, repoFullName, externalRepo string, prNumber int, headRef, headSHA string, runID int64) ([]byte, bool, error) {
	if i == nil || !i.cfg.Enabled || prNumber <= 0 || runID <= 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(i.cfg.ActionsLogBridgeURL) != "" {
		if len(headSHA) != 40 || strings.ToLower(headSHA) != headSHA || !isFullGitObjectID(headSHA) {
			return nil, true, fmt.Errorf("Forgejo workflow run evidence requires a canonical lowercase 40-character head SHA")
		}
		if strings.TrimSpace(headRef) == "" {
			return nil, true, fmt.Errorf("Forgejo workflow run evidence requires the AGS pull request head ref")
		}
		target, err := i.cfg.requireTargetFor(repoFullName)
		if err != nil {
			return nil, true, err
		}
		if strings.TrimSpace(externalRepo) != target.Owner+"/"+target.Repo {
			return nil, true, fmt.Errorf("Forgejo projection target %s does not match configured repository %s/%s", strings.TrimSpace(externalRepo), target.Owner, target.Repo)
		}
		if !target.Enabled {
			return []byte{}, true, nil
		}
		logs, err := readActionsLogBridge(ctx, i.cfg.ActionsLogBridgeURL, i.cfg.ActionsLogBridgeToken, target.Owner, target.Repo, runID, prNumber, headRef, headSHA)
		return logs, true, err
	}
	logger, ok := i.client.(workflowRunLogger)
	if !ok {
		return nil, false, nil
	}
	runs, supported, err := i.WorkflowRunsForPullRequest(ctx, repoFullName, externalRepo, prNumber, headRef, headSHA, 100)
	if err != nil || !supported {
		return nil, supported, err
	}
	found := false
	for _, run := range runs {
		if run.ID == runID {
			found = true
			break
		}
	}
	if !found {
		return nil, true, fmt.Errorf("Forgejo run %d is not bound to the exact AGS pull request head", runID)
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return nil, true, err
	}
	if logs, found, err := readLocalActionsLog(i.cfg.ActionsLogDir, target.Owner, target.Repo, runID); found {
		return logs, true, err
	}
	logs, err := logger.GetWorkflowRunLogs(ctx, target.Owner, target.Repo, runID)
	if err != nil {
		return nil, true, err
	}
	return logs, true, nil
}

// LatestWorkflowRunForPullRequest returns the newest Forgejo Actions run matching a PR/head SHA.
func (i *Integration) LatestWorkflowRunForPullRequest(ctx context.Context, repoFullName string, prNumber int, headSHA string) (WorkflowRun, bool, error) {
	if i == nil || !i.cfg.Enabled || prNumber == 0 {
		return WorkflowRun{}, false, nil
	}
	lister, ok := i.client.(workflowRunLister)
	if !ok {
		return WorkflowRun{}, false, nil
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return WorkflowRun{}, false, err
	}
	if !target.Enabled {
		return WorkflowRun{}, false, nil
	}
	runs, err := lister.ListWorkflowRuns(ctx, target.Owner, target.Repo, 30)
	if err != nil {
		return WorkflowRun{}, false, err
	}
	wantBranch := fmt.Sprintf("#%d", prNumber)
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	for _, run := range runs {
		if run.HeadBranch != wantBranch && !strings.Contains(run.DisplayTitle, "("+wantBranch+")") {
			continue
		}
		if headSHA != "" && !strings.HasPrefix(strings.ToLower(run.HeadSHA), headSHA) {
			continue
		}
		return run, true, nil
	}
	return WorkflowRun{}, false, nil
}

// VerifyAndParseMergedPullRequestWebhook validates a signed Forgejo webhook and extracts merged PR events.
func (i *Integration) VerifyAndParseMergedPullRequestWebhook(signature string, body []byte) (MergedPullRequestEvent, bool, error) {
	if i == nil {
		return MergedPullRequestEvent{}, false, fmt.Errorf("forgejo integration is nil")
	}
	if err := VerifyWebhookSignature(i.cfg.WebhookSecret, signature, body); err != nil {
		return MergedPullRequestEvent{}, false, err
	}
	return ParseMergedPullRequestWebhook(body)
}

// VerifyAndParseClosedPullRequestWebhook validates a signed Forgejo webhook and extracts closed PR events.
func (i *Integration) VerifyAndParseClosedPullRequestWebhook(signature string, body []byte) (ClosedPullRequestEvent, bool, error) {
	if i == nil {
		return ClosedPullRequestEvent{}, false, fmt.Errorf("forgejo integration is nil")
	}
	if err := VerifyWebhookSignature(i.cfg.WebhookSecret, signature, body); err != nil {
		return ClosedPullRequestEvent{}, false, err
	}
	return ParseClosedPullRequestWebhook(body)
}

// VerifyAndParseDeletedBranchWebhook validates a signed Forgejo webhook and extracts branch-delete events.
func (i *Integration) VerifyAndParseDeletedBranchWebhook(signature string, body []byte) (DeletedBranchEvent, bool, error) {
	if i == nil {
		return DeletedBranchEvent{}, false, fmt.Errorf("forgejo integration is nil")
	}
	if err := VerifyWebhookSignature(i.cfg.WebhookSecret, signature, body); err != nil {
		return DeletedBranchEvent{}, false, err
	}
	return ParseDeletedBranchWebhook(body)
}

// VerifyAndParsePullRequestActionLabelWebhook validates a signed Forgejo webhook and extracts AGS workflow label events.
func (i *Integration) VerifyAndParsePullRequestActionLabelWebhook(signature string, body []byte) (PullRequestActionLabelEvent, bool, error) {
	if i == nil {
		return PullRequestActionLabelEvent{}, false, fmt.Errorf("forgejo integration is nil")
	}
	if err := VerifyWebhookSignature(i.cfg.WebhookSecret, signature, body); err != nil {
		return PullRequestActionLabelEvent{}, false, err
	}
	return ParsePullRequestActionLabelWebhook(body)
}

// PullRequestHasLabel reports whether a label is currently attached to the projected PR.
// The second return value is false when the configured client cannot inspect live labels.
func (i *Integration) PullRequestHasLabel(ctx context.Context, externalRepo string, prNumber int, label string) (bool, bool, error) {
	owner, repo, ok := splitRepoFullName(externalRepo)
	if i == nil || !i.cfg.Enabled || !ok || prNumber == 0 || strings.TrimSpace(label) == "" {
		return false, false, nil
	}
	client, ok := i.client.(pullRequestLabelLister)
	if !ok {
		return false, false, nil
	}
	labels, err := client.ListIssueLabels(ctx, owner, repo, prNumber)
	if err != nil {
		return false, true, err
	}
	for _, current := range labels {
		if strings.TrimSpace(current) == strings.TrimSpace(label) {
			return true, true, nil
		}
	}
	return false, true, nil
}

// ListPullRequestLabels returns the exact current Forgejo PR label set. It is
// deliberately separate from a boolean label probe because action intents bind
// the complete label set, not just the trigger label.
func (i *Integration) ListPullRequestLabels(ctx context.Context, externalRepo string, prNumber int) ([]string, bool, error) {
	owner, repo, ok := splitRepoFullName(externalRepo)
	if i == nil || !i.cfg.Enabled || !ok || prNumber == 0 {
		return nil, false, nil
	}
	client, ok := i.client.(pullRequestLabelLister)
	if !ok {
		return nil, false, nil
	}
	labels, err := client.ListIssueLabels(ctx, owner, repo, prNumber)
	if err != nil {
		return nil, true, err
	}
	return labels, true, nil
}

// AddPullRequestLabels adds Forgejo issue/PR labels on the projected PR.
func (i *Integration) AddPullRequestLabels(ctx context.Context, externalRepo string, prNumber int, labels []string) error {
	owner, repo, ok := splitRepoFullName(externalRepo)
	if i == nil || !i.cfg.Enabled || !ok || prNumber == 0 || len(labels) == 0 {
		return nil
	}
	client, ok := i.client.(pullRequestLabelClient)
	if !ok {
		return fmt.Errorf("forgejo integration client does not support PR labels")
	}
	return client.AddIssueLabels(ctx, owner, repo, prNumber, labels)
}

// RemovePullRequestLabel removes a Forgejo issue/PR label on the projected PR.
func (i *Integration) RemovePullRequestLabel(ctx context.Context, externalRepo string, prNumber int, label string) error {
	owner, repo, ok := splitRepoFullName(externalRepo)
	if i == nil || !i.cfg.Enabled || !ok || prNumber == 0 || strings.TrimSpace(label) == "" {
		return nil
	}
	client, ok := i.client.(pullRequestLabelClient)
	if !ok {
		return fmt.Errorf("forgejo integration client does not support PR labels")
	}
	return client.RemoveIssueLabel(ctx, owner, repo, prNumber, label)
}

// ActorHasWriteAccess reports whether a Forgejo actor has write-or-higher permission on the projected repository.
// Forgejo restricts reads of another collaborator's permission, so a configured
// authority-policy read client is preferred without transferring its authority
// to projection or workflow writes.
func (i *Integration) ActorHasWriteAccess(ctx context.Context, externalRepo, username string) (bool, string, error) {
	owner, repo, ok := splitRepoFullName(externalRepo)
	if i == nil || !i.cfg.Enabled || !ok || strings.TrimSpace(username) == "" {
		return false, "", nil
	}
	client, ok := i.authorityClient.(collaboratorPermissionClient)
	if !ok {
		client, ok = i.client.(collaboratorPermissionClient)
	}
	if !ok {
		return false, "", fmt.Errorf("forgejo integration client does not support collaborator permissions")
	}
	permission, err := client.CollaboratorPermission(ctx, owner, repo, username)
	if err != nil {
		return false, "", err
	}
	return forgejoPermissionAtLeastWrite(permission), permission, nil
}

func forgejoPermissionAtLeastWrite(permission string) bool {
	switch strings.ToLower(strings.TrimSpace(permission)) {
	case "write", "admin", "owner":
		return true
	default:
		return false
	}
}

// CreatePullRequestComment writes a Forgejo issue/PR comment on the projected PR.
func (i *Integration) CreatePullRequestComment(ctx context.Context, externalRepo string, prNumber int, body string) error {
	owner, repo, ok := splitRepoFullName(externalRepo)
	if i == nil || !i.cfg.Enabled || !ok || prNumber == 0 || strings.TrimSpace(body) == "" {
		return nil
	}
	client, ok := i.client.(pullRequestLabelClient)
	if !ok {
		return fmt.Errorf("forgejo integration client does not support PR comments")
	}
	return client.CreateIssueComment(ctx, owner, repo, prNumber, body)
}

// HasPullRequestComment performs provider readback for a deterministic marker.
// It fails closed if the configured client cannot enumerate comments.
func (i *Integration) HasPullRequestComment(ctx context.Context, externalRepo string, prNumber int, marker string) (bool, error) {
	owner, repo, ok := splitRepoFullName(externalRepo)
	if i == nil || !i.cfg.Enabled || !ok || prNumber == 0 || strings.TrimSpace(marker) == "" {
		return false, fmt.Errorf("invalid Forgejo PR comment readback coordinate")
	}
	client, ok := i.client.(pullRequestCommentLister)
	if !ok {
		return false, fmt.Errorf("forgejo integration client does not support PR comment readback")
	}
	const limit = 50
	for page := 1; page <= 1000; page++ {
		comments, err := client.ListIssueComments(ctx, owner, repo, prNumber, page, limit)
		if err != nil {
			return false, err
		}
		for _, comment := range comments {
			if strings.Contains(comment.Body, marker) {
				return true, nil
			}
		}
		if len(comments) < limit {
			return false, nil
		}
	}
	return false, fmt.Errorf("Forgejo PR comment readback exceeded pagination bound")
}

// HandlePush mirrors eligible pushed branches and optionally ensures Forgejo PRs.
func (i *Integration) HandlePush(ctx context.Context, ev PushEvent) error {
	_, err := i.HandlePushWithResult(ctx, ev)
	return err
}

// HandlePushWithResult mirrors eligible pushed branches. Forgejo PR creation is
// intentionally reserved for EnsurePullRequest after an AGS PR exists, keeping
// AGS as the pull-request source of truth.
func (i *Integration) HandlePushWithResult(ctx context.Context, ev PushEvent) (PushResult, error) {
	var out PushResult
	if i == nil || !i.cfg.Enabled {
		return out, nil
	}
	if strings.TrimSpace(i.cfg.BaseURL) == "" {
		return out, fmt.Errorf("forgejo integration: base URL is required")
	}
	if strings.TrimSpace(i.cfg.Token) == "" {
		return out, fmt.Errorf("forgejo integration: token is required")
	}
	type mirrorChange struct {
		change RefChange
		branch string
	}
	mirrorChanges := make([]mirrorChange, 0, len(ev.Changes))
	for _, change := range ev.Changes {
		branch, ok := branchFromRef(change.Ref)
		if !ok || !i.cfg.MirrorBranchEnabled(branch) {
			continue
		}
		mirrorChanges = append(mirrorChanges, mirrorChange{change: change, branch: branch})
	}
	if len(mirrorChanges) == 0 {
		return out, nil
	}
	target, err := i.cfg.requireTargetFor(ev.RepoFullName)
	if err != nil {
		return out, err
	}
	if !target.Enabled {
		return out, nil
	}
	remoteURL, err := i.cfg.authenticatedRemoteURL(target)
	if err != nil {
		return out, err
	}
	for _, item := range mirrorChanges {
		change := item.change
		branch := item.branch
		if branch == target.BaseBranch && (change.Deleted || change.After == zeroSHA) {
			cause := fmt.Errorf("refusing deletion of authoritative base branch %s", branch)
			return out, &ProjectionError{
				Type: ProjectionFailureNonFastForward, Repo: ev.RepoFullName,
				TargetRepo: target.Owner + "/" + target.Repo, Ref: change.Ref,
				Branch: branch, ExpectedSHA: change.Before, ErrorSummary: cause.Error(), Cause: cause,
			}
		}
		if branch == target.BaseBranch && change.Forced {
			cause := fmt.Errorf("refusing forced update of authoritative base branch %s", branch)
			return out, &ProjectionError{
				Type: ProjectionFailureNonFastForward, Repo: ev.RepoFullName,
				TargetRepo: target.Owner + "/" + target.Repo, Ref: change.Ref,
				Branch: branch, ExpectedSHA: change.After, ActualSHA: change.Before,
				ErrorSummary: cause.Error(), Cause: cause,
			}
		}
		if change.Deleted || change.After == zeroSHA {
			if err := runBeforeProviderWrite(ctx, ev.BeforeWrite); err != nil {
				return out, err
			}
			if err := i.push(ctx, i.pushRequest(ev.RepoPath, remoteURL, deleteRefspec(change.Ref), target, branch)); err != nil {
				projectionErr := ClassifyProjectionError(change.Ref, change.After, err)
				projectionErr.Repo = ev.RepoFullName
				projectionErr.TargetRepo = target.Owner + "/" + target.Repo
				projectionErr.Branch = branch
				return out, fmt.Errorf("forgejo integration: delete %s from %s/%s: %w", branch, target.Owner, target.Repo, &projectionErr)
			}
			continue
		}
		if change.After == "" {
			continue
		}
		if i.cfg.AutoCreateRepo {
			if err := runBeforeProviderWrite(ctx, ev.BeforeWrite); err != nil {
				return out, err
			}
			if err := i.client.EnsureRepository(ctx, target.Owner, target.Repo, i.cfg.PrivateRepos); err != nil {
				return out, fmt.Errorf("forgejo integration: ensure repository %s/%s: %w", target.Owner, target.Repo, err)
			}
		}
		refspec := mirrorRefspec(change.Ref, change.Forced)
		if err := runBeforeProviderWrite(ctx, ev.BeforeWrite); err != nil {
			return out, err
		}
		if err := i.push(ctx, i.pushRequest(ev.RepoPath, remoteURL, refspec, target, branch)); err != nil {
			projectionErr := ClassifyProjectionError(change.Ref, change.After, err)
			projectionErr.Repo = ev.RepoFullName
			projectionErr.TargetRepo = target.Owner + "/" + target.Repo
			projectionErr.Branch = branch
			return out, fmt.Errorf("forgejo integration: push %s to %s/%s: %w", branch, target.Owner, target.Repo, &projectionErr)
		}
		// Push events mirror eligible branches only. Creating a Forgejo PR here
		// would bypass AGS PR creation and produce orphan projections.
	}
	return out, nil
}

// ConfiguredRepoFullNames returns explicitly mapped AGS repositories.
func (i *Integration) ConfiguredRepoFullNames() []string {
	if i == nil {
		return nil
	}
	repos := make([]string, 0, len(i.cfg.RepoMap))
	for repo, mapping := range i.cfg.RepoMap {
		if mapping.Enabled != nil && !*mapping.Enabled {
			continue
		}
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}

// ResolveMergedPullRequestRepo maps a signed Forgejo merged-PR event back to an
// AGS repository only when the target repo and branch policies match the AGS
// integration config. This is used as a strict fallback for push-created
// Forgejo PRs that do not have an AGS PR projection row.
func (i *Integration) ResolveMergedPullRequestRepo(event MergedPullRequestEvent) (string, bool) {
	if i == nil || !i.cfg.Enabled {
		return "", false
	}
	externalRepo := strings.TrimSpace(event.RepoFullName)
	head := strings.TrimSpace(event.HeadBranch)
	base := strings.TrimSpace(event.BaseBranch)
	if externalRepo == "" || head == "" || base == "" {
		return "", false
	}
	for repoFullName := range i.cfg.RepoMap {
		target, err := i.cfg.requireTargetFor(repoFullName)
		if err != nil || !target.Enabled {
			continue
		}
		if target.Owner+"/"+target.Repo != externalRepo {
			continue
		}
		if target.BaseBranch != base || !target.AutoPR {
			continue
		}
		if !i.cfg.MirrorBranchEnabled(head) || !i.cfg.PullRequestBranchEnabled(head) {
			continue
		}
		return repoFullName, true
	}
	return "", false
}

func (c Config) targetFor(repoFullName string) (TargetRepo, bool) {
	target, ok, _ := c.resolveTarget(repoFullName)
	return target, ok
}
func (c Config) requireTargetFor(repoFullName string) (TargetRepo, error) {
	target, ok, err := c.resolveTarget(repoFullName)
	if err != nil {
		return TargetRepo{}, err
	}
	if !ok {
		return TargetRepo{}, fmt.Errorf("forgejo integration: repository %s is not mapped", repoFullName)
	}
	return target, nil
}
func (c Config) resolveTarget(repoFullName string) (TargetRepo, bool, error) {
	mapping, mapped := c.RepoMap[repoFullName]
	if mapped && mapping.Enabled != nil && !*mapping.Enabled {
		return TargetRepo{Enabled: false}, true, nil
	}
	sourceOwner := ownerFromFullName(repoFullName)
	sourceRepo := repoNameFromFullName(repoFullName)
	owner := ""
	repo := ""
	if mapped {
		owner = mapping.Owner
		if owner == "" {
			owner = sourceOwner
		}
		repo = mapping.Repo
		if repo == "" {
			repo = sourceRepo
		}
	} else {
		if c.DefaultOwner != "" && sourceOwner != c.DefaultOwner {
			return TargetRepo{}, false, fmt.Errorf("forgejo integration: repository %s needs an explicit Forgejo mapping; default_owner=%s must not decide org-owned repo projection", repoFullName, c.DefaultOwner)
		}
		owner = sourceOwner
		if owner == "" {
			owner = c.DefaultOwner
		}
		repo = sourceRepo
	}
	if owner == "" || repo == "" {
		return TargetRepo{}, false, nil
	}
	base := mapping.BaseBranch
	if base == "" {
		base = c.DefaultBaseBranch
	}
	autoPR := c.AutoPullRequest
	if mapping.AutoPR != nil {
		autoPR = *mapping.AutoPR
	}
	return TargetRepo{Owner: owner, Repo: repo, BaseBranch: base, AutoPR: autoPR, Enabled: true}, true, nil
}

func ownerFromFullName(fullName string) string {
	owner, _, ok := strings.Cut(fullName, "/")
	if !ok {
		return ""
	}
	return owner
}

func repoNameFromFullName(fullName string) string {
	_, repo, ok := strings.Cut(fullName, "/")
	if !ok {
		return ""
	}
	return repo
}

func splitRepoFullName(fullName string) (string, string, bool) {
	owner, repo, ok := strings.Cut(strings.TrimSpace(fullName), "/")
	owner = strings.TrimSpace(owner)
	repo = strings.TrimSpace(repo)
	return owner, repo, ok && owner != "" && repo != ""
}

func branchFromRef(ref string) (string, bool) {
	branch := strings.TrimPrefix(ref, "refs/heads/")
	return branch, branch != ref && branch != ""
}

func mirrorRefspec(ref string, force bool) string {
	refspec := ref + ":" + ref
	if force {
		return "+" + refspec
	}
	return refspec
}

func deleteRefspec(ref string) string {
	return ":" + ref
}

func (i *Integration) pushAndVerify(ctx context.Context, req PushRequest, ref, expectedSHA string) error {
	if err := i.push(ctx, req); err != nil {
		if i.remoteRef == nil || strings.TrimSpace(expectedSHA) == "" {
			return err
		}
		waitErr := i.waitForRemoteRef(ctx, req.RepoPath, req.RemoteURL, ref, expectedSHA, err)
		if waitErr == nil {
			return nil
		}
		var projectionErr *ProjectionError
		if errors.As(waitErr, &projectionErr) {
			return waitErr
		}
		return err
	}
	return nil
}

func (i *Integration) waitForRemoteRef(ctx context.Context, repoPath, remoteURL, ref, expectedSHA string, pushErr error) error {
	delays := []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}
	var lastErr error
	for _, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		actual, err := i.remoteRef(ctx, repoPath, remoteURL, ref)
		if err != nil {
			lastErr = err
			continue
		}
		actual = strings.TrimSpace(actual)
		if actual == "" {
			lastErr = fmt.Errorf("forgejo integration: remote ref %s is absent after push failure", ref)
			continue
		}
		if err := verifyProjectedHeadSHA(expectedSHA, actual, ref); err == nil {
			return nil
		}
		return remoteRefDriftError(ref, expectedSHA, actual, pushErr)
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("forgejo integration: remote ref %s did not reach expected head", ref)
}

func leaseDriftError(ref, expectedSHA, actualSHA, detail string) error {
	ref = strings.TrimSpace(ref)
	expectedSHA = strings.TrimSpace(expectedSHA)
	actualSHA = strings.TrimSpace(actualSHA)
	branch, _ := branchFromRef(ref)
	return &ProjectionError{
		Type: ProjectionFailureSHADrift, Ref: ref, Branch: branch,
		ExpectedSHA: expectedSHA, ActualSHA: actualSHA,
		ErrorSummary: fmt.Sprintf("%s: remote ref %s is at %s, accepted old head was %s; refusing unsafe overwrite", strings.TrimSpace(detail), ref, firstNonEmpty(actualSHA, "<absent>"), expectedSHA),
	}
}

func exactGitSHA(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) && strings.TrimSpace(a) != ""
}

func isFullGitObjectID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func remoteRefDriftError(ref, expectedSHA, actualSHA string, cause error) error {
	ref = strings.TrimSpace(ref)
	expectedSHA = strings.TrimSpace(expectedSHA)
	actualSHA = strings.TrimSpace(actualSHA)
	summary := fmt.Sprintf("remote ref %s is at %s, expected %s; refusing unsafe overwrite", ref, actualSHA, expectedSHA)
	if cause != nil {
		if pushSummary := redactProjectionError(cause); pushSummary != "" {
			summary += "; push error: " + pushSummary
		}
	}
	branch, _ := branchFromRef(ref)
	return &ProjectionError{
		Type:         ProjectionFailureNonFastForward,
		Ref:          ref,
		Branch:       branch,
		ExpectedSHA:  expectedSHA,
		ActualSHA:    actualSHA,
		ErrorSummary: summary,
		Cause:        cause,
	}
}

func verifyProjectedHeadSHA(expected, actual, branch string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	actual = strings.ToLower(strings.TrimSpace(actual))
	if expected == "" {
		return nil
	}
	if actual == "" {
		return fmt.Errorf("forgejo integration: projected PR %s did not report head SHA after branch mirror", branch)
	}
	// Projection is an identity check, not a user-facing abbreviated-SHA search.
	// A prefix from either side must never attest exact ref/PR convergence.
	if expected == actual {
		return nil
	}
	return fmt.Errorf("forgejo integration: projected PR %s head SHA mismatch: got %s, want %s", branch, actual, expected)
}

func (c Config) authenticatedRemoteURL(target TargetRepo) (string, error) {
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil {
		return "", fmt.Errorf("forgejo integration: invalid base URL: %w", err)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + target.Owner + "/" + target.Repo + ".git"
	// A configured local/SSH transport does not use an HTTP credential. In
	// particular, never manufacture credential-bearing file URLs for local
	// real-Git fixtures or operator-managed filesystem transports.
	if base.Scheme == "http" || base.Scheme == "https" {
		base.User = url.UserPassword("x-access-token", c.Token)
	}
	return base.String(), nil
}

// GitPush mirrors a ref to Forgejo using git push.
func GitPush(ctx context.Context, req PushRequest) error {
	if err := validateLeasePushRequest(req); err != nil {
		return err
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	_, err := gittransport.Run(ctx, req.RemoteURL, "", gitPushArgs(req)...)
	if err != nil {
		return fmt.Errorf("git push: %w", err)
	}
	return nil
}

func validateLeasePushRequest(req PushRequest) error {
	leaseRef := strings.TrimSpace(req.ForceWithLeaseRef)
	leaseSHA := strings.TrimSpace(req.ForceWithLeaseSHA)
	if leaseRef == "" && leaseSHA == "" {
		return nil
	}
	if leaseRef == "" || !strings.HasPrefix(leaseRef, "refs/heads/") || !isFullGitObjectID(leaseSHA) {
		return fmt.Errorf("forgejo integration: invalid force-with-lease ref or object ID")
	}
	if strings.HasPrefix(strings.TrimSpace(req.Refspec), "+") || strings.HasPrefix(leaseRef, "refs/tags/") {
		return fmt.Errorf("forgejo integration: unconditional force is forbidden for lease push")
	}
	parts := strings.Split(strings.TrimSpace(req.Refspec), ":")
	if len(parts) != 2 || parts[0] != leaseRef || parts[1] != leaseRef {
		return fmt.Errorf("forgejo integration: lease ref must exactly match push refspec")
	}
	return nil
}

func gitPushArgs(req PushRequest) []string {
	args := []string{"-C", req.RepoPath}
	for _, cfg := range normalizeGitConfig(req.GitConfig) {
		args = append(args, "-c", cfg)
	}
	args = append(args, "push")
	if strings.TrimSpace(req.ForceWithLeaseRef) != "" || strings.TrimSpace(req.ForceWithLeaseSHA) != "" {
		args = append(args, "--force-with-lease="+strings.TrimSpace(req.ForceWithLeaseRef)+":"+strings.TrimSpace(req.ForceWithLeaseSHA))
	}
	args = append(args, req.RemoteURL, req.Refspec)
	return args
}

func normalizeGitConfig(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

// GitRemoteRefSHA reads one remote Git ref SHA using git ls-remote.
func GitRemoteRefSHA(ctx context.Context, repoPath, remoteURL, ref string) (string, error) {
	out, err := gittransport.Run(ctx, remoteURL, "", "-C", repoPath, "ls-remote", remoteURL, ref)
	if err != nil {
		return "", fmt.Errorf("git ls-remote: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", nil
}

func scrubURLSecrets(text string) string {
	for _, scheme := range []string{"http://", "https://"} {
		start := 0
		for {
			i := strings.Index(text[start:], scheme)
			if i < 0 {
				break
			}
			i += start + len(scheme)
			at := strings.Index(text[i:], "@")
			if at < 0 {
				break
			}
			at += i
			slash := strings.Index(text[i:], "/")
			if slash >= 0 && i+slash < at {
				start = i + slash
				continue
			}
			text = text[:i] + "<redacted>@" + text[at+1:]
			start = i + len("<redacted>@")
		}
	}
	return text
}

// LoadTokenFile reads a token file when Token is unset.
func LoadTokenFile(cfg Config) (Config, error) {
	if strings.TrimSpace(cfg.Token) != "" || strings.TrimSpace(cfg.TokenFile) == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return cfg, fmt.Errorf("read forgejo token file: %w", err)
	}
	cfg.Token = strings.TrimSpace(string(data))
	return cfg, nil
}

// LoadAuthorityPolicyTokenFile reads the policy operator token when unset.
func LoadAuthorityPolicyTokenFile(cfg Config) (Config, error) {
	if strings.TrimSpace(cfg.AuthorityPolicyToken) != "" || strings.TrimSpace(cfg.AuthorityPolicyTokenFile) == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(cfg.AuthorityPolicyTokenFile)
	if err != nil {
		return cfg, fmt.Errorf("read Forgejo authority policy token file: %w", err)
	}
	cfg.AuthorityPolicyToken = strings.TrimSpace(string(data))
	return cfg, nil
}

// LoadWebhookSecretFile reads a webhook secret file when WebhookSecret is unset.
func LoadWebhookSecretFile(cfg Config) (Config, error) {
	if strings.TrimSpace(cfg.WebhookSecret) != "" || strings.TrimSpace(cfg.WebhookSecretFile) == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(cfg.WebhookSecretFile)
	if err != nil {
		return cfg, fmt.Errorf("read forgejo webhook secret file: %w", err)
	}
	cfg.WebhookSecret = strings.TrimSpace(string(data))
	return cfg, nil
}

// LoadRepoMapFile loads optional repository mappings from JSON.
func LoadRepoMapFile(cfg Config) (Config, error) {
	if strings.TrimSpace(cfg.RepoMapFile) == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(cfg.RepoMapFile)
	if err != nil {
		return cfg, fmt.Errorf("read forgejo repo map file: %w", err)
	}
	var mappings map[string]RepoMapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		return cfg, fmt.Errorf("decode forgejo repo map file: %w", err)
	}
	if cfg.RepoMap == nil {
		cfg.RepoMap = map[string]RepoMapping{}
	}
	for repo, mapping := range mappings {
		cfg.RepoMap[repo] = mapping
	}
	return cfg, nil
}
