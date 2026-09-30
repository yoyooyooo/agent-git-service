package forgejointegration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ArtifactRetirementProtectionState is a credential-free binding for the
// temporary force-push maintenance window. Fingerprint excludes only the force
// toggle itself, so any concurrent protection-policy drift blocks both opening
// and restoration.
type ArtifactRetirementProtectionState struct {
	Exists            bool
	ForcePushEnabled  bool
	PolicyFingerprint string
}

type artifactRetirementAuthorityClient interface {
	InspectArtifactRetirementProtection(context.Context, string, string, string) (ArtifactRetirementProtectionState, error)
	SetArtifactRetirementForce(context.Context, string, string, string, string, bool) error
}

var artifactRetirementProtectionKeys = []string{
	"branch_name", "rule_name",
	"enable_push", "enable_push_whitelist", "push_whitelist_usernames", "push_whitelist_teams", "push_whitelist_deploy_keys",
	"enable_merge_whitelist", "merge_whitelist_usernames", "merge_whitelist_teams",
	"enable_status_check", "status_check_contexts", "required_approvals",
	"enable_approvals_whitelist", "approvals_whitelist_username", "approvals_whitelist_teams",
	"block_on_rejected_reviews", "block_on_official_review_requests", "block_on_outdated_branch",
	"dismiss_stale_approvals", "ignore_stale_approvals", "require_signed_commits",
	"protected_file_patterns", "unprotected_file_patterns", "apply_to_admins",
}

func decodeArtifactRetirementProtection(body []byte) (ArtifactRetirementProtectionState, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ArtifactRetirementProtectionState{}, errors.New("decode artifact retirement branch protection")
	}
	forceRaw, ok := raw["enable_force_push"]
	if !ok {
		return ArtifactRetirementProtectionState{}, errors.New("artifact retirement branch protection lacks force-push field")
	}
	var force bool
	if err := json.Unmarshal(forceRaw, &force); err != nil {
		return ArtifactRetirementProtectionState{}, errors.New("artifact retirement branch protection force-push field is invalid")
	}
	stable := make(map[string]json.RawMessage, len(artifactRetirementProtectionKeys))
	for _, key := range artifactRetirementProtectionKeys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		stable[key] = value
	}
	encoded, err := json.Marshal(stable)
	if err != nil {
		return ArtifactRetirementProtectionState{}, err
	}
	sum := sha256.Sum256(encoded)
	return ArtifactRetirementProtectionState{
		Exists: true, ForcePushEnabled: force, PolicyFingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

func (c *HTTPClient) InspectArtifactRetirementProtection(ctx context.Context, owner, repo, branch string) (ArtifactRetirementProtectionState, error) {
	path := apiPath("/api/v1/repos/%s/%s/branch_protections/%s", owner, repo, branch)
	status, body, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ArtifactRetirementProtectionState{}, err
	}
	if status == http.StatusNotFound {
		return ArtifactRetirementProtectionState{}, nil
	}
	if status < 200 || status >= 300 {
		return ArtifactRetirementProtectionState{}, fmt.Errorf("artifact retirement branch protection lookup returned status %d", status)
	}
	return decodeArtifactRetirementProtection(body)
}

func (c *HTTPClient) SetArtifactRetirementForce(ctx context.Context, owner, repo, branch, fingerprint string, enabled bool) error {
	if len(fingerprint) != sha256.Size*2 {
		return errors.New("artifact retirement branch protection fingerprint is invalid")
	}
	before, err := c.InspectArtifactRetirementProtection(ctx, owner, repo, branch)
	if err != nil {
		return err
	}
	if !before.Exists || before.PolicyFingerprint != strings.ToLower(fingerprint) {
		return errors.New("artifact retirement branch protection drifted before mutation")
	}
	if before.ForcePushEnabled == enabled {
		return nil
	}
	path := apiPath("/api/v1/repos/%s/%s/branch_protections/%s", owner, repo, branch)
	if err := c.authorityRequest(ctx, http.MethodPatch, path, map[string]any{"enable_force_push": enabled}); err != nil {
		return err
	}
	after, err := c.InspectArtifactRetirementProtection(ctx, owner, repo, branch)
	if err != nil {
		return err
	}
	if !after.Exists || after.PolicyFingerprint != before.PolicyFingerprint || after.ForcePushEnabled != enabled {
		return errors.New("artifact retirement branch protection mutation did not converge exactly")
	}
	return nil
}

// ArtifactRetirementForcePolicy returns the exact durable binding used by the
// startup migration. Only the configured protected base branch may request the
// temporary force window.
func (i *Integration) ArtifactRetirementForcePolicy(ctx context.Context, repoFullName, branch string) (ArtifactRetirementProtectionState, bool, error) {
	if i == nil || !i.cfg.Enabled {
		return ArtifactRetirementProtectionState{}, false, nil
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return ArtifactRetirementProtectionState{}, false, err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" || branch != strings.TrimSpace(target.BaseBranch) {
		return ArtifactRetirementProtectionState{}, false, nil
	}
	client, ok := i.authorityClient.(artifactRetirementAuthorityClient)
	if !ok {
		return ArtifactRetirementProtectionState{}, true, errors.New("Forgejo artifact retirement policy operator is unavailable")
	}
	authority, err := i.VerifyPullRequestMergeAuthority(ctx, repoFullName, branch)
	if err != nil {
		return ArtifactRetirementProtectionState{}, true, err
	}
	if !authority.BaseBranchProtected || !authority.ForcePushBlocked || !authority.IntegrationBotCollaborator || !authority.IntegrationBotAuthorized {
		return ArtifactRetirementProtectionState{}, true, errors.New("Forgejo artifact retirement requires converged protected-base authority")
	}
	state, err := client.InspectArtifactRetirementProtection(ctx, target.Owner, target.Repo, branch)
	if err != nil {
		return ArtifactRetirementProtectionState{}, true, err
	}
	if !state.Exists || state.ForcePushEnabled || state.PolicyFingerprint == "" {
		return ArtifactRetirementProtectionState{}, true, errors.New("Forgejo artifact retirement force policy is not closed")
	}
	return state, true, nil
}

// SetArtifactRetirementForce toggles only enable_force_push under an exact
// protection fingerprint. The operator credential never enters Git transport.
func (i *Integration) SetArtifactRetirementForce(ctx context.Context, repoFullName, branch, fingerprint string, enabled bool) error {
	if i == nil || !i.cfg.Enabled {
		return errors.New("Forgejo artifact retirement integration is unavailable")
	}
	target, err := i.cfg.requireTargetFor(repoFullName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(branch) != strings.TrimSpace(target.BaseBranch) {
		return errors.New("Forgejo artifact retirement force window is restricted to the configured base branch")
	}
	client, ok := i.authorityClient.(artifactRetirementAuthorityClient)
	if !ok {
		return errors.New("Forgejo artifact retirement policy operator is unavailable")
	}
	if err := client.SetArtifactRetirementForce(ctx, target.Owner, target.Repo, branch, fingerprint, enabled); err != nil {
		return err
	}
	if !enabled {
		if _, err := i.VerifyPullRequestMergeAuthority(ctx, repoFullName, branch); err != nil {
			return fmt.Errorf("restore Forgejo retirement authority: %w", err)
		}
	}
	return nil
}
