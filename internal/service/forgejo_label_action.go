package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/ngaut/agent-git-service/internal/forgejointegration"
	"gorm.io/gorm"
)

const forgejoLabelActionRequestSource = "forgejo_label"

type forgejoLabelActionAdmission struct {
	Intent       db.PullRequestActionIntent
	MappedActor  bool
	Admitted     bool
	Replay       bool
	Stale        bool
	DenialReason string
}

// admitForgejoLabelActionIntent turns one signed, externally initiated label
// event into the same exact durable authority record consumed by the rebase
// kernel. The label remains a request surface: only an explicit actor binding,
// live Forgejo write permission and the shared AGS evaluator can admit it.
func (s *Service) admitForgejoLabelActionIntent(ctx context.Context, event forgejointegration.PullRequestActionLabelEvent) (forgejoLabelActionAdmission, error) {
	admission := forgejoLabelActionAdmission{}
	idempotencyKey := ""
	if strings.TrimSpace(event.CorrelationID) != "" {
		idempotencyKey = forgejoLabelActionIdempotencyKey(event)
		var prior db.PullRequestActionIntent
		if err := s.DBForCtx(ctx).Where("idempotency_key = ?", idempotencyKey).First(&prior).Error; err == nil {
			if prior.RequestSource != forgejoLabelActionRequestSource || !strings.EqualFold(strings.TrimSpace(prior.RequestActor), strings.TrimSpace(event.SenderLogin)) {
				return admission, fmt.Errorf("Forgejo label delivery identity collides with a different durable action request")
			}
			return forgejoLabelActionAdmission{Intent: prior, Replay: true}, nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return admission, fmt.Errorf("read prior Forgejo label action delivery: %w", err)
		}
	}
	labels, checked, err := s.ForgejoIntegration.ListPullRequestLabels(ctx, event.RepoFullName, event.PRNumber)
	if err != nil {
		return admission, fmt.Errorf("read exact Forgejo labels for action request: %w", err)
	}
	if checked && !stringInSlice(forgejointegration.AGSActionRebaseLabel, labels) {
		return forgejoLabelActionAdmission{Stale: true}, nil
	}
	if idempotencyKey == "" {
		admission.DenialReason = "Forgejo delivery identity is unavailable"
		return admission, nil
	}
	principalID, bindingRevision, mapped := s.ForgejoIntegration.ActionPrincipalBinding(event.SenderLogin)
	if !mapped {
		return forgejoLabelActionAdmission{DenialReason: "Forgejo actor has no AGS action principal binding"}, nil
	}
	admission.MappedActor = true
	allowed, _, err := s.ForgejoIntegration.ActorHasWriteAccess(ctx, event.RepoFullName, event.SenderLogin)
	if err != nil {
		return admission, fmt.Errorf("verify Forgejo action actor permission: %w", err)
	}
	if !allowed {
		admission.DenialReason = "Forgejo actor does not have write permission"
		return admission, nil
	}

	var principal db.User
	if err := s.DBForCtx(ctx).Where("id = ? AND type = ?", principalID, db.TypeUser).First(&principal).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			admission.DenialReason = "mapped AGS action principal is unavailable"
			return admission, nil
		}
		return admission, fmt.Errorf("load mapped AGS action principal: %w", err)
	}
	if !isUserStatusActive(principal.Status) {
		admission.DenialReason = "mapped AGS action principal is inactive"
		return admission, nil
	}

	pr, err := s.FindPullRequestByProjection(ctx, ProjectionProviderForgejo, event.RepoFullName, event.PRNumber)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			admission.DenialReason = "Forgejo pull request has no authoritative AGS projection"
			return admission, nil
		}
		return admission, fmt.Errorf("resolve AGS pull request for label action: %w", err)
	}
	if pr.Repository.FullName == "" || pr.State != db.StateOpen || pr.Merged ||
		pr.HeadRef != strings.TrimSpace(event.HeadBranch) || pr.BaseRef != strings.TrimSpace(event.BaseBranch) ||
		!exactGitSHA(pr.HeadSHA, event.HeadSHA) {
		admission.DenialReason = "live Forgejo action facts do not match the authoritative AGS pull request"
		return admission, nil
	}

	postLabels, err := actionLabelSet(labels)
	if err != nil || !checked || !stringInSlice(forgejointegration.AGSActionRebaseLabel, postLabels) {
		admission.DenialReason = "exact Forgejo action label state is unavailable or changed"
		return admission, nil
	}
	preLabels := make([]string, 0, len(postLabels)-1)
	for _, label := range postLabels {
		if label != forgejointegration.AGSActionRebaseLabel {
			preLabels = append(preLabels, label)
		}
	}
	preLabelsJSON, err := actionLabelsJSON(preLabels)
	if err != nil {
		return admission, err
	}
	postLabelsJSON, err := actionLabelsJSON(postLabels)
	if err != nil {
		return admission, err
	}
	baseSHA, err := s.Git.HeadSHA(ctx, pr.Repository.FullName, pr.BaseRef)
	if err != nil {
		return admission, fmt.Errorf("read authoritative AGS base for label action: %w", err)
	}
	if !isValidGitSHA(baseSHA) {
		return admission, fmt.Errorf("read authoritative AGS base for label action: non-canonical SHA")
	}
	authorization, err := s.AuthorizeDurableOperation(ctx, principal, "ags", pr.Repository.FullName, "pr.rebase",
		forgejoRebaseDurableConstraints(pr.Number, event.PRNumber, pr.HeadSHA, baseSHA))
	if err != nil || !authorization.Authorized {
		if err != nil && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrValidation) {
			return admission, fmt.Errorf("authorize mapped AGS action principal: %w", err)
		}
		admission.DenialReason = "mapped AGS principal is not authorized for this exact rebase"
		return admission, nil
	}

	expiresIn := 5 * time.Minute
	now := time.Now().UTC()
	intent := db.PullRequestActionIntent{
		ID: uuid.NewString(), IdempotencyKey: idempotencyKey, Action: "pr.rebase", State: ForgejoActionIntentPlanned,
		PullRequestID: pr.ID, RepositoryID: pr.RepositoryID, AGSPRNumber: pr.Number, Repository: pr.Repository.FullName,
		ForgejoRepo: event.RepoFullName, ForgejoPRNumber: event.PRNumber, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
		ExpectedHeadSHA: pr.HeadSHA, ExpectedBaseSHA: baseSHA, ExpectedLabels: preLabelsJSON, PostLabels: postLabelsJSON,
		PrincipalID: principal.ID, TeamIdentityID: authorization.AuthorizationBasis.TeamIdentityID,
		PolicyClass: authorization.AuthorizationBasis.PolicyClass, MembershipEpoch: authorization.AuthorizationBasis.MembershipEpoch,
		AuthorityRev: durableActionAuthorityRevision(authorization), ProviderEffectStatus: ProviderEffectStatusNotAttempted,
		RequestSource: forgejoLabelActionRequestSource, RequestActor: strings.TrimSpace(event.SenderLogin), RequestBindingRev: bindingRevision,
		ExpiresInSeconds: int64(expiresIn / time.Second), ExpiresAt: now.Add(expiresIn),
	}
	preflight, err := s.preflightForgejoActionRebase(ctx, event, pr, intent)
	if err != nil || !exactGitSHA(preflight.BaseSHA, baseSHA) {
		admission.DenialReason = "live AGS or Forgejo action facts changed during admission"
		return admission, nil
	}
	if err := s.commitForgejoActionIntent(ctx, &intent); err != nil {
		if errors.Is(err, ErrForbidden) || errors.Is(err, ErrValidation) {
			admission.DenialReason = "another action or changed pull request facts blocked admission"
			return admission, nil
		}
		return admission, err
	}
	slog.InfoContext(ctx, "admitted Forgejo label action intent",
		"intent_id", intent.ID, "repository", intent.Repository, "ags_pr", intent.AGSPRNumber,
		"forgejo_repo", intent.ForgejoRepo, "forgejo_pr", intent.ForgejoPRNumber,
		"request_actor", intent.RequestActor, "principal_id", intent.PrincipalID, "binding_revision", intent.RequestBindingRev)
	if intent.State == ForgejoActionIntentPlanned {
		intent, err = s.beginForgejoActionIntentDispatch(ctx, intent.ID)
		if err != nil {
			return admission, err
		}
		if intent.State == ForgejoActionIntentDispatching {
			intent, err = s.completeForgejoActionIntentDispatch(ctx, intent.ID)
			if err != nil {
				return admission, err
			}
		}
	}
	if !stringInSlice(intent.State, forgejoActionDispatchStates()) {
		admission.DenialReason = "label action intent is not dispatchable"
		admission.Intent = intent
		return admission, nil
	}
	admission.Intent, admission.Admitted = intent, true
	return admission, nil
}

func forgejoLabelActionIdempotencyKey(event forgejointegration.PullRequestActionLabelEvent) string {
	facts := struct {
		Delivery string `json:"delivery"`
		Repo     string `json:"repo"`
		PR       int    `json:"pr"`
		Head     string `json:"head"`
		Base     string `json:"base"`
		Actor    string `json:"actor"`
	}{
		Delivery: strings.TrimSpace(event.CorrelationID), Repo: strings.TrimSpace(event.RepoFullName), PR: event.PRNumber,
		Head: event.HeadSHA, Base: strings.TrimSpace(event.BaseBranch), Actor: strings.ToLower(strings.TrimSpace(event.SenderLogin)),
	}
	encoded, _ := json.Marshal(facts)
	digest := sha256.Sum256(encoded)
	return "forgejo-label:" + hex.EncodeToString(digest[:])
}
