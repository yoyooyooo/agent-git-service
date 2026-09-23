package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
)

// HumanProviderMergeInput is the closed Human/operator request accepted by the
// AGS single frontdoor. Provider coordinates and credentials are deliberately
// absent: AGS resolves both from its deployment-owned projection binding.
type HumanProviderMergeInput struct {
	ExpectedHeadSHA string
	MergeMethod     string
}

// HumanProviderMergeReceipt separates the AGS caller from the server-owned
// provider executor while exposing only non-secret authoritative coordinates.
type HumanProviderMergeReceipt struct {
	Schema           string `json:"schema"`
	Repository       string `json:"repository"`
	AGSPRNumber      int    `json:"ags_pr"`
	ActorID          uint   `json:"actor_id"`
	ActorLogin       string `json:"actor_login"`
	Provider         string `json:"provider"`
	ProviderRepo     string `json:"provider_repo"`
	ProviderPRNumber int    `json:"provider_pr"`
	ExpectedHeadSHA  string `json:"expected_head_sha"`
	MergeMethod      string `json:"merge_method"`
	ProviderMerged   bool   `json:"provider_merged"`
	ProviderMergeSHA string `json:"provider_merge_sha"`
	ProjectionStatus string `json:"projection_status"`
	Outcome          string `json:"outcome,omitempty"`
	AGSBaseSHA       string `json:"ags_base_sha,omitempty"`
	ProviderBaseSHA  string `json:"provider_base_sha,omitempty"`
}

// ExecuteHumanProviderMerge authorizes one Human exclusively through AGS and
// executes the mapped provider merge with the deployment-owned integration
// executor. It never accepts a provider token, login, repository, or PR number.
func (s *Service) ExecuteHumanProviderMerge(ctx context.Context, repository string, prNumber int, input HumanProviderMergeInput) (HumanProviderMergeReceipt, error) {
	if s == nil || s.ForgejoIntegration == nil || s.Git == nil {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: provider merge executor is unavailable", ErrInvalidState)
	}
	repository = strings.TrimSpace(repository)
	expectedHead := strings.ToLower(strings.TrimSpace(input.ExpectedHeadSHA))
	mergeMethod := strings.ToLower(strings.TrimSpace(input.MergeMethod))
	if repository == "" || prNumber <= 0 || !canonicalActionSHA(expectedHead) {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: repository, pull request, and expected head are required", ErrValidation)
	}
	switch mergeMethod {
	case "merge", "rebase", "rebase-merge", "squash", "fast-forward-only":
	default:
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: merge method is invalid", ErrValidation)
	}

	actor, err := s.GetCurrentUser(ctx)
	if err != nil {
		return HumanProviderMergeReceipt{}, err
	}
	if actor.UserKind != db.UserKindHuman {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: Human provider merge requires a Human AGS identity", ErrForbidden)
	}
	pr, err := s.GetPR(ctx, repository, prNumber)
	if err != nil {
		return HumanProviderMergeReceipt{}, err
	}
	if !exactGitSHA(pr.HeadSHA, expectedHead) {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: expected head does not match current AGS pull request", ErrConflict)
	}
	projection, err := s.forgejoEvidenceBinding(ctx, pr)
	if err != nil || projection.Kind != ProjectionProviderForgejo || projection.ExternalNumber <= 0 {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: authoritative provider projection is unavailable", ErrConflict)
	}

	receipt := HumanProviderMergeReceipt{
		Schema: "ags.provider-merge-receipt.v1", Repository: repository, AGSPRNumber: prNumber,
		ActorID: actor.ID, ActorLogin: actor.Login, Provider: ProjectionProviderForgejo,
		ProviderRepo: projection.ExternalRepo, ProviderPRNumber: projection.ExternalNumber,
		ExpectedHeadSHA: expectedHead, MergeMethod: mergeMethod,
	}
	if pr.Merged {
		if !exactGitSHA(pr.MergeCommitSHA, expectedHead) {
			return HumanProviderMergeReceipt{}, fmt.Errorf("%w: terminal AGS merge SHA conflicts with expected head", ErrConflict)
		}
		receipt.ProviderMergeSHA = strings.ToLower(pr.MergeCommitSHA)
		if pr.MergedByLogin == providerMergeAckMergedBy {
			receipt.ProviderMerged = false
			receipt.ProjectionStatus = providerMergeProjectionPRPending
			receipt.Outcome = providerMergeOutcomeSourceSuccess
			return receipt, nil
		}
		receipt.ProviderMerged = true
		receipt.ProjectionStatus = "converged"
		return receipt, nil
	}
	// A retry after a lost response must read the exact provider coordinate and
	// return without another provider POST. The signed provider webhook remains
	// the sole owner of AGS base/terminal convergence.
	if snapshot, found, readErr := s.ForgejoIntegration.ExactPullRequestSnapshot(ctx, repository, projection.ExternalRepo, projection.ExternalNumber); readErr == nil && found && snapshot.Merged {
		if mergeSHA, canonical := canonicalProviderMergeSnapshotSHA(snapshot, expectedHead); canonical {
			receipt.ProviderMerged = true
			receipt.ProviderMergeSHA = strings.ToLower(mergeSHA)
			receipt.ProjectionStatus = "pending"
			return receipt, nil
		}
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: provider terminal facts conflict with expected head", ErrConflict)
	}
	if pr.State != db.StateOpen || pr.Draft {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: pull request is not open and mergeable", ErrInvalidState)
	}
	if err := s.enforceMergePolicy(ctx, actor, &pr); err != nil {
		return HumanProviderMergeReceipt{}, err
	}

	targetInstance := "ags-local"
	if s.MulticaProjection != nil && strings.TrimSpace(s.MulticaProjection.TargetInstance()) != "" {
		targetInstance = strings.TrimSpace(s.MulticaProjection.TargetInstance())
	}
	binding, err := s.ForgejoIntegration.ProviderMergeBindingForBase(repository, pr.RepositoryID, targetInstance, pr.BaseRef)
	if err != nil || binding.ProviderRepository != projection.ExternalRepo || binding.BaseRef != pr.BaseRef {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: provider merge binding is unavailable", ErrConflict)
	}
	if binding.MergeMethod != mergeMethod {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: requested merge method does not match provider binding", ErrConflict)
	}
	sides, err := s.loadProviderMergeSides(ctx, repository, pr.BaseRef, expectedHead)
	if err != nil {
		return HumanProviderMergeReceipt{}, err
	}
	ffAck := s.fastForwardAckEligible(repository, mergeMethod, sides.ProviderBase, expectedHead)
	if !ffAck {
		containsBase, err := s.Git.IsAncestor(ctx, repository, sides.AGSBase, pr.HeadSHA)
		if err != nil || !containsBase {
			return HumanProviderMergeReceipt{}, fmt.Errorf("%w: pull request head does not contain current AGS base", ErrConflict)
		}
	}
	if _, err := s.ForgejoIntegration.VerifyPullRequestMergeAuthority(ctx, repository, pr.BaseRef); err != nil {
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: provider merge authority is unavailable", ErrForbidden)
	}
	if ffAck {
		return s.acknowledgeFastForwardMerge(ctx, receipt, pr, projection, sides)
	}
	if _, err := s.providerMergePreflight(ctx, repository, projection.ExternalRepo, projection.ExternalNumber, expectedHead, pr.BaseRef, sides.AGSBase, sides.ProviderBase); err != nil {
		return HumanProviderMergeReceipt{}, err
	}
	// Human merge timing is an explicit operator decision. Provider CI remains
	// observable and may continue asynchronously; identity, exact head/base,
	// repository mapping, mergeability, and provider authority still fail closed.
	result, mergeErr := s.ForgejoIntegration.MergePullRequest(ctx, repository, projection.ExternalRepo, projection.ExternalNumber,
		forgejointegration.PullRequestMergeRequest{Method: mergeMethod, ExpectedHead: expectedHead, DeleteBranch: false})
	mergeSHA, confirmed := canonicalProviderMergeResult(result, mergeErr, expectedHead)
	if !confirmed {
		observed, obsErr := s.ObserveHumanProviderMerge(ctx, repository, prNumber, expectedHead)
		if obsErr == nil && (observed.ProviderMerged || observed.Outcome == providerMergeOutcomeSourceSuccess) {
			return observed, nil
		}
		return HumanProviderMergeReceipt{}, fmt.Errorf("%w: provider merge outcome is unknown; use exact AGS readback", ErrConflict)
	}

	receipt.ProviderMerged = true
	receipt.ProviderMergeSHA = strings.ToLower(mergeSHA)
	receipt.ProjectionStatus = "pending"
	return receipt, nil
}
