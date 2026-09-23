package forgejointegration

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

var authorityWorkflowLabels = []string{
	AGSActionRebaseLabel,
	AGSStatusRebasingLabel,
	AGSStatusProjectionDriftLabel,
	AGSStatusRebaseConflictLabel,
	AGSStatusBlockedLabel,
	AGSStatusNeedsRebaseLabel,
}

// RepositoryAuthorityState is the live Forgejo state relevant to AGS authority.
type RepositoryAuthorityState struct {
	AllowRebaseUpdate             bool
	BaseBranchProtected           bool
	DirectPushBlocked             bool
	ForcePushBlocked              bool
	IntegrationBotCollaborator    bool
	IntegrationBotAuthorized      bool
	IntegrationBotMergeAuthorized bool
	MergeWhitelistUsernames       []string
	Labels                        map[string]bool
	WebhookID                     int64
	WebhookActive                 bool
	WebhookPullRequests           bool
	WebhookIssues                 bool
	WebhookDelete                 bool
}

// RepositoryAuthorityPlan is an auditable plan/apply/verify unit for one mapped repository.
type RepositoryAuthorityPlan struct {
	AGSRepo     string   `json:"ags_repo"`
	ForgejoRepo string   `json:"forgejo_repo"`
	BaseBranch  string   `json:"base_branch"`
	Changes     []string `json:"changes"`
	Converged   bool     `json:"converged"`
}

type repositoryAuthorityClient interface {
	InspectRepositoryAuthority(ctx context.Context, owner, repo, baseBranch, webhookURL, integrationBot string) (RepositoryAuthorityState, error)
	ApplyRepositoryAuthority(ctx context.Context, owner, repo, baseBranch, webhookURL, webhookSecret, integrationBot string, current RepositoryAuthorityState) error
}

// AuthorityPolicyConfigured reports whether workflow-action authority can be verified.
func (i *Integration) AuthorityPolicyConfigured() error {
	if i == nil || !i.cfg.Enabled {
		return fmt.Errorf("Forgejo integration is not enabled")
	}
	if strings.TrimSpace(i.cfg.WebhookSecret) == "" {
		return fmt.Errorf("Forgejo webhook secret is empty")
	}
	if !i.cfg.AuthorityPolicyEnabled {
		return fmt.Errorf("Forgejo authority policy is not enabled")
	}
	if strings.TrimSpace(i.cfg.WebhookURL) == "" {
		return fmt.Errorf("Forgejo authority policy webhook URL is empty")
	}
	if strings.TrimSpace(i.cfg.IntegrationBot) == "" {
		return fmt.Errorf("Forgejo authority policy integration bot is empty")
	}
	targets, err := i.authorityPolicyTargets()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("Forgejo authority policy requires at least one explicit repository mapping")
	}
	if i.authorityClient == nil {
		return fmt.Errorf("Forgejo authority policy verifier is not configured")
	}
	return nil
}

// PlanRepositoryAuthority reads live provider state and returns only required mutations.
func (i *Integration) PlanRepositoryAuthority(ctx context.Context) ([]RepositoryAuthorityPlan, error) {
	if err := i.AuthorityPolicyConfigured(); err != nil {
		return nil, err
	}
	client := i.authorityClient
	targets, err := i.authorityPolicyTargets()
	if err != nil {
		return nil, err
	}
	plans := make([]RepositoryAuthorityPlan, 0, len(targets))
	for _, target := range targets {
		state, err := client.InspectRepositoryAuthority(ctx, target.owner, target.repo, target.baseBranch, i.cfg.WebhookURL, i.cfg.IntegrationBot)
		if err != nil {
			return nil, fmt.Errorf("inspect Forgejo authority for %s: %w", target.externalRepo(), err)
		}
		changes := authorityPolicyChanges(state)
		plans = append(plans, RepositoryAuthorityPlan{
			AGSRepo: target.agsRepo, ForgejoRepo: target.externalRepo(), BaseBranch: target.baseBranch,
			Changes: changes, Converged: len(changes) == 0,
		})
	}
	return plans, nil
}

// ApplyRepositoryAuthority idempotently applies a generated plan and verifies convergence.
func (i *Integration) ApplyRepositoryAuthority(ctx context.Context) ([]RepositoryAuthorityPlan, error) {
	plans, err := i.PlanRepositoryAuthority(ctx)
	if err != nil {
		return nil, err
	}
	client := i.authorityClient
	targets, err := i.authorityPolicyTargets()
	if err != nil {
		return nil, err
	}
	for index, plan := range plans {
		if plan.Converged {
			continue
		}
		target := targets[index]
		state, err := client.InspectRepositoryAuthority(ctx, target.owner, target.repo, target.baseBranch, i.cfg.WebhookURL, i.cfg.IntegrationBot)
		if err != nil {
			return nil, fmt.Errorf("refresh Forgejo authority for %s: %w", target.externalRepo(), err)
		}
		if err := client.ApplyRepositoryAuthority(ctx, target.owner, target.repo, target.baseBranch, i.cfg.WebhookURL, i.cfg.WebhookSecret, i.cfg.IntegrationBot, state); err != nil {
			return nil, fmt.Errorf("apply Forgejo authority for %s: %w", target.externalRepo(), err)
		}
	}
	return i.VerifyRepositoryAuthority(ctx)
}

// VerifyRepositoryAuthority fails unless every mapped repository preserves AGS authority.
func (i *Integration) VerifyRepositoryAuthority(ctx context.Context) ([]RepositoryAuthorityPlan, error) {
	plans, err := i.PlanRepositoryAuthority(ctx)
	if err != nil {
		return nil, err
	}
	for _, plan := range plans {
		if !plan.Converged {
			return plans, fmt.Errorf("Forgejo authority policy drift for %s: %s", plan.ForgejoRepo, strings.Join(plan.Changes, ", "))
		}
	}
	return plans, nil
}

// VerifyPullRequestMergeAuthority performs the exact provider preflight needed
// by the compatibility merge gateway. It preserves protected-branch and
// server-owned credential checks while leaving approval/status enforcement to
// the provider's merge endpoint.
func (i *Integration) VerifyPullRequestMergeAuthority(ctx context.Context, repoFullName, baseBranch string) (RepositoryAuthorityState, error) {
	if i == nil || !i.cfg.Enabled || i.authorityClient == nil {
		return RepositoryAuthorityState{}, fmt.Errorf("Forgejo merge authority verifier is unavailable")
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return RepositoryAuthorityState{}, err
	}
	baseBranch = strings.TrimSpace(baseBranch)
	configuredBase := strings.TrimSpace(target.BaseBranch)
	if baseBranch == "" || configuredBase == "" {
		return RepositoryAuthorityState{}, fmt.Errorf("Forgejo merge base is unavailable")
	}
	dynamicBase := baseBranch != configuredBase
	if dynamicBase && !i.cfg.MirrorBranchEnabled(baseBranch) {
		return RepositoryAuthorityState{}, fmt.Errorf("Forgejo merge base %q is not mirrored", baseBranch)
	}
	state, err := i.authorityClient.InspectRepositoryAuthority(ctx, target.Owner, target.Repo, baseBranch, i.cfg.WebhookURL, i.cfg.IntegrationBot)
	if err != nil {
		return RepositoryAuthorityState{}, fmt.Errorf("inspect Forgejo merge authority for %s/%s: %w", target.Owner, target.Repo, err)
	}
	if !state.IntegrationBotCollaborator {
		return state, fmt.Errorf("Forgejo merge authority lacks the server executor for %s/%s", target.Owner, target.Repo)
	}
	if dynamicBase {
		return state, nil
	}
	if !state.BaseBranchProtected || !state.ForcePushBlocked {
		return state, fmt.Errorf("Forgejo merge authority is not protected for %s/%s", target.Owner, target.Repo)
	}
	return state, nil
}

func authorityPolicyChanges(state RepositoryAuthorityState) []string {
	changes := make([]string, 0, 8)
	if state.AllowRebaseUpdate {
		changes = append(changes, "disable_allow_rebase_update")
	}
	if !state.BaseBranchProtected || !state.ForcePushBlocked {
		changes = append(changes, "protect_base_for_ags_authority")
	}
	if !state.IntegrationBotCollaborator {
		changes = append(changes, "grant_integration_bot_write")
	}
	for _, label := range authorityWorkflowLabels {
		if !state.Labels[label] {
			changes = append(changes, "create_label:"+label)
		}
	}
	if !state.WebhookActive || !state.WebhookPullRequests || !state.WebhookIssues || !state.WebhookDelete {
		changes = append(changes, "configure_ags_webhook")
	}
	return changes
}

type authorityPolicyTarget struct {
	agsRepo, owner, repo, baseBranch string
}

func (t authorityPolicyTarget) externalRepo() string { return t.owner + "/" + t.repo }

func (i *Integration) authorityPolicyTargets() ([]authorityPolicyTarget, error) {
	agsRepos := make([]string, 0, len(i.cfg.RepoMap))
	for agsRepo := range i.cfg.RepoMap {
		agsRepos = append(agsRepos, agsRepo)
	}
	sort.Strings(agsRepos)
	seen := map[string]string{}
	targets := make([]authorityPolicyTarget, 0, len(agsRepos))
	for _, agsRepo := range agsRepos {
		mapping := i.cfg.RepoMap[agsRepo]
		if mapping.Enabled != nil && !*mapping.Enabled {
			continue
		}
		owner := strings.TrimSpace(mapping.Owner)
		repo := strings.TrimSpace(mapping.Repo)
		if owner == "" || repo == "" {
			return nil, fmt.Errorf("Forgejo authority mapping %s requires explicit owner and repo", agsRepo)
		}
		base := strings.TrimSpace(mapping.BaseBranch)
		if base == "" {
			base = i.cfg.DefaultBaseBranch
		}
		key := owner + "/" + repo
		if previousAGSRepo, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("Forgejo authority target %s is mapped from both %s and %s", key, previousAGSRepo, agsRepo)
		}
		seen[key] = agsRepo
		targets = append(targets, authorityPolicyTarget{agsRepo: agsRepo, owner: owner, repo: repo, baseBranch: base})
	}
	sort.Slice(targets, func(a, b int) bool { return targets[a].externalRepo() < targets[b].externalRepo() })
	return targets, nil
}
